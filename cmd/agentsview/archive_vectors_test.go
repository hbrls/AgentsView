package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestUsageOnlyRejectsEmbeddingsBuild(t *testing.T) {
	dir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("archive_content = \"usage\"\n"), 0o600))
	err := runEmbeddingsBuild(t.Context(), io.Discard, strings.NewReader("yes\n"), EmbeddingsBuildOptions{})
	require.ErrorIs(t, err, db.ErrArchiveContentExcluded)
}

func TestUsageOnlyClearsExistingVectorContent(t *testing.T) {
	cfg := enabledVectorConfig(t)
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	buildTestVectorsDB(t, cfg)
	// Recall has an independent store in the same vector database.
	recall, err := vector.OpenSpec(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), vector.RecallIndexSpec(), false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	_, err = recall.Build(t.Context(), testPushUnitSource(), fakePushEncoder(), kitvec.Generation{Model: "fake-model", Dimensions: 4}, vector.BuildOptions{})
	require.NoError(t, err)
	require.NoError(t, recall.Close())
	cfg.ArchiveContent = config.ArchiveContentUsage
	cfg.Vector.Enabled = false
	database, err := openDB(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	// Usage-only gating also applies when vectors are explicitly enabled.
	cfg.Vector.Enabled = true
	require.ErrorIs(t, requireVectorEnabled(cfg), db.ErrArchiveContentExcluded)
	assert.Nil(t, newVectorPushSource(cfg))
	assert.Nil(t, installDirectVectorSearcher(t.Context(), cfg, database))
	serving, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	assert.Nil(t, serving.Scheduler)
	// Read the actual previously built generation: no old documents remain indexed.
	for _, spec := range []vector.IndexSpec{vector.MessageIndexSpec(), vector.RecallIndexSpec()} {
		ix, err := vector.OpenSpec(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), spec, true, cfg.Vector.Embeddings.MaxInputChars)
		require.NoError(t, err)
		generations, err := ix.Generations(t.Context())
		require.NoError(t, err)
		require.NotEmpty(t, generations)
		for _, generation := range generations {
			assert.Zero(t, generation.Embedded)
			assert.Zero(t, generation.Missing)
		}
		require.NoError(t, ix.Close())
	}
}
