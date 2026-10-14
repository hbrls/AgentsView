package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
)

func TestStartupWorkerDoesNotAddBlankLineForNonTerminalProgress(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ syncWorkerRequest, onLine func(workerLine),
	) (workerResult, error) {
		onLine(workerLine{Progress: &agentsync.Progress{
			Phase:         agentsync.PhaseSyncing,
			Detail:        "Syncing sessions",
			SessionsDone:  1,
			SessionsTotal: 1,
		}})
		return workerResult{}, errors.New("worker failed")
	})
	defer restore()

	var err error
	out := captureStdout(t, func() {
		_, err = runStartupSyncViaWorker(
			t.Context(), cfg, newStartupStateWriter(cfg.DataDir, time.Now),
		)
	})

	require.Error(t, err)
	assert.Equal(t, "Running initial sync...\n", out)
}

func isolateDirectCLISources(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, def := range parser.Registry {
		if def.EnvVar != "" {
			t.Setenv(def.EnvVar, filepath.Join(root, string(def.Type)))
		}
		if def.DefaultRootEnvVar != "" {
			t.Setenv(def.DefaultRootEnvVar, root)
		}
	}
}

func TestRunLocalSyncZeroContributorRetainsAbortFallback(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{DataDir: dataDir, DBPath: filepath.Join(dataDir, "sessions.db")}
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	missingPath := filepath.Join(dataDir, "missing.jsonl")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "old-file-session", Agent: "claude", Machine: "local",
		Project: "preserved", FilePath: &missingPath, MessageCount: 1,
	}))

	didResync := runLocalSync(t.Context(), cfg, database, true)

	assert.True(t, didResync)
	preserved, getErr := database.GetSession(t.Context(), "old-file-session")
	require.NoError(t, getErr)
	assert.NotNil(t, preserved,
		"legacy zero-contributor fallback must keep the active archive")
}

func TestUseDaemonForSync(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		want     bool
	}{
		{"skips read-only daemon", true, false},
		{"uses writable daemon", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			use := useDaemonForSync(transport{
				Mode:     transportHTTP,
				URL:      "http://127.0.0.1:8080",
				ReadOnly: tt.readOnly,
			})
			assert.Equal(t, tt.want, use)
		})
	}
}

func TestParseDaemonSyncSSEAllowsLargeDoneEvent(t *testing.T) {
	largeWarning := strings.Repeat("x", 70*1024)
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
		Warnings:      []string{largeWarning},
	}

	got, err := consumeDaemonSyncEvents(daemonEventStream(doneSSE(t, want, true)))
	require.NoError(t, err)
	assert.Equal(t, want.TotalSessions, got.TotalSessions)
	assert.Equal(t, want.Synced, got.Synced)
	require.Len(t, got.Warnings, 1)
	assert.Equal(t, largeWarning, got.Warnings[0])
}

func TestParseDaemonSyncSSEFlushesUnterminatedDoneEvent(t *testing.T) {
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
	}

	got, err := consumeDaemonSyncEvents(daemonEventStream(doneSSE(t, want, false)))
	require.NoError(t, err)
	assert.Equal(t, want.TotalSessions, got.TotalSessions)
	assert.Equal(t, want.Synced, got.Synced)
}

func TestParseDaemonSyncSSEReportsErrorEventPayload(t *testing.T) {
	_, err := consumeDaemonSyncEvents(daemonEventStream(strings.NewReader(
		"event: error\n" +
			"data: remote sync failed\n" +
			"data: permission denied\n\n",
	)))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon sync error")
	assert.Contains(t, err.Error(), "remote sync failed\npermission denied")
}

func TestParseDaemonSyncSSEReportsProgressEvents(t *testing.T) {
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
	}
	var progress []agentsync.Progress

	got, err := consumeDaemonSyncEvents(daemonEventStream(strings.NewReader(
		"event: progress\n"+
			"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n"+
			sseString(t, doneSSE(t, want, true)),
	)), func(p agentsync.Progress) {
		progress = append(progress, p)
	})

	require.NoError(t, err)
	assert.Equal(t, want.Synced, got.Synced)
	require.Len(t, progress, 1)
	assert.Equal(t, agentsync.PhaseRebuildingSearch, progress[0].Phase)
	assert.Equal(t, "Rebuilding search index", progress[0].Detail)
	assert.True(t, progress[0].Resync)
}

func TestWriteSyncProgressClearsShorterOverwritesInTerminalStyle(t *testing.T) {
	var out bytes.Buffer
	writeSyncProgress(&out, true, agentsync.Progress{
		Detail: "Rebuilding search index",
		Hint:   "Rebuilding the search index may take a while on large archives.",
	})
	writeSyncProgress(&out, true, agentsync.Progress{
		Detail: "Swapping rebuilt database into place",
	})

	outString := out.String()

	require.GreaterOrEqual(t, strings.Count(outString, "\x1b[K"), 2,
		"each carriage-return progress line must clear stale text")
	assert.Contains(t, outString, "\r  Swapping rebuilt database into place\x1b[K")
}

func TestPrintSyncProgressStaysCleanOnNonTerminalStdout(t *testing.T) {
	out := captureStdout(t, func() {
		printProgress := newSyncProgressPrinter(os.Stdout)
		for _, progress := range []agentsync.Progress{
			{Phase: agentsync.PhaseDiscovering, Detail: "Discovering sessions"},
			{Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 3},
			{Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 3, SessionsDone: 1},
			{Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 3, SessionsDone: 2},
			{Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 3, SessionsDone: 3},
			{Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 3, SessionsDone: 3, MessagesIndexed: 6},
		} {
			printProgress(progress)
		}
		printSyncSummary(agentsync.SyncStats{Synced: 3}, time.Now())
	})

	assert.NotContains(t, out, "\r")
	assert.NotContains(t, out, "\x1b[K")
	assert.True(t, strings.HasPrefix(out, "Sync complete: 3 sessions synced"),
		"a redirected summary must not start with a blank line")
}

func TestResyncProgressPrinterStaysLineOrientedOnNonTerminalFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "resync-progress-*.txt")
	require.NoError(t, err)
	defer file.Close()

	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	printer := newResyncProgressPrinter(file, func() time.Time { return now })
	printer.Print(agentsync.Progress{
		Phase: agentsync.PhasePreparingResync, Detail: "Preparing full resync", Resync: true,
	})
	printer.Print(agentsync.Progress{
		Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 3,
		SessionsDone: 1, Resync: true,
	})
	printer.Print(agentsync.Progress{
		Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 3,
		SessionsDone: 3, Resync: true,
	})
	printer.Print(agentsync.Progress{Phase: agentsync.PhaseDone, SessionsTotal: 3, Resync: true})
	printer.Finish()
	printer.Finish()
	printer.Print(agentsync.Progress{Detail: "after finish"})
	require.NoError(t, file.Close())

	content, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	out := string(content)
	assert.NotContains(t, out, "\r")
	assert.NotContains(t, out, "\x1b[K")
	assert.Contains(t, out, "Preparing full resync...")
	assert.Contains(t, out, "Syncing sessions...")
	assert.Contains(t, out, "Syncing sessions completed in")
	assert.NotContains(t, out, "after finish")
}

func TestIsTerminalWriterClassifiesDestinations(t *testing.T) {
	assert.False(t, isTerminalWriter(&bytes.Buffer{}))

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	assert.False(t, isTerminalWriter(reader))
	assert.False(t, isTerminalWriter(writer))
	require.NoError(t, reader.Close())
	require.NoError(t, writer.Close())

	file, err := os.CreateTemp(t.TempDir(), "terminal-classification-*.txt")
	require.NoError(t, err)
	assert.False(t, isTerminalWriter(file))
	require.NoError(t, file.Close())
	assert.False(t, isTerminalWriter(file))

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	assert.False(t, isTerminalWriter(null))
	require.NoError(t, null.Close())
}

func TestUsageResyncProgressStaysCleanOnNonTerminalStderr(t *testing.T) {
	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() {
			printer := newResyncProgressPrinter(os.Stderr, time.Now)
			printer.Print(agentsync.Progress{
				Phase: agentsync.PhasePreparingResync, Detail: "Preparing full resync",
			})
			printer.Print(agentsync.Progress{
				Phase: agentsync.PhaseSyncing, Detail: "Syncing sessions", SessionsTotal: 1,
				SessionsDone: 1,
			})
			printer.Print(agentsync.Progress{Phase: agentsync.PhaseDone, SessionsTotal: 1})
			printer.Finish()
			printSyncSummaryStderr(agentsync.SyncStats{Synced: 1}, time.Now())
		})
		assert.NotContains(t, stderr, "\r")
		assert.NotContains(t, stderr, "\x1b[K")
		assert.NotContains(t, stderr, "\n\n",
			"a redirected summary must follow the completed phase without a blank line")
		assert.Contains(t, stderr, "Sync complete: 1 sessions synced")
	})

	assert.Empty(t, stdout)
}

func TestResyncProgressPrinterShowsRepairCounts(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			var out bytes.Buffer
			now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
			printer := newResyncProgressPrinter(&out, func() time.Time { return now })
			printer.terminal = terminal
			p := agentsync.Progress{
				Phase: agentsync.PhaseFinalizing, Resync: true,
				Detail: "Finalizing sync: repairing subagent relationships",
			}
			printer.Print(p)
			p.SessionsTotal = 1000
			printer.Print(p)
			p.SessionsDone = 250
			printer.Print(p)
			now = now.Add(5 * time.Second)
			p.SessionsDone = 500
			printer.Print(p)
			assert.Equal(t, "Finalizing sync: repairing subagent relationships: 500/1000 sessions checked (50%)", startupProgressDetail(p))
			p.SessionsDone = 1000
			printer.Print(p)
			printer.Finish()
			assert.Contains(t, out.String(), "0/1000 sessions checked (0%)")
			assert.Contains(t, out.String(), "500/1000 sessions checked (50%)")
			assert.Contains(t, out.String(), "1000/1000 sessions checked (100%)")
			assert.NotContains(t, out.String(), "messages")
			assert.Equal(t, 1, strings.Count(out.String(), "completed in"))
			if !terminal {
				assert.NotContains(t, out.String(), "250/1000")
				assert.NotContains(t, out.String(), "\r")
			}
		})
	}
}

func TestResyncProgressPrinterWritesPhaseTimingsOnNewLines(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newResyncProgressPrinter(&out, clock)
	printer.terminal = true

	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhasePreparingResync,
		Detail: "Preparing full resync",
		Resync: true,
	})
	now = now.Add(150 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
		Resync:          true,
	})
	now = now.Add(2 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
		Resync:          true,
	})
	now = now.Add(350 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhaseFinalizing,
		Detail: "Finalizing sync: committing session writes",
		Resync: true,
	})
	now = now.Add(500 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhaseRebuildingSearch,
		Detail: "Rebuilding search index",
		Hint:   "Rebuilding the search index may take a while on large archives.",
		Resync: true,
	})
	now = now.Add(3 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhaseSwappingDatabase,
		Detail: "Swapping rebuilt database into place",
		Resync: true,
	})

	got := out.String()
	assert.Contains(t, got, "  Preparing full resync...\n")
	assert.Contains(t, got, "  Preparing full resync completed in 150ms\n")
	assert.Contains(t, got, "\r  Syncing sessions into rebuilt database: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Syncing sessions into rebuilt database completed in 2.35s\n")
	assert.Contains(t, got,
		"  Finalizing sync: committing session writes...\n")
	assert.Contains(t, got,
		"  Finalizing sync: committing session writes completed in 500ms\n")
	assert.NotContains(t, got,
		"\r  Finalizing sync: committing session writes")
	assert.Contains(t, got, "  Rebuilding search index - Rebuilding the search index may take a while on large archives...\n")
	assert.Contains(t, got, "  Rebuilding search index completed in 3s\n")
	assert.NotContains(t, got, "\r  Rebuilding search index",
		"non-session resync phases must not be overwritten in place")
}

func TestResyncProgressPrinterRendersDoneProgressBeforeCompletion(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newResyncProgressPrinter(&out, clock)
	printer.terminal = true

	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   1,
		SessionsDone:    1,
		MessagesIndexed: 0,
		Resync:          true,
	})
	now = now.Add(time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseDone,
		SessionsTotal:   1,
		SessionsDone:    1,
		MessagesIndexed: 42,
		Resync:          true,
	})

	got := out.String()
	assert.Contains(t, got,
		"\r  Syncing sessions into rebuilt database: 1/1 sessions (100%) · 42 messages\x1b[K")
	assert.Contains(t, got,
		"\n  Syncing sessions into rebuilt database completed in 1s\n")
}

func TestRunLocalSyncUsesCallerContextForResync(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	database, err := db.Open(t.Context(), dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), "PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	database, err = db.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	require.True(t, database.NeedsResync())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var didResync bool
	captureStdout(t, func() {
		didResync = runLocalSync(ctx, config.Config{
			DataDir: dataDir,
			DBPath:  dbPath,
		}, database, false)
	})

	assert.True(t, didResync)
	assert.True(t, database.NeedsResync())
}

func TestDoSyncUsesDaemonRouteWhenWritableDaemonRunning(t *testing.T) {
	env := newSyncCLIEnv(t)

	var syncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/api/v1/sync", r.URL.Path) {
			return
		}
		if !assert.Equal(t, http.MethodPost, r.Method) {
			return
		}
		syncCalled = true
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 7})
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures := doSync(SyncConfig{})
	require.False(t, hadFailures)
	assert.True(t, syncCalled)
	env.assertNoLocalDB(t)
}

func TestDoSyncFullUsesDaemonResyncRoute(t *testing.T) {
	env := newSyncCLIEnv(t)

	var resyncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/api/v1/resync", r.URL.Path) {
			return
		}
		if !assert.Equal(t, http.MethodPost, r.Method) {
			return
		}
		resyncCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: progress\n"+
				"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n",
		)
		if !assert.NoError(t, err) {
			return
		}
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 9})
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	var hadFailures bool
	out := captureStdout(t, func() {
		hadFailures = doSync(SyncConfig{Full: true})
	})
	require.False(t, hadFailures)
	assert.True(t, resyncCalled)
	assert.Contains(t, out, "Rebuilding search index")
	env.assertNoLocalDB(t)
}

func TestDoSyncPrintsStatusBeforeWaitingForDaemonStartup(t *testing.T) {
	env := newSyncCLIEnv(t)
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/api/v1/resync", r.URL.Path) {
			return
		}
		writeDoneSSE(t, w, agentsync.SyncStats{})
	})
	endpoint := serverEndpoint(t, ts)
	startupEntered := make(chan struct{})
	releaseStartup := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseStartup:
		default:
			close(releaseStartup)
		}
	})
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration, bool,
	) (*DaemonRuntime, error) {
		close(startupEntered)
		<-releaseStartup
		return &DaemonRuntime{Host: endpoint.Host, Port: endpoint.Port}, nil
	})

	stdout := filepath.Join(t.TempDir(), "stdout")
	outFile, err := os.Create(stdout)
	require.NoError(t, err)
	oldStdout := os.Stdout
	os.Stdout = outFile
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = outFile.Close()
	})

	done := make(chan bool, 1)
	go func() {
		done <- doSync(SyncConfig{Full: true})
	}()
	<-startupEntered
	require.NoError(t, outFile.Sync())
	output, err := os.ReadFile(stdout)
	require.NoError(t, err)
	assert.Contains(t, string(output), "Preparing full sync...")

	close(releaseStartup)
	assert.False(t, <-done)
	require.NoError(t, outFile.Close())
	os.Stdout = oldStdout
	env.assertNoLocalDB(t)
}

func TestDoSyncFullSkipsRedundantDaemonInitialSync(t *testing.T) {
	env := newSyncCLIEnv(t)
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/api/v1/resync", r.URL.Path) {
			return
		}
		writeDoneSSE(t, w, agentsync.SyncStats{})
	})
	endpoint := serverEndpoint(t, ts)
	var skipInitialSync bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration, _ bool,
	) (*DaemonRuntime, error) {
		skipInitialSync = cfg.SkipInitialSync
		return &DaemonRuntime{Host: endpoint.Host, Port: endpoint.Port}, nil
	})

	hadFailures := doSync(SyncConfig{Full: true})

	assert.False(t, hadFailures)
	assert.True(t, skipInitialSync)
	env.assertNoLocalDB(t)
}

func TestDoSyncSkipsRedundantDaemonInitialSync(t *testing.T) {
	env := newSyncCLIEnv(t)
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/api/v1/sync", r.URL.Path) {
			return
		}
		writeDoneSSE(t, w, agentsync.SyncStats{})
	})
	endpoint := serverEndpoint(t, ts)
	var skipInitialSync bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration, _ bool,
	) (*DaemonRuntime, error) {
		skipInitialSync = cfg.SkipInitialSync
		return &DaemonRuntime{Host: endpoint.Host, Port: endpoint.Port}, nil
	})

	hadFailures := doSync(SyncConfig{})

	assert.False(t, hadFailures)
	assert.True(t, skipInitialSync)
	env.assertNoLocalDB(t)
}

func TestRunDaemonSyncTrimsBaseURLTrailingSlash(t *testing.T) {
	var syncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/api/v1/sync", r.URL.Path) {
			return
		}
		if !assert.Equal(t, strings.TrimSuffix(tsURL(t, r), "/"), r.Header.Get("Origin")) {
			return
		}
		syncCalled = true
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 7})
	})

	stats, err := runDaemonSync(
		t.Context(),
		transport{URL: ts.URL + "/"},
		"",
		false,
		nil,
	)
	require.NoError(t, err)
	assert.True(t, syncCalled)
	assert.Equal(t, 7, stats.Synced)
}

func TestRunDaemonSyncWaitsForBusyEngine(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelWait), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{})
			t.Cleanup(engine.Close)
			entered := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			var releaseOnce stdsync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			go func() {
				done <- engine.RunExclusive(func() error {
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			defer func() {
				unblock()
				require.NoError(t, <-done)
			}()

			var runs atomic.Int32
			ts := httptest.NewUnstartedServer(nil)
			defer ts.Close()
			cfg := config.Config{Host: "127.0.0.1", Port: ts.Listener.Addr().(*net.TCPAddr).Port}
			srv := server.New(cfg, database, engine,
				server.WithLocalSyncRunner(func(context.Context, func(agentsync.Progress)) (agentsync.SyncStats, error) {
					err := engine.TryRunExclusive(func() error {
						runs.Add(1)
						return nil
					})
					return agentsync.SyncStats{Synced: 7}, err
				}),
			)
			ts.Config.Handler = srv.Handler()
			ts.Start()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var waiting bool
			stats, err := runDaemonSync(ctx, transport{URL: ts.URL}, "", false,
				func(p agentsync.Progress) {
					waiting = true
					assert.Contains(t, p.Detail, "Waiting")
					assert.Zero(t, runs.Load(), "waiting must not run overlapping work")
					if cancelWait {
						cancel()
					} else {
						unblock()
					}
				},
			)
			assert.True(t, waiting, "the CLI must receive progress while the engine is busy")
			if cancelWait {
				require.ErrorIs(t, err, context.Canceled)
				assert.Zero(t, runs.Load())
			} else {
				require.NoError(t, err)
				assert.Equal(t, 7, stats.Synced)
				assert.EqualValues(t, 1, runs.Load())
			}
		})
	}
}

// TestRunDaemonSyncDetectsResyncRequired pins the stale-archive UX: only a
// /sync rejection carrying the resync-required header maps to
// errDaemonResyncRequired (so the CLI retries via /api/v1/resync); a plain
// non-200, or the same header on an already-full resync request, stays a
// generic error.
func TestRunDaemonSyncDetectsResyncRequired(t *testing.T) {
	tests := []struct {
		name       string
		withHeader bool
		full       bool
		wantResync bool
	}{
		{name: "409 with header on sync", withHeader: true, wantResync: true},
		{name: "409 without header", withHeader: false, wantResync: false},
		{name: "409 with header on resync", withHeader: true, full: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := syncRouteTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if tt.withHeader {
					w.Header().Set(server.ResyncRequiredHeader, "true")
				}
				w.WriteHeader(http.StatusConflict)
			})

			_, err := runDaemonSync(
				t.Context(), transport{URL: ts.URL}, "", tt.full, nil,
			)
			require.Error(t, err)
			assert.Equal(t, tt.wantResync,
				errors.Is(err, errDaemonResyncRequired))
			assert.Contains(t, err.Error(), "HTTP 409")
		})
	}
}

// syncCLIEnv is a daemon-backed CLI test environment: an isolated data dir
// exported via AGENTSVIEW_DATA_DIR with the global log writer restored on
// cleanup.
type syncCLIEnv struct {
	DataDir string
	DBPath  string
}

func newSyncCLIEnv(t *testing.T) syncCLIEnv {
	t.Helper()
	dataDir := testDataDir(t)
	restoreTestLogOutput(t)
	return syncCLIEnv{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
	}
}

// assertNoLocalDB verifies the CLI deferred to the daemon instead of opening a
// local SQLite archive.
func (e syncCLIEnv) assertNoLocalDB(t *testing.T) {
	t.Helper()
	assert.NoFileExists(t, e.DBPath)
}

// doneSSE renders stats as a daemon sync "done" SSE event. When terminated is
// false the trailing blank line is omitted to exercise flush-on-EOF parsing.
func doneSSE(t *testing.T, stats agentsync.SyncStats, terminated bool) io.Reader {
	t.Helper()
	payload, err := json.Marshal(stats)
	require.NoError(t, err)
	suffix := "\n\n"
	if !terminated {
		suffix = "\n"
	}
	return strings.NewReader("event: done\ndata: " + string(payload) + suffix)
}

func sseString(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}

// writeDoneSSE writes a terminated daemon sync "done" SSE event to w.
func writeDoneSSE(t *testing.T, w http.ResponseWriter, stats agentsync.SyncStats) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	_, err := io.Copy(w, doneSSE(t, stats, true))
	require.NoError(t, err)
}

// daemonRouteTestServer starts an httptest server that answers daemon ping
// probes and dispatches the given routes by exact path.
func daemonRouteTestServer(
	t *testing.T,
	routes map[string]http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/api/ping" {
			ping.ServeHTTP(w, r)
			return
		}
		if h, ok := routes[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func syncRouteTestServer(
	t *testing.T,
	syncHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	return daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sync":   syncHandler,
		"/api/v1/resync": syncHandler,
	})
}

func registerSyncRouteTestRuntime(
	t *testing.T,
	dataDir string,
	rawURL string,
) {
	t.Helper()
	registerTestRuntime(t, dataDir, rawURL, false)
}

func registerTestRuntime(
	t *testing.T,
	dataDir string,
	rawURL string,
	readOnly bool,
) {
	t.Helper()

	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	_, err = WriteDaemonRuntime(dataDir, host, port, "test", readOnly)
	require.NoError(t, err)
}

func tsURL(t *testing.T, r *http.Request) string {
	t.Helper()
	return "http://" + r.Host
}
