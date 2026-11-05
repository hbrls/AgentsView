package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	stdlibsync "sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type syncRouteFixture struct {
	dir       string
	dbPath    string
	claudeDir string
	db        *db.DB
	srv       *Server
	handler   http.Handler
}

type syncRouteFixtureConfig struct {
	stale          bool
	archiveContent config.ArchiveContent
	disabledAgents []parser.AgentType
	extraAgentDirs map[parser.AgentType][]string
	broadcaster    *Broadcaster
	engine         *syncpkg.Engine
	syncRunner     LocalSyncRunner
	resyncRunner   LocalResyncRunner
	version        string
}

type syncRouteFixtureOption func(*syncRouteFixtureConfig)

func withStaleDB() syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.stale = true }
}

func withUsageOnlyStorage() syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) {
		c.archiveContent = config.ArchiveContentUsage
	}
}

func withVersion(version string) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.version = version }
}

func withLocalSyncRunner(r LocalSyncRunner) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.syncRunner = r }
}

func withLocalResyncRunner(r LocalResyncRunner) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.resyncRunner = r }
}

func withDisabledAgents(
	disabled []parser.AgentType,
	dirs map[parser.AgentType][]string,
) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) {
		c.disabledAgents = append([]parser.AgentType(nil), disabled...)
		c.extraAgentDirs = dirs
	}
}

func newSyncRouteFixture(
	t *testing.T,
	opts ...syncRouteFixtureOption,
) *syncRouteFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	claudeDir := filepath.Join(dir, "claude")

	var cfg syncRouteFixtureConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.stale {
		dbtest.EnsureTestDBAt(t, dbPath)
		markDBStale(t, dbPath)
	}

	var database *db.DB
	var err error
	if cfg.stale {
		database, err = db.Open(t.Context(), dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { database.Close() })
	} else {
		database = dbtest.OpenTestDBAt(t, dbPath)
	}

	serverConfig := config.Config{
		Host:           "127.0.0.1",
		Port:           0,
		DataDir:        dir,
		DBPath:         dbPath,
		WriteTimeout:   30 * time.Second,
		ArchiveContent: cfg.archiveContent,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		DisabledAgents: cfg.disabledAgents,
	}
	for agent, dirs := range cfg.extraAgentDirs {
		serverConfig.AgentDirs[agent] = append([]string(nil), dirs...)
	}
	serverOptions := []Option{}
	if cfg.broadcaster != nil {
		serverOptions = append(serverOptions, WithBroadcaster(cfg.broadcaster))
	}
	if cfg.syncRunner != nil {
		serverOptions = append(serverOptions, WithLocalSyncRunner(cfg.syncRunner))
	}
	if cfg.resyncRunner != nil {
		serverOptions = append(serverOptions,
			WithLocalResyncRunner(cfg.resyncRunner))
	}
	if cfg.version != "" {
		serverOptions = append(serverOptions, WithVersion(VersionInfo{Version: cfg.version}))
	}
	srv := New(serverConfig, database, cfg.engine, serverOptions...)
	return &syncRouteFixture{
		dir:       dir,
		dbPath:    dbPath,
		claudeDir: claudeDir,
		db:        database,
		srv:       srv,
		handler:   srv.Handler(),
	}
}

func (f *syncRouteFixture) writeClaudeSession(
	t *testing.T,
	relPath string,
	firstMessage string,
) string {
	t.Helper()
	return f.writeClaudeTranscript(t, relPath, testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", firstMessage).String())
}

func (f *syncRouteFixture) writeClaudeTranscript(t *testing.T, relPath, content string) string {
	t.Helper()
	path := filepath.Join(f.claudeDir, relPath)
	dbtest.WriteTestFile(t, path, []byte(content))
	return path
}

func markDBStale(t *testing.T, dbPath string) {
	t.Helper()

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), "PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())
}

type syncRouteRequestOption func(*http.Request)

func withAccept(value string) syncRouteRequestOption {
	return func(req *http.Request) { req.Header.Set("Accept", value) }
}

func serveJSON(
	t *testing.T,
	h http.Handler,
	method string,
	path string,
	body any,
	opts ...syncRouteRequestOption,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://127.0.0.1:0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func postSessionSync(
	t *testing.T,
	h http.Handler,
	sessionPath string,
) *httptest.ResponseRecorder {
	t.Helper()
	return serveJSON(t, h, http.MethodPost, "/api/v1/sessions/sync",
		service.SyncInput{Path: sessionPath})
}

func decodeRecorder[T any](
	t *testing.T,
	w *httptest.ResponseRecorder,
) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func assertFirstMessageContains(t *testing.T, msg *string, want string) {
	t.Helper()
	require.NotNil(t, msg)
	assert.Contains(t, *msg, want)
}

func assertOnlySessionFirstMessageContains(
	t *testing.T,
	database *db.DB,
	want string,
) {
	t.Helper()
	page, err := database.ListSessions(t.Context(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assertFirstMessageContains(t, page.Sessions[0].FirstMessage, want)
}

func assertSessionCount(t *testing.T, database *db.DB, want int) {
	t.Helper()
	page, err := database.ListSessions(t.Context(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, want)
}

func TestSyncEngineForLocalReusesNoSyncEngineConcurrently(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)

	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
	}, database, nil)

	const workers = 8
	engines := make([]*syncpkg.Engine, workers)
	var wg stdlibsync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			engines[i] = srv.syncEngineForLocal(t.Context(), database)
		}()
	}
	wg.Wait()

	require.NotNil(t, engines[0])
	for _, engine := range engines[1:] {
		assert.Same(t, engines[0], engine)
	}
}

func TestArchiveMaintenanceNoSyncSharesBarrier(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Server, context.Context, func() error) error
	}{
		{"foreground", (*Server).tryArchiveWrite},
		{"background", (*Server).serializeArchiveWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			t.Cleanup(func() { require.NoError(t, f.srv.Shutdown(t.Context())) })
			started := make(chan struct{})
			unblock := make(chan struct{})
			release := stdlibsync.OnceFunc(func() { close(unblock) })
			done := make(chan error, 1)
			go func() {
				done <- tc.run(f.srv, t.Context(), func() error {
					close(started)
					<-unblock
					return nil
				})
			}()
			t.Cleanup(func() {
				release()
				require.NoError(t, <-done)
			})
			<-started

			for _, request := range []struct {
				path string
				body map[string]any
			}{
				{"/api/v1/data/compact", map[string]any{}},
				{"/api/v1/data/strip-images", map[string]any{"confirmed": true}},
			} {
				payload, err := json.Marshal(request.body)
				require.NoError(t, err)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, request.path, bytes.NewReader(payload))
				req.Host = "127.0.0.1:0"
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Origin", "http://127.0.0.1:0")
				req.Header.Set("Content-Type", "application/json")
				response := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, req)
					response <- w
				}()
				select {
				case w := <-response:
					assert.Equal(t, http.StatusConflict, w.Code, "%s: %s", request.path, w.Body.String())
				case <-time.After(5 * time.Second):
					release()
					<-response
					require.FailNow(t, request.path+" waited for maintenance instead of returning a conflict")
				}
			}
		})
	}
}

func TestSyncEngineForLocalCarriesUsageOnlyStoragePolicy(t *testing.T) {
	f := newSyncRouteFixture(t, withUsageOnlyStorage())
	f.writeClaudeSession(t, "proj/private.jsonl", "private prompt")

	stats := f.srv.syncEngineForLocal(t.Context(), f.db).SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	page, err := f.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Nil(t, page.Sessions[0].FirstMessage)

	messages, err := f.db.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	assert.Empty(t, messages,
		"a session without usage or assistant activity needs no message rows")
	for _, message := range messages {
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
	}
}

func TestHumaSyncStatusUsesExistingOnDemandEngine(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)

	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
	}, database, nil)
	engine := srv.syncEngineForLocal(t.Context(), database)
	out, err := srv.humaSyncStatus(t.Context(), &emptyInput{})
	require.NoError(t, err)
	assert.False(t, out.Body.StartupPending)
	engine.SyncAll(t.Context(), nil)

	out, err = srv.humaSyncStatus(t.Context(), &emptyInput{})

	require.NoError(t, err)
	require.NotNil(t, out.Body.Stats)
	assert.Equal(t, engine.LastSyncStats(), *out.Body.Stats)
}

func TestHumaSyncSessionLocalNoSyncUsesOnDemandEngine(t *testing.T) {
	f := newSyncRouteFixture(t)
	sessionPath := f.writeClaudeSession(t, "proj/session.jsonl", "no sync route")
	w := postSessionSync(t, f.handler, sessionPath)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	detail := decodeRecorder[service.SessionDetail](t, w)
	assert.Equal(t, "claude", detail.Agent)
	assertFirstMessageContains(t, detail.FirstMessage, "no sync route")
}

func TestHumaSyncSessionRouteIsNotWriteTimeoutWrapped(t *testing.T) {
	srv := testServer(
		t, 10*time.Millisecond,
		withHandlerDelay(100*time.Millisecond),
	)
	w := serveJSON(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/sync",
		map[string]any{})

	resp := w.Result()
	defer resp.Body.Close()
	assert.False(t, isTimeoutResponse(t, resp))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHumaTriggerSyncLocalNoSyncResyncsStaleDB(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	f.writeClaudeSession(t, "proj/session.jsonl", "stale no sync route")
	require.True(t, f.db.NeedsResync())
	require.Nil(t, f.srv.engine)
	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync", nil)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.False(t, f.db.NeedsResync())
	assertOnlySessionFirstMessageContains(t, f.db, "stale no sync route")
}

// TestHumaTriggerSyncWorkerBackedRejectsStaleArchive pins the new UX: with the
// worker-backed runner wired, /sync on a stale archive returns 409 pointing at
// /resync and never runs the runner, since the worker refuses to swap the
// archive under the live daemon.
func TestHumaTriggerSyncWorkerBackedRejectsStaleArchive(t *testing.T) {
	ran := false
	f := newSyncRouteFixture(t, withStaleDB(), withLocalSyncRunner(
		func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
			ran = true
			return syncpkg.SyncStats{}, nil
		},
	))
	require.True(t, f.db.NeedsResync())

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync", nil)

	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "resync")
	assert.Equal(t, "true", w.Header().Get(ResyncRequiredHeader),
		"the rejection must carry the machine-readable resync signal for the CLI")
	assert.False(t, ran, "the worker-backed runner must not run for a stale archive")
	assert.True(t, f.db.NeedsResync(), "a rejected sync must not resync")
}

func TestHumaForegroundSyncTerminalErrors(t *testing.T) {
	for _, path := range []string{"/api/v1/sync", "/api/v1/resync"} {
		for _, tc := range []struct {
			name  string
			stats syncpkg.SyncStats
			err   error
			code  string
		}{
			{"complete stats", syncpkg.SyncStats{}, errors.New("worker failed"), ""},
			{"incomplete without worker error", syncpkg.SyncStats{Deferred: 1}, nil, ""},
			{"busy", syncpkg.SyncStats{}, syncpkg.ErrSyncInProgress, "sync_running"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				runner := func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) { return tc.stats, tc.err }
				f := newSyncRouteFixture(t, withLocalSyncRunner(runner), withLocalResyncRunner(runner))
				body := serveJSON(t, f.handler, http.MethodPost, path, nil).Body.String()
				assert.Contains(t, body, "event: error")
				assert.NotContains(t, body, "event: done")
				if tc.err == nil {
					assert.Contains(t, body, "local sync processing incomplete")
				} else {
					assert.Contains(t, body, tc.err.Error())
				}
				if tc.code == "" {
					assert.NotContains(t, body, `"code"`)
				} else {
					assert.Contains(t, body, `"code":"`+tc.code+`"`)
				}
			})
		}
	}
}

func TestHumaTriggerSyncRetryDecision(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		err        error
		retry      bool
	}{
		{"busy with wait", "/api/v1/sync?wait=true", syncpkg.ErrSyncInProgress, true},
		{"busy without wait", "/api/v1/sync", syncpkg.ErrSyncInProgress, false},
		{"worker failed with wait", "/api/v1/sync?wait=true", errors.New("worker failed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			require.NoError(t, f.db.CloseWriter())
			calls := 0
			f.srv.localSyncRunner = func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
				calls++
				if calls == 1 {
					return syncpkg.SyncStats{}, tc.err
				}
				return syncpkg.SyncStats{}, f.db.ReopenWriter()
			}
			w := serveJSON(t, f.handler, http.MethodPost, tc.path, nil)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Empty(t, w.Header().Get("X-Agentsview-Archive-Stale"))
			if tc.retry {
				assert.Contains(t, w.Body.String(), "event: done")
				assert.Equal(t, 2, calls)
				assert.False(t, f.db.WriterClosed())
			} else {
				assert.Contains(t, w.Body.String(), "event: error")
				assert.Contains(t, w.Body.String(), tc.err.Error())
				assert.Equal(t, 1, calls)
				require.NoError(t, f.db.ReopenWriter())
			}
		})
	}
}

func TestForegroundSyncReleasesDeferredStartupMaintenance(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Server, *syncpkg.Engine)
	}{
		{
			name: "sync",
			run: func(srv *Server, engine *syncpkg.Engine) {
				srv.runSyncWithResyncFallback(
					t.Context(), engine, nil,
				)
			},
		},
		{
			name: "resync",
			run: func(srv *Server, engine *syncpkg.Engine) {
				srv.runResyncWithFallback(
					t.Context(), engine, nil,
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
				Machine:                 "local",
				DeferStartupMaintenance: true,
			})
			t.Cleanup(engine.Close)
			srv := &Server{db: database}

			maintenanceStarted := make(chan struct{})
			maintenanceDone := make(chan error, 1)
			go func() {
				maintenanceDone <- engine.RunStartupMaintenance(
					t.Context(),
					func() error {
						close(maintenanceStarted)
						return nil
					},
				)
			}()
			assert.Never(t, func() bool {
				select {
				case <-maintenanceStarted:
					return true
				default:
					return false
				}
			}, 100*time.Millisecond, 10*time.Millisecond,
				"maintenance started before foreground synchronization")

			tt.run(srv, engine)
			select {
			case err := <-maintenanceDone:
				require.NoError(t, err)
			case <-time.After(time.Second):
				require.FailNow(t,
					"foreground synchronization did not release maintenance")
			}
		})
	}
}

func TestCanceledForegroundSyncLeavesStartupFallbackEligible(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	srv := &Server{db: database}

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	cancelRequest()
	srv.runSyncWithResyncFallback(requestCtx, engine, nil)

	_, ran, err := engine.RunStartupSyncFallback(t.Context(), nil)
	require.NoError(t, err)
	assert.True(t, ran,
		"a canceled HTTP sync must leave daemon startup recovery eligible")
}

func TestHumaSyncSessionRejectsStaleDB(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	sessionPath := f.writeClaudeSession(t, "proj/session.jsonl",
		"stale session sync route")
	require.True(t, f.db.NeedsResync())
	require.NoError(t, f.db.CloseWriter())
	w := postSessionSync(t, f.handler, sessionPath)

	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "true", w.Header().Get(ResyncRequiredHeader))
	assert.True(t, f.db.NeedsResync())
}

func TestHumaSyncSessionCanceledServiceSyncReturnsNil(t *testing.T) {
	f := newSyncRouteFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := f.srv.humaSyncSession(ctx, &sessionSyncInput{
		Body: service.SyncInput{Path: filepath.Join(f.dir, "missing.jsonl")},
	})

	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestOnDemandSyncEngineExcludesDisabledProvider(t *testing.T) {
	geminiDir := filepath.Join(t.TempDir(), "gemini")
	f := newSyncRouteFixture(t, withDisabledAgents(
		[]parser.AgentType{parser.AgentGemini},
		map[parser.AgentType][]string{parser.AgentGemini: {geminiDir}},
	))

	engine := f.srv.syncEngineForLocal(t.Context(), f.db)

	assert.Empty(t, engine.ReconciliationRootsForAgent(string(parser.AgentGemini)))
	assert.Equal(t, []string{f.claudeDir},
		engine.ReconciliationRootsForAgent(string(parser.AgentClaude)))
}

func TestOnDemandSyncEngineKeepsStartupProvidersUntilRestart(t *testing.T) {
	geminiDir := filepath.Join(t.TempDir(), "gemini")
	f := newSyncRouteFixture(t, withDisabledAgents(nil,
		map[parser.AgentType][]string{parser.AgentGemini: {geminiDir}},
	))
	f.srv.mu.Lock()
	f.srv.cfg.DisabledAgents = []parser.AgentType{parser.AgentGemini}
	f.srv.mu.Unlock()

	engine := f.srv.syncEngineForLocal(t.Context(), f.db)

	assert.Equal(t, []string{geminiDir},
		engine.ReconciliationRootsForAgent(string(parser.AgentGemini)))
}

func TestStartupRebuildWriteGate(t *testing.T) {
	t.Setenv("AGENTSVIEW_GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	originalTokenLookup := ghAuthTokenOutput
	ghAuthTokenOutput = func(context.Context) ([]byte, error) { return nil, errors.New("test has no credentials") }
	t.Cleanup(func() { ghAuthTokenOutput = originalTokenLookup })
	f := newSyncRouteFixture(t, withStaleDB())
	dbtest.SeedSessionWithMessages(t, f.db, "retained", "project", []db.Message{dbtest.UserMsg("retained", 0, "retained transcript"), dbtest.UserMsg("retained", 1, "follow-up")}, dbtest.WithMessageCounts(2, 2), func(session *db.Session) { session.Cwd = "/work/branch" })
	syncCalls := 0
	f.srv.localSyncRunner = func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
		syncCalls++
		return syncpkg.SyncStats{}, errors.New("startup rebuild must not ingest")
	}
	require.NoError(t, f.db.CloseWriter())
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	f.srv.engine = engine
	require.False(t, engine.StartupReconciled())
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- engine.RunExclusive(func() error { close(locked); <-release; return nil })
	}()
	t.Cleanup(func() { close(release); require.NoError(t, <-done) })
	<-locked
	direct := serveJSON(t, f.srv.mux, http.MethodPatch, "/api/v1/sessions/retained/rename", map[string]any{"display_name": "renamed"})
	require.Equal(t, http.StatusServiceUnavailable, direct.Code, direct.Body.String())
	assert.Equal(t, writerClosedRetryAfterSeconds, direct.Header().Get("Retry-After"))
	assert.Contains(t, direct.Body.String(), "archive is read-only while it rebuilds")
	for _, tc := range []struct {
		method, path, body string
		blocked, probe     bool
		status             int
		response           string
	}{
		{http.MethodPost, "/api/v1/sync?wait=true", "", true, false, http.StatusServiceUnavailable, ""},
		{http.MethodPost, "/api/v1/sync?startup_only=true", "", false, false, http.StatusOK, ""},
		{http.MethodPost, "/api/v1/sync?startup_only", "", false, false, http.StatusOK, ""},
		{http.MethodPost, "/api/v1/recall/import?dry_run=true", `{"candidate_id":"dry-run","type":"debugging_method","scope":"repository","title":"Check transcript","body":"Read the retained transcript.","project":"project","agent":"claude","session_id":"retained","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":1}}`, false, false, http.StatusOK, ""},
		{http.MethodPatch, "/api/v1/sessions/retained/rename", `{"display_name":"renamed"}`, true, false, http.StatusServiceUnavailable, ""},
		{http.MethodPost, "/api/v1/config/github", `{"token":" "}`, false, false, http.StatusBadRequest, "token required"},
		{http.MethodPost, "/api/v1/sessions/retained/publish", `{}`, false, false, http.StatusUnauthorized, "GitHub token not configured"},
		{http.MethodPost, "/api/v1/sessions/retained/resume", `{"command_only":true}`, false, false, http.StatusOK, "command"},
		{http.MethodPost, "/api/v1/sessions/retained/open", `{}`, false, false, http.StatusBadRequest, "opener"},
		{http.MethodPost, "/api/v1/settings/worktree-mappings/preview", `{"machine":"local","path_prefix":"/work","project":"new-project","enabled":true}`, false, false, http.StatusOK, ""},
		{http.MethodGet, "/api/v1/sessions", "", false, false, http.StatusOK, ""},
		{http.MethodGet, "/api/v1/sync/status", "", false, true, http.StatusOK, ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body any
			if tc.body != "" {
				require.NoError(t, json.Unmarshal([]byte(tc.body), &body))
			}
			w := serveJSON(t, f.handler, tc.method, tc.path, body)
			if strings.HasPrefix(tc.path, "/api/v1/sync?startup_only") {
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Zero(t, syncCalls)
			}
			if tc.path == "/api/v1/recall/import?dry_run=true" {
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), `"would_import":1`)
				assert.Contains(t, w.Body.String(), `"imported":0`)
			}
			if tc.path == "/api/v1/sessions" {
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var page service.SessionList
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
				require.Len(t, page.Sessions, 1)
				assert.Equal(t, "retained", page.Sessions[0].ID)
			}
			if tc.path == "/api/v1/settings/worktree-mappings/preview" {
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Equal(t, "true", w.Header().Get("X-Agentsview-Archive-Stale"))
				return
			}
			if tc.path == "/api/v1/sync/status" {
				var status map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &status))
				assert.Equal(t, true, status["archive_stale"])
			}
			if tc.probe {
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Empty(t, w.Header().Get("X-Agentsview-Archive-Stale"))
			} else if tc.blocked {
				assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
				assert.Equal(t, writerClosedRetryAfterSeconds, w.Header().Get("Retry-After"))
				assert.Equal(t, "true", w.Header().Get("X-Agentsview-Archive-Stale"))
				assert.Contains(t, w.Body.String(), "archive is read-only while it rebuilds")
			} else {
				assert.Equal(t, tc.status, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), tc.response)
				assert.Equal(t, "true", w.Header().Get("X-Agentsview-Archive-Stale"))
				assert.NotContains(t, w.Body.String(), "archive is read-only")
			}
		})
	}
}

func TestHandlerErrorsPreserveOnlyWriterClosedCause(t *testing.T) {
	for _, convert := range []func(error) error{
		serverError,
		func(err error) error { return internalError("test", err) },
	} {
		wrappedStatus := fmt.Errorf("write failed: %w", apiError(http.StatusConflict, "conflict"))
		status, ok := errors.AsType[huma.StatusError](convert(wrappedStatus))
		require.True(t, ok)
		assert.Equal(t, http.StatusInternalServerError, status.GetStatus())
		assert.ErrorIs(t, convert(fmt.Errorf("write failed: %w", db.ErrWriterClosed)), db.ErrWriterClosed)
	}
}

func TestUsageRefreshRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "nil engine allows refresh when startup sync is disabled",
			run: func(t *testing.T) {
				t.Helper()
				f := newSyncRouteFixture(t)
				require.Nil(t, f.srv.engine)
				content := testjsonl.NewSessionBuilder().
					AddClaudeUserWithSessionID("2026-01-01T10:00:00Z", "start", "fresh").
					AddClaudeAssistantUsage("2026-01-01T10:00:05Z", "done",
						testjsonl.ClaudeAssistantUsage{MessageID: "msg-fresh", OutputTokens: 42}).
					String()
				f.writeClaudeTranscript(t, "project/fresh.jsonl", content)
				engine := f.srv.syncEngineForLocal(t.Context(), f.db)
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "fresh"))
				w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sessions/sync", map[string]any{
					"id": "fresh", "subagents": true, "usage_refresh": true,
				})
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			},
		},
		{
			name: "defers a known session until startup finishes",
			run: func(t *testing.T) {
				t.Helper()
				f := newSyncRouteFixture(t)
				content := testjsonl.NewSessionBuilder().
					AddClaudeUserWithSessionID("2026-01-01T10:00:00Z", "start", "parent").
					AddClaudeAssistantUsage("2026-01-01T10:00:05Z", "done",
						testjsonl.ClaudeAssistantUsage{MessageID: "msg-parent", OutputTokens: 20}).
					String()
				f.writeClaudeTranscript(t, "project/parent.jsonl", content)
				engine := f.srv.syncEngineForLocal(t.Context(), f.db)
				f.srv.engine = engine
				f.srv.sessions = service.NewDirectBackend(f.db, engine)
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "parent"))
				f.writeClaudeTranscript(t, "project/parent.jsonl",
					strings.ReplaceAll(content, `"output_tokens":20`, `"output_tokens":30`))

				body := map[string]any{"id": "parent", "subagents": true, "usage_refresh": true}
				w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sessions/sync", body)
				require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
				usage, err := service.SessionUsageWithSubagents(t.Context(), f.db, "parent", false)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, 20, usage.TotalOutputTokens)

				w = serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync?wait=true", nil)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				w = serveJSON(t, f.handler, http.MethodPost, "/api/v1/sessions/sync", body)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				usage, err = service.SessionUsageWithSubagents(t.Context(), f.db, "parent", false)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, 30, usage.TotalOutputTokens)
			},
		},
		{
			name: "returns conflict while another sync owns the engine",
			run: func(t *testing.T) {
				t.Helper()
				f := newSyncRouteFixture(t)
				dbtest.SeedSessionWithMessages(t, f.db, "retained", "project",
					[]db.Message{dbtest.UserMsg("retained", 0, "stored transcript")})
				engine := f.srv.syncEngineForLocal(t.Context(), f.db)
				f.srv.engine = engine
				f.srv.sessions = service.NewDirectBackend(f.db, engine)
				engine.RecordStartupReconciled(syncpkg.SyncStats{}, nil)
				locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					done <- engine.RunExclusive(func() error {
						close(locked)
						<-release
						return nil
					})
				}()
				<-locked
				t.Cleanup(func() {
					close(release)
					require.NoError(t, <-done)
				})

				w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sessions/sync", map[string]any{
					"id": "retained", "subagents": true, "usage_refresh": true,
				})
				require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			},
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestUsageRefreshValidationPrecedesStartupGate(t *testing.T) {
	f := newSyncRouteFixture(t)
	engine := syncpkg.NewEngine(t.Context(), f.db, syncpkg.EngineConfig{Ephemeral: true})
	t.Cleanup(engine.Close)
	f.srv.engine = engine
	f.srv.sessions = service.NewDirectBackend(f.db, engine)

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sessions/sync", map[string]any{
		"path": "session.jsonl", "usage_refresh": true,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
