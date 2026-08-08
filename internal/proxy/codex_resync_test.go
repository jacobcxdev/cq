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

func TestCodexWebSocketPreupgradeRelaysSafeFinalResponse(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	response.Header.Set("Retry-After", "30")
	response.Header.Set("X-Request-Id", "safe-request")
	response.Header.Set("Authorization", "secret")
	recorder := httptest.NewRecorder()
	relayCodexWebSocketPreupgrade(recorder, response, []byte(`{"error":{"type":"usage_limit_reached"}}`))
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "30" || recorder.Header().Get("Authorization") != "" {
		t.Fatalf("preupgrade response = code %d headers %v", recorder.Code, recorder.Header())
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
