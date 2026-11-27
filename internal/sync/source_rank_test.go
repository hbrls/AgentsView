package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

type sourceRankCountingFactory struct {
	parser.ProviderFactory
	lookups *int
}

func (f sourceRankCountingFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return &sourceRankCountingProvider{Provider: f.ProviderFactory.NewProvider(cfg), lookups: f.lookups}
}

type sourceRankCountingProvider struct {
	parser.Provider
	lookups *int
}

func (p *sourceRankCountingProvider) DiscoverEach(ctx context.Context, yield func(parser.SourceRef) error) error {
	return p.Provider.(parser.StreamingDiscoverer).DiscoverEach(ctx, yield)
}

func (p *sourceRankCountingProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (parser.SourceRef, bool, error) {
	*p.lookups++
	return p.Provider.(parser.ReconciliationSourceResolver).SourceForReconciliation(ctx, path, project)
}

func (p *sourceRankCountingProvider) ReconciliationSourceRank(source parser.SourceRef) parser.ReconciliationSourceRank {
	return p.Provider.(parser.ReconciliationSourceRanker).ReconciliationSourceRank(source)
}

func writeSourceRankOpenClawSession(t *testing.T, root, id, content string) string {
	t.Helper()
	path := filepath.Join(root, "main", "sessions", id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-09-22T10:00:00Z","cwd":"/workspace/project-a"}
{"type":"message","id":"m1","timestamp":"2026-09-22T10:00:01Z","message":{"role":"user","content":%q,"timestamp":"2026-09-22T10:00:01Z"}}
`, id, content)
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	return path
}

func TestOpenClawUnchangedSourcesSkipStoredRankLookup(t *testing.T) {
	for _, count := range []int{1, 64} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			root := t.TempDir()
			for i := range count {
				path := writeSourceRankOpenClawSession(t, root, fmt.Sprintf("session-%d", i), "saved question")
				require.NoError(t, os.Rename(path, path+".deleted.2026-09-22T10-00-00.000Z"))
			}
			database := openTestDB(t)
			factory, ok := parser.ProviderFactoryByType(parser.AgentOpenClaw)
			require.True(t, ok)
			lookups := 0
			cfg := EngineConfig{
				AgentDirs:         map[parser.AgentType][]string{parser.AgentOpenClaw: {root}},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{sourceRankCountingFactory{factory, &lookups}},
			}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, count, engine.SyncAll(t.Context(), nil).Synced)

			// Exercise both the warm engine and the persisted freshness gates.
			fresh := NewEngine(t.Context(), database, cfg)
			t.Cleanup(fresh.Close)
			provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, count)
			for _, current := range []*Engine{engine, fresh} {
				lookups = 0
				for _, source := range sources {
					result, used := current.processProviderFile(t.Context(), parser.DiscoveredFile{
						Agent: parser.AgentOpenClaw, Path: source.DisplayPath,
						ProviderSource: &source, ProviderProcess: true,
					})
					require.True(t, used)
					require.NoError(t, result.err)
					assert.True(t, result.skip)
				}
				assert.Zero(t, lookups, "unchanged sources must not resolve the stored source again")
			}
		})
	}
}

func TestReconciliationMissingMemberSkipsStoredRankLookup(t *testing.T) {
	root := t.TempDir()
	stored := writeSourceRankOpenClawSession(t, root, "missing", "saved question")
	provider, ok := parser.NewProvider(parser.AgentOpenClaw, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	require.NoError(t, os.Remove(stored))
	lookups := 0
	counting := &sourceRankCountingProvider{Provider: provider, lookups: &lookups}
	engine := &Engine{}
	relocated, err := engine.reconciliationMemberRelocated(t.Context(), counting, "openclaw:main:missing", stored)
	require.NoError(t, err)
	assert.False(t, relocated)
	assert.Zero(t, lookups, "a missing candidate needs no stored-source rank")
}
