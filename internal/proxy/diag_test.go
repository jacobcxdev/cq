package proxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticsWriterCreatesAndAppendsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")

	w, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	if err := w.Write(RouteEvent{
		Time:       time.Unix(1, 0).UTC(),
		Method:     "POST",
		Path:       "/v1/messages",
		Provider:   "claude",
		RouteKind:  "anthropic_messages",
		Model:      "claude-sonnet",
		StatusCode: 200,
		LatencyMS:  12,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w, err = OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("reopen diagnostics writer: %v", err)
	}
	if err := w.Write(RouteEvent{
		Time:       time.Unix(2, 0).UTC(),
		Method:     "POST",
		Path:       "/responses",
		Provider:   "codex",
		RouteKind:  "codex_native",
		Model:      "gpt-5.4",
		StatusCode: 201,
		LatencyMS:  34,
	}); err != nil {
		t.Fatalf("append Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("append Close: %v", err)
	}

	events := readDiagnosticsEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Path != "/v1/messages" || events[1].Path != "/responses" {
		t.Fatalf("events paths = %q, %q", events[0].Path, events[1].Path)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat diagnostics log: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %#o, want 0600", got)
		}
	}
}

func TestDiagnosticsWriterNilSafe(t *testing.T) {
	var w *DiagnosticsWriter
	if err := w.Write(RouteEvent{Path: "/v1/messages"}); err != nil {
		t.Fatalf("nil Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestDiagnosticsWriterConcurrentWritesProduceValidJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	w, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}

	const count = 64
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := w.Write(RouteEvent{
				Time:       time.Unix(int64(i), 0).UTC(),
				Method:     "POST",
				Path:       "/v1/messages",
				Provider:   "claude",
				StatusCode: 200,
			}); err != nil {
				t.Errorf("Write(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readDiagnosticsEvents(t, path)
	if len(events) != count {
		t.Fatalf("events = %d, want %d", len(events), count)
	}
	for i, ev := range events {
		if ev.Method != "POST" || ev.Path != "/v1/messages" || ev.Provider != "claude" {
			t.Fatalf("event %d = %+v", i, ev)
		}
	}
}

func TestDiagnosticsWriterCloseSafeAndStopsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	w, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	if err := w.Write(RouteEvent{Time: time.Unix(1, 0).UTC(), Method: "GET", Path: "/health", Provider: "proxy"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := w.Write(RouteEvent{Time: time.Unix(2, 0).UTC(), Method: "GET", Path: "/health", Provider: "proxy"}); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}

	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events after closed write = %d, want 1", len(events))
	}
}

// ── sessionCorrelation tests ─────────────────────────────────────────────────

func TestSessionCorrelationClaudeSessionId(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "session-abc-123")
	key, source := sessionCorrelation(h)
	if source != "x-claude-code-session-id" {
		t.Fatalf("source = %q, want x-claude-code-session-id", source)
	}
	if key == "" || key[:len("claude-session:")] != "claude-session:" {
		t.Fatalf("key = %q, want claude-session:<hash>", key)
	}
	// Deterministic: same input → same key.
	key2, _ := sessionCorrelation(h)
	if key != key2 {
		t.Fatalf("non-deterministic: key1=%q key2=%q", key, key2)
	}
	// Does not contain raw header value.
	if key == "claude-session:session-abc-123" {
		t.Fatal("key leaks raw header value")
	}
}

func TestSessionCorrelationCodexSessionId(t *testing.T) {
	// http.CanonicalHeaderKey("session_id") = "Session_id"
	// Use the canonical form so h.Get() finds it.
	h := http.Header{}
	h["Session_id"] = []string{"codex-sess-xyz"}
	key, source := sessionCorrelation(h)
	if source != "session_id" {
		t.Fatalf("source = %q, want session_id", source)
	}
	if key == "" || key[:len("codex-session:")] != "codex-session:" {
		t.Fatalf("key = %q, want codex-session:<hash>", key)
	}
}

func TestSessionCorrelationCodexWindowId(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Window-Id", "window-001")
	key, source := sessionCorrelation(h)
	if source != "x-codex-window-id" {
		t.Fatalf("source = %q, want x-codex-window-id", source)
	}
	if key == "" || key[:len("codex-window:")] != "codex-window:" {
		t.Fatalf("key = %q, want codex-window:<hash>", key)
	}
}

func TestSessionCorrelationUnknownClient(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "claude-code/1.2.3")
	key, source := sessionCorrelation(h)
	if source != "unknown-client" {
		t.Fatalf("source = %q, want unknown-client", source)
	}
	if key == "" || key[:len("unknown-client:")] != "unknown-client:" {
		t.Fatalf("key = %q, want unknown-client:<hash>", key)
	}
	// Raw User-Agent is not exposed.
	if key == "unknown-client:claude-code/1.2.3" {
		t.Fatal("key leaks raw User-Agent")
	}
}

func TestSessionCorrelationNone(t *testing.T) {
	h := http.Header{}
	key, source := sessionCorrelation(h)
	if source != "none" {
		t.Fatalf("source = %q, want none", source)
	}
	if key != "" {
		t.Fatalf("key = %q, want empty", key)
	}
}

func TestSessionCorrelationPriority(t *testing.T) {
	// Claude session ID takes priority over Codex headers.
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "claude-sess")
	h.Set("X-Codex-Window-Id", "codex-win")
	h["session_id"] = []string{"codex-sess"}
	_, source := sessionCorrelation(h)
	if source != "x-claude-code-session-id" {
		t.Fatalf("source = %q, want x-claude-code-session-id (highest priority)", source)
	}
}

func TestSessionCorrelationDistinctSessions(t *testing.T) {
	h1 := http.Header{}
	h1.Set("X-Claude-Code-Session-Id", "session-alpha")
	h2 := http.Header{}
	h2.Set("X-Claude-Code-Session-Id", "session-beta")
	key1, _ := sessionCorrelation(h1)
	key2, _ := sessionCorrelation(h2)
	if key1 == key2 {
		t.Fatalf("distinct sessions produced same key: %q", key1)
	}
}

func TestSessionCorrelationNoCredentialHeaders(t *testing.T) {
	// Authorization, cookies, x-api-key, etc. must never influence the key.
	// We verify that a request with only credential headers produces source="none".
	h := http.Header{}
	h.Set("Authorization", "Bearer secret-token")
	h.Set("Cookie", "session=abc")
	h.Set("X-Api-Key", "api-key-secret")
	key, source := sessionCorrelation(h)
	if source != "none" {
		t.Fatalf("source = %q, want none (credential headers must be ignored)", source)
	}
	if key != "" {
		t.Fatalf("key = %q, want empty", key)
	}
}

func TestPayloadSessionCorrelationConversationIDOverridesUnknownClient(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "claude-code/1.0.0")
	raw := []byte(`{"model":"claude-sonnet","conversation_id":"conv-alpha","messages":[]}`)
	key, source := payloadSessionCorrelation(h, raw)
	if source != "body:conversation_id" {
		t.Fatalf("source = %q, want body:conversation_id", source)
	}
	if key == "" || !strings.HasPrefix(key, "body-session:") {
		t.Fatalf("key = %q, want body-session:<hash>", key)
	}
	if strings.Contains(key, "conv-alpha") {
		t.Fatalf("key leaks raw conversation ID: %q", key)
	}
}

func TestPayloadSessionCorrelationNestedThreadID(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "codex/1.0.0")
	raw := []byte(`{"model":"gpt-5.5","metadata":{"thread_id":"thread-123"},"input":[]}`)
	key, source := payloadSessionCorrelation(h, raw)
	if source != "body:thread_id" {
		t.Fatalf("source = %q, want body:thread_id", source)
	}
	if key == "" || !strings.HasPrefix(key, "body-session:") {
		t.Fatalf("key = %q, want body-session:<hash>", key)
	}
}

func TestPayloadSessionCorrelationPreviousResponseID(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "codex/1.0.0")
	raw := []byte(`{"model":"gpt-5.5","previous_response_id":"resp_123","input":[{"role":"user","content":"continue"}]}`)
	key, source := payloadSessionCorrelation(h, raw)
	if source != "body:previous_response_id" {
		t.Fatalf("source = %q, want body:previous_response_id", source)
	}
	if key == "" || !strings.HasPrefix(key, "body-session:") {
		t.Fatalf("key = %q, want body-session:<hash>", key)
	}
}

func TestPayloadSessionCorrelationHeaderSessionBeatsBody(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "header-session")
	raw := []byte(`{"conversation_id":"body-session"}`)
	key, source := payloadSessionCorrelation(h, raw)
	if source != "x-claude-code-session-id" {
		t.Fatalf("source = %q, want x-claude-code-session-id", source)
	}
	if key == "" || !strings.HasPrefix(key, "claude-session:") {
		t.Fatalf("key = %q, want claude-session:<hash>", key)
	}
}

func TestPayloadSessionCorrelationCodexHeaderBeatsBodySessionID(t *testing.T) {
	h := http.Header{}
	h["Session_id"] = []string{"header-session"}
	raw := []byte(`{"session_id":"body-session"}`)
	key, source := payloadSessionCorrelation(h, raw)
	if source != "session_id" {
		t.Fatalf("source = %q, want session_id", source)
	}
	if key == "" || !strings.HasPrefix(key, "codex-session:") {
		t.Fatalf("key = %q, want codex-session:<hash>", key)
	}
}

func TestPayloadSessionCorrelationUnknownClientFallback(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "claude-code/1.0.0")
	raw := []byte(`{"model":"claude-sonnet","messages":[]}`)
	key, source := payloadSessionCorrelation(h, raw)
	if source != "unknown-client" {
		t.Fatalf("source = %q, want unknown-client", source)
	}
	if key == "" || !strings.HasPrefix(key, "unknown-client:") {
		t.Fatalf("key = %q, want unknown-client:<hash>", key)
	}
}

func TestPayloadSessionCorrelationInvalidJSONFallback(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "claude-code/1.0.0")
	raw := []byte(`not json conversation_id=abc`)
	key, source := payloadSessionCorrelation(h, raw)
	if source != "unknown-client" {
		t.Fatalf("source = %q, want unknown-client", source)
	}
	if key == "" || !strings.HasPrefix(key, "unknown-client:") {
		t.Fatalf("key = %q, want unknown-client:<hash>", key)
	}
}

func TestCodexWebSocketFrameSignalNewSession(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"model":"gpt-5.5","thread_id":"thread-new"}}`)
	key, source, signal := codexWebSocketFrameCorrelation(nil, frame)
	if signal != "new_session" {
		t.Fatalf("signal = %q, want new_session", signal)
	}
	if source != "ws:thread_id" {
		t.Fatalf("source = %q, want ws:thread_id", source)
	}
	if key == "" || !strings.HasPrefix(key, "ws-session:") {
		t.Fatalf("key = %q, want ws-session:<hash>", key)
	}
}

func TestCodexWebSocketFrameSignalContinuation(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":2,"method":"response/create","params":{"previous_response_id":"resp_prev"}}`)
	_, source, signal := codexWebSocketFrameCorrelation(nil, frame)
	if signal != "continuation" {
		t.Fatalf("signal = %q, want continuation", signal)
	}
	if source != "ws:previous_response_id" {
		t.Fatalf("source = %q, want ws:previous_response_id", source)
	}
}

func TestCodexWebSocketFrameSignalClearCompactLongUnknown(t *testing.T) {
	tests := []struct {
		name   string
		frame  string
		signal string
	}{
		{"clear", `{"method":"thread/clear","params":{"thread_id":"t"}}`, "clear_transition"},
		{"compact", `{"method":"thread/compact","params":{"thread_id":"t"}}`, "compact_transition"},
		{"long", `{"method":"response/create","params":{"thread_id":"t","messages":[1,2,3,4,5,6,7,8,9,10,11]}}`, "long_session"},
		{"continuation beats long", `{"method":"response/create","params":{"previous_response_id":"resp_prev","messages":[1,2,3,4,5,6,7,8,9,10,11]}}`, "continuation"},
		{"unknown", `{"method":"ping","params":{}}`, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, signal := codexWebSocketFrameCorrelation(nil, []byte(tc.frame))
			if signal != tc.signal {
				t.Fatalf("signal = %q, want %q", signal, tc.signal)
			}
		})
	}
}

// ── encodeBody tests ─────────────────────────────────────────────────────────

func TestEncodeBodyValidJSON(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet","messages":[]}`)
	result := encodeBody(raw)
	if !json.Valid(result) {
		t.Fatalf("result is not valid JSON: %s", result)
	}
	// Should be embedded as-is (not double-encoded as a string).
	if result[0] != '{' {
		t.Fatalf("expected object literal, got: %s", result)
	}
}

func TestEncodeBodyInvalidFallback(t *testing.T) {
	raw := []byte("not json at all \x00\x01")
	result := encodeBody(raw)
	if !json.Valid(result) {
		t.Fatalf("result is not valid JSON: %s", result)
	}
	// Should be a JSON string.
	var s string
	if err := json.Unmarshal(result, &s); err != nil {
		t.Fatalf("expected JSON string, got %s: %v", result, err)
	}
}

func TestEncodeBodyEmpty(t *testing.T) {
	// Empty bytes are valid JSON (they're not, but we accept nil/empty gracefully).
	result := encodeBody([]byte{})
	// Should produce a JSON string (empty bytes are not valid JSON).
	if !json.Valid(result) {
		t.Fatalf("result is not valid JSON: %s", result)
	}
}

// ── PayloadWriter tests ──────────────────────────────────────────────────────

func TestPayloadWriterCreatesAndAppendsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")

	w, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	if err := w.Write(PayloadEvent{
		Time:      time.Unix(1, 0).UTC(),
		Method:    "POST",
		Path:      "/v1/messages",
		Provider:  "claude",
		RouteKind: "anthropic_messages",
		Model:     "claude-sonnet",
		BodyBytes: 42,
		Body:      encodeBody([]byte(`{"model":"claude-sonnet"}`)),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and append.
	w, err = OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("reopen PayloadWriter: %v", err)
	}
	if err := w.Write(PayloadEvent{
		Time:      time.Unix(2, 0).UTC(),
		Method:    "POST",
		Path:      "/responses",
		Provider:  "codex",
		RouteKind: "codex_native",
		Model:     "gpt-5.4",
		BodyBytes: 22,
		Body:      encodeBody([]byte(`{"model":"gpt-5.4"}`)),
	}); err != nil {
		t.Fatalf("append Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("append Close: %v", err)
	}

	events := readPayloadEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Path != "/v1/messages" || events[1].Path != "/responses" {
		t.Fatalf("events paths = %q, %q", events[0].Path, events[1].Path)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat payload log: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %#o, want 0600", got)
		}
	}
}

func TestPayloadWriterNilSafe(t *testing.T) {
	var w *PayloadWriter
	if err := w.Write(PayloadEvent{Path: "/v1/messages"}); err != nil {
		t.Fatalf("nil Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestPayloadWriterZeroValueSafe(t *testing.T) {
	var w PayloadWriter
	if err := w.Write(PayloadEvent{Path: "/v1/messages"}); err != nil {
		t.Fatalf("zero Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zero Close: %v", err)
	}
}

func TestPayloadWriterConcurrentWritesProduceValidJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	w, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}

	const count = 64
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := w.Write(PayloadEvent{
				Time:      time.Unix(int64(i), 0).UTC(),
				Method:    "POST",
				Path:      "/v1/messages",
				Provider:  "claude",
				BodyBytes: i,
				Body:      encodeBody([]byte(`{"model":"claude-sonnet"}`)),
			}); err != nil {
				t.Errorf("Write(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readPayloadEvents(t, path)
	if len(events) != count {
		t.Fatalf("events = %d, want %d", len(events), count)
	}
}

func TestPayloadWriterCloseSafeAndStopsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	w, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}
	if err := w.Write(PayloadEvent{Time: time.Unix(1, 0).UTC(), Method: "POST", Path: "/v1/messages", Provider: "claude"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := w.Write(PayloadEvent{Time: time.Unix(2, 0).UTC(), Method: "POST", Path: "/v1/messages", Provider: "claude"}); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}

	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events after closed write = %d, want 1", len(events))
	}
}

func TestPayloadEventIncludesSessionCorrelation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	w, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatalf("OpenPayloadWriter: %v", err)
	}

	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "test-session-id")
	sessionKey, sessionSource := sessionCorrelation(h)

	if err := w.Write(PayloadEvent{
		Time:          time.Unix(1, 0).UTC(),
		Method:        "POST",
		Path:          "/v1/messages",
		Provider:      "claude",
		RouteKind:     "anthropic_messages",
		Model:         "claude-sonnet",
		SessionKey:    sessionKey,
		SessionSource: sessionSource,
		BodyBytes:     10,
		Body:          encodeBody([]byte(`{"model":"claude-sonnet"}`)),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.SessionSource != "x-claude-code-session-id" {
		t.Fatalf("SessionSource = %q, want x-claude-code-session-id", ev.SessionSource)
	}
	if ev.SessionKey == "" || ev.SessionKey[:len("claude-session:")] != "claude-session:" {
		t.Fatalf("SessionKey = %q, want claude-session:<hash>", ev.SessionKey)
	}
	// Verify raw session ID is not in the log.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "test-session-id") {
		t.Fatalf("payload log leaked raw session ID: %s", raw)
	}
}

func readPayloadEvents(t *testing.T, path string) []PayloadEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open payload log: %v", err)
	}
	defer f.Close()

	var events []PayloadEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event PayloadEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid payload JSON line %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan payload log: %v", err)
	}
	return events
}

func readDiagnosticsEvents(t *testing.T, path string) []RouteEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open diagnostics log: %v", err)
	}
	defer f.Close()

	var events []RouteEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event RouteEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid diagnostics JSON line %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan diagnostics log: %v", err)
	}
	return events
}
