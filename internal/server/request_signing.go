package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/requestsign"
	"go.kenn.io/agentsview/internal/service"
)

func restrictedIngress(ctx context.Context) bool {
	value, _ := ctx.Value(ctxKeyRestrictedIngress).(bool)
	return value
}

func (s *Server) validateRestrictedPolicyAuth(policy requestsign.Policy) error {
	for _, key := range policy.Keys {
		if key.Grant != "reader" {
			continue
		}
		s.mu.RLock()
		token := s.cfg.AuthToken
		s.mu.RUnlock()
		if token == "" {
			return errors.New("machine signing reader grant requires a configured auth_token")
		}
		break
	}
	return nil
}

// RestrictedHandler mounts only signed machine operations. Its fixed external
// URI configuration is trusted operator input, never taken from proxy headers.
// Main Handler and browser authentication retain their existing behavior.
func (s *Server) RestrictedHandler(policyFile string) (http.Handler, func() error, error) {
	policy, _, err := requestsign.ReadPolicy(policyFile)
	if err != nil {
		return nil, nil, err
	}
	if err := s.validateRestrictedPolicyAuth(policy); err != nil {
		return nil, nil, err
	}
	replay, err := requestsign.OpenReplay(policy.ReplayDB)
	if err != nil {
		return nil, nil, err
	}
	verifier, err := requestsign.NewVerifier(requestsign.VerifyConfig{
		ExternalURL: policy.ExternalURL, Replay: replay,
		Keys: func() (map[string]requestsign.Key, error) {
			updated, keys, err := requestsign.ReadPolicy(policyFile)
			if err != nil {
				return nil, err
			}
			if updated.ExternalURL != policy.ExternalURL || updated.ReplayDB != policy.ReplayDB || updated.Listen != policy.Listen {
				return nil, errors.New("listener signing policy changed; restart required")
			}
			if err := s.validateRestrictedPolicyAuth(updated); err != nil {
				return nil, err
			}
			return keys, nil
		},
	})
	if err != nil {
		_ = replay.Close()
		return nil, nil, err
	}
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), ctxKeyRestrictedIngress, true)
			s.authMiddlewareRequired(next, true).ServeHTTP(w, r.WithContext(ctx))
		})
	}
	authorize := func(key requestsign.Key, r *http.Request) bool {
		if !restrictedOperation(key.Grant, r.Method, r.URL.Path) {
			return false
		}
		s.mu.RLock()
		token := s.cfg.AuthToken
		s.mu.RUnlock()
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return ok && token != "" && hmac.Equal([]byte(provided), []byte(token))
	}
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/version" && service.SupportsRecallQueries(s.sessions) {
			w.Header().Set("X-Agentsview-Recall-Queries", "non-recording")
		}
		gzipMiddleware(logMiddleware(s.archiveStaleMiddleware(s.mux))).ServeHTTP(w, r)
	})
	// Requests are logged only after signature verification, so unsigned
	// traffic from the public terminator cannot write log lines.
	return s.idle.Wrap(verifier.Wrap(dispatch, authenticate, authorize)), replay.Close, nil
}

func restrictedOperation(grant, method, path string) bool {
	if grant != "reader" {
		return false
	}
	if method == http.MethodPost {
		return path == "/api/v1/recall/query"
	}
	if method != http.MethodGet {
		return false
	}
	switch path {
	case "/api/ping", "/api/v1/version",
		"/api/v1/sessions", "/api/v1/sessions/sidebar-index", "/api/v1/session-ids/resolve",
		"/api/v1/projects", "/api/v1/machines", "/api/v1/agents", "/api/v1/branches",
		"/api/v1/search", "/api/v1/search/content", "/api/v1/events",
		"/api/v1/recall/entries", "/api/v1/memory/status", "/api/v1/recent-edits",
		"/api/v1/usage/summary", "/api/v1/usage/pairwise-comparison",
		"/api/v1/analytics/tools", "/api/v1/analytics/signals", "/api/v1/analytics/signal-sessions",
		"/api/v1/activity/report":
		return true
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/activity/report/"); ok {
		id, ok := strings.CutSuffix(rest, "/sessions")
		return ok && id != "" && !strings.Contains(id, "/")
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/sessions/"); ok {
		parts := strings.Split(rest, "/")
		if parts[0] == "" {
			return false
		}
		if len(parts) == 1 {
			return true
		}
		if len(parts) != 2 {
			return false
		}
		switch parts[1] {
		case "messages", "tool-calls", "children", "activity", "timing", "usage", "watch", "md", "search":
			return true
		}
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/recall/entries/"); ok {
		return rest != "" && !strings.Contains(rest, "/")
	}
	return false
}
