package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type CodexObservationHealth struct {
	Leases              map[string]int `json:"leases"`
	Requests            uint64         `json:"requests"`
	Attempts            uint64         `json:"attempts"`
	StrongKeys          uint64         `json:"strong_keys"`
	MetadataHeaders     uint64         `json:"metadata_headers"`
	ZstdRequests        uint64         `json:"zstd_requests"`
	RequestDecodeErrors uint64         `json:"request_decode_errors"`
	MetadataParseErrors uint64         `json:"metadata_parse_errors"`
	MissingTurnIdentity uint64         `json:"missing_turn_identity"`
	Failovers           uint64         `json:"failovers"`
	QuotaEvents         uint64         `json:"quota_events"`
	Resyncs             uint64         `json:"resyncs"`
	Unknown             uint64         `json:"unknown"`
	Late                uint64         `json:"late"`
	Stale               uint64         `json:"stale"`
	ContinuityErrors    uint64         `json:"continuity_errors"`
	RefreshSuspended    uint64         `json:"refresh_suspended"`
	CanaryErrors        uint64         `json:"canary_errors"`
}

type CodexTurnObserver struct {
	Leases  *CodexTurnLeaseManager
	Prewarm *CodexPrewarmManager
	Store   *CodexLeaseStore

	hintKey []byte
	storeMu sync.Mutex

	requests         atomic.Uint64
	attempts         atomic.Uint64
	strongKeys       atomic.Uint64
	metadataHeaders  atomic.Uint64
	zstdRequests     atomic.Uint64
	requestDecodeErr atomic.Uint64
	metadataParseErr atomic.Uint64
	missingTurnID    atomic.Uint64
	failovers        atomic.Uint64
	quotaEvents      atomic.Uint64
	resyncs          atomic.Uint64
	unknown          atomic.Uint64
	late             atomic.Uint64
	stale            atomic.Uint64
	continuityErrors atomic.Uint64
	refreshSuspended atomic.Uint64
	canaryErrors     atomic.Uint64
	socketGeneration atomic.Uint64
	canary           atomic.Pointer[CodexCanaryRecorder]
	capacity         atomic.Pointer[CodexCapacityLedger]
}

func NewCodexTurnObserver(leases *CodexTurnLeaseManager, store *CodexLeaseStore) (*CodexTurnObserver, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate Codex observation hint key: %w", err)
	}
	return newCodexTurnObserverWithKey(leases, store, key), nil
}

func newCodexTurnObserverWithKey(leases *CodexTurnLeaseManager, store *CodexLeaseStore, key []byte) *CodexTurnObserver {
	if leases == nil {
		leases = NewCodexTurnLeaseManager(1, false, nil)
	}
	return &CodexTurnObserver{Leases: leases, Prewarm: NewCodexPrewarmManager(leases, nil), Store: store, hintKey: append([]byte(nil), key...)}
}

func (observer *CodexTurnObserver) SetCanary(canary *CodexCanaryRecorder) {
	if observer != nil {
		observer.canary.Store(canary)
	}
}

// BindCapacity connects response observation to the same capacity ledger used
// by selection. Passing nil disables capacity fact production.
func (observer *CodexTurnObserver) BindCapacity(capacity *CodexCapacityLedger) {
	if observer != nil {
		observer.capacity.Store(capacity)
	}
}

func (observer *CodexTurnObserver) recordCanaryAdmitted(now time.Time) {
	if observer == nil {
		return
	}
	if canary := observer.canary.Load(); canary != nil {
		if err := canary.RecordAdmitted(now); err != nil {
			observer.canaryErrors.Add(1)
		}
	}
}

func (observer *CodexTurnObserver) recordCanaryMismatch() {
	if observer == nil {
		return
	}
	if canary := observer.canary.Load(); canary != nil {
		if err := canary.RecordKeyedMismatch(); err != nil {
			observer.canaryErrors.Add(1)
		}
	}
}

func (observer *CodexTurnObserver) recordCanaryUnexplained() {
	if observer == nil {
		return
	}
	if canary := observer.canary.Load(); canary != nil {
		if err := canary.RecordUnexplainedLifecycle(); err != nil {
			observer.canaryErrors.Add(1)
		}
	}
}

type CodexTurnObservation struct {
	observer *CodexTurnObserver
	ctx      context.Context
	request  CodexProtocolRequest
	key      LeaseKey
	choice   RouteChoice
	compact  bool
	jsonBody bool
	ws       bool
	socket   uint64

	mu              sync.Mutex
	leaseAcquired   bool
	routingReleased bool
	admitted        bool
	completed       bool
	failed          bool
	diagnosticFinal bool
	prewarm         bool
	prewarmAnchor   string
	parser          *CodexSSEParser
	parserFailed    bool
	body            []byte
	bodyOverflow    bool
	capacity        *codexRateLimitProducer
	finishOnce      sync.Once
}

func (observer *CodexTurnObserver) BeginHTTP(ctx context.Context, body []byte, contentEncoding, directMetadata string, compact bool) *CodexTurnObservation {
	return observer.begin(ctx, body, contentEncoding, directMetadata, nil, compact, false, 0)
}

func (observer *CodexTurnObserver) beginHTTPDecoded(ctx context.Context, body []byte, contentEncoding, directMetadata string, compact bool) *CodexTurnObservation {
	return observer.beginDecoded(ctx, body, contentEncoding, directMetadata, nil, compact, false, 0)
}

type codexObservationContextKey struct{}

func withCodexObservation(ctx context.Context, value any) context.Context {
	return context.WithValue(ctx, codexObservationContextKey{}, value)
}

func observeCodexAttempt(ctx context.Context, choice RouteChoice, attempt CandidateAttempt) {
	if ctx == nil {
		return
	}
	var observer *CodexTurnObserver
	switch value := ctx.Value(codexObservationContextKey{}).(type) {
	case *CodexTurnObservation:
		observer = value.observer
	case *CodexTurnObserver:
		observer = value
	}
	if observer == nil {
		return
	}
	observer.attempts.Add(1)
	noteCodexObservation(ctx, codexObservationFields{AccountHint: redactedAccountHint("codex", string(choice.AccountKey)), Reason: fmt.Sprintf("candidate_attempt_%d", attempt.Ordinal)})
}

func (observer *CodexTurnObserver) BeginWebSocket(ctx context.Context, frame []byte, handshake *CodexTurnMetadata, socketGeneration uint64) *CodexTurnObservation {
	return observer.begin(ctx, frame, "identity", "", handshake, false, true, socketGeneration)
}

func (observer *CodexTurnObserver) begin(ctx context.Context, body []byte, contentEncoding, directMetadata string, handshake *CodexTurnMetadata, compact, ws bool, socketGeneration uint64) *CodexTurnObservation {
	handle := observer.newObservation(ctx, contentEncoding, directMetadata, compact, ws, socketGeneration)
	if handle == nil {
		return nil
	}
	decoded, err := DecodeCodexRequest(body, contentEncoding, DefaultCodexZstdLimits)
	if err != nil {
		observer.unknown.Add(1)
		observer.requestDecodeErr.Add(1)
		noteCodexObservation(ctx, codexObservationFields{Decision: "shadow_unknown", Reason: "request_decode"})
		return handle
	}
	return observer.parseObservation(handle, decoded.Decoded(), directMetadata, handshake)
}

func (observer *CodexTurnObserver) beginDecoded(ctx context.Context, body []byte, contentEncoding, directMetadata string, handshake *CodexTurnMetadata, compact, ws bool, socketGeneration uint64) *CodexTurnObservation {
	handle := observer.newObservation(ctx, contentEncoding, directMetadata, compact, ws, socketGeneration)
	if handle == nil {
		return nil
	}
	return observer.parseObservation(handle, body, directMetadata, handshake)
}

func (observer *CodexTurnObserver) newObservation(ctx context.Context, contentEncoding, directMetadata string, compact, ws bool, socketGeneration uint64) *CodexTurnObservation {
	if observer == nil {
		return nil
	}
	observer.requests.Add(1)
	if directMetadata != "" {
		observer.metadataHeaders.Add(1)
	}
	if strings.EqualFold(strings.TrimSpace(contentEncoding), "zstd") {
		observer.zstdRequests.Add(1)
	}
	return &CodexTurnObservation{observer: observer, ctx: ctx, compact: compact, ws: ws, socket: socketGeneration, parser: NewCodexSSEParser(codexSSEDefaultMaxEventBytes)}
}

func (observer *CodexTurnObserver) parseObservation(handle *CodexTurnObservation, body []byte, directMetadata string, handshake *CodexTurnMetadata) *CodexTurnObservation {
	request, err := ParseCodexProtocolRequest(body, directMetadata, handshake)
	if err != nil {
		observer.unknown.Add(1)
		observer.metadataParseErr.Add(1)
		noteCodexObservation(handle.ctx, codexObservationFields{Decision: "shadow_unknown", Reason: "metadata_parse"})
		return handle
	}
	handle.request = request
	if request.Metadata.Found {
		handle.key = NewCodexLeaseKey(request.Metadata.Metadata)
		if request.Metadata.Strong {
			observer.strongKeys.Add(1)
		}
		noteCodexObservation(handle.ctx, codexObservationFields{
			TurnHint:    observer.turnHint(handle.key),
			RequestKind: string(request.Metadata.Metadata.RequestKind),
			Decision:    "shadow_parsed",
		})
		if request.Metadata.Metadata.RequestKind == CodexRequestPrewarm {
			handle.prewarm = true
		}
	} else {
		observer.unknown.Add(1)
		observer.missingTurnID.Add(1)
		noteCodexObservation(handle.ctx, codexObservationFields{Decision: "shadow_unknown", Reason: "turn_identity_missing"})
	}
	return handle
}

func (handle *CodexTurnObservation) Selected(choice RouteChoice, failover bool) {
	if handle == nil || handle.observer == nil {
		return
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	handle.choice = choice
	if failover {
		handle.observer.failovers.Add(1)
	}
	metadata := handle.request.Metadata.Metadata
	if handle.prewarm {
		reservation, err := handle.observer.Prewarm.Create(metadata, handle.prewarmCorrelation())
		if err == nil && handle.ws && handle.socket != 0 {
			_, _ = handle.observer.Prewarm.Bind(reservation.Lane, choice.AccountKey, handle.socket)
		}
		return
	}
	if !handle.request.Metadata.Found || metadata.TurnID == "" || (metadata.RequestKind != CodexRequestTurn && metadata.RequestKind != CodexRequestCompaction) || choice.AccountKey == "" {
		return
	}
	handle.observer.restoreJournalLease(handle.key, choice.AccountKey)
	if handle.request.PreviousResponseID != "" {
		if _, err := handle.observer.Prewarm.Adopt(handle.key, handle.request.PreviousResponseID); err == nil {
			// Acquire below takes routing reference on adopted lease.
		}
	}
	lease, err := handle.observer.Leases.AcquireRoute(handle.ctx, handle.key, func(context.Context) (RouteChoice, error) {
		return choice, nil
	})
	if err != nil {
		handle.observer.observeLeaseError(err)
		handle.diagnosticFinal = true
		noteCodexObservation(handle.ctx, codexObservationFields{TurnHint: handle.observer.turnHint(handle.key), RequestKind: string(metadata.RequestKind), Decision: "shadow_rejected", Reason: safeLeaseReason(err)})
		return
	}
	handle.leaseAcquired = true
	noteCodexObservation(handle.ctx, codexObservationFields{
		TurnHint:        handle.observer.turnHint(handle.key),
		RequestKind:     string(metadata.RequestKind),
		LeasePhase:      lease.State.String(),
		LeaseGeneration: lease.Generation,
		Decision:        "shadow_selected",
		AccountHint:     redactedAccountHint("codex", string(choice.AccountKey)),
	})
}

func (handle *CodexTurnObservation) pinnedChoice() (RouteChoice, bool, error) {
	if handle == nil || handle.observer == nil || handle.prewarm || !handle.request.Metadata.Strong {
		return RouteChoice{}, false, nil
	}
	metadata := handle.request.Metadata.Metadata
	if metadata.TurnID == "" || (metadata.RequestKind != CodexRequestTurn && metadata.RequestKind != CodexRequestCompaction) {
		return RouteChoice{}, false, nil
	}
	return handle.observer.Leases.ObservedRouteChoice(handle.key)
}

func (observer *CodexTurnObserver) restoreJournalLease(key LeaseKey, selected codex.AccountKey) {
	if observer.Store == nil {
		return
	}
	if _, found := observer.Leases.Get(key); found {
		return
	}
	modeEpoch, authoritative := observer.Leases.Mode()
	record, account, found := observer.Store.LookupMode(key, []codex.AccountKey{selected}, modeEpoch, authoritative)
	if !found {
		return
	}
	lease := CodexTurnLease{
		Key:                  key,
		State:                record.State,
		AccountKey:           account,
		Generation:           record.LeaseGeneration,
		ModeEpoch:            record.ModeEpoch,
		Authoritative:        record.Authoritative,
		HasEncryptedState:    record.HasEncryptedState,
		TurnStateUnavailable: record.HasTurnState,
		NonMigratable:        record.NonMigratable || account == "",
		LastSeen:             record.LastSeen,
	}
	observer.Leases.Restore([]CodexTurnLease{lease})
}

// Response observes an upstream response with transport decoding provenance.
func (handle *CodexTurnObservation) Response(response *http.Response) {
	if response == nil {
		return
	}
	handle.responseHeaders(response.StatusCode, response.Header, !response.Uncompressed && codexAttemptResponseHasIdentityEncoding(response.Header))
}

func (handle *CodexTurnObservation) ResponseHeaders(status int, header http.Header) {
	handle.responseHeaders(status, header, codexAttemptResponseHasIdentityEncoding(header))
}

func (handle *CodexTurnObservation) responseHeaders(status int, header http.Header, liveEventsAuthoritative bool) {
	if handle == nil || handle.observer == nil {
		return
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	for name := range header {
		if _, relevant := codexRateLimitHeaderFamily(strings.ToLower(name)); relevant {
			handle.observer.quotaEvents.Add(1)
			break
		}
	}
	accepted := status >= 200 && status < 300 || status == http.StatusSwitchingProtocols
	if !accepted {
		handle.failUnadmittedLocked("upstream_rejected")
		return
	}
	if capacity := handle.observer.capacity.Load(); capacity != nil {
		handle.capacity = newCodexRateLimitProducer(
			capacity,
			capacity.NewObservationStream(),
			handle.choice.AccountKey,
			capacity.now,
			liveEventsAuthoritative,
		)
		if handle.capacity != nil {
			if err := handle.capacity.ObserveHeaders(header); err != nil {
				handle.observer.unknown.Add(1)
			}
		}
	}
	contentType := header.Get("Content-Type")
	if separator := strings.IndexByte(contentType, ';'); separator >= 0 {
		contentType = contentType[:separator]
	}
	if !handle.compact && strings.EqualFold(strings.TrimSpace(contentType), "application/json") {
		handle.jsonBody = true
		handle.parser = nil
	}
	handle.admitLocked()
	if !handle.admitted {
		return
	}
	state, found, err := ParseCodexTurnStateHeader(header)
	if err != nil {
		handle.observer.unknown.Add(1)
	} else if found {
		if err := handle.observer.Leases.SetTurnState(handle.key, state); err != nil {
			handle.observer.observeLeaseError(err)
		} else {
			handle.persistMutationLocked()
		}
	}
}

func (handle *CodexTurnObservation) admitLocked() {
	if handle.prewarm {
		return
	}
	if handle.admitted || handle.diagnosticFinal || !handle.leaseAcquired {
		return
	}
	lease, err := handle.observer.Leases.Admit(handle.key, handle.choice.AccountKey, handle.socket, handle.observer.persist)
	if err != nil {
		handle.observer.observeLeaseError(err)
		handle.diagnosticFinal = true
		noteCodexObservation(handle.ctx, codexObservationFields{TurnHint: handle.observer.turnHint(handle.key), Decision: "shadow_continuity_error", Reason: safeLeaseReason(err), Continuity: "fail_closed_if_enforced"})
		handle.releaseRoutingLocked()
		return
	}
	handle.admitted = true
	handle.releaseRoutingLocked()
	noteCodexObservation(handle.ctx, codexObservationFields{TurnHint: handle.observer.turnHint(handle.key), LeasePhase: lease.State.String(), LeaseGeneration: lease.Generation, Decision: "shadow_admitted", Continuity: "pinned"})
}

func (handle *CodexTurnObservation) ObserveBytes(chunk []byte) {
	if handle == nil || handle.observer == nil || len(chunk) == 0 {
		return
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.parserFailed {
		return
	}
	if !handle.compact && !handle.jsonBody && len(bytes.TrimSpace(handle.body)) == 0 {
		trimmed := bytes.TrimSpace(chunk)
		if len(trimmed) != 0 && trimmed[0] == '{' {
			handle.jsonBody = true
			handle.parser = nil
		}
	}
	if handle.compact || handle.jsonBody {
		if handle.bodyOverflow {
			return
		}
		var ok bool
		handle.body, ok = appendCodexObservationBody(handle.body, chunk, codexProtocolMaxBytes)
		if !ok {
			handle.body = nil
			handle.bodyOverflow = true
			handle.recordParserFailureLocked()
		}
		return
	}
	observations, err := handle.parser.Feed(chunk)
	for _, observation := range observations {
		handle.observeEventLocked(observation)
		if handle.parserFailed {
			break
		}
	}
	if handle.parserFailed {
		return
	}
	if err != nil {
		handle.recordParserFailureLocked()
		return
	}
	if !handle.bodyOverflow {
		bodyLimit := codexSSEDefaultMaxEventBytes
		if handle.parser != nil && handle.parser.maxEvent > 0 {
			bodyLimit = handle.parser.maxEvent
		}
		var ok bool
		handle.body, ok = appendCodexObservationBody(handle.body, chunk, bodyLimit)
		if !ok {
			handle.body = nil
			handle.bodyOverflow = true
		}
	}
}

func appendCodexObservationBody(body, chunk []byte, limit int) ([]byte, bool) {
	if limit < 0 || len(chunk) > limit-len(body) {
		return nil, false
	}
	needed := len(body) + len(chunk)
	if needed <= cap(body) {
		return append(body, chunk...), true
	}
	capacity := cap(body) * 2
	if capacity < 64 {
		capacity = 64
	}
	if capacity < needed {
		capacity = needed
	}
	if capacity > limit {
		capacity = limit
	}
	retained := make([]byte, len(body), capacity)
	copy(retained, body)
	return append(retained, chunk...), true
}

func (handle *CodexTurnObservation) recordParserFailureLocked() {
	if handle.parserFailed {
		return
	}
	handle.body = nil
	handle.bodyOverflow = true
	handle.parserFailed = true
	handle.observer.unknown.Add(1)
	handle.observer.recordCanaryUnexplained()
}

func (handle *CodexTurnObservation) ObserveWebSocketEvent(frame []byte) {
	if handle == nil || handle.observer == nil {
		return
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.parserFailed {
		return
	}
	handle.observeEventLocked(classifyCodexSSEData(frame))
}

func (handle *CodexTurnObservation) observeEventLocked(observation CodexSSEObservation) {
	switch observation.Kind {
	case CodexSSECreated:
		if handle.prewarm {
			handle.prewarmAnchor = observation.ResponseID
			return
		}
		handle.admitLocked()
		if handle.admitted && (observation.ResponseID != "" || observation.HasEncryptedState) {
			if err := handle.observer.Leases.SetResponseAnchor(handle.key, observation.ResponseID, observation.HasEncryptedState); err != nil {
				handle.observer.observeLeaseError(err)
			} else {
				handle.persistMutationLocked()
			}
		}
	case CodexSSEMetadata:
		if handle.leaseAcquired && observation.TurnState != "" {
			if err := handle.observer.Leases.SetTurnState(handle.key, observation.TurnState); err != nil {
				handle.observer.observeLeaseError(err)
			} else if handle.admitted {
				handle.persistMutationLocked()
			}
		}
	case CodexSSECompleted:
		if handle.prewarm {
			anchor := observation.ResponseID
			if anchor == "" {
				anchor = handle.prewarmAnchor
			}
			if anchor != "" {
				_ = handle.observer.Prewarm.ReplaceCorrelation(handle.key.Lane, handle.prewarmCorrelation(), anchor)
				_, _ = handle.observer.Prewarm.Ready(handle.key.Lane, anchor, observation.TurnState)
			}
			return
		}
		if handle.admitted && !handle.completed {
			if observation.ResponseID != "" || observation.HasEncryptedState {
				if err := handle.observer.Leases.SetResponseAnchor(handle.key, observation.ResponseID, observation.HasEncryptedState); err != nil {
					handle.observer.observeLeaseError(err)
				} else {
					handle.persistMutationLocked()
				}
			}
			if lease, err := handle.observer.Leases.ObserveCompleted(handle.key, observation.EndTurn); err == nil {
				handle.completed = true
				handle.persistMutationLocked()
				noteCodexObservation(handle.ctx, codexObservationFields{TurnHint: handle.observer.turnHint(handle.key), LeasePhase: lease.State.String(), LeaseGeneration: lease.Generation, Decision: "shadow_sampling_complete"})
			} else {
				handle.observer.observeLeaseError(err)
			}
		}
	case CodexSSEError:
		if handle.admitted && !handle.failed && !handle.completed {
			if _, err := handle.observer.Leases.ObserveProviderFailed(handle.key); err != nil {
				handle.observer.observeLeaseError(err)
			} else {
				handle.persistMutationLocked()
			}
			handle.failed = true
		}
	case CodexSSERateLimits:
		handle.observer.quotaEvents.Add(1)
		if handle.capacity != nil {
			if err := handle.capacity.ObserveEvent(observation.Data); err != nil {
				handle.observer.unknown.Add(1)
			}
		}
	case CodexSSEUnknown:
		handle.observer.unknown.Add(1)
		handle.observer.recordCanaryUnexplained()
		noteCodexObservation(handle.ctx, codexObservationFields{Decision: "shadow_unknown", Reason: safeCodexEventReason(observation.Type)})
	case CodexSSEMalformed:
		handle.recordParserFailureLocked()
		noteCodexObservation(handle.ctx, codexObservationFields{Decision: "shadow_unknown", Reason: "response_event_malformed"})
	}
}

func safeCodexEventReason(eventType string) string {
	if eventType == "" || len(eventType) > 128 {
		return "response_event_invalid"
	}
	for _, char := range eventType {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '.' && char != '_' && char != '-' {
			return "response_event_invalid"
		}
	}
	return "response_event_" + strings.ReplaceAll(eventType, ".", "_")
}

func (handle *CodexTurnObservation) prewarmCorrelation() string {
	metadata := handle.request.Metadata.Metadata
	if metadata.WindowID != "" {
		return metadata.WindowID
	}
	return metadata.SessionID + "\x00" + metadata.ThreadID
}

func (handle *CodexTurnObservation) Finish(readErr error) {
	if handle == nil || handle.observer == nil {
		return
	}
	handle.finishOnce.Do(func() {
		handle.mu.Lock()
		defer handle.mu.Unlock()
		if readErr == nil && !handle.compact && !handle.jsonBody && !handle.parserFailed {
			if final, err := handle.parser.Finish(); err == nil {
				for _, observation := range final {
					handle.observeEventLocked(observation)
					if handle.parserFailed {
						break
					}
				}
			} else {
				handle.recordParserFailureLocked()
			}
		}
		if handle.compact && handle.admitted && !handle.completed && !handle.parserFailed && readErr == nil {
			if compact, err := ParseCodexCompactResponse(handle.body); err == nil {
				if err := handle.observer.Leases.SetResponseAnchor(handle.key, compact.ResponseID, compact.HasEncryptedState); err != nil {
					handle.observer.observeLeaseError(err)
				} else {
					handle.persistMutationLocked()
				}
				_, _ = handle.observer.Leases.ObserveCompleted(handle.key, nil)
				handle.completed = true
				handle.persistMutationLocked()
			} else {
				handle.body = nil
				handle.bodyOverflow = true
				handle.recordParserFailureLocked()
			}
		}
		if handle.jsonBody && handle.admitted && !handle.completed && !handle.failed && !handle.parserFailed && readErr == nil && len(handle.body) != 0 {
			var response struct {
				Status string `json:"status"`
			}
			if err := validateCodexUnaryAuthority(handle.body); err != nil {
				handle.body = nil
				handle.bodyOverflow = true
				handle.recordParserFailureLocked()
			} else if err := jsonUnmarshalObject(handle.body, &response); err != nil {
				handle.body = nil
				handle.bodyOverflow = true
				handle.recordParserFailureLocked()
			} else if response.Status == "completed" {
				if _, err := handle.observer.Leases.ObserveCompleted(handle.key, nil); err == nil {
					handle.completed = true
					handle.persistMutationLocked()
				} else {
					handle.observer.observeLeaseError(err)
				}
			}
		}
		if handle.admitted && !handle.completed && !handle.failed {
			if _, err := handle.observer.Leases.ObserveIndeterminate(handle.key); err == nil {
				handle.persistMutationLocked()
			}
		}
		if !handle.admitted {
			handle.failUnadmittedLocked("unadmitted_end")
		}
	})
}

func (handle *CodexTurnObservation) persistMutationLocked() {
	if handle == nil || handle.observer == nil || handle.observer.Store == nil {
		return
	}
	if err := handle.observer.persist(handle.observer.Leases.Snapshot()); err != nil {
		_ = handle.observer.Leases.MarkNonMigratable(handle.key)
		_ = handle.observer.persist(handle.observer.Leases.Snapshot())
		handle.observer.observeLeaseError(err)
	}
}

func (handle *CodexTurnObservation) failUnadmittedLocked(reason string) {
	if handle.leaseAcquired {
		_ = handle.observer.Leases.FailUnadmitted(handle.key)
		handle.releaseRoutingLocked()
	}
	if !handle.diagnosticFinal {
		noteCodexObservation(handle.ctx, codexObservationFields{TurnHint: handle.observer.turnHint(handle.key), Decision: "shadow_unadmitted", Reason: reason})
	}
}

func (handle *CodexTurnObservation) releaseRoutingLocked() {
	if handle.leaseAcquired && !handle.routingReleased {
		_ = handle.observer.Leases.ReleaseRouting(handle.key)
		handle.routingReleased = true
	}
}

func (observer *CodexTurnObserver) persist(leases []CodexTurnLease) error {
	if observer.Store == nil {
		return nil
	}
	observer.Leases.Compact(DefaultCodexLeaseRetention)
	leases = observer.Leases.Snapshot()
	observer.storeMu.Lock()
	defer observer.storeMu.Unlock()
	return observer.Store.CommitCurrentLeases(leases)
}

func (observer *CodexTurnObserver) observeLeaseError(err error) {
	switch {
	case errors.Is(err, ErrCodexStaleTurn):
		observer.stale.Add(1)
	case errors.Is(err, ErrCodexContinuity):
		observer.continuityErrors.Add(1)
		observer.recordCanaryMismatch()
	case errors.Is(err, ErrCodexConcurrentTurn):
		observer.late.Add(1)
	default:
		observer.unknown.Add(1)
		observer.recordCanaryUnexplained()
	}
}

func safeLeaseReason(err error) string {
	switch {
	case errors.Is(err, ErrCodexStaleTurn):
		return "stale_turn"
	case errors.Is(err, ErrCodexConcurrentTurn):
		return "concurrent_turn"
	case errors.Is(err, ErrCodexContinuity):
		return "continuity"
	default:
		return "lease_error"
	}
}

func (observer *CodexTurnObserver) turnHint(key LeaseKey) string {
	mac := hmac.New(sha256.New, observer.hintKey)
	_, _ = mac.Write([]byte(key.Lane.Session))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(key.Lane.Thread))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(key.Turn))
	sum := mac.Sum(nil)
	return "turn:" + hex.EncodeToString(sum[:6])
}

func (observer *CodexTurnObserver) NextSocketGeneration() uint64 {
	if observer == nil {
		return 0
	}
	return observer.socketGeneration.Add(1)
}

func (observer *CodexTurnObserver) Health() CodexObservationHealth {
	health := CodexObservationHealth{
		Leases:              make(map[string]int),
		Requests:            observer.requests.Load(),
		Attempts:            observer.attempts.Load(),
		StrongKeys:          observer.strongKeys.Load(),
		MetadataHeaders:     observer.metadataHeaders.Load(),
		ZstdRequests:        observer.zstdRequests.Load(),
		RequestDecodeErrors: observer.requestDecodeErr.Load(),
		MetadataParseErrors: observer.metadataParseErr.Load(),
		MissingTurnIdentity: observer.missingTurnID.Load(),
		Failovers:           observer.failovers.Load(),
		QuotaEvents:         observer.quotaEvents.Load(),
		Resyncs:             observer.resyncs.Load(),
		Unknown:             observer.unknown.Load(),
		Late:                observer.late.Load(),
		Stale:               observer.stale.Load(),
		ContinuityErrors:    observer.continuityErrors.Load(),
		RefreshSuspended:    observer.refreshSuspended.Load(),
		CanaryErrors:        observer.canaryErrors.Load(),
	}
	for _, lease := range observer.Leases.Snapshot() {
		health.Leases[lease.State.String()]++
	}
	return health
}

type codexWSObservationSession struct {
	observer *CodexTurnObserver
	ctx      context.Context
	choice   RouteChoice
	socket   uint64
	capacity *codexRateLimitProducer

	mu      sync.Mutex
	current *CodexTurnObservation
}

func newCodexWSObservationSession(observer *CodexTurnObserver, ctx context.Context, choice RouteChoice, capacity *codexRateLimitProducer) *codexWSObservationSession {
	if observer == nil && capacity == nil {
		return nil
	}
	var socket uint64
	if observer != nil {
		socket = observer.NextSocketGeneration()
	}
	return &codexWSObservationSession{observer: observer, ctx: ctx, choice: choice, socket: socket, capacity: capacity}
}

func (session *codexWSObservationSession) ObserveClient(frame []byte) {
	if session == nil || session.observer == nil {
		return
	}
	handle := session.observer.BeginWebSocket(session.ctx, frame, nil, session.socket)
	handle.Selected(session.choice, false)
	session.mu.Lock()
	previous := session.current
	session.current = handle
	session.mu.Unlock()
	if previous != nil {
		previous.Finish(nil)
	}
}

func (session *codexWSObservationSession) ObserveUpstream(frame []byte) {
	if session == nil {
		return
	}
	if session.capacity != nil {
		if err := session.capacity.ObserveEvent(frame); err != nil {
			if session.observer != nil {
				session.observer.unknown.Add(1)
			}
		}
	}
	if session.observer == nil {
		return
	}
	session.mu.Lock()
	current := session.current
	session.mu.Unlock()
	if current != nil {
		current.ObserveWebSocketEvent(frame)
	}
}

func (session *codexWSObservationSession) Close(err error) {
	if session == nil {
		return
	}
	session.mu.Lock()
	current := session.current
	session.current = nil
	session.mu.Unlock()
	if current != nil {
		current.Finish(err)
	}
}

type codexObservedBody struct {
	body     io.ReadCloser
	handle   *CodexTurnObservation
	once     sync.Once
	terminal atomic.Bool
}

func (s *Server) beginCodexHTTPObservation(ctx context.Context, body []byte, header http.Header, compact bool) *CodexTurnObservation {
	if s == nil || s.CodexObserver == nil {
		return nil
	}
	return s.CodexObserver.BeginHTTP(ctx, body, header.Get("Content-Encoding"), header.Get(codexTurnMetadataKey), compact)
}

func (s *Server) beginCodexHTTPObservationDecoded(ctx context.Context, body []byte, header http.Header, compact bool) *CodexTurnObservation {
	if s == nil || s.CodexObserver == nil {
		return nil
	}
	return s.CodexObserver.beginHTTPDecoded(ctx, body, header.Get("Content-Encoding"), header.Get(codexTurnMetadataKey), compact)
}

func observeCodexResponseBody(response *http.Response, handle *CodexTurnObservation) {
	if response == nil || response.Body == nil || handle == nil {
		return
	}
	response.Body = &codexObservedBody{body: response.Body, handle: handle}
}

func (body *codexObservedBody) Read(buffer []byte) (int, error) {
	read, err := body.body.Read(buffer)
	if read > 0 {
		body.handle.ObserveBytes(buffer[:read])
	}
	if err != nil {
		body.terminal.Store(true)
		body.finish(err)
	}
	return read, err
}

func (body *codexObservedBody) Close() error {
	err := body.body.Close()
	finishErr := err
	if finishErr == nil && !body.terminal.Load() {
		finishErr = context.Canceled
	}
	body.finish(finishErr)
	return err
}

func (body *codexObservedBody) finish(err error) {
	body.once.Do(func() {
		if errors.Is(err, io.EOF) {
			err = nil
		}
		body.handle.Finish(err)
	})
}

func jsonUnmarshalObject(data []byte, target any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("JSON value is not an object")
	}
	return json.Unmarshal(trimmed, target)
}
