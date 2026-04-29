package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type RouteEvent struct {
	Time          time.Time `json:"time"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Provider      string    `json:"provider"`
	RouteKind     string    `json:"route_kind,omitempty"`
	Model         string    `json:"model,omitempty"`
	AccountHint   string    `json:"account_hint,omitempty"`
	PinActive     bool      `json:"pin_active,omitempty"`
	Failover      bool      `json:"failover,omitempty"`
	StatusCode    int       `json:"status_code,omitempty"`
	LatencyMS     int64     `json:"latency_ms,omitempty"`
	Error         string    `json:"error,omitempty"`
	SessionKey    string    `json:"session_key,omitempty"`
	SessionSource string    `json:"session_source,omitempty"`
}

type routeDiagnosticsContextKey struct{}

type routeDiagnostics struct {
	mu          sync.Mutex
	accountHint string
	failover    bool
}

func withRouteDiagnostics(ctx context.Context) (context.Context, *routeDiagnostics) {
	diag := &routeDiagnostics{}
	return context.WithValue(ctx, routeDiagnosticsContextKey{}, diag), diag
}

func noteRouteAccount(ctx context.Context, accountHint string, failover bool) {
	if ctx == nil {
		return
	}
	diag, _ := ctx.Value(routeDiagnosticsContextKey{}).(*routeDiagnostics)
	if diag == nil {
		return
	}
	diag.mu.Lock()
	defer diag.mu.Unlock()
	if accountHint != "" {
		diag.accountHint = accountHint
	}
	diag.failover = diag.failover || failover
}

func (d *routeDiagnostics) fields() (accountHint string, failover bool) {
	if d == nil {
		return "", false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.accountHint, d.failover
}

func (event *RouteEvent) applyRouteDiagnostics(diag *routeDiagnostics) {
	if event == nil {
		return
	}
	accountHint, failover := diag.fields()
	if accountHint != "" {
		event.AccountHint = accountHint
	}
	if failover {
		event.Failover = true
	}
}

func (event *RouteEvent) applySessionCorrelation(headers http.Header) {
	if event == nil {
		return
	}
	event.SessionKey, event.SessionSource = sessionCorrelation(headers)
}

func redactedAccountHint(prefix string, identifiers ...string) string {
	for _, identifier := range identifiers {
		if identifier == "" {
			continue
		}
		sum := sha256.Sum256([]byte(identifier))
		return prefix + ":" + hex.EncodeToString(sum[:])[:12]
	}
	return ""
}

// sessionCorrelation derives a stable, non-secret session key and source label
// from request headers. It never exposes raw header values; all keys are
// truncated SHA-256 hashes. Authorization, cookies, API keys, local proxy
// tokens, emails, and account UUIDs are never used.
//
// Priority:
//  1. X-Claude-Code-Session-Id  → "claude-session:<12 hex>"  source "x-claude-code-session-id"
//  2. session_id / Session_id   → "codex-session:<12 hex>"   source "session_id"
//  3. X-Codex-Window-Id         → "codex-window:<12 hex>"    source "x-codex-window-id"
//  4. stable non-secret headers → "unknown-client:<12 hex>"  source "unknown-client"
//  5. nothing                   → ""                          source "none"
func sessionCorrelation(headers http.Header) (key string, source string) {
	return headerSessionCorrelation(headers, true)
}

func payloadSessionCorrelation(headers http.Header, body []byte) (key string, source string) {
	if key, source := headerSessionCorrelation(headers, false); key != "" {
		return key, source
	}
	if key, source := bodySessionCorrelation(body); key != "" {
		return key, source
	}
	return headerSessionCorrelation(headers, true)
}

func headerSessionCorrelation(headers http.Header, includeUnknownClient bool) (key string, source string) {
	// 1. Claude Code session ID
	if v := headers.Get("X-Claude-Code-Session-Id"); v != "" {
		return hashPrefix("claude-session", v), "x-claude-code-session-id"
	}

	// 2. Codex session_id — http.CanonicalHeaderKey("session_id") = "Session_id".
	// Both spellings canonicalize to "Session_id", so one .Get is sufficient.
	if v := headers.Get("Session_id"); v != "" {
		return hashPrefix("codex-session", v), "session_id"
	}

	// 3. Codex window ID
	if v := headers.Get("X-Codex-Window-Id"); v != "" {
		return hashPrefix("codex-window", v), "x-codex-window-id"
	}

	if !includeUnknownClient {
		return "", "none"
	}

	// 4. Stable non-secret client fingerprint from User-Agent + known safe headers.
	//    Deliberately excludes Authorization, Cookie, x-api-key, local token values.
	var parts []string
	if ua := headers.Get("User-Agent"); ua != "" {
		parts = append(parts, ua)
	}
	for _, safe := range []string{"X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Lang"} {
		if v := headers.Get(safe); v != "" {
			parts = append(parts, safe+"="+v)
		}
	}
	if len(parts) > 0 {
		combined := strings.Join(parts, "|")
		return hashPrefix("unknown-client", combined), "unknown-client"
	}

	return "", "none"
}

func bodySessionCorrelation(body []byte) (key string, source string) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return "", "none"
	}
	for _, field := range []string{"conversation_id", "thread_id", "session_id", "response_id", "previous_response_id", "parent_response_id"} {
		if v := findStringField(value, field); v != "" {
			return hashPrefix("body-session", field+":"+v), "body:" + field
		}
	}
	return "", "none"
}

func findStringField(value any, field string) string {
	switch v := value.(type) {
	case map[string]any:
		if raw, ok := v[field]; ok {
			if s, ok := raw.(string); ok && s != "" {
				return s
			}
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == field {
				continue
			}
			if s := findStringField(v[key], field); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range v {
			if s := findStringField(child, field); s != "" {
				return s
			}
		}
	}
	return ""
}

func codexWebSocketFrameCorrelation(headers http.Header, frame []byte) (key string, source string, signal string) {
	if key, source := headerSessionCorrelation(headers, false); key != "" {
		return key, source, codexWebSocketFrameSignal(frame)
	}
	if key, source := codexWebSocketFrameSession(frame); key != "" {
		return key, source, codexWebSocketFrameSignal(frame)
	}
	key, source = headerSessionCorrelation(headers, true)
	return key, source, codexWebSocketFrameSignal(frame)
}

func codexWebSocketFrameSession(frame []byte) (key string, source string) {
	var value any
	if err := json.Unmarshal(frame, &value); err != nil {
		return "", "none"
	}
	for _, field := range []string{"thread_id", "conversation_id", "session_id", "response_id", "previous_response_id", "parent_response_id"} {
		if v := findStringField(value, field); v != "" {
			return hashPrefix("ws-session", field+":"+v), "ws:" + field
		}
	}
	return "", "none"
}

func codexWebSocketFrameSignal(frame []byte) string {
	var payload struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}
	if err := json.Unmarshal(frame, &payload); err != nil {
		return "unknown"
	}
	method := strings.ToLower(payload.Method)
	switch {
	case strings.Contains(method, "compact"):
		return "compact_transition"
	case strings.Contains(method, "clear") || strings.Contains(method, "reset"):
		return "clear_transition"
	case method == "thread/start" || strings.Contains(method, "start"):
		return "new_session"
	case findStringField(payload.Params, "previous_response_id") != "" || findStringField(payload.Params, "parent_response_id") != "":
		return "continuation"
	case countMessages(payload.Params) >= 10:
		return "long_session"
	default:
		return "unknown"
	}
}

func countMessages(value any) int {
	switch v := value.(type) {
	case map[string]any:
		if raw, ok := v["messages"]; ok {
			if messages, ok := raw.([]any); ok {
				return len(messages)
			}
		}
		if raw, ok := v["input"]; ok {
			if messages, ok := raw.([]any); ok {
				return len(messages)
			}
		}
		maxCount := 0
		for _, child := range v {
			if count := countMessages(child); count > maxCount {
				maxCount = count
			}
		}
		return maxCount
	case []any:
		maxCount := 0
		for _, child := range v {
			if count := countMessages(child); count > maxCount {
				maxCount = count
			}
		}
		return maxCount
	default:
		return 0
	}
}

func hashPrefix(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + ":" + hex.EncodeToString(sum[:])[:12]
}

// PayloadEvent is a single payload diagnostics log entry. It records
// request-body metadata (and the body itself) for buffered requests.
// It never records headers, tokens, or credential values.
type PayloadEvent struct {
	Time          time.Time       `json:"time"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Provider      string          `json:"provider"`
	RouteKind     string          `json:"route_kind,omitempty"`
	Model         string          `json:"model,omitempty"`
	ClientKind    string          `json:"client_kind,omitempty"`
	SessionKey    string          `json:"session_key,omitempty"`
	SessionSource string          `json:"session_source,omitempty"`
	SessionSignal string          `json:"session_signal,omitempty"`
	FrameIndex    int             `json:"frame_index,omitempty"`
	BodyBytes     int             `json:"body_bytes"`
	Body          json.RawMessage `json:"body,omitempty"`
}

// encodeBody returns raw as an embedded JSON value if it is valid JSON,
// or as a JSON string otherwise. This keeps the payload log valid JSONL
// regardless of whether the request body was JSON or binary.
func encodeBody(raw []byte) json.RawMessage {
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	encoded, err := json.Marshal(string(raw))
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(encoded)
}

// jsonlWriter is a low-level JSONL file writer with a mutex for concurrent safety.
type jsonlWriter struct {
	mu   sync.Mutex
	file *os.File
}

func openJSONLWriter(path string) (*jsonlWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &jsonlWriter{file: f}, nil
}

func (w *jsonlWriter) encode(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return json.NewEncoder(w.file).Encode(v)
}

func (w *jsonlWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// DiagnosticsWriter writes RouteEvents to a JSONL file.
type DiagnosticsWriter struct {
	w *jsonlWriter
}

func OpenDiagnosticsWriter(path string) (*DiagnosticsWriter, error) {
	jw, err := openJSONLWriter(path)
	if err != nil {
		return nil, err
	}
	return &DiagnosticsWriter{w: jw}, nil
}

func (w *DiagnosticsWriter) Write(event RouteEvent) error {
	if w == nil || w.w == nil {
		return nil
	}
	return w.w.encode(event)
}

func (w *DiagnosticsWriter) Close() error {
	if w == nil || w.w == nil {
		return nil
	}
	return w.w.close()
}

// PayloadWriter writes PayloadEvents to a JSONL file.
type PayloadWriter struct {
	w *jsonlWriter
}

// OpenPayloadWriter opens (or creates) a JSONL file for payload diagnostics.
func OpenPayloadWriter(path string) (*PayloadWriter, error) {
	jw, err := openJSONLWriter(path)
	if err != nil {
		return nil, err
	}
	return &PayloadWriter{w: jw}, nil
}

// Write appends a PayloadEvent. Nil-safe and zero-value-safe.
func (w *PayloadWriter) Write(event PayloadEvent) error {
	if w == nil || w.w == nil {
		return nil
	}
	return w.w.encode(event)
}

// Close closes the underlying file. Nil-safe and idempotent.
func (w *PayloadWriter) Close() error {
	if w == nil || w.w == nil {
		return nil
	}
	return w.w.close()
}
