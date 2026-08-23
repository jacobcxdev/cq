package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	codexTurnReceiptDomain      = "cq/codex-turn-receipt/v1"
	codexTurnReceiptMaxEntries  = 4096
	codexTurnReceiptTerminalTTL = time.Hour
)

type CodexTurnReceiptState string

const (
	CodexTurnReceiptPlanned       CodexTurnReceiptState = "planned"
	CodexTurnReceiptAttempted     CodexTurnReceiptState = "attempted"
	CodexTurnReceiptCompleted     CodexTurnReceiptState = "completed"
	CodexTurnReceiptFailed        CodexTurnReceiptState = "failed"
	CodexTurnReceiptRejected      CodexTurnReceiptState = "rejected"
	CodexTurnReceiptIndeterminate CodexTurnReceiptState = "indeterminate"
)

type CodexTurnReceiptTransport string

const (
	CodexTurnReceiptTransportHTTP      CodexTurnReceiptTransport = "http"
	CodexTurnReceiptTransportWebSocket CodexTurnReceiptTransport = "websocket"
)

type CodexTurnReceiptRouteReason string

const (
	CodexTurnReceiptRouteBound           CodexTurnReceiptRouteReason = "bound"
	CodexTurnReceiptRouteAffinityReuse   CodexTurnReceiptRouteReason = "affinity_reuse"
	CodexTurnReceiptRouteFairnessSelect  CodexTurnReceiptRouteReason = "fairness_select"
	CodexTurnReceiptRouteTerminalDefault CodexTurnReceiptRouteReason = "terminal_default"
	CodexTurnReceiptRouteUnknown         CodexTurnReceiptRouteReason = "unknown"
)

type CodexTurnReceiptShadowComparison string

const (
	CodexTurnReceiptShadowSameAccount        CodexTurnReceiptShadowComparison = "same_account"
	CodexTurnReceiptShadowAlternativeAccount CodexTurnReceiptShadowComparison = "alternative_account"
	CodexTurnReceiptShadowNotApplicable      CodexTurnReceiptShadowComparison = "not_applicable"
	CodexTurnReceiptShadowUnavailable        CodexTurnReceiptShadowComparison = "unavailable"
)

// CodexTurnReceiptV1 contains only closed, privacy-safe route facts.
type CodexTurnReceiptV1 struct {
	State                    CodexTurnReceiptState       `json:"state"`
	Transport                CodexTurnReceiptTransport   `json:"transport"`
	RequestKind              string                      `json:"request_kind"`
	RequestLineage           string                      `json:"request_lineage"`
	RequestedModelClass      string                      `json:"requested_model_class"`
	RequestedReasoningEffort string                      `json:"requested_reasoning_effort"`
	CompactionPhase          string                      `json:"compaction_phase"`
	Pool                     string                      `json:"pool,omitempty"`
	RouteReason              CodexTurnReceiptRouteReason `json:"route_reason"`
	PlannedAccountHint       string                      `json:"planned_account_hint,omitempty"`
	ActualAccountHint        string                      `json:"actual_account_hint,omitempty"`
}

// CodexTurnReceiptV2 adds closed, privacy-safe shadow-routing facts.
type CodexTurnReceiptV2 struct {
	CodexTurnReceiptV1
	ShadowComparison             CodexTurnReceiptShadowComparison `json:"shadow_comparison"`
	ShadowAlternativeAccountHint string                           `json:"shadow_alternative_account_hint,omitempty"`
}

type CodexTurnReceiptLookupV1 struct {
	SchemaVersion int                 `json:"schema_version"`
	Found         bool                `json:"found"`
	Receipt       *CodexTurnReceiptV1 `json:"receipt,omitempty"`
}

type CodexTurnReceiptLookupV2 struct {
	SchemaVersion int                 `json:"schema_version"`
	Found         bool                `json:"found"`
	Receipt       *CodexTurnReceiptV2 `json:"receipt,omitempty"`
}

type codexTurnReceiptKey [sha256.Size]byte

type codexTurnReceiptEntry struct {
	receipt   CodexTurnReceiptV2
	updatedAt time.Time
	sequence  uint64
}

// CodexTurnReceiptStore is process-local receipt authority. It stores no raw
// session or turn identifiers and deliberately has no persistence surface.
type CodexTurnReceiptStore struct {
	mu      sync.RWMutex
	key     [sha256.Size]byte
	entries map[codexTurnReceiptKey]codexTurnReceiptEntry
	now     func() time.Time
	next    uint64
}

type codexTurnReceiptHandle struct {
	store *CodexTurnReceiptStore
	key   codexTurnReceiptKey
}

func NewCodexTurnReceiptStore(random io.Reader, now func() time.Time) (*CodexTurnReceiptStore, error) {
	store := &CodexTurnReceiptStore{entries: make(map[codexTurnReceiptKey]codexTurnReceiptEntry), now: now}
	if random == nil {
		return nil, io.ErrUnexpectedEOF
	}
	if _, err := io.ReadFull(random, store.key[:]); err != nil {
		return nil, err
	}
	if store.now == nil {
		store.now = time.Now
	}
	return store, nil
}

func (store *CodexTurnReceiptStore) register(session, turn []byte, receipt CodexTurnReceiptV2) *codexTurnReceiptHandle {
	if store == nil || !validCanonicalSessionID(session) || !validCanonicalSessionID(turn) || !validCodexTurnReceipt(receipt) {
		return nil
	}
	key := store.digest(session, turn)
	now := store.now()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pruneExpiredLocked(now)
	if _, exists := store.entries[key]; !exists && len(store.entries) >= codexTurnReceiptMaxEntries {
		store.evictOldestLocked()
	}
	store.next++
	store.entries[key] = codexTurnReceiptEntry{receipt: receipt, updatedAt: now, sequence: store.next}
	return &codexTurnReceiptHandle{store: store, key: key}
}

func (store *CodexTurnReceiptStore) lookup(session, turn []byte) (CodexTurnReceiptV2, bool) {
	if store == nil || !validCanonicalSessionID(session) || !validCanonicalSessionID(turn) {
		return CodexTurnReceiptV2{}, false
	}
	key := store.digest(session, turn)
	now := store.now()
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, found := store.entries[key]
	if !found {
		return CodexTurnReceiptV2{}, false
	}
	if codexTurnReceiptTerminal(entry.receipt.State) && !now.Before(entry.updatedAt.Add(codexTurnReceiptTerminalTTL)) {
		delete(store.entries, key)
		return CodexTurnReceiptV2{}, false
	}
	return entry.receipt, true
}

func (store *CodexTurnReceiptStore) digest(session, turn []byte) codexTurnReceiptKey {
	digest := hmac.New(sha256.New, store.key[:])
	_, _ = digest.Write([]byte(codexTurnReceiptDomain))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(session)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(session)
	binary.BigEndian.PutUint32(length[:], uint32(len(turn)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(turn)
	var key codexTurnReceiptKey
	copy(key[:], digest.Sum(nil))
	return key
}

func (store *CodexTurnReceiptStore) update(key codexTurnReceiptKey, update func(*CodexTurnReceiptV2)) {
	if store == nil || update == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, found := store.entries[key]
	if !found || codexTurnReceiptTerminal(entry.receipt.State) {
		return
	}
	update(&entry.receipt)
	store.next++
	entry.sequence = store.next
	entry.updatedAt = store.now()
	store.entries[key] = entry
}

func (store *CodexTurnReceiptStore) pruneExpiredLocked(now time.Time) {
	for key, entry := range store.entries {
		if codexTurnReceiptTerminal(entry.receipt.State) && !now.Before(entry.updatedAt.Add(codexTurnReceiptTerminalTTL)) {
			delete(store.entries, key)
		}
	}
}

func (store *CodexTurnReceiptStore) evictOldestLocked() {
	var oldestKey codexTurnReceiptKey
	var oldestSequence uint64
	found := false
	for key, entry := range store.entries {
		if !found || entry.sequence < oldestSequence {
			oldestKey = key
			oldestSequence = entry.sequence
			found = true
		}
	}
	if found {
		delete(store.entries, oldestKey)
	}
}

func (handle *codexTurnReceiptHandle) attempt(account codex.AccountKey) {
	if handle == nil || handle.store == nil || account == "" {
		return
	}
	handle.store.update(handle.key, func(receipt *CodexTurnReceiptV2) {
		receipt.State = CodexTurnReceiptAttempted
		receipt.ActualAccountHint = redactedAccountHint("codex", string(account))
	})
}

func (handle *codexTurnReceiptHandle) terminal(state CodexTurnReceiptState) {
	if handle == nil || handle.store == nil || !codexTurnReceiptTerminal(state) {
		return
	}
	handle.store.update(handle.key, func(receipt *CodexTurnReceiptV2) {
		receipt.State = state
	})
}

func codexTurnReceiptTerminal(state CodexTurnReceiptState) bool {
	switch state {
	case CodexTurnReceiptCompleted, CodexTurnReceiptFailed, CodexTurnReceiptRejected, CodexTurnReceiptIndeterminate:
		return true
	default:
		return false
	}
}

func validCodexTurnReceipt(receipt CodexTurnReceiptV2) bool {
	if !validCodexTurnReceiptV1(receipt.CodexTurnReceiptV1) {
		return false
	}
	switch receipt.ShadowComparison {
	case CodexTurnReceiptShadowAlternativeAccount:
		return validCodexTurnReceiptAccountHint(receipt.ShadowAlternativeAccountHint)
	case CodexTurnReceiptShadowSameAccount, CodexTurnReceiptShadowNotApplicable, CodexTurnReceiptShadowUnavailable:
		return receipt.ShadowAlternativeAccountHint == ""
	default:
		return false
	}
}

func validCodexTurnReceiptV1(receipt CodexTurnReceiptV1) bool {
	if receipt.State != CodexTurnReceiptPlanned || receipt.RequestKind != string(CodexRequestTurn) || receipt.RequestLineage == "" || receipt.RequestedModelClass == "" || receipt.RequestedReasoningEffort == "" || receipt.CompactionPhase == "" {
		return false
	}
	if receipt.Transport != CodexTurnReceiptTransportHTTP && receipt.Transport != CodexTurnReceiptTransportWebSocket {
		return false
	}
	if receipt.Pool != "" && !poolNamePattern.MatchString(receipt.Pool) {
		return false
	}
	switch receipt.RouteReason {
	case CodexTurnReceiptRouteBound, CodexTurnReceiptRouteAffinityReuse, CodexTurnReceiptRouteFairnessSelect, CodexTurnReceiptRouteTerminalDefault, CodexTurnReceiptRouteUnknown:
		return true
	default:
		return false
	}
}

func validCodexTurnReceiptAccountHint(value string) bool {
	if len(value) != len("codex:")+12 || value[:len("codex:")] != "codex:" {
		return false
	}
	for _, current := range value[len("codex:"):] {
		if (current < '0' || current > '9') && (current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

type codexTurnReceiptLifecycle struct {
	inner   CodexHTTPRequestLifecycle
	receipt *codexTurnReceiptHandle
}

func wrapCodexTurnReceiptLifecycle(inner CodexHTTPRequestLifecycle, receipt *codexTurnReceiptHandle) CodexHTTPRequestLifecycle {
	if inner == nil || receipt == nil {
		return inner
	}
	return &codexTurnReceiptLifecycle{inner: inner, receipt: receipt}
}

func (lifecycle *codexTurnReceiptLifecycle) EverAdmitted() bool {
	return lifecycle != nil && lifecycle.inner != nil && lifecycle.inner.EverAdmitted()
}

func (lifecycle *codexTurnReceiptLifecycle) AccountKey() codex.AccountKey {
	if lifecycle == nil || lifecycle.inner == nil {
		return ""
	}
	return lifecycle.inner.AccountKey()
}

func (lifecycle *codexTurnReceiptLifecycle) next(next CodexHTTPRequestLifecycle, err error, state CodexTurnReceiptState, attempted bool) (CodexHTTPRequestLifecycle, error) {
	if err != nil || next == nil {
		return next, err
	}
	if attempted {
		lifecycle.receipt.attempt(next.AccountKey())
	}
	if codexTurnReceiptTerminal(state) {
		lifecycle.receipt.terminal(state)
	}
	return &codexTurnReceiptLifecycle{inner: next, receipt: lifecycle.receipt}, nil
}

func (lifecycle *codexTurnReceiptLifecycle) MarkDispatchedContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.MarkDispatchedContext(ctx)
	return lifecycle.next(next, err, "", true)
}

func (lifecycle *codexTurnReceiptLifecycle) RejectAndPrepareContext(ctx context.Context, slot uint32) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.RejectAndPrepareContext(ctx, slot)
	return lifecycle.next(next, err, "", false)
}

func (lifecycle *codexTurnReceiptLifecycle) AbandonBeforeDispatchContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.AbandonBeforeDispatchContext(ctx)
	return lifecycle.next(next, err, CodexTurnReceiptRejected, false)
}

func (lifecycle *codexTurnReceiptLifecycle) FinishRejected() (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.FinishRejected()
	return lifecycle.next(next, err, CodexTurnReceiptRejected, false)
}

func (lifecycle *codexTurnReceiptLifecycle) IndeterminateContext(ctx context.Context, evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.IndeterminateContext(ctx, evidence)
	return lifecycle.next(next, err, CodexTurnReceiptIndeterminate, false)
}

func (lifecycle *codexTurnReceiptLifecycle) Drain() (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.Drain()
	return lifecycle.next(next, err, "", false)
}

func (lifecycle *codexTurnReceiptLifecycle) AdmitHTTP2xxContext(ctx context.Context, evidence CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.AdmitHTTP2xxContext(ctx, evidence)
	return lifecycle.next(next, err, "", false)
}

func (lifecycle *codexTurnReceiptLifecycle) ProviderCompleted(evidence CodexHTTPCompletionEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.ProviderCompleted(evidence)
	return lifecycle.next(next, err, CodexTurnReceiptCompleted, false)
}

func (lifecycle *codexTurnReceiptLifecycle) ProviderFailed(evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	next, err := lifecycle.inner.ProviderFailed(evidence)
	return lifecycle.next(next, err, CodexTurnReceiptFailed, false)
}
