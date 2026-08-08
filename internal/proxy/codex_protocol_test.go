package proxy

import (
	"net/http"
	"testing"
)

func TestCodexProtocolRequest(t *testing.T) {
	t.Parallel()
	body := []byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp-old","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}},"input":[{"encrypted_content":"opaque"}]}`)
	got, err := ParseCodexProtocolRequest(body, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "response.create" || got.Model != "gpt-5" || got.PreviousResponseID != "resp-old" || !got.HasEncryptedState || got.Metadata.Metadata.TurnID != "u" {
		t.Fatalf("request = %#v", got)
	}
}

func TestCodexProtocolWrappedErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		found   bool
		auth    bool
		hard429 bool
	}{
		{"401", `{"type":"error","status":401,"error":{"type":"authentication_error"}}`, true, true, false},
		{"403 alias", `{"type":"error","status_code":403,"error":{"type":"authentication_error"}}`, true, true, false},
		{"exact hard 429", `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`, true, false, true},
		{"wrong nested type", `{"type":"error","status":429,"error":{"type":"rate_limit_exceeded"}}`, true, false, false},
		{"wrong status", `{"type":"error","status":400,"error":{"type":"usage_limit_reached"}}`, true, false, false},
		{"wrong envelope", `{"type":"response.failed","status":429,"error":{"type":"usage_limit_reached"}}`, false, false, false},
		{"missing status", `{"type":"error","error":{"type":"usage_limit_reached"}}`, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCodexWrappedError([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got.Found != tc.found || got.AuthFailure != tc.auth || got.HardUsageLimit != tc.hard429 {
				t.Fatalf("error = %#v", got)
			}
		})
	}
}

func TestCodexProtocolCompactAndTurnState(t *testing.T) {
	t.Parallel()
	compact, err := ParseCodexCompactResponse([]byte(`{"id":"resp-compact","output":[{"encrypted_content":"opaque"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if compact.ResponseID != "resp-compact" || !compact.HasEncryptedState {
		t.Fatalf("compact = %#v", compact)
	}

	header := make(http.Header)
	header.Add("X-Codex-Turn-State", "state-a")
	state, found, err := ParseCodexTurnStateHeader(header)
	if err != nil || !found || state != "state-a" {
		t.Fatalf("state = %q, found = %v, err = %v", state, found, err)
	}
	header.Add("X-Codex-Turn-State", "state-b")
	if _, _, err := ParseCodexTurnStateHeader(header); err == nil {
		t.Fatal("expected conflicting header error")
	}
}
