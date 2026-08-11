package proxy

import (
	"bytes"
	"errors"
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
}

func TestCodexWSPendingFrameRejectsInvalidAuthorityAndBounds(t *testing.T) {
	valid := []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	tests := []struct {
		name        string
		messageType int
		payload     []byte
	}{
		{name: "binary", messageType: websocket.BinaryMessage, payload: valid},
		{name: "oversize", messageType: websocket.TextMessage, payload: bytes.Repeat([]byte{'x'}, maxRequestBody+1)},
		{name: "wrong type", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.cancel","model":"gpt-5.6-sol"}`)},
		{name: "missing metadata", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)},
		{name: "missing model", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if pending, err := newCodexWSPendingFrame(test.messageType, test.payload); pending != nil || !errors.Is(err, ErrCodexWSInvalidFrame) {
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

func TestCodexWSPendingFrameMarksGenerationBoundInputNonPortable(t *testing.T) {
	for _, field := range []string{
		`"previous_response_id":"response",`,
		`"input":[{"type":"reasoning","encrypted_content":"opaque"}],`,
	} {
		payload := []byte(`{"type":"response.create",` + field + `"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
		pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
		if err != nil {
			t.Fatal(err)
		}
		if pending.portable {
			t.Fatalf("generation-bound field %s marked portable", field)
		}
		pending.Release()
	}
}
