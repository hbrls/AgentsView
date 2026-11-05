package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopedLoadersValidateUsedSections(t *testing.T) {
	for _, tt := range []struct {
		data, want string
		scope      ValidationScope
	}{
		{"[vector]\nenabled=true", "model is required", ScopeVector},
		{"[recall.extract]\nenabled=true\nmodel='local'", "recall.extract", ScopeRecall},
		{"[insights]\nendpoint='http://models.example/v1'\nmodel='remote'", "plaintext", ScopeInsights},
	} {
		t.Run(tt.want, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tt.data), 0o600))
			_, err := LoadMinimalFor(tt.scope)
			require.ErrorContains(t, err, tt.want)
			_, err = LoadReadOnlyFor(tt.scope)
			require.ErrorContains(t, err, tt.want)
			_, err = LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), tt.scope)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestScopedLoadersIgnoreUnusedDeploymentEnvironment(t *testing.T) {
	unsetDeploymentEnv(t)
	setupTestEnv(t)
	t.Setenv("AGENTSVIEW_MODE", "invalid")
	t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "invalid")
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", "/nonexistent/scoped-config-token")
	for _, load := range []func() (Config, error){
		func() (Config, error) { return LoadMinimalFor(ScopeLocal) },
		func() (Config, error) { return LoadReadOnlyFor(ScopeLocal) },
		func() (Config, error) {
			return LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), ScopeLocal)
		},
	} {
		_, err := load()
		require.NoError(t, err)
	}
	_, err := LoadMinimalFor(ScopePG | ScopeDeployment)
	require.ErrorContains(t, err, "AGENTSVIEW_MODE")
}

// A mistyped AGENTSVIEW_MODE makes serve refuse to start. Other commands must
// not apply deployment secrets or the PG insecure override under it: a reader
// reports the mode once one of its variables is set, and is a no-op otherwise.
func TestInvalidDeploymentModeAppliesNoDeploymentSettings(t *testing.T) {
	for _, tt := range []struct {
		name, env, value string
		// apply reports whether the deployment value took effect.
		apply func(cfg *Config) (bool, error)
		// validate replaces apply for a reader whose error is its only
		// observable result.
		validate func(cfg *Config) error
	}{
		{name: "auth token file", env: "AGENTSVIEW_AUTH_TOKEN_FILE", value: "deployment-secret", apply: func(cfg *Config) (bool, error) {
			err := cfg.ApplyDeploymentAuth()
			return cfg.AuthToken != "", err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			writeConfig(t, dir, map[string]any{"pg": map[string]any{"url": "postgres://db.example/archive"}})
			cfg, err := LoadMinimalFor(ScopeLocal)
			require.NoError(t, err)
			cfg.RequireAuth = true
			t.Setenv("AGENTSVIEW_MODE", "srve")
			check := func() error {
				if tt.validate != nil {
					return tt.validate(&cfg)
				}
				applied, err := tt.apply(&cfg)
				assert.False(t, applied)
				return err
			}
			require.NoError(t, check(), "a reader with no variables set ignores the mode")

			value := tt.value
			if strings.HasSuffix(tt.env, "_FILE") {
				value = writeSecret(t, t.TempDir(), "secret", tt.value)
			}
			t.Setenv(tt.env, value)
			require.ErrorContains(t, check(), `AGENTSVIEW_MODE must be "serve" or "pg-serve", got "srve"`)
		})
	}
}

func TestScopedReadOnlyDoesNotGenerateSecrets(t *testing.T) {
	unsetDeploymentEnv(t)
	dir := setupTestEnv(t)
	data := []byte("[insights]\nmodel='incomplete'\n")
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, err := LoadReadOnlyFor(ScopeLocal)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "config.toml", entries[0].Name())
}

func TestScopedLoadersUnusedPresentation(t *testing.T) {
	for _, data := range []string{"chart_palette='invalid'", "zoom_level=12"} {
		t.Run(data, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(data), 0o600))
			_, err := LoadReadOnlyFor(ScopeLocal)
			require.NoError(t, err)
			_, err = LoadReadOnly()
			require.Error(t, err)
		})
	}
}

// Any remote plaintext endpoint is invalid for insights without allow_http,
// regardless of its path. It must remain intact and usable by unrelated loads.
func FuzzScopedInsightsEndpoint(f *testing.F) {
	f.Add("", false)
	f.Add("v1/chat/completions", true)
	f.Add("quotes'\"\n雪", false)
	f.Fuzz(func(t *testing.T, path string, allowHTTP bool) {
		unsetDeploymentEnv(t)
		dir := setupTestEnv(t)
		endpoint := "http://models.example/" + url.PathEscape(path)
		writeConfig(t, dir, map[string]any{"insights": map[string]any{"endpoint": endpoint, "model": "remote", "allow_http": allowHTTP}})
		for _, load := range []func() (Config, error){
			func() (Config, error) { return LoadMinimalFor(ScopeLocal) },
			func() (Config, error) { return LoadReadOnlyFor(ScopeLocal) },
			func() (Config, error) {
				return LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), ScopeLocal)
			},
		} {
			cfg, err := load()
			require.NoError(t, err)
			assert.Equal(t, endpoint, cfg.Insights.Endpoint)
			assert.Equal(t, "remote", cfg.Insights.Model)
		}
		_, err := LoadMinimal()
		if allowHTTP {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "plaintext")
		}
	})
}
