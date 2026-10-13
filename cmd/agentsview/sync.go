// ABOUTME: CLI subcommand that syncs session data into the database
// ABOUTME: through the shared daemon or an explicit offline run.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sync"
)

// SyncConfig holds parsed CLI options for the sync command.
type SyncConfig struct {
	Full bool
	// CPUProfile, MemProfile, and Trace are hidden flags that capture a
	// pprof CPU profile, allocation snapshot, and runtime trace for the
	// sync pass. Empty strings disable each independently.
	CPUProfile string
	MemProfile string
	Trace      string
}

func runSync(cfg SyncConfig) {
	if doSync(cfg) {
		os.Exit(1)
	}
}

func doSync(cfg SyncConfig) (failed bool) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}

	setupLogFile(appCfg.DataDir)

	stopProfile := startSyncProfile(cfg)
	defer stopProfile()

	applyClassifierConfig(appCfg)

	{
		operation := "sync"
		if cfg.Full {
			operation = "full sync"
		}
		fmt.Printf("Preparing %s...\n", operation)
		appCfg.SkipInitialSync = true
		tr, err := ensureTransport(
			&appCfg, transportIntentArchiveWrite, 0,
		)
		if err != nil {
			fatal("detecting daemon: %v", err)
		}
		if tr.Mode == transportHTTP {
			useDaemon := useDaemonForSync(tr)
			if useDaemon {
				fmt.Printf("Server: %s\n", tr.URL)
				fmt.Println("  Remains running after sync; stop with `agentsview daemon stop`.")
			}
			if useDaemon {
				start := time.Now()
				var onProgress sync.ProgressFunc
				var progress *resyncProgressPrinter
				if cfg.Full {
					fmt.Println("Running full resync via daemon...")
					progress = newResyncProgressPrinter(os.Stdout, time.Now)
					onProgress = progress.Print
				} else {
					fmt.Println("Running sync via daemon...")
					onProgress = newSyncProgressPrinter(os.Stdout)
				}
				stats, err := runDaemonSync(
					context.Background(), tr, appCfg.AuthToken, cfg.Full,
					onProgress,
				)
				if progress != nil {
					progress.Finish()
				}
				if errors.Is(err, errDaemonResyncRequired) {
					// The archive's data version changed and the
					// worker-backed daemon refuses to swap it under itself
					// via /sync; the dedicated resync route rebuilds and
					// swaps safely, preserving the previously automatic
					// upgrade behavior.
					fmt.Println(
						"Archive data version changed; running full resync via daemon...",
					)
					progress = newResyncProgressPrinter(os.Stdout, time.Now)
					stats, err = runDaemonSync(
						context.Background(), tr, appCfg.AuthToken, true,
						progress.Print,
					)
					progress.Finish()
				}
				if err != nil {
					fatal("daemon sync: %v", err)
				}
				printSyncSummary(stats, start)
				return false
			}
		}
		if tr.DirectReadOnly {
			fatal(
				"local daemon owns the SQLite archive but is not " +
					"responding; refusing to sync directly",
			)
		}
	}

	database, writeLock, err := openWriteDB(context.Background(), appCfg)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer closeWriteDB(database, writeLock)

	runLocalSync(context.Background(), appCfg, database, cfg.Full)
	return false
}

func useDaemonForSync(tr transport) bool {
	if tr.Mode != transportHTTP {
		return false
	}
	if tr.ReadOnly {
		return false
	}
	return true
}

// runLocalSync runs a local sync (incremental or full resync).
// It returns true if a full resync was performed, which callers
// can use to force a full PG push (watermarks become stale after
// a local resync).
func runLocalSync(
	ctx context.Context, appCfg config.Config, database *db.DB, full bool,
) bool {
	didResync, _, err := runLocalSyncResult(ctx, appCfg, database, full)
	if err != nil {
		log.Printf("local sync failed: %v", err)
	}
	return didResync
}

// runLocalSyncAuthoritative runs a local sync and returns an error unless its
// provider discovery completed authoritatively. Push-watch callers use it so
// a mirror update cannot acknowledge watcher reconciliation that never
// established a complete view of the local sources.
func runLocalSyncAuthoritative(
	ctx context.Context, appCfg config.Config, database *db.DB, full bool,
) (bool, error) {
	didResync, stats, err := runLocalSyncResult(ctx, appCfg, database, full)
	if err != nil {
		return didResync, err
	}
	if !stats.AuthoritativeDiscoveryComplete() {
		return didResync, errors.New("local sync discovery incomplete")
	}
	if !stats.ProcessingComplete() {
		return didResync, errors.New("local sync processing incomplete")
	}
	return didResync, nil
}

func runLocalSyncResult(
	ctx context.Context, appCfg config.Config, database *db.DB, full bool,
) (bool, sync.SyncStats, error) {
	didResync := full || database.NeedsResync()
	var progress sync.ProgressFunc
	var resyncProgress *resyncProgressPrinter
	if didResync {
		fmt.Println("Data version changed, running full resync...")
		resyncProgress = newResyncProgressPrinter(os.Stdout, time.Now)
		progress = resyncProgress.Print
	} else {
		fmt.Println("Running initial sync...")
		progress = newSyncProgressPrinter(os.Stdout)
	}
	started := time.Now()
	didResync, stats, err := coordinateLocalSync(
		ctx, appCfg, database, full, progress,
	)
	if resyncProgress != nil {
		resyncProgress.Finish()
	}
	printDirectSyncResult(ctx, database, stats, started)
	return didResync, stats, err
}

func coordinateLocalSync(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	full bool,
	progress sync.ProgressFunc,
) (didResync bool, stats sync.SyncStats, err error) {
	didResync = full || database.NeedsResync()
	for _, def := range parser.Registry {
		if !appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(
			appCfg.ResolveDirs(def.Type),
			string(def.Type),
		)
	}

	cleanResyncTemp(appCfg.DBPath)

	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs:               appCfg.AgentDirs,
		SourceMachines:          appCfg.SourceMachines,
		ProviderMetadata:        appCfg.ProviderMetadata,
		DisabledAgents:          appCfg.DisabledAgents,
		IncludeCwdPrefixes:      appCfg.SyncIncludeCwdPrefixes,
		ScanProtectedPaths:      appCfg.ScanProtectedPaths,
		Machine:                 appCfg.InstallationID,
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
		ArchiveContent:          appCfg.ArchiveContent,
	})
	defer engine.Close()

	stats, err = engine.SyncThenRun(ctx, full, progress, func(bool) error { return nil })
	engine.PhaseStats().Log("sync")
	if err != nil {
		return didResync, stats, err
	}
	if !stats.ProcessingComplete() {
		return didResync, stats, errors.New("local sync processing incomplete")
	}
	return didResync, stats, nil
}

func printDirectSyncResult(
	ctx context.Context,
	database *db.DB,
	stats sync.SyncStats,
	started time.Time,
) {
	printSyncSummary(stats, started)
	fmt.Println()
	databaseStats, err := database.GetStats(
		ctx, false, false,
	)
	if err == nil {
		fmt.Printf(
			"Database: %d sessions, %d messages\n",
			databaseStats.SessionCount, databaseStats.MessageCount,
		)
	}
}

// errDaemonResyncRequired marks a /sync rejected because the archive's data
// version changed: the worker-backed daemon will not swap a stale archive
// under itself, so the CLI must retry through /api/v1/resync.
var errDaemonResyncRequired = errors.New("daemon requires a full resync")

func runDaemonSync(
	ctx context.Context,
	tr transport,
	authToken string,
	full bool,
	onProgress sync.ProgressFunc,
) (sync.SyncStats, error) {
	api, err := apiclient.NewHTTPClient(tr.URL, authToken, &http.Client{Timeout: 0})
	if err != nil {
		return sync.SyncStats{}, err
	}
	var resp *http.Response
	var body []byte
	var stream *runtime.Stream[[]byte]
	if full {
		response, requestErr := api.PostAPIV1ResyncStreamWithResponse(ctx)
		if response == nil {
			return sync.SyncStats{}, requestErr
		}
		resp, body, stream = response.HTTPResponse, response.Body, response.Stream200
	} else {
		response, requestErr := api.PostAPIV1SyncStreamWithResponse(ctx, &apiclient.PostAPIV1SyncRequestOptions{Query: &apiclient.PostAPIV1SyncQuery{Wait: new(true)}})
		if response == nil {
			return sync.SyncStats{}, requestErr
		}
		resp, body, stream = response.HTTPResponse, response.Body, response.Stream200
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := body
		httpErr := fmt.Errorf(
			"HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)),
		)
		if !full && resp.Header.Get(server.ResyncRequiredHeader) != "" {
			return sync.SyncStats{}, fmt.Errorf(
				"%w: %w", errDaemonResyncRequired, httpErr,
			)
		}
		return sync.SyncStats{}, httpErr
	}
	if strings.HasPrefix(
		resp.Header.Get("Content-Type"), "application/json",
	) {
		var stats sync.SyncStats
		if err := json.Unmarshal(body, &stats); err != nil {
			return sync.SyncStats{}, err
		}
		return stats, nil
	}
	return consumeDaemonSyncEvents(stream, onProgress)
}

func consumeDaemonSyncEvents(stream *runtime.Stream[[]byte], progressFns ...sync.ProgressFunc) (sync.SyncStats, error) {
	defer stream.Close()
	var onProgress sync.ProgressFunc
	if len(progressFns) > 0 {
		onProgress = progressFns[0]
	}
	var lastNonDoneData string
	for stream.Next() {
		frame := stream.Event()
		switch frame.Type {
		case "done":
			var stats sync.SyncStats
			if err := json.Unmarshal(frame.Data, &stats); err != nil {
				return sync.SyncStats{}, err
			}
			return stats, nil
		case "progress":
			if len(frame.Data) > 0 {
				if err := reportDaemonSyncProgress(string(frame.Data), onProgress); err != nil {
					return sync.SyncStats{}, err
				}
			}
		default:
			if len(frame.Data) > 0 {
				lastNonDoneData = string(frame.Data)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return sync.SyncStats{}, err
	}
	if lastNonDoneData != "" {
		return sync.SyncStats{}, fmt.Errorf("daemon sync error: %s", lastNonDoneData)
	}
	return sync.SyncStats{}, errors.New("daemon sync response missing done event")
}

func reportDaemonSyncProgress(raw string, onProgress sync.ProgressFunc) error {
	if onProgress == nil {
		return nil
	}
	var progress sync.Progress
	if err := json.Unmarshal([]byte(raw), &progress); err != nil {
		return fmt.Errorf("decoding daemon sync progress: %w", err)
	}
	onProgress(progress)
	return nil
}
