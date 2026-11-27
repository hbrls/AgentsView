package server

import (
	"time"
)

// pushProgressLogInterval bounds how often the daemon-side push handlers log
// progress to debug.log. A package var so tests can shrink it.
var pushProgressLogInterval = 15 * time.Second

// pushProgressStreamInterval bounds how often a push handler forwards progress
// reports onto its SSE stream: the session loop reports per session, which
// would otherwise emit one event per row. A package var so tests can shrink it.
var pushProgressStreamInterval = 200 * time.Millisecond

// newPushProgressStreamSender wraps send so it forwards at most one progress
// report per pushProgressStreamInterval.
func newPushProgressStreamSender[P any](send func(P)) func(P) {
	var last time.Time
	return func(p P) {
		if time.Since(last) < pushProgressStreamInterval {
			return
		}
		last = time.Now()
		send(p)
	}
}
