package proxy

import (
	"bytes"
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

func TestCodexProtocolRequestAcceptsBodyOverLegacyLimit(t *testing.T) {
	body := codexProtocolRequestBodyAtSize(t, maxRequestBody+1)
	got, err := ParseCodexProtocolRequest(body, "", nil)
	if err != nil {
		t.Fatalf("body over legacy limit rejected: %v", err)
	}
	if got.Model != "gpt-5" || !got.Metadata.Strong || got.Metadata.Metadata.TurnID != "u" {
		t.Fatalf("request = %#v", got)
	}
}

func codexProtocolRequestBodyAtSize(t *testing.T, size int) []byte {
	t.Helper()
	prefix := []byte(`{"type":"response.create","model":"gpt-5","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"`)
	suffix := []byte(`}}}`)
	if size < len(prefix)+len(suffix) {
		t.Fatalf("body size %d is below fixture minimum %d", size, len(prefix)+len(suffix))
	}
	body := make([]byte, 0, size)
	body = append(body, prefix...)
	body = append(body, bytes.Repeat([]byte{' '}, size-len(prefix)-len(suffix))...)
	body = append(body, suffix...)
	return body
}

func TestCodexProtocolWrappedErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		found   bool
		auth    bool
		hard429 bool
		wantErr bool
	}{
		{"401", `{"type":"error","status":401,"error":{"type":"authentication_error"}}`, true, true, false, false},
		{"403 alias", `{"type":"error","status_code":403,"error":{"type":"authentication_error"}}`, true, true, false, false},
		{"exact hard 429", `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`, true, false, true, false},
		{"exact hard 429 alias", `{"type":"error","status_code":429,"error":{"type":"usage_limit_reached"}}`, true, false, true, false},
		{"matching status aliases", `{"type":"error","status":429,"status_code":429,"error":{"type":"usage_limit_reached"}}`, true, false, true, false},
		{"conflicting status aliases", `{"type":"error","status":429,"status_code":400,"error":{"type":"usage_limit_reached"}}`, true, false, false, false},
		{"legacy insufficient quota", `{"type":"error","status":429,"error":{"type":"insufficient_quota"}}`, true, false, false, false},
		{"wrong nested type", `{"type":"error","status":429,"error":{"type":"rate_limit_exceeded"}}`, true, false, false, false},
		{"wrong status", `{"type":"error","status":400,"error":{"type":"usage_limit_reached"}}`, true, false, false, false},
		{"wrong envelope", `{"type":"response.failed","status":429,"error":{"type":"usage_limit_reached"}}`, false, false, false, false},
		{"missing status", `{"type":"error","error":{"type":"usage_limit_reached"}}`, true, false, false, false},
		{"null envelope", `null`, false, false, false, false},
		{"malformed envelope", `{`, false, false, false, true},
		{"oversized envelope", string(bytes.Repeat([]byte(" "), codexProtocolMaxBytes+1)), false, false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCodexWrappedError([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Found != tc.found || got.AuthFailure != tc.auth || got.HardUsageLimit != tc.hard429 {
				t.Fatalf("error = %#v", got)
			}
		})
	}
}

func TestCodexProtocolWrappedErrorRejectsDuplicateAuthorityFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"top-level type", `{"type":"response.failed","type":"error","status":429,"error":{"type":"usage_limit_reached"}}`},
		{"case-variant top-level type", `{"Type":"response.failed","type":"error","status":429,"error":{"type":"usage_limit_reached"}}`},
		{"top-level status", `{"type":"error","status":400,"status":429,"error":{"type":"usage_limit_reached"}}`},
		{"top-level status code", `{"type":"error","status_code":400,"status_code":429,"error":{"type":"usage_limit_reached"}}`},
		{"top-level error", `{"type":"error","status":429,"error":{"type":"rate_limit_exceeded"},"error":{"type":"usage_limit_reached"}}`},
		{"nested error type", `{"type":"error","status":429,"error":{"type":"rate_limit_exceeded","type":"usage_limit_reached"}}`},
		{"case-variant nested error type", `{"type":"error","status":429,"error":{"Type":"rate_limit_exceeded","type":"usage_limit_reached"}}`},
		{"nested error code", `{"type":"error","status":429,"error":{"type":"usage_limit_reached","code":"first","code":"second"}}`},
		{"Unicode-fold top-level status", `{"type":"error","status":400,"\u017ftatus":429,"error":{"type":"usage_limit_reached"}}`},
		{"lone Unicode-fold top-level status", `{"type":"error","\u017ftatus":429,"error":{"type":"usage_limit_reached"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseCodexWrappedError([]byte(tc.body)); err == nil {
				t.Fatalf("error = nil, parsed = %#v", got)
			}
		})
	}
}

func TestCodexProtocolCompactAndTurnState(t *testing.T) {
	t.Parallel()
	compact, err := ParseCodexCompactResponse([]byte(`{"id":"resp-compact","output":[{"type":"compaction","encrypted_content":"opaque"}]}`))
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

func TestCodexProtocolCompactValidatesPinnedResponseItems(t *testing.T) {
	t.Parallel()
	valid := []string{
		`{"output":[]}`,
		`{"output":[{"type":"compaction","encrypted_content":"opaque"}]}`,
		`{"output":[{"type":"future_response_item"}]}`,
		`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`,
		`{"output":[{"type":"additional_tools","role":"assistant","tools":[]}]}`,
		`{"output":[{"type":"additional_tools","role":"assistant","tools":[],"internal_chat_message_metadata_passthrough":42}]}`,
		`{"output":[{"type":"agent_message","author":"agent","recipient":"user","content":[{"type":"input_text","text":"done"}]}]}`,
		`{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary"}],"content":[{"type":"reasoning_text","text":"reasoning"}],"encrypted_content":null}]}`,
		`{"output":[{"type":"local_shell_call","status":"completed","action":{"type":"exec","command":["pwd"],"timeout_ms":1000}}]}`,
		`{"output":[{"type":"function_call","name":"tool","arguments":"{}","call_id":"call"}]}`,
		`{"output":[{"type":"tool_search_call","execution":"server","arguments":null}]}`,
		`{"output":[{"type":"function_call_output","call_id":"call","output":"done"}]}`,
		`{"output":[{"type":"custom_tool_call","call_id":"call","name":"tool","input":"ping"}]}`,
		`{"output":[{"type":"custom_tool_call_output","call_id":"call","output":[{"type":"input_text","text":"done"}]}]}`,
		`{"output":[{"type":"tool_search_output","status":"completed","execution":"server","tools":[]}]}`,
		`{"output":[{"type":"web_search_call","action":{"type":"search","queries":["query"]}}]}`,
		`{"output":[{"type":"image_generation_call","status":"completed","result":"image"}]}`,
		`{"output":[{"type":"compaction_summary","encrypted_content":"opaque"}]}`,
		`{"output":[{"type":"compaction_trigger"}]}`,
		`{"output":[{"type":"context_compaction","encrypted_content":null}]}`,
	}
	for _, body := range valid {
		body := body
		t.Run("valid "+body, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseCodexCompactResponse([]byte(body)); err != nil {
				t.Fatal(err)
			}
		})
	}

	invalid := []struct {
		name string
		body string
	}{
		{name: "null item", body: `{"output":[null]}`},
		{name: "array item", body: `{"output":[[]]}`},
		{name: "missing item type", body: `{"output":[{}]}`},
		{name: "non-string item type", body: `{"output":[{"type":42}]}`},
		{name: "null item type", body: `{"output":[{"type":null}]}`},
		{name: "duplicate item type", body: `{"output":[{"type":"future","type":"compaction","encrypted_content":"opaque"}]}`},
		{name: "compaction missing encrypted content", body: `{"output":[{"type":"compaction"}]}`},
		{name: "compaction wrong encrypted content", body: `{"output":[{"type":"compaction","encrypted_content":{}}]}`},
		{name: "compaction null encrypted content", body: `{"output":[{"type":"compaction","encrypted_content":null}]}`},
		{name: "message missing role", body: `{"output":[{"type":"message","content":[]}]}`},
		{name: "message null role", body: `{"output":[{"type":"message","role":null,"content":[]}]}`},
		{name: "message malformed content", body: `{"output":[{"type":"message","role":"assistant","content":[{}]}]}`},
		{name: "duplicate message role", body: `{"output":[{"type":"message","role":null,"role":"assistant","content":[]}]}`},
		{name: "duplicate nested item type", body: `{"output":[{"type":"message","role":"assistant","content":[{"type":"future","type":"output_text","text":"done"}]}]}`},
		{name: "duplicate nested required field", body: `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":null,"text":"done"}]}]}`},
		{name: "message invalid phase", body: `{"output":[{"type":"message","role":"assistant","content":[],"phase":42}]}`},
		{name: "reasoning missing summary", body: `{"output":[{"type":"reasoning"}]}`},
		{name: "reasoning malformed content", body: `{"output":[{"type":"reasoning","summary":[],"content":[{}]}]}`},
		{name: "reasoning invalid encrypted content", body: `{"output":[{"type":"reasoning","summary":[],"encrypted_content":{}}]}`},
		{name: "local shell invalid status", body: `{"output":[{"type":"local_shell_call","status":"done","action":{"type":"exec","command":[]}}]}`},
		{name: "local shell malformed action", body: `{"output":[{"type":"local_shell_call","status":"completed","action":{"type":"exec"}}]}`},
		{name: "function call missing call ID", body: `{"output":[{"type":"function_call","name":"tool","arguments":"{}"}]}`},
		{name: "function output malformed payload", body: `{"output":[{"type":"function_call_output","call_id":"call","output":{}}]}`},
		{name: "context compaction invalid encrypted content", body: `{"output":[{"type":"context_compaction","encrypted_content":{}}]}`},
		{name: "web search malformed queries", body: `{"output":[{"type":"web_search_call","action":{"type":"search","queries":[1]}}]}`},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseCodexCompactResponse([]byte(tc.body)); err == nil {
				t.Fatalf("error = nil, parsed = %#v", got)
			}
		})
	}
}

func TestCodexProtocolCompactRequiresUniqueOutputArray(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "error object", body: `{"error":{"type":"usage_limit_reached"}}`},
		{name: "error object with output", body: `{"error":{"type":"usage_limit_reached"},"output":[]}`},
		{name: "missing output", body: `{"id":"response"}`},
		{name: "null output", body: `{"id":"response","output":null}`},
		{name: "object output", body: `{"id":"response","output":{}}`},
		{name: "string output", body: `{"id":"response","output":"value"}`},
		{name: "duplicate output", body: `{"id":"response","output":{},"output":[]}`},
		{name: "case-variant duplicate output", body: `{"id":"response","Output":[],"output":[]}`},
		{name: "null response ID", body: `{"id":null,"output":[]}`},
		{name: "case-variant null response ID", body: `{"ID":null,"output":[]}`},
		{name: "duplicate response ID", body: `{"id":"first","id":"second","output":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseCodexCompactResponse([]byte(tc.body)); err == nil {
				t.Fatalf("error = nil, parsed = %#v", got)
			}
		})
	}
}
