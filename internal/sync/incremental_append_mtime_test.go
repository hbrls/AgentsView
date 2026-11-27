package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestIncrementalAppendUnchangedSourceStillSkips guards the ordinary append
// path: when nothing writes during the parse, the stored size and mtime still
// describe the same moment and the next sync skips the unchanged transcript.
func TestIncrementalAppendUnchangedSourceStillSkips(t *testing.T) {
	ctx := t.Context()
	database := openTestDB(t)
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "steady-append.jsonl")

	builder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "ok", "b", "a")
	initial := builder.String()
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	engine := NewEngine(ctx, database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)

	builder.AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "next", "c", "b")
	appendFile(t, path, builder.String()[len(initial):])
	appendedAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	require.NoError(t, os.Chtimes(path, appendedAt, appendedAt))

	stats := engine.SyncAll(ctx, nil)
	require.Equal(t, 1, stats.Synced)
	info, err := os.Stat(path)
	require.NoError(t, err)

	inc, found := database.GetSessionForIncremental(
		ctx, path, string(parser.AgentClaude),
	)
	require.True(t, found)
	storedSize, storedMtime, ok := database.GetSessionFileInfo(ctx, inc.ID)
	require.True(t, ok)
	assert.Equal(t, info.Size(), storedSize)
	assert.Equal(t, info.ModTime().UnixNano(), storedMtime)
	assert.True(t, engine.shouldSkipFile(ctx, inc.ID, info),
		"a transcript that did not change must still skip",
	)

	unchanged := engine.SyncAll(ctx, nil)
	assert.Equal(t, 1, unchanged.Skipped,
		"the second sync must skip the unchanged transcript")
	assert.Zero(t, unchanged.Synced)
}
