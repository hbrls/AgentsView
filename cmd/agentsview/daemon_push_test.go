package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/storage"
)

func TestParseDaemonPushSSE(t *testing.T) {
	stream := func(events ...string) string {
		return strings.Join(events, "")
	}
	progressEvent := `event: progress` + "\n" +
		`data: {"SessionsDone":3,"SessionsTotal":10}` + "\n\n"
	doneEvent := `event: done` + "\n" +
		`data: {"SessionsPushed":10,"MessagesPushed":42}` + "\n\n"

	t.Run("progress then done", func(t *testing.T) {
		var progress []storage.PushProgress
		result, err := consumeDaemonPushEvents[storage.PushResult](daemonEventStream(strings.NewReader(stream(progressEvent, doneEvent))),
			func(p storage.PushProgress) { progress = append(progress, p) },
		)
		require.NoError(t, err)
		assert.Equal(t, 10, result.SessionsPushed)
		assert.Equal(t, 42, result.MessagesPushed)
		require.Len(t, progress, 1)
		assert.Equal(t, 3, progress[0].SessionsDone)
		assert.Equal(t, 10, progress[0].SessionsTotal)
	})

	t.Run("nil onProgress is safe", func(t *testing.T) {
		result, err := consumeDaemonPushEvents[storage.PushResult, storage.PushProgress](daemonEventStream(strings.NewReader(stream(progressEvent, doneEvent))), nil)
		require.NoError(t, err)
		assert.Equal(t, 10, result.SessionsPushed)
	})

	t.Run("error event fails the push", func(t *testing.T) {
		errEvent := "event: error\n" + `data: {"error":"schema: boom"}` + "\n\n"
		_, err := consumeDaemonPushEvents[storage.PushResult, storage.PushProgress](daemonEventStream(strings.NewReader(stream(progressEvent, errEvent))), nil)
		require.Error(t, err)
		assert.Equal(t, "schema: boom", err.Error())
	})

	t.Run("stream without done event fails", func(t *testing.T) {
		_, err := consumeDaemonPushEvents[storage.PushResult, storage.PushProgress](daemonEventStream(strings.NewReader(stream(progressEvent))), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing done event")
	})

	// A dropped stream keeps a --full request unless the daemon rebuilt the
	// archive, after which every mirror pushes in full.
	t.Run("stream that ends tells whether the archive was rebuilt", func(t *testing.T) {
		waitingEvent := "event: progress\n" + `data: {"Phase":"waiting"}` + "\n\n"
		rebuiltEvent := "event: progress\n" + `data: {"Phase":"rebuilt"}` + "\n\n"
		for _, tt := range []struct {
			name    string
			events  []string
			started bool
		}{
			{"while waiting", []string{waitingEvent}, false},
			{"during a copy", []string{waitingEvent, progressEvent}, false},
			{"after a rebuild", []string{waitingEvent, rebuiltEvent, progressEvent}, true},
		} {
			t.Run(tt.name, func(t *testing.T) {
				_, err := consumeDaemonPushEvents[storage.PushResult, storage.PushProgress](
					daemonEventStream(strings.NewReader(stream(tt.events...))), nil,
				)
				require.ErrorContains(t, err, "missing done event")
				_, notStarted := errors.AsType[storage.PushNotStartedError](err)
				assert.Equal(t, tt.started, !notStarted)
			})
		}
	})

	t.Run("terminal report can exceed ten mebibytes", func(t *testing.T) {
		type largeReport struct {
			ProjectMetadata string `json:"project_metadata"`
		}
		metadata := strings.Repeat("x", 10*1024*1024+1)
		reportEvent := "event: report\ndata: {\"project_metadata\":\"" +
			metadata + "\"}\n\n"

		result, err := consumeDaemonPushEvents[largeReport, struct{}](daemonEventStream(strings.NewReader(reportEvent)), nil)
		require.NoError(t, err)
		assert.Equal(t, metadata, result.ProjectMetadata)
	})
}

func daemonEventStream(body io.Reader) *runtime.Stream[[]byte] {
	return runtime.NewEventStream[[]byte](&http.Response{Body: io.NopCloser(body)})
}
