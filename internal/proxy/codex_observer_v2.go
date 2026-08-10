package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// codexV2ObserveBackend records the accepted result of legacy routing in the
// durable v2 shadow journal. It deliberately has no route-planning surface.
type codexV2ObserveBackend struct {
	runtime   *CodexLeaseRuntime
	authority CodexLeaseAuthorityPolicy
}

// NewCodexV2TurnObserver preserves the legacy parser and counters while
// replacing its journal with the durable v2 shadow backend. The returned
// observer never reads either journal to influence legacy route selection.
func NewCodexV2TurnObserver(runtime *CodexLeaseRuntime, authority CodexLeaseAuthorityPolicy) (*CodexTurnObserver, error) {
	if runtime == nil || runtime.coordinator == nil || runtime.store == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if authority.Authoritative {
		return nil, fmt.Errorf("%w: v2 observer requires shadow authority", ErrCodexLeaseAuthorityMismatch)
	}
	if err := validateCodexLeaseAuthorityPolicy(authority); err != nil {
		return nil, err
	}
	shared := runtime.coordinator.leases.ForMode(authority.ModeEpoch, false)
	observer, err := NewCodexTurnObserver(shared, nil)
	if err != nil {
		return nil, err
	}
	observer.Prewarm = runtime.coordinator.prewarms
	observer.v2 = &codexV2ObserveBackend{
		runtime:   runtime,
		authority: cloneCodexLeaseAuthorityPolicy(authority),
	}
	return observer, nil
}

// observeV2RequestHeaders retains request continuity evidence only until the
// v2 runtime hashes it. The legacy observer did not otherwise parse this
// header.
func (handle *CodexTurnObservation) observeV2RequestHeaders(header http.Header) {
	if handle == nil || handle.observer == nil || handle.observer.v2 == nil {
		return
	}
	state, found, err := ParseCodexTurnStateHeader(header)
	handle.mu.Lock()
	defer handle.mu.Unlock()
	handle.request.TurnState = state
	handle.request.HasTurnState = found
	handle.v2RequestErr = err
}

func (handle *CodexTurnObservation) recordV2Actual(choice RouteChoice, attempt CandidateAttempt) {
	if handle == nil || handle.observer == nil || handle.observer.v2 == nil {
		return
	}
	handle.mu.Lock()
	handle.v2ActualChoice = cloneRouteChoice(choice)
	handle.v2ActualAttempt = attempt
	handle.v2ActualFound = true
	handle.mu.Unlock()
}

// PrepareV2Response durably records only the final accepted legacy dispatch.
// The server calls it synchronously before copying any downstream headers.
func (handle *CodexTurnObservation) PrepareV2Response(response *http.Response) error {
	if handle == nil || handle.observer == nil || handle.observer.v2 == nil {
		return nil
	}
	backend := handle.observer.v2
	if handle.observer.Store != nil {
		return errors.New("Codex v2 observer legacy store must be nil")
	}
	if response == nil {
		return errors.New("Codex v2 observer accepted response is nil")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	if response.Body == nil {
		return errors.New("Codex v2 observer accepted response body is nil")
	}

	turnState, hasTurnState, err := ParseCodexTurnStateHeader(response.Header)
	if err != nil {
		return err
	}
	handle.mu.Lock()
	request := handle.request
	key := handle.key
	selected := cloneRouteChoice(handle.choice)
	actualChoice := cloneRouteChoice(handle.v2ActualChoice)
	actual := handle.v2ActualAttempt
	actualFound := handle.v2ActualFound
	requestErr := handle.v2RequestErr
	compact := handle.compact
	handle.mu.Unlock()
	if requestErr != nil {
		return requestErr
	}
	if !request.Metadata.Found || !request.Metadata.Strong {
		return nil
	}
	metadata := request.Metadata.Metadata
	if metadata.RequestKind != CodexRequestTurn && metadata.RequestKind != CodexRequestCompaction {
		return nil
	}
	if err := key.validate(); err != nil {
		return err
	}
	if !actualFound || actual.AccountKey == "" || actual.Candidate.AccountKey == "" || actual.Candidate.CandidateID == "" {
		return errors.New("Codex v2 observer has no final dispatched candidate")
	}
	if selected.AccountKey != actual.AccountKey || actualChoice.AccountKey != actual.AccountKey || actual.Candidate.AccountKey != actual.AccountKey || !equalCodexObserveRouteChoice(selected, actualChoice) {
		return fmt.Errorf("%w: accepted legacy route changed after dispatch", ErrCodexLeaseAuthorityMismatch)
	}
	if actualChoice.RequestedModel == "" || strings.TrimSpace(actualChoice.RequestedModel) != actualChoice.RequestedModel || actualChoice.EffectiveModel == "" || strings.TrimSpace(actualChoice.EffectiveModel) != actualChoice.EffectiveModel || !validCodexLeaseBuckets(actualChoice.RequiredBuckets, actualChoice.EffectiveModel) {
		return fmt.Errorf("%w: accepted legacy route is incomplete", ErrCodexLeaseInvalidMutation)
	}

	plan := CodexLeaseRequestPlan{
		Key:             key,
		Accounts:        []codex.AccountKey{actual.AccountKey},
		Authority:       cloneCodexLeaseAuthorityPolicy(backend.authority),
		RequestKind:     metadata.RequestKind,
		CompactionPhase: metadata.CompactionPhase,
		RequestedModel:  actualChoice.RequestedModel,
		EffectiveModel:  actualChoice.EffectiveModel,
		RequiredBuckets: append([]CapacityBucket(nil), actualChoice.RequiredBuckets...),
		Slots: []CodexLeaseAttemptSlotPlan{{
			AccountKey:  actual.AccountKey,
			CandidateID: string(actual.Candidate.CandidateID),
			Kind:        CodexAttemptSlotDirect,
		}},
		InitialSlot: 1,
		Evidence: CodexLeaseRequestEvidence{
			PreviousResponseID: request.PreviousResponseID,
			TurnState:          request.TurnState,
			HasTurnState:       request.HasTurnState,
			HasEncryptedState:  request.HasEncryptedState,
		},
	}
	ctx := handle.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	prepared, err := backend.runtime.BeginRequestContext(ctx, plan)
	if err != nil {
		return fmt.Errorf("begin Codex v2 shadow observation: %w", err)
	}
	dispatched, err := prepared.MarkDispatchedContext(ctx)
	if err != nil {
		return fmt.Errorf("mark Codex v2 shadow dispatch: %w", err)
	}
	admitted, err := dispatched.AdmitHTTP2xxContext(ctx, CodexHTTPAdmissionEvidence{
		TurnState:    turnState,
		HasTurnState: hasTurnState,
	})
	if err != nil {
		return fmt.Errorf("admit Codex v2 shadow response: %w", err)
	}
	mode := codexHTTPResponseModeSSE
	if compact {
		mode = codexHTTPResponseModeCompact
	}
	response.Body = &codexV2ObservedBody{
		body:     response.Body,
		observer: newCodexHTTPResponseObserver(ctx, mode, NewCodexHTTPRequestLifecycle(admitted)),
	}
	return nil
}

func equalCodexObserveRouteChoice(left, right RouteChoice) bool {
	return left.AccountKey == right.AccountKey &&
		left.RequestedModel == right.RequestedModel &&
		left.EffectiveModel == right.EffectiveModel &&
		slices.Equal(left.RequiredBuckets, right.RequiredBuckets)
}

// codexV2ObservedBody owns the durable terminal and drain transitions. Its
// errors remain visible to the relay even though admission already completed.
type codexV2ObservedBody struct {
	body     io.ReadCloser
	observer *codexHTTPResponseObserver
	terminal atomic.Bool
}

func (body *codexV2ObservedBody) Read(buffer []byte) (int, error) {
	read, readErr := body.body.Read(buffer)
	if read > 0 {
		body.observer.Observe(buffer[:read])
	}
	if readErr == nil {
		return read, nil
	}
	body.terminal.Store(true)
	cause := readErr
	if errors.Is(cause, io.EOF) {
		cause = nil
	}
	finishErr := body.observer.Finish(cause)
	if finishErr != nil {
		return read, errors.Join(cause, finishErr)
	}
	return read, readErr
}

func (body *codexV2ObservedBody) Close() error {
	closeErr := body.body.Close()
	cause := closeErr
	if cause == nil && !body.terminal.Load() {
		cause = context.Canceled
	}
	return errors.Join(closeErr, body.observer.Finish(cause))
}
