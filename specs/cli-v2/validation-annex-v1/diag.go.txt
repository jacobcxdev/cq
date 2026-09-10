package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RouteEvent struct {
	Time                     time.Time `json:"time"`
	Method                   string    `json:"method"`
	Path                     string    `json:"path"`
	Provider                 string    `json:"provider"`
	RouteKind                string    `json:"route_kind,omitempty"`
	Model                    string    `json:"model,omitempty"`
	RequestLineage           string    `json:"request_lineage,omitempty"`
	RequestedReasoningEffort string    `json:"requested_reasoning_effort,omitempty"`
	RequestedModelClass      string    `json:"requested_model_class,omitempty"`
	CompactionPhase          string    `json:"compaction_phase,omitempty"`
	AccountHint              string    `json:"account_hint,omitempty"`
	PinActive                bool      `json:"pin_active,omitempty"`
	Failover                 bool      `json:"failover,omitempty"`
	StatusCode               int       `json:"status_code,omitempty"`
	LatencyMS                int64     `json:"latency_ms,omitempty"`
	Error                    string    `json:"error,omitempty"`
	SessionKey               string    `json:"session_key,omitempty"`
	SessionSource            string    `json:"session_source,omitempty"`
	TurnHint                 string    `json:"turn_hint,omitempty"`
	RequestKind              string    `json:"request_kind,omitempty"`
	LeasePhase               string    `json:"lease_phase,omitempty"`
	LeaseGeneration          uint64    `json:"lease_generation,omitempty"`
	Decision                 string    `json:"decision,omitempty"`
	Reason                   string    `json:"reason,omitempty"`
	Bucket                   string    `json:"bucket,omitempty"`
	Continuity               string    `json:"continuity,omitempty"`
	CallerDomain             string    `json:"caller_domain,omitempty"`
	CallerIdentityPresent    *bool     `json:"caller_identity_present,omitempty"`
	CallerContinuityMapped   *bool     `json:"caller_continuity_mapped,omitempty"`
	CallerRoutingMapped      *bool     `json:"caller_routing_mapped,omitempty"`
	CallerIndexEpoch         uint64    `json:"caller_index_epoch,omitempty"`
}

type routeDiagnosticsContextKey struct{}

type routeDiagnostics struct {
	mu          sync.Mutex
	accountHint string
	failover    bool
	codex       codexObservationFields
}

type codexObservationFields struct {
	TurnHint                 string
	RequestKind              string
	RequestLineage           string
	RequestedReasoningEffort string
	RequestedModelClass      string
	CompactionPhase          string
	LeasePhase               string
	LeaseGeneration          uint64
	Decision                 string
	Reason                   string
	Bucket                   string
	AccountHint              string
	Continuity               string
	CallerMappingObserved    bool
	CallerDomain             string
	CallerIdentityPresent    bool
	CallerContinuityMapped   bool
	CallerRoutingMapped      bool
	CallerIndexEpoch         uint64
}

func withRouteDiagnostics(ctx context.Context) (context.Context, *routeDiagnostics) {
	diag := &routeDiagnostics{}
	return context.WithValue(ctx, routeDiagnosticsContextKey{}, diag), diag
}

func noteCodexObservation(ctx context.Context, fields codexObservationFields) {
	if ctx == nil {
		return
	}
	diag, _ := ctx.Value(routeDiagnosticsContextKey{}).(*routeDiagnostics)
	if diag == nil {
		return
	}
	diag.mu.Lock()
	defer diag.mu.Unlock()
	if fields.TurnHint != "" {
		diag.codex.TurnHint = fields.TurnHint
	}
	if fields.RequestKind != "" {
		diag.codex.RequestKind = fields.RequestKind
	}
	if fields.RequestLineage != "" {
		diag.codex.RequestLineage = fields.RequestLineage
	}
	if fields.RequestedReasoningEffort != "" {
		diag.codex.RequestedReasoningEffort = fields.RequestedReasoningEffort
	}
	if fields.RequestedModelClass != "" {
		diag.codex.RequestedModelClass = fields.RequestedModelClass
	}
	if fields.CompactionPhase != "" {
		diag.codex.CompactionPhase = fields.CompactionPhase
	}
	if fields.LeasePhase != "" {
		diag.codex.LeasePhase = fields.LeasePhase
	}
	if fields.LeaseGeneration != 0 {
		diag.codex.LeaseGeneration = fields.LeaseGeneration
	}
	if fields.Decision != "" {
		diag.codex.Decision = fields.Decision
	}
	if fields.Reason != "" {
		diag.codex.Reason = fields.Reason
	}
	if fields.Bucket != "" {
		diag.codex.Bucket = fields.Bucket
	}
	if fields.AccountHint != "" {
		diag.accountHint = fields.AccountHint
	}
	if fields.Continuity != "" {
		diag.codex.Continuity = fields.Continuity
	}
	if fields.CallerMappingObserved {
		diag.codex.CallerMappingObserved = true
		diag.codex.CallerDomain = fields.CallerDomain
		diag.codex.CallerIdentityPresent = fields.CallerIdentityPresent
		diag.codex.CallerContinuityMapped = fields.CallerContinuityMapped
		diag.codex.CallerRoutingMapped = fields.CallerRoutingMapped
		diag.codex.CallerIndexEpoch = fields.CallerIndexEpoch
	}
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
	if diag == nil {
		return
	}
	diag.mu.Lock()
	codex := diag.codex
	diag.mu.Unlock()
	event.TurnHint = codex.TurnHint
	event.RequestKind = codex.RequestKind
	event.RequestLineage = codex.RequestLineage
	event.RequestedReasoningEffort = codex.RequestedReasoningEffort
	event.RequestedModelClass = codex.RequestedModelClass
	event.CompactionPhase = codex.CompactionPhase
	event.LeasePhase = codex.LeasePhase
	event.LeaseGeneration = codex.LeaseGeneration
	event.Decision = codex.Decision
	event.Reason = codex.Reason
	event.Bucket = codex.Bucket
	event.Continuity = codex.Continuity
	if codex.CallerMappingObserved {
		callerIdentityPresent := codex.CallerIdentityPresent
		callerContinuityMapped := codex.CallerContinuityMapped
		callerRoutingMapped := codex.CallerRoutingMapped
		event.CallerDomain = codex.CallerDomain
		event.CallerIdentityPresent = &callerIdentityPresent
		event.CallerContinuityMapped = &callerContinuityMapped
		event.CallerRoutingMapped = &callerRoutingMapped
		event.CallerIndexEpoch = codex.CallerIndexEpoch
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
	w      *jsonlWriter
	canary atomic.Pointer[CodexCanaryRecorder]
}

var ErrUnsafeCodexDiagnostics = errors.New("unsafe Codex diagnostics event")

const (
	codexDiagnosticsModelGPT       = "model_family_gpt"
	codexDiagnosticsModelCodex     = "model_family_codex"
	codexDiagnosticsModelReasoning = "model_family_reasoning"
	codexDiagnosticsModelUnknown   = "model_family_unknown"

	codexRequestedModelClassSol     = "gpt_5_6_sol"
	codexRequestedModelClassTerra   = "gpt_5_6_terra"
	codexRequestedModelClassLuna    = "gpt_5_6_luna"
	codexRequestedModelClassOther   = "other"
	codexRequestedModelClassUnknown = "unknown"

	codexDiagnosticsBucketBase        = "capacity_base"
	codexDiagnosticsBucketModelScoped = "capacity_model_scoped"
	codexDiagnosticsBucketUnknown     = "capacity_unknown"
)

// projectCodexDiagnostics removes caller-controlled model and capacity-bucket
// bytes before a Codex route event reaches the diagnostics writer. The writer
// deliberately does not call this helper: callers must project trusted runtime
// observations explicitly, while a direct writer bypass remains fail closed.
func projectCodexDiagnostics(event RouteEvent) RouteEvent {
	if event.Provider != "codex" {
		return event
	}
	event.Model = projectCodexDiagnosticsModel(event.Model)
	event.RequestedModelClass = projectCodexRequestedModelClass(event.RequestedModelClass)
	event.Bucket = projectCodexDiagnosticsBucket(event.Bucket)
	return event
}

func projectCodexRequestedModelClass(value string) string {
	switch value {
	case codexRequestedModelClassSol, "gpt-5.6-sol":
		return codexRequestedModelClassSol
	case codexRequestedModelClassTerra, "gpt-5.6-terra":
		return codexRequestedModelClassTerra
	case codexRequestedModelClassLuna, "gpt-5.6-luna":
		return codexRequestedModelClassLuna
	case codexRequestedModelClassOther:
		return codexRequestedModelClassOther
	case "":
		return ""
	case codexRequestedModelClassUnknown:
		return codexRequestedModelClassUnknown
	default:
		return codexRequestedModelClassOther
	}
}

func projectCodexDiagnosticsModel(value string) string {
	switch value {
	case "":
		return ""
	case codexDiagnosticsModelGPT,
		codexDiagnosticsModelCodex,
		codexDiagnosticsModelReasoning,
		codexDiagnosticsModelUnknown:
		return value
	}
	if strings.HasPrefix(value, "gpt-") {
		return codexDiagnosticsModelGPT
	}
	if strings.HasPrefix(value, "codex-") {
		return codexDiagnosticsModelCodex
	}
	for _, prefix := range []string{"o1", "o3", "o4"} {
		if value == prefix || strings.HasPrefix(value, prefix+"-") {
			return codexDiagnosticsModelReasoning
		}
	}
	return codexDiagnosticsModelUnknown
}

func projectCodexDiagnosticsBucket(value string) string {
	switch value {
	case "":
		return ""
	case codexDiagnosticsBucketBase,
		codexDiagnosticsBucketModelScoped,
		codexDiagnosticsBucketUnknown:
		return value
	case string(CapacityBucketBase):
		return codexDiagnosticsBucketBase
	}
	if strings.HasPrefix(value, capacityBucketModelPrefix) {
		return codexDiagnosticsBucketModelScoped
	}
	return codexDiagnosticsBucketUnknown
}

func OpenDiagnosticsWriter(path string) (*DiagnosticsWriter, error) {
	jw, err := openJSONLWriter(path)
	if err != nil {
		return nil, err
	}
	return &DiagnosticsWriter{w: jw}, nil
}

// SetCodexCanary attaches the active canary recorder to the final diagnostics
// output boundary. The writer retains no event values when validation fails.
func (w *DiagnosticsWriter) SetCodexCanary(recorder *CodexCanaryRecorder) {
	if w == nil {
		return
	}
	w.canary.Store(recorder)
}

func (w *DiagnosticsWriter) codexCanary() *CodexCanaryRecorder {
	if w == nil {
		return nil
	}
	return w.canary.Load()
}

func (w *DiagnosticsWriter) Write(event RouteEvent) error {
	if w == nil || w.w == nil {
		return nil
	}
	if err := rejectUnsafeCodexDiagnostics(event, w.canary.Load()); err != nil {
		return err
	}
	return w.w.encode(event)
}

func rejectUnsafeCodexDiagnostics(event RouteEvent, recorder *CodexCanaryRecorder) error {
	if event.Provider != "codex" || safeCodexRouteEvent(event) {
		return nil
	}
	if recorder != nil {
		if err := recorder.RecordSecretLeak(); err != nil {
			return errors.Join(ErrUnsafeCodexDiagnostics, errors.New("record Codex canary leak counter"))
		}
	}
	return ErrUnsafeCodexDiagnostics
}

func safeCodexRouteEvent(event RouteEvent) bool {
	return safeCodexDiagnosticsMethod(event.Method) &&
		safeCodexRouteKind(event.RouteKind) &&
		safeCodexAccountHint(event.AccountHint) &&
		safeCodexSessionCorrelation(event.SessionKey, event.SessionSource) &&
		safeHashedHint(event.TurnHint, "turn") &&
		safeCodexRequestKind(event.RequestKind) &&
		safeCodexRequestLineage(event.RequestLineage) &&
		safeCodexRequestedReasoningEffort(event.RequestedReasoningEffort) &&
		safeCodexRequestedModelClass(event.RequestedModelClass) &&
		safeCodexCompactionPhase(event.CompactionPhase) &&
		safeCodexLeasePhase(event.LeasePhase) &&
		safeCodexDecision(event.Decision) &&
		safeCodexReason(event.Reason) &&
		safeCodexBucket(event.Bucket) &&
		safeCodexContinuity(event.Continuity) &&
		safeCodexCallerDomain(event.CallerDomain) &&
		safeCodexDiagnosticsError(event.Error) &&
		safeCodexModel(event.Model) &&
		safeCodexDiagnosticsPath(event.Path)
}

func safeCodexDiagnosticsMethod(value string) bool {
	switch value {
	case "", http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodHead:
		return true
	default:
		return false
	}
}

func safeCodexRouteKind(value string) bool {
	switch value {
	case "", "anthropic_count_tokens", "anthropic_messages", "codex_app_server", "codex_compact", "codex_images", "codex_legacy_websocket", "codex_live_call", "codex_live_sideband", "codex_native", "codex_search", "codex_websocket_broker", "codex_websocket_frame", "openai_unsupported":
		return true
	default:
		return false
	}
}

func safeCodexAccountHint(value string) bool {
	return value == "" || safeHashedHint(value, "codex")
}

func safeCodexSessionCorrelation(key, source string) bool {
	if key == "" {
		return source == "" || source == "none"
	}
	switch source {
	case "x-claude-code-session-id":
		return safeHashedHint(key, "claude-session")
	case "session_id":
		return safeHashedHint(key, "codex-session")
	case "x-codex-window-id":
		return safeHashedHint(key, "codex-window")
	case "unknown-client":
		return safeHashedHint(key, "unknown-client")
	case "body:conversation_id", "body:thread_id", "body:session_id", "body:response_id", "body:previous_response_id", "body:parent_response_id":
		return safeHashedHint(key, "body-session")
	default:
		return false
	}
}

func safeCodexRequestKind(value string) bool {
	switch value {
	case "", string(CodexRequestTurn), string(CodexRequestPrewarm), string(CodexRequestCompaction), string(CodexRequestMemory):
		return true
	default:
		return false
	}
}

func safeCodexRequestLineage(value string) bool {
	switch value {
	case "", codexRequestLineagePreviousResponseIDAbsent, codexRequestLineagePreviousResponseIDPresent, codexRequestLineageUnknown:
		return true
	default:
		return false
	}
}

func safeCodexRequestedReasoningEffort(value string) bool {
	switch value {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", codexRequestedReasoningEffortUnspecified, codexRequestedReasoningEffortUnknown:
		return true
	default:
		return false
	}
}

func safeCodexRequestedModelClass(value string) bool {
	switch value {
	case "", codexRequestedModelClassSol, codexRequestedModelClassTerra, codexRequestedModelClassLuna, codexRequestedModelClassOther, codexRequestedModelClassUnknown:
		return true
	default:
		return false
	}
}

func safeCodexCompactionPhase(value string) bool {
	switch value {
	case "", string(CodexCompactionStandalone), string(CodexCompactionPreTurn), string(CodexCompactionMidTurn), "not_applicable", "unknown":
		return true
	default:
		return false
	}
}

func safeCodexLeasePhase(value string) bool {
	switch value {
	case "", "reserving", "provisional", "bound_active", "continuation_pending", "bound_quiescent", "orphaned", "superseded", "expired", "failed_unadmitted":
		return true
	default:
		return false
	}
}

func safeCodexDecision(value string) bool {
	switch value {
	case "", "affinity_reuse", "broker_failed", "fairness_select", "terminal_default", "plan_failed", "session_failed", "shadow_unknown", "shadow_parsed", "shadow_rejected", "shadow_selected", "shadow_continuity_error", "shadow_admitted", "shadow_sampling_complete", "shadow_unadmitted":
		return true
	default:
		return false
	}
}

func safeCodexReason(value string) bool {
	if safeCodexRequestFailureReason(CodexRequestFailureReason(value)) != CodexRequestFailureUnknown || value == string(CodexRequestFailureUnknown) {
		return true
	}
	switch value {
	case "", "request_decode", "metadata_parse", "turn_identity_missing", "response_event_invalid", "response_event_malformed", "response_event_unknown", "stale_turn", "concurrent_turn", "continuity", "lease_error", "lease_transition", "stale_generation", "upstream_rejected", "unadmitted_end", "upstream_closed", "upstream_outcome_indeterminate", "invalid_frame", "downstream_read_failed", "response_unavailable", "cancelled", "deadline", "timeout", "dns", "connect", "connection_reset", "broken_pipe", "server_closed_idle", "unexpected_eof", "eof", "tls", "protocol", "unknown":
		return true
	}
	if suffix, ok := strings.CutPrefix(value, "candidate_attempt_"); ok {
		return safePositiveDecimal(suffix, 10)
	}
	return false
}

func safeCodexBucket(value string) bool {
	switch value {
	case "", codexDiagnosticsBucketBase, codexDiagnosticsBucketModelScoped, codexDiagnosticsBucketUnknown:
		return true
	default:
		return false
	}
}

func safeCodexContinuity(value string) bool {
	switch value {
	case "", "fail_closed_if_enforced", "pinned":
		return true
	default:
		return false
	}
}

func safeCodexCallerDomain(value string) bool {
	switch NormalCallerDomain(value) {
	case "", NormalCallerLocal, NormalCallerClaude, NormalCallerCodex:
		return true
	default:
		return false
	}
}

func safeCodexDiagnosticsError(value string) bool {
	if value == "" {
		return true
	}
	errType, code, hasCode := strings.Cut(value, ":")
	switch errType {
	case "api_error", "authentication_error", "internal_error", "invalid_request_error":
	default:
		return false
	}
	if !hasCode {
		return true
	}
	if code == "" || strings.Contains(code, ":") {
		return false
	}
	switch code {
	case "invalid_proxy_token",
		"no_codex_accounts",
		"unsupported_websocket_transport",
		"unsupported_openai_endpoint",
		"websocket_upgrade_required",
		"method_not_allowed",
		"invalid_codex_upstream",
		"invalid_upstream",
		"codex_upstream_error",
		"request_translation_failed",
		"read_request_body",
		"request_body_too_large",
		"invalid_route_model",
		"stream_collection_failed",
		"response_assembly_failed",
		"decode_count_tokens_response",
		"model_registry_refresher_not_configured",
		"model_registry_not_configured",
		"registry_refresh_failed",
		"upstream_error":
		return true
	default:
		return false
	}
}

func safeCodexModel(value string) bool {
	switch value {
	case "", codexDiagnosticsModelGPT, codexDiagnosticsModelCodex, codexDiagnosticsModelReasoning, codexDiagnosticsModelUnknown:
		return true
	default:
		return false
	}
}

func safeCodexDiagnosticsPath(value string) bool {
	switch value {
	case "",
		"/v1/responses", "/responses",
		"/v1/responses/compact", "/responses/compact",
		"/app-server",
		"/v1/images/generations", "/images/generations",
		"/alpha/search",
		"/live", "/v1/live",
		"/realtime/calls", "/v1/realtime/calls",
		"/realtime", "/v1/realtime",
		"/v1/messages", "/v1/messages/count_tokens",
		"/v1/chat/completions", "/chat/completions",
		"/v1/completions", "/completions",
		"/v1/embeddings", "/embeddings",
		"/v1/moderations", "/moderations",
		"/v1/files", "/files",
		"/v1/uploads", "/uploads",
		"/v1/vector_stores", "/vector_stores",
		"/v1/batches", "/batches",
		"/v1/assistants", "/assistants",
		"/v1/threads", "/threads",
		"/v1/evals", "/evals",
		"/v1/containers", "/containers",
		"/v1/conversations", "/conversations":
		return true
	}
	return false
}

func safePositiveDecimal(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength || value[0] == '0' {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func safeHashedHint(value, prefix string) bool {
	if value == "" {
		return true
	}
	wantPrefix := prefix + ":"
	if !strings.HasPrefix(value, wantPrefix) || len(value) != len(wantPrefix)+12 {
		return false
	}
	for _, char := range value[len(wantPrefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
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
