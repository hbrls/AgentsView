package server

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type openAPISpec struct {
	Paths map[string]map[string]openAPIOperation `json:"paths"`
}

type openAPIOperation struct {
	Parameters []openAPIParameter         `json:"parameters"`
	Responses  map[string]openAPIResponse `json:"responses"`
}

type openAPIParameter struct {
	In       string `json:"in"`
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

type openAPIResponse struct {
	Content map[string]any `json:"content"`
}

func readOpenAPISpec(tb testing.TB, h http.Handler) openAPISpec {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodGet, "/api/openapi.json", nil)
	req.Host = "127.0.0.1:0"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(tb, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var spec openAPISpec
	require.NoError(tb, json.Unmarshal(w.Body.Bytes(), &spec))
	return spec
}

func requireOpenAPIOperation(
	tb testing.TB,
	spec openAPISpec,
	method string,
	path string,
) openAPIOperation {
	tb.Helper()
	require.Contains(tb, spec.Paths, path)
	require.Contains(tb, spec.Paths[path], method)
	return spec.Paths[path][method]
}

// TestNewPushProgressStreamSenderThrottles pins the SSE fan-out throttle: the
// session loop reports per session, and forwarding every report would emit
// one SSE event per row.
func TestNewPushProgressStreamSenderThrottles(t *testing.T) {
	origInterval := pushProgressStreamInterval
	pushProgressStreamInterval = time.Hour
	t.Cleanup(func() { pushProgressStreamInterval = origInterval })

	var got []int
	send := newPushProgressStreamSender(func(v int) { got = append(got, v) })
	send(1)
	send(2)
	assert.Equal(t, []int{1}, got,
		"second report inside the throttle window must not send")

	pushProgressStreamInterval = 0
	send(3)
	assert.Equal(t, []int{1, 3}, got)
}
