package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexTraceStart supplies stable identity known when a request or WebSocket
// connection first reaches CQ. One WebSocket connection owns a connection ID;
// every response.create frame receives a distinct trace ID.
type CodexTraceStart struct {
	Transport     string
	ConnectionID  string
	SessionKey    string
	SessionSource string
	ThreadKey     string
	RequestKind   string
}

// CodexTraceCandidate records one frozen routing candidate and why it could or
// could not be selected. AccountHint is a one-way identifier, never a token.
type CodexTraceCandidate struct {
	Ordinal           int    `json:"ordinal"`
	AccountHint       string `json:"account_hint,omitempty"`
	Value             int    `json:"value,omitempty"`
	Compatible        bool   `json:"compatible"`
	Routable          bool   `json:"routable"`
	CapacityState     string `json:"capacity_state,omitempty"`
	ProvisionalLeases int    `json:"provisional_leases,omitempty"`
	Selected          bool   `json:"selected,omitempty"`
	ExclusionReason   string `json:"exclusion_reason,omitempty"`
}

// CodexTraceLeaseState is a point-in-time view of durable route authority.
type CodexTraceLeaseState struct {
	Classification        string   `json:"classification,omitempty"`
	State                 string   `json:"state,omitempty"`
	JournalGeneration     uint64   `json:"journal_generation,omitempty"`
	RequestGeneration     uint64   `json:"request_generation,omitempty"`
	AttemptGeneration     uint64   `json:"attempt_generation,omitempty"`
	EverAdmitted          bool     `json:"ever_admitted,omitempty"`
	BoundAccountHint      string   `json:"bound_account_hint,omitempty"`
	BoundRecordGeneration uint64   `json:"bound_record_generation,omitempty"`
	AffinityPresent       bool     `json:"affinity_present,omitempty"`
	AffinityInvalidated   bool     `json:"affinity_invalidated,omitempty"`
	AffinityAccountHint   string   `json:"affinity_account_hint,omitempty"`
	UnavailableAccounts   []string `json:"unavailable_accounts,omitempty"`
	QuotaExhausted        []string `json:"quota_exhausted_accounts,omitempty"`
	ProvisionalAccounts   []string `json:"provisional_accounts,omitempty"`
}

// CodexTraceEvent is one ordered phase in one Codex prompt lifecycle.
// EventType distinguishes these records from legacy route summaries sharing
// the same JSONL file.
type CodexTraceEvent struct {
	Time           time.Time             `json:"time"`
	EventType      string                `json:"event_type"`
	TraceID        string                `json:"trace_id"`
	ConnectionID   string                `json:"connection_id,omitempty"`
	Sequence       uint64                `json:"sequence"`
	SessionKey     string                `json:"session_key,omitempty"`
	SessionSource  string                `json:"session_source,omitempty"`
	ThreadKey      string                `json:"thread_key,omitempty"`
	Transport      string                `json:"transport"`
	Phase          string                `json:"phase"`
	Stage          string                `json:"stage,omitempty"`
	Outcome        string                `json:"outcome,omitempty"`
	Reason         string                `json:"reason,omitempty"`
	Method         string                `json:"method,omitempty"`
	Path           string                `json:"path,omitempty"`
	RequestKind    string                `json:"request_kind,omitempty"`
	Attempt        int                   `json:"attempt,omitempty"`
	AccountHint    string                `json:"account_hint,omitempty"`
	Pool           string                `json:"pool,omitempty"`
	PoolID         string                `json:"pool_id,omitempty"`
	StatusCode     int                   `json:"status_code,omitempty"`
	UpstreamStatus int                   `json:"upstream_status,omitempty"`
	FrameType      string                `json:"frame_type,omitempty"`
	FrameBytes     int                   `json:"frame_bytes,omitempty"`
	EventName      string                `json:"event_name,omitempty"`
	CloseCode      int                   `json:"close_code,omitempty"`
	CloseReason    string                `json:"close_reason,omitempty"`
	BytesIn        int64                 `json:"bytes_in,omitempty"`
	BytesOut       int64                 `json:"bytes_out,omitempty"`
	LatencyMS      int64                 `json:"latency_ms,omitempty"`
	ErrorClass     string                `json:"error_class,omitempty"`
	Retry          bool                  `json:"retry,omitempty"`
	Failover       bool                  `json:"failover,omitempty"`
	Candidates     []CodexTraceCandidate `json:"candidates,omitempty"`
	Lease          *CodexTraceLeaseState `json:"lease,omitempty"`
}

type codexTraceContextKey struct{}

type codexTrace struct {
	mu            sync.Mutex
	writer        *DiagnosticsWriter
	payload       *PayloadWriter
	traceID       string
	connectionID  string
	sequence      uint64
	sessionKey    string
	sessionSource string
	threadKey     string
	transport     string
	requestKind   string
}

var codexTraceFallback atomic.Uint64

func newCodexTraceID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return prefix + ":" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%s:%016x", prefix, codexTraceFallback.Add(1))
}

func withCodexTrace(ctx context.Context, writer *DiagnosticsWriter, payload *PayloadWriter, start CodexTraceStart) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	connectionID := start.ConnectionID
	if connectionID == "" && start.Transport == "websocket" {
		connectionID = newCodexTraceID("connection")
	}
	trace := &codexTrace{
		writer: writer, payload: payload, traceID: newCodexTraceID("trace"),
		connectionID: connectionID, sessionKey: start.SessionKey,
		sessionSource: start.SessionSource, threadKey: start.ThreadKey, transport: start.Transport,
		requestKind: start.RequestKind,
	}
	return context.WithValue(ctx, codexTraceContextKey{}, trace)
}

func withCodexTraceSpan(ctx context.Context, start CodexTraceStart) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	parent, _ := ctx.Value(codexTraceContextKey{}).(*codexTrace)
	if parent != nil {
		parent.mu.Lock()
		if start.ConnectionID == "" {
			start.ConnectionID = parent.connectionID
		}
		writer, payload := parent.writer, parent.payload
		parent.mu.Unlock()
		return withCodexTrace(ctx, writer, payload, start)
	}
	return withCodexTrace(ctx, nil, nil, start)
}

func codexTraceFromContext(ctx context.Context) *codexTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(codexTraceContextKey{}).(*codexTrace)
	return trace
}

func codexTraceIDs(ctx context.Context) (traceID, connectionID string) {
	trace := codexTraceFromContext(ctx)
	if trace == nil {
		return "", ""
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.traceID, trace.connectionID
}

func updateCodexTraceIdentity(ctx context.Context, sessionKey, sessionSource, threadKey, requestKind string) {
	trace := codexTraceFromContext(ctx)
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if sessionKey != "" {
		trace.sessionKey = sessionKey
		trace.sessionSource = sessionSource
	}
	if threadKey != "" {
		trace.threadKey = threadKey
	}
	if requestKind != "" {
		trace.requestKind = requestKind
	}
}

func emitCodexTrace(ctx context.Context, event CodexTraceEvent) {
	trace := codexTraceFromContext(ctx)
	if trace == nil || trace.writer == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.sequence++
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	event.EventType = "codex_trace"
	event.TraceID = trace.traceID
	event.ConnectionID = trace.connectionID
	event.Sequence = trace.sequence
	if event.SessionKey == "" {
		event.SessionKey = trace.sessionKey
		event.SessionSource = trace.sessionSource
	}
	if event.ThreadKey == "" {
		event.ThreadKey = trace.threadKey
	}
	if event.Transport == "" {
		event.Transport = trace.transport
	}
	if event.RequestKind == "" {
		event.RequestKind = trace.requestKind
	}
	if err := trace.writer.WriteTrace(event); err != nil {
		fmt.Fprintf(os.Stderr, "%s cq: diagnostics: trace write trace_id=%s connection_id=%s: %v\n", time.Now().UTC().Format(time.RFC3339Nano), trace.traceID, trace.connectionID, err)
	}
}

func codexTraceAccountHint(account codex.AccountKey) string {
	if account == "" {
		return ""
	}
	return redactedAccountHint("account", string(account))
}

func codexTraceAccountHints(accounts []codex.AccountKey) []string {
	hints := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if hint := codexTraceAccountHint(account); hint != "" {
			hints = append(hints, hint)
		}
	}
	return hints
}

func codexTraceLeaseSnapshot(snapshot CodexLeaseRouteSnapshot) *CodexTraceLeaseState {
	provisional := make([]codex.AccountKey, 0, len(snapshot.Provisional))
	for account := range snapshot.Provisional {
		provisional = append(provisional, account)
	}
	sort.Slice(provisional, func(left, right int) bool { return provisional[left] < provisional[right] })
	return &CodexTraceLeaseState{
		Classification:        string(snapshot.Classification),
		JournalGeneration:     snapshot.JournalGeneration,
		BoundAccountHint:      codexTraceAccountHint(snapshot.BoundAccountKey),
		BoundRecordGeneration: snapshot.BoundRecordGeneration,
		AffinityPresent:       snapshot.AffinityPresent,
		AffinityInvalidated:   snapshot.AffinityInvalidated,
		AffinityAccountHint:   codexTraceAccountHint(snapshot.AffinityAccountKey),
		UnavailableAccounts:   codexTraceAccountHints(snapshot.UnavailableAccountKeys),
		QuotaExhausted:        codexTraceAccountHints(snapshot.QuotaExhaustedAccountKeys),
		ProvisionalAccounts:   codexTraceAccountHints(provisional),
	}
}

func codexTraceLeaseHandle(handle *CodexLeaseRequestHandle) *CodexTraceLeaseState {
	if handle == nil {
		return nil
	}
	return &CodexTraceLeaseState{
		State:             handle.State().String(),
		RequestGeneration: handle.RequestGeneration(),
		AttemptGeneration: handle.AttemptGeneration(),
		EverAdmitted:      handle.EverAdmitted(),
		BoundAccountHint:  codexTraceAccountHint(handle.AccountKey()),
	}
}

func beginCodexTraceLeaseTransition(ctx context.Context, stage string, handle *CodexLeaseRequestHandle) func(*CodexLeaseRequestHandle, error) {
	emitCodexTrace(ctx, CodexTraceEvent{
		Phase: "lease_transition", Stage: stage, Outcome: "before", Lease: codexTraceLeaseHandle(handle),
	})
	return func(after *CodexLeaseRequestHandle, err error) {
		outcome := "after"
		if err != nil {
			outcome = "error"
		}
		emitCodexTrace(ctx, CodexTraceEvent{
			Phase: "lease_transition", Stage: stage, Outcome: outcome,
			Reason: codexTraceErrorReason(err), Lease: codexTraceLeaseHandle(after),
		})
	}
}

func codexTraceDispatchCandidates(plan CodexFrozenDispatchPlan) []CodexTraceCandidate {
	selected := map[codex.AccountKey]bool{}
	for _, account := range append(plan.Accounts(), plan.AccountUnavailableFallbacks()...) {
		selected[account.Choice().AccountKey] = true
	}
	candidates := make([]CodexTraceCandidate, 0, len(plan.policyCandidates))
	for index, candidate := range plan.policyCandidates {
		capacityState := "unknown"
		for _, view := range candidate.RequiredCapacity {
			switch view.State {
			case CapacityZero:
				capacityState = "zero"
			case CapacityPositive:
				if capacityState != "zero" {
					capacityState = "positive"
				}
			}
		}
		exclusion := ""
		switch {
		case !candidate.Compatible:
			exclusion = "incompatible"
		case !candidate.Routable:
			exclusion = "unroutable"
		case capacityState == "zero":
			exclusion = "capacity_exhausted"
		case !selected[candidate.Choice.AccountKey]:
			exclusion = "not_selected"
		}
		candidates = append(candidates, CodexTraceCandidate{
			Ordinal: index + 1, AccountHint: codexTraceAccountHint(candidate.Choice.AccountKey),
			Value: int(candidate.Value), Compatible: candidate.Compatible, Routable: candidate.Routable,
			CapacityState: capacityState, ProvisionalLeases: candidate.ProvisionalLeases,
			Selected: selected[candidate.Choice.AccountKey], ExclusionReason: exclusion,
		})
	}
	return candidates
}

func codexTraceHTTPOutcome(status int) string {
	if status == http.StatusSwitchingProtocols || status >= 200 && status < 400 {
		return "success"
	}
	if status == 0 {
		return "disconnected"
	}
	return "error"
}

func codexTraceRelayOutcome(err error) string {
	if err == nil {
		return "success"
	}
	return "error"
}

func codexTraceErrorReason(err error) string {
	if err == nil {
		return ""
	}
	return string(codexRequestFailureReason(err))
}

func codexTraceStartFromRouteDiagnostics(diagnostics *routeDiagnostics, transport string) CodexTraceStart {
	start := CodexTraceStart{Transport: transport}
	if diagnostics == nil {
		return start
	}
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	start.SessionKey = diagnostics.codex.SessionKey
	start.SessionSource = diagnostics.codex.SessionSource
	start.ThreadKey = diagnostics.codex.ThreadKey
	start.RequestKind = diagnostics.codex.RequestKind
	return start
}

func codexTraceWebSocketFrameType(messageType int) string {
	switch messageType {
	case 1:
		return "text"
	case 2:
		return "binary"
	case 8:
		return "close"
	case 9:
		return "ping"
	case 10:
		return "pong"
	default:
		return fmt.Sprintf("type_%d", messageType)
	}
}

func codexTraceWebSocketClose(err error) (int, string) {
	var closeError *websocket.CloseError
	if !errors.As(err, &closeError) {
		return 0, ""
	}
	reason := "peer_close"
	switch closeError.Code {
	case websocket.CloseNormalClosure:
		reason = "normal_closure"
	case websocket.CloseGoingAway:
		reason = "going_away"
	case websocket.CloseProtocolError:
		reason = "protocol_error"
	case websocket.CloseUnsupportedData:
		reason = "unsupported_data"
	case websocket.CloseNoStatusReceived:
		reason = "no_status_received"
	case websocket.CloseAbnormalClosure:
		reason = "abnormal_closure"
	case websocket.CloseInvalidFramePayloadData:
		reason = "invalid_payload"
	case websocket.ClosePolicyViolation:
		reason = "policy_violation"
	case websocket.CloseMessageTooBig:
		reason = "message_too_big"
	case websocket.CloseMandatoryExtension:
		reason = "mandatory_extension"
	case websocket.CloseInternalServerErr:
		reason = "internal_error"
	case websocket.CloseServiceRestart:
		reason = "service_restart"
	case websocket.CloseTryAgainLater:
		reason = "try_again_later"
	case websocket.CloseTLSHandshake:
		reason = "tls_handshake"
	}
	return closeError.Code, reason
}

func parseCodexTraceStatus(value string) int {
	status, _ := strconv.Atoi(value)
	return status
}

// CodexTraceSessionKeys returns every correlation key CQ can derive from one
// user-facing Codex session or thread selector.
func CodexTraceSessionKeys(selector string) []string {
	selector = strings.TrimSpace(selector)
	if slash := strings.LastIndex(selector, "/"); slash >= 0 {
		selector = strings.TrimSpace(selector[slash+1:])
	}
	if selector == "" {
		return nil
	}
	values := []string{
		hashPrefix("codex-session", selector),
		hashPrefix("codex-thread", selector),
		hashPrefix("codex-window", selector),
		hashPrefix("claude-session", selector),
	}
	for _, field := range []string{"thread_id", "conversation_id", "session_id", "response_id", "previous_response_id", "parent_response_id"} {
		values = append(values, hashPrefix("ws-session", field+":"+selector))
		values = append(values, hashPrefix("body-session", field+":"+selector))
	}
	return values
}

func codexTraceWrappedErrorClass(wrapped CodexWrappedError) string {
	switch {
	case wrapped.HardUsageLimit:
		return "capacity_exhausted"
	case wrapped.AuthFailure:
		return "auth_rejected"
	case wrapped.Found:
		return "provider_error"
	default:
		return ""
	}
}
