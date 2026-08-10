package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCodexResyncRequiresNewGenerationAndPortableFullRequest(t *testing.T) {
	for _, budget := range []int{0, 1, 2} {
		budget := budget
		t.Run(fmt.Sprintf("budget-%d", budget), func(t *testing.T) {
			for trial := 0; trial < 100; trial++ {
				key := testCodexLeaseKey("thread", "turn")
				intent, err := NewCodexWSRotationIntent(key, 7, 11, 13, "0.147.0", budget)
				if err != nil {
					t.Fatal(err)
				}
				matched, err := intent.ObserveUpstreamError([]byte(`{"type":"error","error":{"code":"previous_response_not_found","message":"missing"}}`))
				if err != nil || !matched {
					t.Fatalf("matched=%v error=%v", matched, err)
				}
				request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
				transport := "websocket"
				if trial%2 == 1 {
					transport = "http"
				}
				if err := intent.ConsumeReplacement(request, 12, 14, "0.147.0", budget, transport); err != nil {
					t.Fatal(err)
				}
				if !intent.Consumed || intent.ReplacementTransport != transport {
					t.Fatalf("intent = %+v", intent)
				}
			}
		})
	}
}

func TestCodexResyncFullNewTurnUsesSeparateReconnectTrigger(t *testing.T) {
	predecessor := testCodexLeaseKey("thread", "old-turn")
	target := testCodexLeaseKey("thread", "new-turn")
	intent, _ := NewCodexWSRotationIntent(target, 7, 11, 13, "0.147.0", 1)
	if err := intent.ArmFullNewTurn(predecessor); err != nil {
		t.Fatal(err)
	}
	request := strongHTTPProtocolRequest(t, "thread", "new-turn", CodexRequestTurn, "")
	if err := intent.ConsumeReplacement(request, 12, 14, "0.147.0", 0, "websocket"); err != nil {
		t.Fatal(err)
	}
	wrongLane := testCodexLeaseKey("other-thread", "old-turn")
	other, _ := NewCodexWSRotationIntent(target, 7, 11, 13, "0.147.0", 1)
	if err := other.ArmFullNewTurn(wrongLane); !errors.Is(err, ErrCodexWSResyncRequired) {
		t.Fatalf("wrong-lane error = %v", err)
	}
}

func TestCodexResyncNeverReplaysIncrementalFrameOrSameSocket(t *testing.T) {
	key := testCodexLeaseKey("thread", "turn")
	base, _ := NewCodexWSRotationIntent(key, 7, 11, 13, "0.147.0", 1)
	_, _ = base.ObserveUpstreamError([]byte(`{"type":"error","error":{"code":"previous_response_not_found"}}`))
	tests := []struct {
		name       string
		request    CodexProtocolRequest
		downstream uint64
		upstream   uint64
		retries    int
	}{
		{name: "same downstream", request: strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, ""), downstream: 11, upstream: 14},
		{name: "same upstream", request: strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, ""), downstream: 12, upstream: 13},
		{name: "retry exhausted", request: strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, ""), downstream: 12, upstream: 14, retries: 2},
		{name: "previous response", request: protocolWithPrevious(t), downstream: 12, upstream: 14},
		{name: "encrypted state", request: protocolWithEncrypted(t), downstream: 12, upstream: 14},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			intent := base
			if err := intent.ConsumeReplacement(test.request, test.downstream, test.upstream, "0.147.0", test.retries, "websocket"); !errors.Is(err, ErrCodexWSResyncRequired) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCodexWSHandshakeRequiresModelAndFirstFrameMatch(t *testing.T) {
	header := make(http.Header)
	header.Set(codexTurnMetadataKey, `{"session_id":"session","thread_id":"old-thread","turn_id":"turn","request_kind":"turn"}`)
	if _, err := ParseCodexWSHandshake(header); !errors.Is(err, ErrCodexWSHandshakeUnsupported) {
		t.Fatalf("missing model error = %v", err)
	}
	header.Set(codexWSModelHeader, "gpt-5.4")
	handshake, err := ParseCodexWSHandshake(header)
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"current-thread","turn_id":"turn","request_kind":"turn"}}}`)
	resolved, err := ResolveCodexWSFirstFrame(frame, handshake)
	if err != nil || !resolved.HandshakeStale || resolved.Request.Metadata.Metadata.ThreadID != "current-thread" {
		t.Fatalf("resolved=%+v error=%v", resolved, err)
	}
	mismatch := []byte(`{"type":"response.create","model":"gpt-5.4-mini","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"current-thread","turn_id":"turn","request_kind":"turn"}}}`)
	if _, err := ResolveCodexWSFirstFrame(mismatch, handshake); !errors.Is(err, ErrCodexWSHandshakeUnsupported) {
		t.Fatalf("model mismatch error = %v", err)
	}
}

func TestCodexWebSocketPreupgradeProjectionUnitPassedUnwired(t *testing.T) {
	// This characterises the isolated copier only. Production proxyCodexUpgrade
	// sends downstream 101 before upstream dispatch and does not call this helper.
	statuses := []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusUpgradeRequired,
	}
	safe := map[string]string{
		"Content-Type":         "application/problem+json",
		"Retry-After":          "30",
		"X-Request-Id":         "safe-request",
		"Cf-Ray":               "safe-ray",
		"Openai-Processing-Ms": "12",
	}
	forbidden := map[string]string{
		"Authorization":            "Bearer secret",
		"Connection":               "upgrade",
		"Proxy-Authenticate":       "secret",
		"Sec-Websocket-Accept":     "secret",
		"Sec-Websocket-Extensions": "permessage-deflate",
		"Sec-Websocket-Protocol":   "secret-protocol",
		"Set-Cookie":               "session=secret",
		"Upgrade":                  "websocket",
		"Www-Authenticate":         "Bearer secret",
		"X-Api-Key":                "secret",
	}
	for _, status := range statuses {
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"error":{"status":%d}}`, status))
			response := &http.Response{StatusCode: status, Header: make(http.Header)}
			for name, value := range safe {
				response.Header.Set(name, value)
			}
			for name, value := range forbidden {
				response.Header.Set(name, value)
			}

			recorder := httptest.NewRecorder()
			relayCodexWebSocketPreupgrade(recorder, response, body)
			if recorder.Code != status || recorder.Body.String() != string(body) {
				t.Fatalf("projection = status %d body %q, want %d %q", recorder.Code, recorder.Body.String(), status, body)
			}
			for name, want := range safe {
				if got := recorder.Header().Get(name); got != want {
					t.Errorf("safe header %s = %q, want %q", name, got, want)
				}
			}
			for name := range forbidden {
				if got := recorder.Header().Values(name); len(got) != 0 {
					t.Errorf("forbidden header %s projected: %q", name, got)
				}
			}
		})
	}
}

func protocolWithPrevious(t *testing.T) CodexProtocolRequest {
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	request.PreviousResponseID = "response"
	return request
}

func protocolWithEncrypted(t *testing.T) CodexProtocolRequest {
	request := strongHTTPProtocolRequest(t, "thread", "turn", CodexRequestTurn, "")
	request.HasEncryptedState = true
	return request
}
