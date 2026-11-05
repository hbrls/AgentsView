package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestArchiveOnlyRefusesReceivingHostOnRestart(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, database.EnableArchiveOnly(t.Context()))
	require.NoError(t, database.Close())
	markArchiveStale(t, cfg.DBPath)
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	for range 2 {
		out, err := runRuntimeWarningHelperProcess(t, "serve", "TestServeStaleArchiveHelperProcess",
			[]string{"AGENTSVIEW_STALE_SERVE_CONFIG=" + string(data), "AGENTSVIEW_STALE_SERVE_SOURCES=" + cfg.AgentDirs[parser.AgentClaude][0]}, "listening at")
		require.NoError(t, err, string(out))
		// Read only: a writable open can truncate the WAL while Windows still
		// maps it for the helper process that just exited.
		database, err = db.OpenReadOnly(t.Context(), cfg.DBPath)
		require.NoError(t, err)
		var count int
		require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&count))
		assert.Zero(t, count, "receiver files must not become retired-machine sessions")
		assert.False(t, database.NeedsResync())
		require.NoError(t, database.Close())
	}
	for _, mode := range []string{"startup", "sync", "audit"} {
		var out bytes.Buffer
		err := runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: mode}, &out)
		assert.ErrorContains(t, err, "archive-only", mode)
	}
}

func TestArchiveOnlyConfigOnlyBackgroundStart(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host, cfg.NoBrowser, cfg.NoSync = "127.0.0.1", true, true
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, database.EnableArchiveOnly(t.Context()))
	require.NoError(t, database.Close())
	previousStart := startServeBackgroundProcessForRun
	t.Cleanup(func() { startServeBackgroundProcessForRun = previousStart })
	// Re-exec the test binary through the real background launcher. The helper
	// runs the real foreground entry point; only executable argument dispatch
	// differs from the release binary.
	startServeBackgroundProcessForRun = func(ctx context.Context, childCfg config.Config, _ []string) (*exec.Cmd, string, error) {
		data, err := json.Marshal(childCfg)
		require.NoError(t, err)
		t.Setenv("AGENTSVIEW_STALE_SERVE_CONFIG", string(data))
		t.Setenv("AGENTSVIEW_STALE_SERVE_SOURCES", childCfg.AgentDirs[parser.AgentClaude][0])
		return startServeBackgroundProcess(ctx, childCfg, []string{"-test.run=^TestServeStaleArchiveHelperProcess$"})
	}
	result, err := startServeBackground(t.Context(), cfg, []string{"serve"}, serveReplacementOptions{}, backgroundLaunchPolicy{ConfigOnly: true, Context: t.Context()})
	require.NoError(t, err)
	require.NotNil(t, result.Runtime)
	t.Cleanup(func() { _ = stopDaemonProcess(result.Runtime.Record, 5*time.Second) })
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, urlFromDaemonRuntime(result.Runtime)+"/api/v1/sessions", nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, stopDaemonProcess(result.Runtime.Record, 5*time.Second))
}

// ordinaryImportInputs writes one valid export per import type. The ChatGPT
// export includes an image, so a successful import also writes an asset.
func ordinaryImportInputs(t *testing.T) map[string]string {
	t.Helper()
	root := t.TempDir()
	claudeAI := filepath.Join(root, "claude-ai", "conversations.json")
	chatGPT := filepath.Join(root, "chatgpt")
	gemini := filepath.Join(root, "gemini-apps")
	for _, dir := range []string{filepath.Dir(claudeAI), chatGPT, gemini} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	require.NoError(t, os.WriteFile(claudeAI, []byte(`[{"uuid":"archive-import","name":"Import",`+
		`"created_at":"2026-03-01T10:00:00.000000Z","updated_at":"2026-03-01T10:05:00.000000Z",`+
		`"chat_messages":[{"uuid":"m1","text":"Imported","sender":"human",`+
		`"content":[{"type":"text","text":"Imported"}],"created_at":"2026-03-01T10:00:00.000000Z"}]}]`), 0o600))
	for name, data := range testjsonl.ChatGPTImageExport() {
		require.NoError(t, os.WriteFile(filepath.Join(chatGPT, name), []byte(data), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(gemini, "activity.html"), []byte(`<!doctype html>
<html><head><title>My Activity History</title></head><body>
<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 2, 2025, 3:04:05 PM EDT</p></div><div class="content-cell"><p>first prompt</p><p>first answer</p></div></div>
</body></html>`), 0o600))
	return map[string]string{"claude-ai": claudeAI, "chatgpt": chatGPT, "gemini-apps": gemini}
}

// archiveImportState captures what an import could change: session rows and
// stored asset bytes.
func archiveImportState(t *testing.T, dataDir string) (map[string]string, map[string]string) {
	t.Helper()
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	rows, err := database.Reader().Query(t.Context(), "SELECT id, coalesce(first_message,'') FROM sessions ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	sessions := map[string]string{}
	for rows.Next() {
		var id, first string
		require.NoError(t, rows.Scan(&id, &first))
		sessions[id] = first
	}
	require.NoError(t, rows.Err())
	assets := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(dataDir, "assets"))
	if !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dataDir, "assets", entry.Name()))
		require.NoError(t, err)
		assets[entry.Name()] = string(data)
	}
	return sessions, assets
}

func TestArchiveOnlyRefusesOrdinaryImports(t *testing.T) {
	inputs := ordinaryImportInputs(t)
	for _, importType := range []string{"chatgpt"} {
		t.Run(importType, func(t *testing.T) {
			preserved := testDataDir(t)
			database, err := db.Open(t.Context(), filepath.Join(preserved, "sessions.db"))
			require.NoError(t, err)
			first := "preserved history"
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "preserved", Agent: "claude", Project: "project-a", Machine: "retired", FirstMessage: &first,
			}))
			require.NoError(t, database.EnableArchiveOnly(t.Context()))
			require.NoError(t, database.Close())
			require.NoError(t, os.MkdirAll(filepath.Join(preserved, "assets"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(preserved, "assets", "existing.png"), []byte("existing"), 0o600))
			beforeSessions, beforeAssets := archiveImportState(t, preserved)

			err = importSessions(ImportConfig{Type: importType, Path: inputs[importType]})
			require.ErrorIs(t, err, db.ErrArchiveOnly)
			afterSessions, afterAssets := archiveImportState(t, preserved)
			assert.Equal(t, beforeSessions, afterSessions, "a preserved archive does not gain or change sessions")
			assert.Equal(t, beforeAssets, afterAssets, "a preserved archive does not gain or change assets")
		})
	}
}
