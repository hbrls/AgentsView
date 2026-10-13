package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func (s *Server) registerSyncRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Sync")

	s.stream(group, http.MethodPost, "/sync", "Trigger sync", s.humaTriggerSync)
	s.stream(group, http.MethodPost, "/resync", "Trigger full resync", s.humaTriggerResync)
	s.get(group, "/sync/status", "Get sync status", s.humaSyncStatus)
	s.postLong(group, "/sessions/sync", "Sync a session", s.humaSyncSession)
}

type syncStatusResponse struct {
	LastSync string             `json:"last_sync"`
	Stats    *syncpkg.SyncStats `json:"stats"`
	Progress *syncpkg.Progress  `json:"progress,omitempty"`
}

type sessionSyncInput struct {
	Body service.SyncInput
}

type syncInput struct {
	Wait        bool `query:"wait" default:"false" doc:"Wait for an active sync or maintenance pass before starting this sync"`
	StartupOnly bool `query:"startup_only" default:"false" doc:"Complete deferred startup ingestion without repeating a completed startup sync"`
}

func requireProcessingComplete(stats syncpkg.SyncStats) error {
	if stats.ProcessingComplete() {
		return nil
	}
	return errors.New("local sync processing incomplete")
}

func (s *Server) humaSyncStatus(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[syncStatusResponse], error) {
	engine := s.syncStatusEngine()
	if engine == nil {
		return &jsonOutput[syncStatusResponse]{Body: syncStatusResponse{}}, nil
	}
	lastSync := engine.LastSync()
	stats := engine.LastSyncStats()
	var lastSyncStr string
	if !lastSync.IsZero() {
		lastSyncStr = lastSync.Format(time.RFC3339)
	}
	var progress *syncpkg.Progress
	if p, ok := engine.CurrentProgress(); ok {
		progress = &p
	}
	return &jsonOutput[syncStatusResponse]{
		Body: syncStatusResponse{
			LastSync: lastSyncStr,
			Stats:    &stats,
			Progress: progress,
		},
	}, nil
}

func (s *Server) syncStatusEngine() *syncpkg.Engine {
	if s.engine != nil {
		return s.engine
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDemandEngine
}

func (s *Server) syncEngineForRequest(ctx context.Context) (*syncpkg.Engine, error) {
	if s.engine != nil {
		return s.engine, nil
	}
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return s.syncEngineForLocal(ctx, local), nil
}

func (s *Server) syncEngineForLocal(ctx context.Context, local *db.DB) *syncpkg.Engine {
	if s.engine != nil {
		return s.engine
	}
	cfg := s.ingestionConfig()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onDemandEngine != nil {
		return s.onDemandEngine
	}
	var emitter syncpkg.Emitter
	if s.broadcaster != nil {
		emitter = s.broadcaster
	}
	// Initialization belongs to the cached engine, not its first request.
	initCtx := s.baseCtx
	if initCtx == nil {
		initCtx = context.WithoutCancel(ctx)
	}
	s.onDemandEngine = syncpkg.NewEngine(initCtx, local, syncpkg.EngineConfig{
		AgentDirs:               cfg.AgentDirs,
		SourceMachines:          cfg.SourceMachines,
		ProviderMetadata:        cfg.ProviderMetadata,
		DisabledAgents:          cfg.DisabledAgents,
		IncludeCwdPrefixes:      cfg.SyncIncludeCwdPrefixes,
		ScanProtectedPaths:      cfg.ScanProtectedPaths,
		Machine:                 cfg.InstallationID,
		BlockedResultCategories: cfg.ResultContentBlockedCategories,
		ArchiveContent:          cfg.ArchiveContent,
		Emitter:                 emitter,
	})
	return s.onDemandEngine
}

func (s *Server) humaTriggerSync(
	ctx context.Context,
	in *syncInput,
) (*huma.StreamResponse, error) {
	engine := s.engine
	if !in.StartupOnly {
		var err error
		engine, err = s.syncEngineForRequest(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := s.rejectStaleArchiveForSync(); err != nil {
		return nil, err
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			stats, err := s.runSyncWhenReady(ctx, engine, *in, nil)
			if err != nil {
				writeHumaJSON(hctx, http.StatusInternalServerError,
					apiResponseError{Message: err.Error()})
				return
			}
			writeHumaJSON(hctx, http.StatusOK, stats)
			return
		}
		stats, err := s.runSyncWhenReady(ctx, engine, *in, func(p syncpkg.Progress) {
			stream.SendJSON("progress", p)
		})
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

// runSyncWhenReady lets CLI callers wait without changing the fail-fast
// behavior for other clients. ErrSyncInProgress means the runner never acquired
// the engine lock; retrying it cannot repeat a partially completed sync.
func (s *Server) runSyncWhenReady(
	ctx context.Context, engine *syncpkg.Engine, in syncInput,
	progress func(syncpkg.Progress),
) (syncpkg.SyncStats, error) {
	for {
		if err := ctx.Err(); err != nil {
			return syncpkg.SyncStats{}, err
		}
		if in.StartupOnly && (engine == nil || engine.StartupReconciled()) {
			return syncpkg.SyncStats{}, nil
		}
		stats, err := s.runSyncWithResyncFallback(ctx, engine, progress)
		if !in.Wait || !errors.Is(err, syncpkg.ErrSyncInProgress) {
			return stats, err
		}
		if progress != nil {
			progress(syncpkg.Progress{
				Detail: "Waiting for the daemon's active sync or maintenance to finish",
				Hint:   "Ctrl+C to cancel; agentsview daemon status to inspect",
			})
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return syncpkg.SyncStats{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// ResyncRequiredHeader marks a /sync rejection that requires a full resync, so
// the CLI can retry through /api/v1/resync instead of surfacing a raw HTTP
// error. The body remains the human-readable explanation.
const ResyncRequiredHeader = "X-Agentsview-Resync-Required"

// rejectStaleArchiveForSync fails a worker-backed /sync when the archive's data
// version changed, because the worker sync pass refuses to swap a stale archive
// under the live daemon. It points the caller at /resync, which rebuilds through
// the resync-build flow; it deliberately does not auto-trigger a resync. The
// in-process path (no worker runner) safely resyncs a stale archive itself, so
// the gate only applies when the worker-backed runner is wired.
func (s *Server) rejectStaleArchiveForSync() error {
	if s.localSyncRunner == nil {
		return nil
	}
	local, ok := s.db.(*db.DB)
	if !ok || !local.NeedsResync() {
		return nil
	}
	return huma.ErrorWithHeaders(
		apiError(
			http.StatusConflict,
			"archive data version changed; POST /api/v1/resync to rebuild the "+
				"archive before syncing",
		),
		http.Header{ResyncRequiredHeader: []string{"true"}},
	)
}

func (s *Server) runSyncWithResyncFallback(
	ctx context.Context, engine *syncpkg.Engine,
	progress func(syncpkg.Progress),
) (syncpkg.SyncStats, error) {
	if s.localSyncRunner != nil {
		// The worker-backed "sync" runner refuses a stale-version archive rather
		// than swap it under the live daemon (see runSyncWorkerStartup); callers
		// gate on NeedsResync before reaching here and route rebuilds through the
		// resync-build flow. It only returns an error when the worker ran and
		// reported failure, so propagate it to the caller instead of reporting
		// the failed pass as a successful sync.
		stats, err := s.localSyncRunner(ctx, progress)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, syncpkg.ErrSyncInProgress) {
				log.Printf("foreground local sync: %v", err)
			}
			return stats, err
		}
		return stats, requireProcessingComplete(stats)
	}
	stats, err := engine.SyncThenRun(
		ctx, false, progress, func(bool) error { return nil },
	)
	if err != nil {
		return stats, err
	}
	return stats, requireProcessingComplete(stats)
}

func (s *Server) humaTriggerResync(
	ctx context.Context,
	_ *emptyInput,
) (*huma.StreamResponse, error) {
	engine, err := s.syncEngineForRequest(ctx)
	if err != nil {
		return nil, err
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			stats, err := s.runResyncWithFallback(ctx, engine, nil)
			if err != nil {
				writeHumaJSON(hctx, http.StatusInternalServerError,
					apiResponseError{Message: err.Error()})
				return
			}
			writeHumaJSON(hctx, http.StatusOK, stats)
			return
		}
		stats, err := s.runResyncWithFallback(ctx, engine, func(p syncpkg.Progress) {
			stream.SendJSON("progress", p)
		})
		if err != nil {
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) runResyncWithFallback(
	ctx context.Context, engine *syncpkg.Engine,
	progress func(syncpkg.Progress),
) (syncpkg.SyncStats, error) {
	if s.localResyncRunner != nil {
		// The worker-backed runner builds the replacement archive in a child
		// process behind a write barrier and swaps it in. It only returns an
		// error when the worker ran and reported failure, so propagate it to
		// the caller instead of reporting the failed pass as a success.
		stats, err := s.localResyncRunner(ctx, progress)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("foreground resync: %v", err)
			}
			return stats, err
		}
		return stats, requireProcessingComplete(stats)
	}
	stats, err := engine.SyncThenRun(
		ctx, true, progress, func(bool) error { return nil },
	)
	if err != nil {
		return stats, err
	}
	return stats, requireProcessingComplete(stats)
}

func (s *Server) sessionSyncService(ctx context.Context) service.SessionService {
	if s.engine == nil {
		if local, ok := s.db.(*db.DB); ok {
			return service.NewDirectBackend(
				local, s.syncEngineForLocal(ctx, local),
			)
		}
	}
	return s.sessions
}

func (s *Server) humaSyncSession(
	ctx context.Context,
	in *sessionSyncInput,
) (*jsonOutput[*service.SessionDetail], error) {
	if (in.Body.Path == "" && in.Body.ID == "") ||
		(in.Body.Path != "" && in.Body.ID != "") {
		return nil, apiError(http.StatusBadRequest, "exactly one of 'path' or 'id' is required")
	}
	if in.Body.Subagents && in.Body.ID == "" {
		return nil, apiError(http.StatusBadRequest, "'subagents' requires 'id'")
	}
	if err := s.resyncBeforeSessionSync(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, nil
		}
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	detail, err := s.sessionSyncService(ctx).Sync(ctx, in.Body)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, nil
		}
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		if errors.Is(err, db.ErrSessionExcluded) ||
			errors.Is(err, db.ErrSessionTrashed) {
			return nil, apiError(http.StatusConflict, err.Error())
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &jsonOutput[*service.SessionDetail]{Body: detail}, nil
}

func (s *Server) resyncBeforeSessionSync(ctx context.Context) error {
	local, ok := s.db.(*db.DB)
	if !ok || !local.NeedsResync() {
		return nil
	}
	engine, err := s.syncEngineForRequest(ctx)
	if err != nil {
		return err
	}
	if _, err := s.runResyncWithFallback(ctx, engine, nil); err != nil {
		return err
	}
	return ctx.Err()
}
