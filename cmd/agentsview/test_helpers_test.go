package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/daemon"
	kitvec "go.kenn.io/kit/vector"
)

// defaultArchiveQueryPolicy builds an archiveQueryPolicy seeded with the
// defaults shared by the usage tests: skip the read-only daemon and refresh
// usage directly. mut may override any field; pass nil to use the defaults.
func defaultArchiveQueryPolicy(mut func(*archiveQueryPolicy)) archiveQueryPolicy {
	policy := archiveQueryPolicy{
		ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
		DirectReadOnlyAction: "refresh usage directly",
	}
	if mut != nil {
		mut(&policy)
	}
	return policy
}

// resolveTestArchiveQueryBackend resolves an archive-query backend for policy,
// failing the test on error and registering the cleanup hook.
func resolveTestArchiveQueryBackend(
	t *testing.T, policy archiveQueryPolicy,
) archiveQueryBackend {
	t.Helper()
	backend, cleanup, err := resolveArchiveQueryBackend(t.Context(), policy)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return backend
}

// testDataDir creates an isolated data directory, exports it via
// AGENTSVIEW_DATA_DIR for the duration of the test, and returns the path.
func testDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	return dir
}

// captureOutput redirects an os.File-backed stream (os.Stdout or os.Stderr)
// into a buffer while fn runs and returns everything written. set installs the
// pipe writer and restore returns the original stream; both are supplied by the
// thin wrappers below so callers do not repeat the pipe/copy/restore plumbing.
func captureOutput(
	t *testing.T, set func(*os.File), restore func() *os.File, fn func(),
) string {
	t.Helper()

	orig := restore()
	r, w, err := os.Pipe()
	require.NoError(t, err, "pipe")
	set(w)
	t.Cleanup(func() { set(orig) })

	var buf bytes.Buffer
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(&buf, r)
		readDone <- err
	}()

	fn()

	require.NoError(t, w.Close(), "close pipe writer")
	set(orig)

	require.NoError(t, <-readDone, "read pipe")
	require.NoError(t, r.Close(), "close pipe reader")
	return buf.String()
}

// captureStdout returns everything fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureOutput(t,
		func(f *os.File) { os.Stdout = f },
		func() *os.File { return os.Stdout },
		fn,
	)
}

// captureStderr returns everything fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return captureOutput(t,
		func(f *os.File) { os.Stderr = f },
		func() *os.File { return os.Stderr },
		fn,
	)
}

// captureLogOutput redirects the standard logger into a buffer for the
// duration of the test and restores the previous writer on cleanup.
func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// restoreTestLogOutput saves the global log writer and restores it on cleanup,
// closing any file-backed writer installed during the test first so TempDir
// cleanup can remove it on Windows.
func restoreTestLogOutput(t *testing.T) {
	t.Helper()
	orig := log.Writer()
	t.Cleanup(func() {
		if closer, ok := log.Writer().(io.Closer); ok && log.Writer() != orig {
			_ = closer.Close()
		}
		log.SetOutput(orig)
	})
}

// writeTestFile writes content to path, failing the test on error.
func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, content, 0o644), "write %s", path)
}

// requireSymlinkOrSkip creates a symlink from link to target, skipping the test
// when the platform or filesystem does not support symlinks. The target must
// exist first: os.Symlink on Windows types the link (file vs directory) by
// statting the target at creation time, and a link created before its
// directory target exists becomes a file symlink whose directory reads later
// fail with access denied.
func requireSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	_, statErr := os.Stat(target)
	require.NoError(t, statErr,
		"symlink target must exist before creating the link")
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.ENOTSUP) {
		t.Skip("symlinks not supported:", err)
	}
	require.NoError(t, err, "symlink")
}

// daemonRouteTestServer starts a test daemon whose /api/ping answers with the
// kit ping handler and the given routes answer with the supplied handlers.
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

// registerSyncRouteTestRuntime points dataDir's daemon runtime record at the
// test server URL so commands discover it as the running daemon.
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

// isolateDirectCLISources re-homes $HOME and every parser's direct-source
// env var into a temp dir so discovery cannot find the developer's sessions.
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

// enabledVectorConfig returns a config with a small fake-model vector setup.
func enabledVectorConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		DataDir: t.TempDir(),
		Vector: config.VectorConfig{
			Enabled: true,
			Embeddings: config.VectorEmbeddingsConfig{
				Model:         "fake-model",
				Dimension:     4,
				MaxInputChars: 8192,
			},
		},
	}
}

// buildTestVectorsDB builds a vectors.db for cfg from testPushUnitSource.
func buildTestVectorsDB(t *testing.T, cfg config.Config) {
	t.Helper()
	ctx := context.Background()
	ix, err := vector.Open(
		ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false,
		cfg.Vector.Embeddings.MaxInputChars,
	)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 4}
	result, err := ix.Build(
		ctx, testPushUnitSource(), fakePushEncoder(), gen, vector.BuildOptions{},
	)
	require.NoError(t, err)
	require.True(t, result.Activated)
}

// fakePushUnitSource is a fixed UnitSource with the units the vector tests
// build their vectors.db from.
type fakePushUnitSource struct {
	units []db.EmbeddableUnit
}

func (f fakePushUnitSource) ScanEmbeddableUnits(
	_ context.Context, _ string, _ bool, _ int, fn func(db.EmbeddableUnit) error,
) (string, error) {
	for _, u := range f.units {
		if err := fn(u); err != nil {
			return "", err
		}
	}
	return "2024-01-01T00:00:03Z", nil
}

// fakePushEncoder returns a deterministic 4-dimensional encoder matching the
// gen dimension the round-trip test builds with.
func fakePushEncoder() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out, nil
	}
}

// testPushUnitSource returns the fixed unit set the vector tests build their
// vectors.db from: two sessions, three documents.
func testPushUnitSource() fakePushUnitSource {
	return fakePushUnitSource{units: []db.EmbeddableUnit{
		{SessionID: "session-1", Kind: "user", SourceUUID: "u1", Content: "hello"},
		{
			SessionID: "session-1", Kind: "run", SourceUUID: "a1",
			Ordinal: 1, OrdinalEnd: 2, Content: "first\n\nsecond",
			Offsets: []db.UnitOffset{{Ordinal: 1}, {Ordinal: 2, RuneStart: 7, ByteStart: 7}},
		},
		{SessionID: "session-2", Kind: "user", SourceUUID: "u2", Content: "world"},
	}}
}

// syncBuffer is a mutex-guarded bytes.Buffer: a heartbeat goroutine writes
// while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Some helpers strip AGENTSVIEW_* from child environments.
const testRuntimeRootVar = "AVTEST_RUNTIME_ROOT"

// testRuntimeRootEnv returns the runtime root variables for a helper child
// whose environment a test builds by hand.
func testRuntimeRootEnv() []string {
	return []string{
		testRuntimeRootVar + "=" + os.Getenv(testRuntimeRootVar),
		"XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR"),
	}
}

// runWithTestRuntimeRoot points XDG_RUNTIME_DIR at one short private directory
// shared with helper child processes, which inherit the environment. Serve
// tests with long t.TempDir data dirs put the Chrome host socket there, and
// removing it at exit cleans up after children that tests kill.
func runWithTestRuntimeRoot(m *testing.M) int {
	if os.Getenv(testRuntimeRootVar) != "" {
		return m.Run()
	}
	root, err := os.MkdirTemp("", "av")
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			panic(err)
		}
	}()
	for key, value := range map[string]string{testRuntimeRootVar: root, "XDG_RUNTIME_DIR": root} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	return m.Run()
}

func TestMain(m *testing.M) {
	// Match the default daemon fixtures; replacement tests set their own versions.
	version = "test"
	os.Exit(runWithTestRuntimeRoot(m))
}
