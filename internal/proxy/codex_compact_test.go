package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
				CodexRequests: testCodexRequestRouter(&fakeCodexSelector{account: &codex.CodexAccount{
					AccessToken: "codex-tok",
					AccountID:   "acct-1",
				}}, inner),
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

func TestServerCodexCompactRelaysBodyOverLegacyLimit(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("x", maxRequestBody+1) + `"}`)
	var upstreamBody []byte
	server := &Server{
		Config: &Config{CodexUpstream: "https://codex.example"},
		CodexRequests: testCodexRequestRouter(&fakeCodexSelector{account: &codex.CodexAccount{
			AccessToken: "codex-token",
			AccountID:   "account-a",
		}}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			var err error
			upstreamBody, err = io.ReadAll(request.Body)
			return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{"X-Upstream": {"large"}}, Body: io.NopCloser(strings.NewReader("accepted"))}, err
		})),
	}
	request := httptest.NewRequest(http.MethodPost, legacyCodexCompactResponsesPath, bytes.NewReader(body))
	response := httptest.NewRecorder()

	server.handleNativeCodexCompact(response, request, legacyCodexCompactResponsesPath)

	if response.Code != http.StatusCreated || response.Body.String() != "accepted" || response.Header().Get("X-Upstream") != "large" {
		t.Fatalf("response = %d/%q/%q", response.Code, response.Body.String(), response.Header().Get("X-Upstream"))
	}
	if !bytes.Equal(upstreamBody, body) {
		t.Fatalf("upstream body = %d bytes, want %d", len(upstreamBody), len(body))
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
		CodexRequests: nil,
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
	if !strings.Contains(w.Body.String(), legacyCodexResponsesPath) {
		t.Errorf("body = %q, want mention of %s", w.Body.String(), legacyCodexResponsesPath)
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
			if !strings.Contains(w.Body.String(), legacyCodexResponsesPath) {
				t.Errorf("body = %q, want mention of %s", w.Body.String(), legacyCodexResponsesPath)
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
		CodexRequests: testCodexRequestRouter(
			&fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			inner,
		),
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

func TestServer_CodexCompact_ZstdUsesDecodedDiagnosticsAndPreservesFrame(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloadDiag, err := OpenPayloadWriter(payloadPath)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	defer payloadDiag.Close()

	var upstreamBody []byte
	var upstreamHeader http.Header
	inner := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstreamBody, _ = io.ReadAll(request.Body)
		upstreamHeader = request.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"object":"response.compact"}`)),
		}, nil
	})
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	srv := &Server{
		Config: &Config{CodexUpstream: "https://chatgpt.com/backend-api/codex", LocalToken: "tok"},
		CodexRequests: testCodexRequestRouter(
			&fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			inner,
		),
		CodexObserver: observer,
		PayloadDiag:   payloadDiag,
	}

	original := []byte(`{"model":"gpt-5.4","conversation_id":"conversation-compact-zstd","input":"ping"}`)
	encoded := encodeCodexZstd(t, original)
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, legacyCodexCompactResponsesPath, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Content-Length", fmt.Sprint(len(encoded)))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(upstreamBody, encoded) {
		t.Fatal("compact request did not preserve the original zstd frame")
	}
	if upstreamHeader.Get("Content-Encoding") != "zstd" {
		t.Fatalf("upstream Content-Encoding = %q, want zstd", upstreamHeader.Get("Content-Encoding"))
	}
	if upstreamHeader.Get("Content-Length") != fmt.Sprint(len(encoded)) {
		t.Fatalf("upstream Content-Length = %q, want %d", upstreamHeader.Get("Content-Length"), len(encoded))
	}
	if err := payloadDiag.Close(); err != nil {
		t.Fatalf("close payload diagnostics: %v", err)
	}
	events := readPayloadEvents(t, payloadPath)
	if len(events) != 1 {
		t.Fatalf("payload events = %d, want 1", len(events))
	}
	if events[0].Model != "gpt-5.4" {
		t.Fatalf("payload model = %q, want gpt-5.4", events[0].Model)
	}
	if events[0].SessionSource != "body:conversation_id" || events[0].SessionKey == "" {
		t.Fatalf("payload session = %q/%q, want body conversation identity", events[0].SessionSource, events[0].SessionKey)
	}
	if events[0].BodyBytes != len(encoded) {
		t.Fatalf("payload body bytes = %d, want original encoded length %d", events[0].BodyBytes, len(encoded))
	}
	wantCapturedBody, err := json.Marshal(string(encoded))
	if err != nil {
		t.Fatalf("encode expected payload body: %v", err)
	}
	if !bytes.Equal(events[0].Body, wantCapturedBody) {
		t.Fatal("payload diagnostics did not retain existing encoded-body semantics")
	}
	health := observer.Health()
	if health.Requests != 1 || health.ZstdRequests != 1 || health.RequestDecodeErrors != 0 {
		t.Fatalf("observer requests=%d zstd=%d decode_errors=%d, want 1/1/0", health.Requests, health.ZstdRequests, health.RequestDecodeErrors)
	}
}

func TestServer_CodexCompact_ObserverUsesAcceptedDecodedView(t *testing.T) {
	noise := make([]byte, 3<<20)
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	state := uint64(0x9e3779b97f4a7c15)
	for index := range noise {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		noise[index] = alphabet[state&63]
	}
	original := append([]byte(`{"model":"gpt-5.4","conversation_id":"conversation-large-compact","input":"`), noise...)
	original = append(original, []byte(`"}`)...)
	encoded := encodeCodexZstd(t, original)
	if len(encoded) <= DefaultCodexZstdLimits.MaxEncodedBytes {
		t.Fatalf("fixture encoded bytes = %d, want over observer default %d", len(encoded), DefaultCodexZstdLimits.MaxEncodedBytes)
	}
	if len(encoded) > maxRequestBody {
		t.Fatalf("fixture encoded bytes = %d, want within native limit %d", len(encoded), maxRequestBody)
	}

	var upstreamBody []byte
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	srv := &Server{
		Config: &Config{CodexUpstream: "https://chatgpt.com/backend-api/codex", LocalToken: "tok"},
		CodexRequests: testCodexRequestRouter(
			&fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			roundTripFunc(func(request *http.Request) (*http.Response, error) {
				upstreamBody, _ = io.ReadAll(request.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"object":"response.compact"}`)),
				}, nil
			}),
		),
		CodexObserver: observer,
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, legacyCodexCompactResponsesPath, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "zstd")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(upstreamBody, encoded) {
		t.Fatal("large compact request did not preserve the original zstd frame")
	}
	health := observer.Health()
	if health.Requests != 1 || health.ZstdRequests != 1 || health.RequestDecodeErrors != 0 || health.MetadataParseErrors != 0 {
		t.Fatalf("observer requests=%d zstd=%d decode_errors=%d metadata_errors=%d, want 1/1/0/0", health.Requests, health.ZstdRequests, health.RequestDecodeErrors, health.MetadataParseErrors)
	}
}

func TestServer_CodexCompact_RewritesHandlerAcceptedLargeBodies(t *testing.T) {
	identityBody, zstdBody := handlerAcceptedRewriteFixtures(t)
	tests := []struct {
		name     string
		body     []byte
		encoding string
	}{
		{name: "identity", body: identityBody, encoding: "identity"},
		{name: "zstd", body: zstdBody, encoding: "zstd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamBody []byte
			var upstreamEncoding string
			router := testCodexRequestRouter(
				&fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
				roundTripFunc(func(request *http.Request) (*http.Response, error) {
					upstreamBody, _ = io.ReadAll(request.Body)
					upstreamEncoding = request.Header.Get("Content-Encoding")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"object":"response.compact"}`)),
					}, nil
				}),
			)
			router.Scope.(*legacySelectorScope).effective = codexFallbackModel
			srv := &Server{
				Config:        &Config{CodexUpstream: "https://chatgpt.com/backend-api/codex", LocalToken: "tok"},
				CodexRequests: router,
			}
			request := httptest.NewRequest(http.MethodPost, legacyCodexCompactResponsesPath, bytes.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Content-Encoding", test.encoding)
			request.Header.Set("Content-Length", fmt.Sprint(len(test.body)))
			response := httptest.NewRecorder()

			srv.handleNativeCodexCompact(response, request, legacyCodexCompactResponsesPath)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", response.Code, response.Body.String())
			}
			decoded, err := DecodeCodexRequest(upstreamBody, upstreamEncoding, codexHTTPZstdLimits())
			if err != nil {
				t.Fatalf("decode upstream request: %v", err)
			}
			if model := extractModel(decoded.Decoded()); model != codexFallbackModel {
				t.Fatalf("upstream model = %q, want %q", model, codexFallbackModel)
			}
		})
	}
}

func TestServer_CodexCompact_MalformedZstdFailsBeforeDispatch(t *testing.T) {
	upstreamCalled := false
	srv := &Server{
		Config: &Config{CodexUpstream: "https://chatgpt.com/backend-api/codex", LocalToken: "tok"},
		CodexRequests: testCodexRequestRouter(
			&fakeCodexSelector{account: &codex.CodexAccount{AccessToken: "codex-tok"}},
			roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				upstreamCalled = true
				return nil, fmt.Errorf("unexpected upstream request")
			}),
		),
	}
	handler, err := srv.handler()
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, legacyCodexCompactResponsesPath, strings.NewReader("not-zstd"))
	request.Header.Set("Content-Encoding", "zstd")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", response.Code, response.Body.String())
	}
	if upstreamCalled {
		t.Fatal("malformed zstd compact request reached upstream")
	}
}
