package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAiderStreamingDiscoveryReportsTraversalLimits(t *testing.T) {
	writeHistory := func(t *testing.T, root, project string) {
		t.Helper()
		dir := filepath.Join(root, project)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, aiderHistoryFile),
			[]byte("# aider chat started at 2026-07-14 12:00:00\n#### hello\nworld\n"),
			0o600,
		))
	}

	for _, tc := range []struct {
		name   string
		limits discoveryTraversalLimits
		files  int
		want   string
	}{
		{name: "time", limits: discoveryTraversalLimits{expired: func() bool { return true }}, files: 1, want: "time budget"},
		{name: "directories", limits: discoveryTraversalLimits{maxDirs: 1}, files: 2, want: "directory limit"},
		{name: "files", limits: discoveryTraversalLimits{maxFiles: 1}, files: 2, want: "file limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i := range tc.files {
				writeHistory(t, root, fmt.Sprintf("project-%d", i))
			}
			provider, ok := NewProvider(AgentAider, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			ctx := withDiscoveryTraversalLimits(t.Context(), tc.limits)

			err := provider.(StreamingDiscoverer).DiscoverEach(ctx, func(SourceRef) error { return nil })

			var incomplete DiscoveryIncompleteError
			require.ErrorAs(t, err, &incomplete)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotErrorIs(t, err, context.Canceled)
		})
	}
}
