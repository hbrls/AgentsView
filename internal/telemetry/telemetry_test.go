package telemetry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTelemetryStartupDefault(t *testing.T) {
	// A subprocess exercises init before any test changes the environment.
	if want := os.Getenv("AGENTSVIEW_TEST_TELEMETRY_ENABLED"); want != "" {
		assert.Equal(t, want == "true", EnabledFromEnv())
		if want == "false" {
			reporter, err := NewReporter(Options{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reporter.Close()) })
			assert.False(t, reporter.Enabled())
			assert.True(t, reporter.EventAllowed(EventAppOpened))
			assert.False(t, reporter.EventAllowed("unknown_event"))
		}
		return
	}

	for _, tc := range []struct {
		name, enabled, generic, want string
		unset                        bool
	}{
		{name: "unset defaults off", generic: "1", want: "false", unset: true},
		{name: "empty defaults off", generic: "1", want: "false"},
		{name: "whitespace defaults off", enabled: " \t ", generic: "1", want: "false"},
		{name: "explicit opt in", enabled: "1", generic: "1", want: "true"},
		{name: "explicit opt out", enabled: "0", generic: "1", want: "false"},
		{name: "generic opt out wins", enabled: "1", generic: "0", want: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTelemetryStartupDefault$")
			for _, env := range os.Environ() {
				if !strings.HasPrefix(env, EnabledEnv+"=") && !strings.HasPrefix(env, GenericEnabledEnv+"=") {
					cmd.Env = append(cmd.Env, env)
				}
			}
			cmd.Env = append(cmd.Env, GenericEnabledEnv+"="+tc.generic, "AGENTSVIEW_TEST_TELEMETRY_ENABLED="+tc.want)
			if !tc.unset {
				cmd.Env = append(cmd.Env, EnabledEnv+"="+tc.enabled)
			}
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

func TestEnabledFromEnvHonorsAgentsViewAndGenericOptOut(t *testing.T) {
	clearTelemetryVetoes(t)
	t.Setenv(GenericEnabledEnv, "1")

	t.Setenv(EnabledEnv, "0")
	assert.False(t, EnabledFromEnv())

	t.Setenv(EnabledEnv, "1")
	assert.True(t, EnabledFromEnv())

	t.Setenv(GenericEnabledEnv, "0")
	assert.False(t, EnabledFromEnv())
}

func TestNewReporterDisabledDuringTestsDespiteEnabledEnv(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	clearTelemetryVetoes(t)
	t.Setenv(GenericEnabledEnv, "1")

	reporter, err := NewReporter(Options{ScreenClaimsPath: filepath.Join(t.TempDir(), "screens"), InstallationID: "anonymous-install-id"})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func TestNewReporterOptedOutKeepsAllowlist(t *testing.T) {
	clearTelemetryVetoes(t)
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "0")

	reporter, err := NewReporter(Options{ScreenClaimsPath: filepath.Join(t.TempDir(), "screens")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })

	assert.False(t, reporter.Enabled())
	assert.True(t, reporter.EventAllowed(EventAppOpened))
	assert.True(t, reporter.EventAllowed(EventDaemonActive))
	assert.False(t, reporter.EventAllowed("daemon_started"))
}

func TestAllowedEventOptionsConfigureDaemonActiveShape(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	clearTelemetryVetoes(t)
	t.Setenv(GenericEnabledEnv, "1")

	client, err := newKitReporter(Options{
		ScreenClaimsPath: filepath.Join(t.TempDir(), "screens"),
		InstallationID:   "anonymous-install-id", Version: "v1.2.3", Commit: "abc123",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	reporter := &Reporter{client: client}

	props, err := reporter.SanitizeProperties(EventDaemonActive, map[string]any{
		"app":     "legacy-app",
		"project": "private-project",
		"session": "private-session",
	})
	require.NoError(t, err)

	assert.Equal(t, "agentsview", props["application"])
	assert.Equal(t, "v1.2.3", props["version"])
	assert.Equal(t, "abc123", props["commit"])
	assert.Equal(t, "daemon", props["source"])
	assert.NotContains(t, props, "app")
	assert.NotContains(t, props, "project")
	assert.NotContains(t, props, "session")
}

func clearTelemetryVetoes(t *testing.T) {
	t.Helper()
	t.Setenv("DO_NOT_TRACK", "0")
	t.Setenv("CI", "")
	require.NoError(t, os.Unsetenv("CI"))
}
