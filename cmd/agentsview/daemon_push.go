package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/agentsview/internal/apiclient"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// waitForStartupSync waits for the local daemon's initial sync, forwarding
// progress when the daemon returns an event stream.
func waitForStartupSync(ctx context.Context, tr transport, authToken string, onProgress func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
	var zero syncpkg.SyncStats
	api, err := apiclient.NewHTTPClient(tr.URL, authToken, &http.Client{Timeout: 0})
	if err != nil {
		return zero, err
	}
	response, err := api.PostAPIV1SyncStreamWithResponse(ctx, &apiclient.PostAPIV1SyncRequestOptions{
		Query: &apiclient.PostAPIV1SyncQuery{Wait: new(true), StartupOnly: new(true)},
	})
	if response == nil {
		return zero, err
	}
	defer response.HTTPResponse.Body.Close()
	if response.HTTPResponse.StatusCode != http.StatusOK {
		return zero, errors.New(daemonErrorMessage(response.HTTPResponse.StatusCode, response.Body))
	}
	if strings.HasPrefix(response.HTTPResponse.Header.Get("Content-Type"), "text/event-stream") {
		return consumeDaemonPushEvents[syncpkg.SyncStats](response.Stream200, onProgress)
	}
	var out syncpkg.SyncStats
	if err := json.Unmarshal(response.Body, &out); err != nil {
		return zero, err
	}
	return out, nil
}

// consumeDaemonPushEvents applies daemon progress and terminal events decoded
// by the generated client's stream.
func consumeDaemonPushEvents[T, P any](stream *runtime.Stream[[]byte], onProgress func(P)) (T, error) {
	var result, zero T
	var done bool
	var pushErr error
	defer stream.Close()
	for stream.Next() {
		frame := stream.Event()
		if len(frame.Data) == 0 {
			continue
		}
		switch frame.Type {
		case "done", "report":
			if err := json.Unmarshal(frame.Data, &result); err != nil {
				return zero, fmt.Errorf("decoding daemon push result: %w", err)
			}
			done = true
		case "progress":
			if onProgress != nil {
				var progress P
				if err := json.Unmarshal(frame.Data, &progress); err != nil {
					return zero, fmt.Errorf("decoding daemon push progress: %w", err)
				}
				onProgress(progress)
			}
		default:
			var apiErr apiclient.APIErrorResponse
			if err := json.Unmarshal(frame.Data, &apiErr); err == nil && apiErr.ErrorData != "" {
				pushErr = errors.New(apiErr.ErrorData)
			} else {
				pushErr = fmt.Errorf("daemon push error: %s", frame.Data)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return zero, err
	}
	if pushErr != nil {
		return zero, pushErr
	}
	if !done {
		return zero, errors.New("daemon push response missing done event")
	}
	return result, nil
}
