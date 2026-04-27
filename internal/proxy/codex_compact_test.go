package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// TestServer_CodexCompactPaths_ForwardToCompactEndpointWithCodexAuth verifies
// that both /v1/responses/compact and /responses/compact forward to upstream
// /responses/compact with Codex auth injected and the response body proxied.
func TestServer_CodexCompactPaths_ForwardToCompactEndpointWithCodexAuth(t *testing.T) {
	tests := []struct {
		name        string
		requestPath string
	}{
		{"canonical path", codexCompactResponsesPath},
		{"legacy path", legacyCodexCompactResponsesPath},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAuth, gotAcctID string
			inner := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotAcctID = r.Header.Get("ChatGPT-Account-ID")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"object":"response.compact","output":"compact result"}`)),
				}, nil
			})

			srv := &Server{
				Config: &Config{
					CodexUpstream: "https://chatgpt.com",
					LocalToken:    "tok",
				},
				CodexTransport: &CodexTokenTransport{
					Selector: &fakeCodexSelector{account: &codex.CodexAccount{
						AccessToken: "codex-tok",
						AccountID:   "acct-1",
					}},
					Inner: inner,
				},
			}

			handler, err := srv.handler()
			if err != nil {
				t.Fatalf("handler() error = %v", err)
			}

			body := `{"model":"gpt-5.4","previous_response_id":"resp_abc"}`
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tt.requestPath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
			}
			if gotPath != "/responses/compact" {
				t.Errorf("upstream path = %q, want /responses/compact", gotPath)
			}
			if gotAuth != "Bearer codex-tok" {
				t.Errorf("upstream auth = %q, want Bearer codex-tok", gotAuth)
			}
			if gotAcctID != "acct-1" {
				t.Errorf("upstream account-id = %q, want acct-1", gotAcctID)
			}
			if !strings.Contains(w.Body.String(), "output") {
				t.Errorf("response body should contain output: %s", w.Body.String())
			}
		})
	}
}

// TestServer_CodexCompact_NoTransport verifies that POST /responses/compact
// with nil CodexTransport returns 503.
func TestServer_CodexCompact_NoTransport(t *testing.T) {
	srv := &Server{
		Config: &Config{
			CodexUpstream: "https://chatgpt.com",
			LocalToken:    "tok",
		},
		CodexTransport: nil,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/responses/compact", strings.NewReader(`{"model":"gpt-5.4"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestServer_CodexCompact_RejectsWebsocket verifies that POST /responses/compact
// with a WebSocket upgrade header returns 400 mentioning codexAppServerPath.
func TestServer_CodexCompact_RejectsWebsocket(t *testing.T) {
	srv := &Server{
		Config: &Config{
			CodexUpstream: "https://chatgpt.com/backend-api/codex",
			LocalToken:    "tok",
		},
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/responses/compact", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), codexAppServerPath) {
		t.Errorf("body = %q, want mention of %s", w.Body.String(), codexAppServerPath)
	}
}

// TestServer_CodexCompact_RejectsGetWebsocket verifies that real WebSocket
// upgrade requests on compact paths hit the explicit compact rejection.
func TestServer_CodexCompact_RejectsGetWebsocket(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"canonical path", codexCompactResponsesPath},
		{"legacy path", legacyCodexCompactResponsesPath},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{
				Config: &Config{
					CodexUpstream: "https://chatgpt.com/backend-api/codex",
					LocalToken:    "tok",
				},
			}

			handler, err := srv.handler()
			if err != nil {
				t.Fatalf("handler() error = %v", err)
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "invalid_request_error") {
				t.Errorf("body = %q, want invalid_request_error", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), codexAppServerPath) {
				t.Errorf("body = %q, want mention of %s", w.Body.String(), codexAppServerPath)
			}
		})
	}
}

// TestServer_CodexCompact_GetMethodNotAllowed verifies that non-upgrade GET
// requests on compact paths do not fall through to the authenticated proxy.
func TestServer_CodexCompact_GetMethodNotAllowed(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"canonical path", codexCompactResponsesPath},
		{"legacy path", legacyCodexCompactResponsesPath},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{
				Config: &Config{
					CodexUpstream: "https://chatgpt.com/backend-api/codex",
					LocalToken:    "tok",
				},
			}

			handler, err := srv.handler()
			if err != nil {
				t.Fatalf("handler() error = %v", err)
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405, body: %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want %s", got, http.MethodPost)
			}
		})
	}
}

// TestServer_CodexCompact_DoesNotUseHeadroom verifies that a native compact
// request does not invoke the headroom bridge, and the upstream receives the
// original request body unmodified.
func TestServer_CodexCompact_DoesNotUseHeadroom(t *testing.T) {
	bridgeCalled := false

	var gotBody []byte
	inner := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"object":"response.compact"}`)),
		}, nil
	})

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err == nil && req.Operation == "compress_responses" {
			bridgeCalled = true
		}
		return nil
	})

	originalBody := `{"model":"gpt-5.4","previous_response_id":"resp_abc"}`

	srv := &Server{
		Config: &Config{
			CodexUpstream: "https://chatgpt.com/backend-api/codex",
			LocalToken:    "tok",
		},
		CodexTransport: &CodexTokenTransport{
			Selector: &fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			Inner:    inner,
		},
		Headroom: bridge,
	}

	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/responses/compact", strings.NewReader(originalBody))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if bridgeCalled {
		t.Error("headroom bridge compress_responses should not be called for compact requests")
	}
	if string(gotBody) != originalBody {
		t.Errorf("upstream body = %s, want original %s", gotBody, originalBody)
	}
}
