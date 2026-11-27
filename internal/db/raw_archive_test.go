package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawArchiveSnapshot(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	root := RawArchiveRoot{ID: "root", ConfiguredRootID: "root", DeviceID: "device", Machine: "example", Provider: "codex", OriginalPath: "/example"}
	require.NoError(t, d.RegisterRawArchiveRoot(ctx, root))
	insertSession(t, d, "session-a", "example")
	path := filepath.Join(t.TempDir(), "snapshot.db")
	require.NoError(t, d.SnapshotTo(ctx, path))
	snapshot, err := Open(ctx, path)
	require.NoError(t, err)
	defer snapshot.Close()
	roots, err := snapshot.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RawArchiveRoot{root}, roots)
	session, err := snapshot.GetSession(ctx, "session-a")
	require.NoError(t, err)
	assert.Equal(t, "session-a", session.ID)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Error(t, d.SnapshotTo(ctx, path))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	missing := filepath.Join(t.TempDir(), "cancelled.db")
	require.ErrorIs(t, d.SnapshotTo(cancelled, missing), context.Canceled)
	assert.NoFileExists(t, missing)
}

func TestRawArchiveMigrationAndLegacyCopy(t *testing.T) {
	ctx := t.Context()
	legacy := testDB(t)
	// Simulate an archive written before these additive tables existed.
	for _, table := range []string{"raw_archive_sessions", "raw_archive_devices", "raw_archive_sources", "raw_archive_files", "raw_archive_roots"} {
		_, err := legacy.getWriter().ExecContext(ctx, "DROP TABLE "+table)
		require.NoError(t, err)
	}
	target := testDB(t)
	require.NoError(t, target.CopySyncStateFrom(t.Context(), legacy.path))
	roots, err := target.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	assert.Empty(t, roots)
	path := legacy.path
	require.NoError(t, legacy.Close())
	reopened, err := Open(ctx, path)
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.RegisterRawArchiveRoot(ctx, RawArchiveRoot{ID: "root", ConfiguredRootID: "root", DeviceID: "device", Machine: "example", Provider: "claude", OriginalPath: "/example"}))
}

func TestRawArchiveReadOnly(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	root := RawArchiveRoot{ID: "root", ConfiguredRootID: "root", DeviceID: "device", Machine: "example", Provider: "claude", OriginalPath: "/example"}
	require.NoError(t, d.RegisterRawArchiveRoot(ctx, root))
	readOnly, err := OpenReadOnly(ctx, d.path)
	require.NoError(t, err)
	defer readOnly.Close()
	roots, err := readOnly.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RawArchiveRoot{root}, roots)
	require.ErrorIs(t, readOnly.RegisterRawArchiveRoot(ctx, root), ErrReadOnly)
	snapshotPath := filepath.Join(t.TempDir(), "readonly-snapshot.db")
	require.ErrorIs(t, readOnly.SnapshotTo(ctx, snapshotPath), ErrReadOnly)
	assert.NoFileExists(t, snapshotPath)
}
