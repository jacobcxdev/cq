package proxy

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestCodexWSPendingFrameUsesStrongFrameAuthorityWithoutHandshake(t *testing.T) {
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)

	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Release()

	want := testCodexLeaseKey("thread", "turn")
	if pending.key != want {
		t.Fatalf("key = %+v, want %+v", pending.key, want)
	}
	if pending.request.Model != "gpt-5.6-sol" || !pending.request.Metadata.Strong {
		t.Fatalf("request authority = %+v", pending.request)
	}
	if !pending.portable {
		t.Fatal("full first frame is not portable")
	}
	if string(pending.encoded) != string(payload) {
		t.Fatalf("owned frame changed: %q", pending.encoded)
	}
	var event RouteEvent
	event.applyRouteDiagnostics(pending.diagnostics)
	if event.SessionKey != hashPrefix("codex-session", "session") || event.SessionSource != "metadata:session_id" {
		t.Fatalf("frame task correlation = %q/%q", event.SessionKey, event.SessionSource)
	}
	if event.ThreadKey != hashPrefix("codex-thread", "thread") {
		t.Fatalf("frame thread correlation = %q", event.ThreadKey)
	}
}

func TestCodexWSPendingFrameExplicitEmptyPreviousResponseIDIsNotPortable(t *testing.T) {
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","previous_response_id":"","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)

	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Release()
	if pending.portable || !pending.request.HasPreviousResponseID || pending.request.PreviousResponseID != "" {
		t.Fatalf("explicit empty previous_response_id portability = %v request = %+v", pending.portable, pending.request)
	}
}

func TestCodexWSPendingFrameAcceptsSupportedCompactionPhases(t *testing.T) {
	for _, phase := range []string{"standalone_turn", "pre_turn", "mid_turn"} {
		t.Run(phase, func(t *testing.T) {
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"compaction","compaction":"` + phase + `"}}}`)

			pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
			if err != nil {
				t.Fatal(err)
			}
			defer pending.Release()

			if pending.key != testCodexLeaseKey("thread", "turn") || pending.prewarm {
				t.Fatalf("compaction routing = key %+v prewarm %v", pending.key, pending.prewarm)
			}
			if pending.request.Metadata.Metadata.RequestKind != CodexRequestCompaction || string(pending.request.Metadata.Metadata.CompactionPhase) != phase {
				t.Fatalf("compaction metadata = %+v", pending.request.Metadata.Metadata)
			}
		})
	}
}

func TestCodexWSPendingFrameRejectsInvalidAuthorityAndBounds(t *testing.T) {
	valid := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	tests := []struct {
		name        string
		messageType int
		payload     []byte
	}{
		{name: "binary", messageType: websocket.BinaryMessage, payload: valid},
		{name: "oversize", messageType: websocket.TextMessage, payload: bytes.Repeat([]byte{'x'}, codexWebSocketMessageMaxBytes+1)},
		{name: "wrong type", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.cancel","model":"gpt-5.6-sol"}`)},
		{name: "missing metadata", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)},
		{name: "missing model", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)},
		{name: "unsupported compaction phase", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"compaction","compaction":"unsupported"}}}`)},
		{name: "compaction missing lease key", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","request_kind":"compaction","compaction":"mid_turn"}}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if pending, err := newCodexWSPendingFrame(test.messageType, test.payload); pending != nil || !errors.Is(err, ErrCodexWSInvalidFrame) {
				t.Fatalf("pending=%+v error=%v", pending, err)
			}
		})
	}
}

func TestCodexWSPendingFrameClassifiesFailureWithoutPayload(t *testing.T) {
	tests := []struct {
		name        string
		messageType int
		payload     []byte
		wantOrigin  codexWSInvalidFrameOrigin
		wantType    codexWSFrameType
		wantSize    codexWSFrameSize
		wantDetail  codexWSInvalidFrameDetail
	}{
		{
			name:        "binary envelope",
			messageType: websocket.BinaryMessage,
			payload:     []byte("private-binary-frame"),
			wantOrigin:  codexWSInvalidFrameEnvelope,
			wantType:    codexWSFrameBinary,
			wantSize:    codexWSFrameSizeSmall,
		},
		{
			name:        "protocol decode",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":`),
			wantOrigin:  codexWSInvalidFrameProtocol,
			wantType:    codexWSFrameText,
			wantSize:    codexWSFrameSizeSmall,
		},
		{
			name:        "broker authority",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"response.cancel","private":"secret"}`),
			wantOrigin:  codexWSInvalidFrameBrokerAuthority,
			wantType:    codexWSFrameText,
			wantSize:    codexWSFrameSizeSmall,
			wantDetail:  codexWSInvalidFrameResponseType,
		},
		{
			name:        "memory request kind",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"request_kind":"memory"}}}`),
			wantOrigin:  codexWSInvalidFrameBrokerAuthority,
			wantType:    codexWSFrameText,
			wantSize:    codexWSFrameSizeSmall,
			wantDetail:  codexWSInvalidFrameRequestKindMemory,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newCodexWSPendingFrame(test.messageType, test.payload)
			var frameErr *codexWSInvalidFrameError
			if !errors.As(err, &frameErr) {
				t.Fatalf("error = %T, want safe frame error", err)
			}
			if frameErr.Origin != test.wantOrigin || frameErr.Type != test.wantType || frameErr.Size != test.wantSize || frameErr.Detail != test.wantDetail {
				t.Fatalf("failure = %s/%s/%s/%s, want origin/type/size/detail %s/%s/%s/%s", frameErr.Origin, frameErr.Type, frameErr.Size, frameErr.Detail, test.wantOrigin, test.wantType, test.wantSize, test.wantDetail)
			}
			if strings.Contains(frameErr.Error(), "private") || strings.Contains(frameErr.Error(), "secret") {
				t.Fatalf("failure exposed payload: %q", frameErr.Error())
			}
			failure := classifyCodexWebSocketFailure(err)
			if failure.Origin != test.wantOrigin || failure.FrameType != test.wantType || failure.FrameSize != test.wantSize || failure.FrameDetail != test.wantDetail {
				t.Fatalf("broker failure = %+v, want origin/type/size/detail %s/%s/%s/%s", failure, test.wantOrigin, test.wantType, test.wantSize, test.wantDetail)
			}
		})
	}
}

func TestCodexWSInvalidFrameClassifiesAllowlistedEventMetadata(t *testing.T) {
	tests := []struct {
		name        string
		messageType int
		payload     []byte
		wantEvent   codexWSEventType
		wantStatus  string
		wantType    codexWSEventType
		wantCode    codexWSEventType
		private     []string
	}{
		{
			name:        "known application error",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"bad_anchor","message":"private upstream detail"}}`),
			wantEvent:   "error",
			wantStatus:  "400",
			wantType:    "invalid_request_error",
			private:     []string{"bad_anchor", "private"},
		},
		{
			name:        "known usage limit error",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`),
			wantEvent:   "error",
			wantStatus:  "429",
			wantType:    "usage_limit_reached",
		},
		{
			name:        "valid identifier private metadata",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"error","status":400,"error":{"type":"client_secret_ABC123","code":"sk_live_ABC123","message":"private upstream detail"}}`),
			wantEvent:   "error",
			wantStatus:  "400",
			private:     []string{"client_secret_ABC123", "sk_live_ABC123", "private"},
		},
		{
			name:        "invalid identifier private metadata",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"error","status":400,"error":{"type":"private type","code":"private/code","message":"private upstream detail"}}`),
			wantEvent:   "error",
			wantStatus:  "400",
			private:     []string{"private"},
		},
		{
			name:        "known response event",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"response.completed","response":{}}`),
			wantEvent:   "response.completed",
		},
		{
			name:        "valid identifier private event",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"sk_live_ABC123"}`),
			wantEvent:   codexWSEventTypeUnknown,
			private:     []string{"sk_live_ABC123"},
		},
		{
			name:        "unknown response delta",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"response.client_secret.delta"}`),
			wantEvent:   codexWSEventTypeUnknown,
			private:     []string{"client_secret"},
		},
		{
			name:        "malformed text",
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":`),
			wantEvent:   codexWSEventTypeUnknown,
		},
		{
			name:        "binary",
			messageType: websocket.BinaryMessage,
			payload:     []byte("private-binary-frame"),
			wantEvent:   codexWSEventTypeUnknown,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newCodexWSInvalidFrameErrorWithDetail(codexWSInvalidFrameUpstreamResponse, test.messageType, test.payload, codexWSInvalidFrameErrorOrder, errors.New("private cause"))
			var frameErr *codexWSInvalidFrameError
			if !errors.As(err, &frameErr) {
				t.Fatalf("error = %T, want frame error", err)
			}
			if frameErr.Event != test.wantEvent || frameErr.Status != test.wantStatus || frameErr.Kind != test.wantType || frameErr.Code != test.wantCode {
				t.Fatalf("metadata = event %q status %q type %q code %q, want %q/%q/%q/%q", frameErr.Event, frameErr.Status, frameErr.Kind, frameErr.Code, test.wantEvent, test.wantStatus, test.wantType, test.wantCode)
			}
			failure := classifyCodexWebSocketFailure(err)
			if failure.EventType != test.wantEvent || failure.ErrorStatus != test.wantStatus || failure.ErrorType != test.wantType || failure.ErrorCode != test.wantCode {
				t.Fatalf("failure metadata = %+v", failure)
			}
			for _, rendered := range []string{frameErr.Error(), failure.ErrorStatus, string(failure.EventType), string(failure.ErrorType), string(failure.ErrorCode)} {
				for _, private := range test.private {
					if strings.Contains(rendered, private) {
						t.Fatalf("diagnostic exposed private payload %q: %q", private, rendered)
					}
				}
			}
		})
	}
}

func TestCodexWSPendingFrameAcceptsInstalledLimit(t *testing.T) {
	payload := codexProtocolRequestBodyAtSize(t, codexWebSocketMessageMaxBytes)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatalf("installed limit rejected: %v", err)
	}
	pending.Release()
}

func TestCodexWSPendingFrameAcceptsInstalledPrewarmWithoutLeaseKey(t *testing.T) {
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Release()
	if !pending.prewarm || pending.key != (LeaseKey{}) || pending.request.Metadata.Metadata.RequestKind != CodexRequestPrewarm {
		t.Fatalf("prewarm authority = %+v", pending)
	}
}

func TestCodexWSFrameWithoutPrewarmAnchorPreservesOtherFields(t *testing.T) {
	payload := []byte(`{"type":"response.create","x":1,"x":2,"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"input":[{"previous_response_id":"nested"}],"previous_response_id":"prewarm-a"}`)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Release()

	rewritten, err := codexWSFrameWithoutPrewarmAnchor(pending, "prewarm-a")
	if err != nil {
		t.Fatal(err)
	}
	defer rewritten.Release()
	want := `{"type":"response.create","x":1,"x":2,"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"input":[{"previous_response_id":"nested"}]}`
	if string(rewritten.encoded) != want || rewritten.request.PreviousResponseID != "" || !rewritten.portable {
		t.Fatalf("rewritten frame = %s, request = %+v, portable = %v", rewritten.encoded, rewritten.request, rewritten.portable)
	}
	if string(pending.encoded) != string(payload) {
		t.Fatal("original frame changed")
	}
}

func TestCodexWSFrameWithoutPrewarmAnchorRemovesParamsAuthority(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		want    string
	}{
		{
			name:    "params only",
			encoded: `{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"params":{"x":1,"previous_response_id":"prewarm-a","x":2},"input":[]}`,
			want:    `{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"params":{"x":1,"x":2},"input":[]}`,
		},
		{
			name:    "matching root and params",
			encoded: `{"type":"response.create","model":"gpt-5.6-sol","previous_response_id":"prewarm-a","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"params":{"previous_response_id":"prewarm-a","nested":{"previous_response_id":"keep"}},"input":[]}`,
			want:    `{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"params":{"nested":{"previous_response_id":"keep"}},"input":[]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pending, err := newCodexWSPendingFrame(websocket.TextMessage, []byte(test.encoded))
			if err != nil {
				t.Fatal(err)
			}
			defer pending.Release()
			rewritten, err := codexWSFrameWithoutPrewarmAnchor(pending, "prewarm-a")
			if err != nil {
				t.Fatal(err)
			}
			defer rewritten.Release()
			if string(rewritten.encoded) != test.want || rewritten.request.PreviousResponseID != "" || rewritten.request.HasPreviousResponseID {
				t.Fatalf("rewritten frame = %s, request = %+v", rewritten.encoded, rewritten.request)
			}
		})
	}
}

func TestCodexWSFrameWithoutPrewarmAnchorRejectsMismatch(t *testing.T) {
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}},"input":[],"previous_response_id":"prewarm-a"}`)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Release()
	if rewritten, err := codexWSFrameWithoutPrewarmAnchor(pending, "prewarm-b"); rewritten != nil || !errors.Is(err, ErrCodexWSInvalidFrame) {
		t.Fatalf("rewritten=%+v error=%v", rewritten, err)
	}
}

func TestCodexWSPendingFrameRejectsInvalidPrewarmGenerateAuthority(t *testing.T) {
	metadata := `"client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"","request_kind":"prewarm"}}`
	for _, test := range []struct {
		name     string
		generate string
	}{
		{name: "missing"},
		{name: "true", generate: `,"generate":true`},
		{name: "null", generate: `,"generate":null`},
		{name: "string", generate: `,"generate":"false"`},
		{name: "duplicate", generate: `,"generate":false,"generate":false`},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol"` + test.generate + `,` + metadata + `}`)
			pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
			if pending != nil || !errors.Is(err, ErrCodexWSInvalidFrame) {
				t.Fatalf("pending=%+v error=%v", pending, err)
			}
		})
	}
}

func TestCodexWSPendingFrameOwnsAndReleasesPortableBytes(t *testing.T) {
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = '['
	if pending.encoded[0] != '{' {
		t.Fatal("pending frame aliases caller bytes")
	}
	pending.Release()
	pending.Release()
	if pending.encoded != nil || pending.key != (LeaseKey{}) || pending.request != (CodexProtocolRequest{}) || pending.portable || !pending.released {
		t.Fatalf("released frame retained authority: %+v", pending)
	}
}

func TestCodexWSPendingFrameMarksPreviousResponseNonPortable(t *testing.T) {
	payload := []byte(`{"type":"response.create","previous_response_id":"response","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	if pending.portable {
		t.Fatal("previous response input marked portable")
	}
	pending.Release()
}

func TestCodexWSPendingFrameMarksEncryptedInputPortable(t *testing.T) {
	payload := []byte(`{"type":"response.create","input":[{"type":"reasoning","encrypted_content":"opaque"}],"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !pending.portable {
		t.Fatal("encrypted replay input marked non-portable")
	}
	pending.Release()
}
