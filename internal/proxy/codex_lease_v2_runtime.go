package proxy

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexLeaseAttemptSlotPlan is one frozen account/candidate route in a
// request's durable attempt envelope.
type CodexLeaseAttemptSlotPlan struct {
	AccountKey  codex.AccountKey
	CandidateID string
	Kind        CodexAttemptSlotKind
}

// CodexLeaseRequestEvidence is raw request continuity authority. Values are
// used only for keyed comparison and never enter the durable journal.
type CodexLeaseRequestEvidence struct {
	PreviousResponseID string
	TurnState          string
	HasTurnState       bool
	HasEncryptedState  bool
}

// CodexLeaseBoundExpectation fences a request to one already observed exact
// turn binding. It prevents a read-only retained probe from becoming an
// unseen route after the probe and before BeginRequest.
type CodexLeaseBoundExpectation struct {
	Identity         CodexJournalRecordIdentity
	AccountKey       codex.AccountKey
	RecordGeneration uint64
}

// CodexLeaseRequestPlan is the complete request authority persisted before an
// upstream dispatch. Raw account and candidate values are HMACed before they
// enter the journal.
type CodexLeaseRequestPlan struct {
	Key                       LeaseKey
	Accounts                  []codex.AccountKey
	Authority                 CodexLeaseAuthorityPolicy
	RequestKind               CodexRequestKind
	CompactionPhase           CodexCompactionPhase
	RequestedModel            string
	EffectiveModel            string
	RequiredBuckets           []CapacityBucket
	Slots                     []CodexLeaseAttemptSlotPlan
	InitialSlot               uint32
	Evidence                  CodexLeaseRequestEvidence
	ExpectedBound             *CodexLeaseBoundExpectation
	RequiresAccountContinuity bool
	DispatchPermitDigest      string
}

// CodexLeaseRuntime performs the high-level durable request lifecycle over the
// exact store and shared close/account-gate authority owned by one coordinator.
type CodexLeaseRuntime struct {
	coordinator       *CodexContinuityCoordinator
	store             *CodexLeaseStore
	leases            *CodexTurnLeaseManager
	revalidateAccount CodexLeaseAccountRevalidator
	nativeAdmission   *codexNativeHTTPAdmissionOwner
}

// CodexLeaseAccountRevalidator proves that an account still exists, remains
// routable, and has no pending durable removal while its account gate is held.
// It must honour ctx and must not re-enter this runtime, account removal, or
// coordinator Close.
type CodexLeaseAccountRevalidator func(ctx context.Context, account codex.AccountKey) error

type codexLeaseAccountRevalidationError struct {
	err error
}

func (err *codexLeaseAccountRevalidationError) Error() string {
	return "revalidate Codex lease account"
}

func (err *codexLeaseAccountRevalidationError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.err
}

// CodexLeaseRequestHandle is an immutable, generation-fenced request snapshot.
// Transition methods return a new handle; retained copies become stale after a
// successful transition and cannot mutate later authority.
type CodexLeaseRequestHandle struct {
	runtime      *CodexLeaseRuntime
	newTurn      bool
	key          LeaseKey
	accounts     []codex.AccountKey
	authority    CodexLeaseAuthorityPolicy
	identity     CodexJournalRecordIdentity
	record       CodexJournalRecordV2
	account      codex.AccountKey
	slotAccounts []codex.AccountKey
	fence        CodexLeaseGenerationFence
}

// NewCodexLeaseRuntime binds runtime transitions to a coordinator's exact
// durable store, lifecycle barrier, and per-account exclusion gates. The
// revalidator is mandatory because cached route authority must fail closed
// after a credential removal or routability change.
func NewCodexLeaseRuntime(coordinator *CodexContinuityCoordinator, revalidate CodexLeaseAccountRevalidator) (*CodexLeaseRuntime, error) {
	return newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(coordinator, revalidate, nil)
}

// NewCodexCanaryLeaseRuntime binds first durable authoritative admissions to
// the serving process's active canary. Sink failures cannot undo admission;
// they permanently block promotion for this runtime.
func NewCodexCanaryLeaseRuntime(coordinator *CodexContinuityCoordinator, revalidate CodexLeaseAccountRevalidator, recorder *CodexCanaryRecorder) (*CodexLeaseRuntime, error) {
	if recorder == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	return newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(coordinator, revalidate, codexCanaryNativeHTTPAdmissionSink{recorder: recorder})
}

func newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(
	coordinator *CodexContinuityCoordinator,
	revalidate CodexLeaseAccountRevalidator,
	sink codexNativeHTTPAdmissionSink,
) (*CodexLeaseRuntime, error) {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil || coordinator.leases.mu == nil || coordinator.leases.lifecycle == nil || coordinator.leases.accountGates == nil || revalidate == nil || coordinator.leases.writerUnavailable() {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	return &CodexLeaseRuntime{
		coordinator:       coordinator,
		store:             coordinator.store,
		leases:            coordinator.leases,
		revalidateAccount: revalidate,
		nativeAdmission:   newCodexNativeHTTPAdmissionOwner(sink),
	}, nil
}

func (runtime *CodexLeaseRuntime) nativeHTTPAdmissionPromotionBlocked() bool {
	return runtime != nil && runtime.nativeAdmission.promotionBlocked()
}

func (runtime *CodexLeaseRuntime) BeginRequest(plan CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error) {
	return runtime.BeginRequestContext(context.Background(), plan)
}

// BeginRequestContext durably reserves an unseen turn, then atomically installs
// one store-owned request generation with its first prepared attempt.
func (runtime *CodexLeaseRuntime) BeginRequestContext(ctx context.Context, plan CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil request context", ErrCodexLeaseInvalidMutation)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan, err := runtime.validateAndClonePlan(plan)
	if err != nil {
		return nil, err
	}
	selected := plan.Slots[plan.InitialSlot-1]
	release, err := runtime.beginAccountMutation(ctx, selected.AccountKey)
	if err != nil {
		return nil, err
	}
	defer release()

	restored, err := runtime.store.LoadLane(plan.Key, plan.Accounts, plan.Authority)
	if err != nil {
		return nil, err
	}
	newTurn := restored.Classification == CodexRestoredLaneUnseen
	requestIdentity := codexLeaseRuntimeRequestIdentity(restored)
	if err := runtime.validateExpectedBound(restored, plan, selected.AccountKey); err != nil {
		return nil, err
	}
	if restored.Classification == CodexRestoredLaneHistorical {
		return nil, ErrCodexStaleTurn
	}
	restoredRequiresAccount, err := runtime.validateRequestContinuity(restored, requestIdentity, selected.AccountKey, plan.Evidence)
	if err != nil {
		return nil, err
	}
	requiresAccountContinuity := plan.RequiresAccountContinuity || restoredRequiresAccount
	if requiresAccountContinuity {
		for _, slot := range plan.Slots {
			if slot.AccountKey != selected.AccountKey {
				return nil, fmt.Errorf("%w: hard continuity plan contains an alternate account", ErrCodexLeaseInvalidMutation)
			}
		}
	}
	if plan.RequestKind == CodexRequestCompaction && plan.CompactionPhase == CodexCompactionMidTurn {
		current, ok := runtime.restoredRecord(restored, requestIdentity)
		if restored.Classification != CodexRestoredLaneCurrent || restored.Fence.Current != requestIdentity || !ok || !current.Record.EverAdmitted {
			return nil, fmt.Errorf("%w: mid-turn compaction requires admitted turn authority", ErrCodexLeaseAuthorityMismatch)
		}
	}
	revalidated := false
	switch {
	case restored.Classification == CodexRestoredLaneHistorical:
		return nil, ErrCodexStaleTurn
	case restored.Classification == CodexRestoredLaneUnseen && restored.Fence.Lane == 0:
		if err := runtime.revalidateAccountForCommit(ctx, selected.AccountKey); err != nil {
			return nil, err
		}
		revalidated = true
		if err := runtime.reserveUnseen(restored); err != nil {
			return nil, err
		}
	case restored.Classification == CodexRestoredLaneUnseen:
		if err := runtime.reserveSuccessor(ctx, selected.AccountKey, restored); err != nil {
			return nil, err
		}
		revalidated = true
	default:
		break
	}
	if revalidated {
		restored, err = runtime.store.LoadLane(plan.Key, plan.Accounts, plan.Authority)
		if err != nil {
			return nil, err
		}
		requestIdentity = codexLeaseRuntimeRequestIdentity(restored)
	}
	if restored.Classification != CodexRestoredLaneCurrent || restored.Fence.Current != requestIdentity {
		return nil, ErrCodexConcurrentTurn
	}
	current, ok := runtime.restoredRecord(restored, requestIdentity)
	if !ok {
		return nil, fmt.Errorf("%w: current runtime record is absent", ErrCodexLeaseTrustLost)
	}
	if !codexLeaseRuntimeCanBeginRequest(current.Record) {
		return nil, ErrCodexConcurrentTurn
	}

	desired := codexLeaseRuntimeMutationRecord(current.Record)
	desired.State = LeaseProvisional
	if current.Record.EverAdmitted {
		desired.State = LeaseBoundActive
	}
	desired.AccountHash = runtime.store.hash("account", string(selected.AccountKey))
	desired.CodexCurrentRequest = runtime.requestAfterImage(plan)
	if requiresAccountContinuity {
		desired.NonMigratable = true
	}
	if plan.Evidence.HasEncryptedState {
		desired.HasEncryptedState = true
	}
	desired.SocketLineageExtinct = false
	fence, err := runtime.mutationFence(restored, current.Record)
	if err != nil {
		return nil, err
	}
	for index := range fence.TouchedRecords {
		fence.TouchedRecords[index].TouchedAttempts = nil
	}
	currentFence, ok := codexLeaseRuntimeRecordFence(&fence, requestIdentity)
	if !ok {
		return nil, fmt.Errorf("%w: current request fence is absent", ErrCodexLeaseTrustLost)
	}
	currentFence.TouchedAttempts = []CodexAttemptFence{{}}
	if !revalidated {
		if err := runtime.revalidateAccountForCommit(ctx, selected.AccountKey); err != nil {
			return nil, err
		}
	}
	identity := requestIdentity
	var committedRecord CodexJournalRecordV2
	post, err := runtime.store.commitLane(fence, CodexLaneMutation{
		BeginRequest:  &identity,
		UpsertRecords: []CodexJournalRecordV2{desired},
	}, func(_ CodexLeaseGenerationFence, installed codexLeaseJournalEnvelopeV2) {
		for _, record := range installed.Records {
			if record.Identity() == identity {
				committedRecord = cloneCodexJournalRecordV2(record)
				return
			}
		}
	})
	if err != nil {
		return nil, err
	}
	handle, err := runtime.committedRequestHandle(plan.Key, plan.Accounts, plan.Authority, identity, post, committedRecord, true, 1)
	if err != nil {
		return nil, err
	}
	handle.newTurn = newTurn
	return handle, nil
}

func (runtime *CodexLeaseRuntime) adoptWebSocketPrewarmContext(ctx context.Context, accounts []codex.AccountKey, request CodexPrewarmAdoptionRequest) (*CodexLeaseRequestHandle, error) {
	if runtime == nil || runtime.coordinator == nil || runtime.store == nil || runtime.revalidateAccount == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil prewarm adoption context", ErrCodexLeaseInvalidMutation)
	}
	callerRevalidate := request.Revalidate
	if callerRevalidate == nil {
		return nil, fmt.Errorf("%w: missing prewarm socket revalidation", ErrCodexLeaseInvalidMutation)
	}
	request.Revalidate = func(ctx context.Context, account codex.AccountKey, fence CodexPrewarmAdoptionFence) error {
		if err := runtime.revalidateAccountForCommit(ctx, account); err != nil {
			return err
		}
		return callerRevalidate(ctx, account, fence)
	}
	result, err := runtime.coordinator.AdoptPrewarm(ctx, request)
	if err != nil {
		return nil, err
	}
	handle, err := runtime.committedRequestHandle(request.Key, accounts, request.Policy, result.Record.Identity(), result.Fence, result.Record, true, 1)
	if err != nil {
		return nil, err
	}
	handle.newTurn = true
	return handle, nil
}

func (handle *CodexLeaseRequestHandle) State() LeaseState {
	if handle == nil {
		return 0
	}
	return handle.record.State
}

func (handle *CodexLeaseRequestHandle) EverAdmitted() bool {
	return handle != nil && handle.record.EverAdmitted
}

func (handle *CodexLeaseRequestHandle) AccountKey() codex.AccountKey {
	if handle == nil {
		return ""
	}
	return handle.account
}

func (handle *CodexLeaseRequestHandle) RequestGeneration() uint64 {
	if handle == nil {
		return 0
	}
	return handle.record.Generation
}

func (handle *CodexLeaseRequestHandle) AttemptGeneration() uint64 {
	if handle == nil {
		return 0
	}
	return handle.record.CurrentAttemptGeneration
}

// MarkDispatched durably crosses the last pre-send fence.
func (handle *CodexLeaseRequestHandle) MarkDispatched() (*CodexLeaseRequestHandle, error) {
	return handle.MarkDispatchedContext(context.Background())
}

func (handle *CodexLeaseRequestHandle) MarkDispatchedContext(ctx context.Context) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil || handle.account == "" {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil dispatch context", ErrCodexLeaseInvalidMutation)
	}
	release, err := handle.runtime.beginAccountMutation(ctx, handle.account)
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	if err := handle.runtime.revalidateAccountForCommit(ctx, handle.account); err != nil {
		return nil, err
	}
	return handle.transitionAttemptWithFence(fence, CodexAttemptPrepared, CodexAttemptDispatched, nil)
}

func (handle *CodexLeaseRequestHandle) markWebSocketReplacementDispatchedContext(ctx context.Context, downstreamGeneration, upstreamGeneration uint64) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil || handle.account == "" {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil WebSocket replacement context", ErrCodexLeaseInvalidMutation)
	}
	if downstreamGeneration == 0 || upstreamGeneration == 0 || !handle.record.AdoptedPrewarm ||
		handle.record.DownstreamSocketGeneration != downstreamGeneration || handle.record.UpstreamSocketGeneration == 0 ||
		handle.record.UpstreamSocketGeneration == upstreamGeneration {
		return nil, ErrCodexWSStaleGeneration
	}
	release, err := handle.runtime.beginAccountMutation(ctx, handle.account)
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	if err := handle.runtime.revalidateAccountForCommit(ctx, handle.account); err != nil {
		return nil, err
	}
	return handle.transitionAttemptWithFence(fence, CodexAttemptPrepared, CodexAttemptDispatched, func(desired *CodexJournalRecordV2) error {
		desired.UpstreamSocketGeneration = upstreamGeneration
		desired.SocketLineageExtinct = false
		return nil
	})
}

// AbandonBeforeDispatch records that a prepared request was cancelled before
// any upstream send could occur. It releases every request reference so a
// later explicit BeginRequest can safely replace the abandoned generation.
func (handle *CodexLeaseRequestHandle) AbandonBeforeDispatch() (*CodexLeaseRequestHandle, error) {
	return handle.AbandonBeforeDispatchContext(context.Background())
}

func (handle *CodexLeaseRequestHandle) AbandonBeforeDispatchContext(ctx context.Context) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil abandonment context", ErrCodexLeaseInvalidMutation)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, err := handle.runtime.beginLifecycleMutationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || current.State != CodexAttemptPrepared || (handle.record.State != LeaseProvisional && handle.record.State != LeaseBoundActive) {
		return nil, ErrCodexLeaseTransition
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	for index := range desired.Attempts {
		if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
			desired.Attempts[index].State = CodexAttemptAbandonedBeforeDispatch
			break
		}
	}
	if handle.record.EverAdmitted {
		desired.State = LeaseOrphaned
	} else {
		desired.State = LeaseProvisional
	}
	desired.RoutingRefs = 0
	desired.AttemptRefs = 0
	desired.ResponseObserverRefs = 0
	desired.SocketLineageExtinct = true
	return handle.commitRequestMutation(fence, desired, true)
}

// RejectAndPrepare atomically records a definite pre-admission failure and
// prepares one explicit unused slot from the frozen request envelope.
func (handle *CodexLeaseRequestHandle) RejectAndPrepare(nextSlot uint32) (*CodexLeaseRequestHandle, error) {
	return handle.RejectAndPrepareContext(context.Background(), nextSlot)
}

func (handle *CodexLeaseRequestHandle) RejectAndPrepareContext(ctx context.Context, nextSlot uint32) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil retry context", ErrCodexLeaseInvalidMutation)
	}
	if nextSlot == 0 || int(nextSlot) > len(handle.record.AttemptEnvelope.Slots) {
		return nil, fmt.Errorf("%w: retry slot is outside the frozen envelope", ErrCodexLeaseInvalidMutation)
	}
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || current.State != CodexAttemptDispatched || handle.record.State != LeaseProvisional || handle.record.EverAdmitted || handle.record.NonMigratable {
		return nil, ErrCodexLeaseTransition
	}
	if int(nextSlot) > len(handle.slotAccounts) || handle.slotAccounts[nextSlot-1] == "" {
		return nil, fmt.Errorf("%w: retry slot account is unavailable", ErrCodexLeaseAuthorityMismatch)
	}
	for _, attempt := range handle.record.Attempts {
		if attempt.Slot == nextSlot {
			return nil, fmt.Errorf("%w: retry slot is already used", ErrCodexLeaseInvalidMutation)
		}
	}
	nextAccount := handle.slotAccounts[nextSlot-1]
	manager := handle.runtime.leases
	if manager == nil || manager.accountGates == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	guard, err := manager.accountGates.acquire(ctx, nextAccount)
	if err != nil {
		return nil, err
	}
	defer guard.Release()
	release, err := handle.runtime.beginLifecycleMutationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}

	desired := codexLeaseRuntimeMutationRecord(handle.record)
	for index := range desired.Attempts {
		if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
			desired.Attempts[index].State = CodexAttemptProviderFailed
			break
		}
	}
	desired.Attempts = append(desired.Attempts, CodexJournalAttempt{Slot: nextSlot, State: CodexAttemptPrepared})
	desired.CurrentAttemptGeneration = 0
	desired.AccountHash = handle.record.AttemptEnvelope.Slots[nextSlot-1].AccountHash
	currentFence, ok := codexLeaseRuntimeRecordFence(&fence, handle.identity)
	if !ok {
		return nil, fmt.Errorf("%w: current retry fence is absent", ErrCodexLeaseTrustLost)
	}
	currentFence.TouchedAttempts = append(currentFence.TouchedAttempts, CodexAttemptFence{
		RequestGeneration: handle.record.Generation,
	})
	if err := handle.runtime.revalidateAccountForCommit(ctx, nextAccount); err != nil {
		return nil, err
	}
	return handle.commitRequestMutation(fence, desired, true)
}

// FinishRejected records a final definite provider rejection. A never-admitted
// request becomes a failed tombstone; an already-admitted turn remains bound
// and quiescent on its immutable account.
func (handle *CodexLeaseRequestHandle) FinishRejected() (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	release, err := handle.runtime.beginLifecycleMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || current.State != CodexAttemptDispatched {
		return nil, ErrCodexLeaseTransition
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	for index := range desired.Attempts {
		if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
			desired.Attempts[index].State = CodexAttemptProviderFailed
			break
		}
	}
	if handle.record.EverAdmitted {
		desired.State = LeaseBoundQuiescent
	} else {
		if handle.record.State != LeaseProvisional {
			return nil, ErrCodexLeaseTransition
		}
		desired.State = LeaseFailedUnadmitted
	}
	desired.RoutingRefs = 0
	desired.AttemptRefs = 0
	desired.ResponseObserverRefs = 0
	desired.SocketLineageExtinct = true
	return handle.commitRequestMutation(fence, desired, handle.record.EverAdmitted)
}

// Indeterminate records that an upstream send may have occurred. The request
// keeps one lifecycle-observer reference until Drain, even when no response
// headers were accepted, so a newer request cannot race the terminal callback.
func (handle *CodexLeaseRequestHandle) Indeterminate() (*CodexLeaseRequestHandle, error) {
	return handle.IndeterminateContext(context.Background(), CodexHTTPResponseEvidence{})
}

func (handle *CodexLeaseRequestHandle) IndeterminateContext(ctx context.Context, evidence CodexHTTPResponseEvidence) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil indeterminate context", ErrCodexLeaseInvalidMutation)
	}
	var (
		release func()
		err     error
	)
	if handle.record.EverAdmitted {
		release, err = handle.runtime.beginLifecycleMutationContext(ctx)
	} else {
		release, err = handle.runtime.beginAccountMutation(ctx, handle.account)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || (current.State != CodexAttemptDispatched && current.State != CodexAttemptStreaming) {
		return nil, ErrCodexLeaseTransition
	}
	if !handle.record.EverAdmitted {
		if err := handle.runtime.revalidateAccountForCommit(ctx, handle.account); err != nil {
			return nil, err
		}
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	if err := handle.applyResponseEvidence(&desired, evidence); err != nil {
		return nil, err
	}
	for index := range desired.Attempts {
		if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
			desired.Attempts[index].State = CodexAttemptIndeterminate
			break
		}
	}
	desired.State = LeaseOrphaned
	desired.RoutingRefs = 0
	desired.AttemptRefs = 0
	if desired.ResponseObserverRefs == 0 {
		desired.ResponseObserverRefs = 1
	}
	desired.SocketLineageExtinct = false
	return handle.commitRequestMutation(fence, desired, true)
}

// AdmitHTTP2xx atomically persists streaming plus first-admission evidence
// while holding the same account gate used by credential removal.
func (handle *CodexLeaseRequestHandle) AdmitHTTP2xx() (*CodexLeaseRequestHandle, error) {
	return handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{})
}

func (handle *CodexLeaseRequestHandle) AdmitHTTP2xxContext(ctx context.Context, evidence CodexHTTPAdmissionEvidence) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil || handle.account == "" {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil admission context", ErrCodexLeaseInvalidMutation)
	}
	var admitted *CodexLeaseRequestHandle
	var firstAdmission bool
	err := func() error {
		release, err := handle.runtime.beginAccountMutation(ctx, handle.account)
		if err != nil {
			return err
		}
		defer release()
		fence, err := handle.refreshMutationFence()
		if err != nil {
			return err
		}
		if err := handle.runtime.revalidateAccountForCommit(ctx, handle.account); err != nil {
			return err
		}
		admitted, firstAdmission, err = handle.admitHTTP2xxWithFence(fence, evidence)
		return err
	}()
	if err == nil && admitted != nil && firstAdmission {
		handle.runtime.nativeAdmission.observe(codexNativeHTTPAdmissionObservation{
			RequestGeneration: admitted.RequestGeneration(),
			AttemptGeneration: admitted.AttemptGeneration(),
		})
	}
	if err != nil {
		return nil, err
	}
	return admitted, nil
}

// AdmitWebSocketContext atomically persists provider admission and the exact
// downstream/upstream socket generation that produced it. A downstream 101 is
// not evidence: callers must supply either provider turn state or a validated
// response.created event.
func (handle *CodexLeaseRequestHandle) AdmitWebSocketContext(ctx context.Context, evidence CodexWebSocketAdmissionEvidence) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil || handle.account == "" {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil WebSocket admission context", ErrCodexLeaseInvalidMutation)
	}
	if err := evidence.validate(); err != nil {
		return nil, err
	}
	release, err := handle.runtime.beginAccountMutation(ctx, handle.account)
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	if err := handle.runtime.revalidateAccountForCommit(ctx, handle.account); err != nil {
		return nil, err
	}
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || (current.State != CodexAttemptDispatched && current.State != CodexAttemptStreaming) {
		return nil, ErrCodexLeaseTransition
	}
	firstAttemptAdmission := current.State == CodexAttemptDispatched
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	if (desired.DownstreamSocketGeneration != 0 && desired.DownstreamSocketGeneration != evidence.DownstreamGeneration) ||
		(desired.UpstreamSocketGeneration != 0 && desired.UpstreamSocketGeneration != evidence.UpstreamGeneration) {
		return nil, ErrCodexWSStaleGeneration
	}
	if firstAttemptAdmission {
		for index := range desired.Attempts {
			if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
				desired.Attempts[index].State = CodexAttemptStreaming
				break
			}
		}
	}
	if err := handle.applyAdmissionEvidence(&desired, CodexHTTPAdmissionEvidence{
		TurnState: evidence.TurnState, HasTurnState: evidence.HasTurnState,
	}); err != nil {
		return nil, err
	}
	if evidence.ResponseID != "" {
		if err := handle.applyResponseEvidence(&desired, CodexHTTPResponseEvidence{
			ResponseAnchor: evidence.ResponseID, HasResponseAnchor: true,
		}); err != nil {
			return nil, err
		}
	}
	desired.DownstreamSocketGeneration = evidence.DownstreamGeneration
	desired.UpstreamSocketGeneration = evidence.UpstreamGeneration
	desired.SocketLineageExtinct = false
	desired.State = LeaseBoundActive
	if firstAttemptAdmission {
		desired.AttemptRefs++
		desired.ResponseObserverRefs++
	}
	return handle.commitRequestMutation(fence, desired, true)
}

func (handle *CodexLeaseRequestHandle) admitHTTP2xxWithFence(fence CodexLeaseGenerationFence, evidence CodexHTTPAdmissionEvidence) (*CodexLeaseRequestHandle, bool, error) {
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || current.State != CodexAttemptDispatched {
		return nil, false, ErrCodexLeaseTransition
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	for index := range desired.Attempts {
		if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
			desired.Attempts[index].State = CodexAttemptStreaming
			break
		}
	}
	if err := func(record *CodexJournalRecordV2) error {
		if record.State != LeaseProvisional && record.State != LeaseBoundActive {
			return ErrCodexLeaseTransition
		}
		if err := handle.applyAdmissionEvidence(record, evidence); err != nil {
			return err
		}
		record.State = LeaseBoundActive
		record.AttemptRefs++
		record.ResponseObserverRefs++
		return nil
	}(&desired); err != nil {
		return nil, false, err
	}
	firstAdmission := !handle.record.EverAdmitted && desired.Authoritative && codexLeaseCurrentRequestCacheEligible(desired)
	admitted, err := handle.commitRequestMutation(fence, desired, true)
	return admitted, firstAdmission, err
}

// ProviderCompleted records the terminal provider event while keeping its
// response observer reference live until Drain.
func (handle *CodexLeaseRequestHandle) ProviderCompleted(evidence CodexHTTPCompletionEvidence) (*CodexLeaseRequestHandle, error) {
	return handle.transitionAttempt(CodexAttemptStreaming, CodexAttemptProviderCompleted, func(record *CodexJournalRecordV2) error {
		if record.State != LeaseBoundActive || record.ResponseObserverRefs <= 0 {
			return ErrCodexLeaseTransition
		}
		if err := handle.applyResponseEvidence(record, evidence.CodexHTTPResponseEvidence); err != nil {
			return err
		}
		record.RoutingRefs = 0
		record.AttemptRefs = 0
		if evidence.EndTurn {
			record.State = LeaseBoundQuiescent
		} else {
			record.State = LeaseContinuationPending
		}
		return nil
	})
}

// ProviderFailed records a terminal provider failure after admission. The
// observer reference remains live until Drain records callback completion.
func (handle *CodexLeaseRequestHandle) ProviderFailed(evidence CodexHTTPResponseEvidence) (*CodexLeaseRequestHandle, error) {
	return handle.transitionAttempt(CodexAttemptStreaming, CodexAttemptProviderFailed, func(record *CodexJournalRecordV2) error {
		if record.State != LeaseBoundActive || record.ResponseObserverRefs <= 0 {
			return ErrCodexLeaseTransition
		}
		if err := handle.applyResponseEvidence(record, evidence); err != nil {
			return err
		}
		record.State = LeaseBoundQuiescent
		record.RoutingRefs = 0
		record.AttemptRefs = 0
		return nil
	})
}

// Drain releases the request's final response-observer reference. A later
// explicit BeginRequest remains blocked until every persisted reference is zero.
func (handle *CodexLeaseRequestHandle) Drain() (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	release, err := handle.runtime.beginLifecycleMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	if !codexLeaseAttemptTerminalForRequest(codexLeaseCurrentAttemptState(handle.record)) || handle.record.ResponseObserverRefs <= 0 {
		return nil, ErrCodexLeaseTransition
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	desired.ResponseObserverRefs--
	if desired.RoutingRefs == 0 && desired.AttemptRefs == 0 && desired.ResponseObserverRefs == 0 {
		desired.SocketLineageExtinct = true
	}
	currentFence, ok := codexLeaseRuntimeRecordFence(&fence, handle.identity)
	if !ok {
		return nil, fmt.Errorf("%w: current drain fence is absent", ErrCodexLeaseTrustLost)
	}
	currentFence.TouchedAttempts = nil
	return handle.commitRequestMutation(fence, desired, true)
}

func (handle *CodexLeaseRequestHandle) transitionAttempt(from, to CodexAttemptState, mutate func(*CodexJournalRecordV2) error) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	release, err := handle.runtime.beginLifecycleMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	return handle.transitionAttemptWithinLifecycle(from, to, mutate)
}

func (handle *CodexLeaseRequestHandle) transitionAttemptWithinLifecycle(from, to CodexAttemptState, mutate func(*CodexJournalRecordV2) error) (*CodexLeaseRequestHandle, error) {
	fence, err := handle.refreshMutationFence()
	if err != nil {
		return nil, err
	}
	return handle.transitionAttemptWithFence(fence, from, to, mutate)
}

func (handle *CodexLeaseRequestHandle) transitionAttemptWithFence(fence CodexLeaseGenerationFence, from, to CodexAttemptState, mutate func(*CodexJournalRecordV2) error) (*CodexLeaseRequestHandle, error) {
	current, ok := codexLeaseAttemptByGeneration(handle.record.Attempts, handle.record.CurrentAttemptGeneration)
	if !ok || current.State != from {
		return nil, ErrCodexLeaseTransition
	}
	desired := codexLeaseRuntimeMutationRecord(handle.record)
	for index := range desired.Attempts {
		if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
			desired.Attempts[index].State = to
			break
		}
	}
	if mutate != nil {
		if err := mutate(&desired); err != nil {
			return nil, err
		}
	}
	return handle.commitRequestMutation(fence, desired, true)
}

func (handle *CodexLeaseRequestHandle) commitRequestMutation(fence CodexLeaseGenerationFence, desired CodexJournalRecordV2, requireCurrent bool) (*CodexLeaseRequestHandle, error) {
	if handle == nil || handle.runtime == nil || handle.runtime.store == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	identity := handle.identity
	recordFence, found := codexLeaseRuntimeRecordFence(&fence, identity)
	if !found {
		return nil, fmt.Errorf("%w: request mutation fence is absent", ErrCodexLeaseTrustLost)
	}
	expectedTouchedAttempts := len(recordFence.TouchedAttempts)
	var committedRecord CodexJournalRecordV2
	post, err := handle.runtime.store.commitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}}, func(_ CodexLeaseGenerationFence, installed codexLeaseJournalEnvelopeV2) {
		for _, record := range installed.Records {
			if record.Identity() == identity {
				committedRecord = cloneCodexJournalRecordV2(record)
				return
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return handle.runtime.committedRequestHandle(handle.key, handle.accounts, handle.authority, identity, post, committedRecord, requireCurrent, expectedTouchedAttempts)
}

func (handle *CodexLeaseRequestHandle) refreshMutationFence() (CodexLeaseGenerationFence, error) {
	if handle == nil || handle.runtime == nil || len(handle.fence.TouchedRecords) == 0 {
		return CodexLeaseGenerationFence{}, ErrCodexLeaseWriterUnavailable
	}
	restored, err := handle.runtime.store.LoadLane(handle.key, handle.accounts, handle.authority)
	if err != nil {
		return CodexLeaseGenerationFence{}, err
	}
	if restored.Classification != CodexRestoredLaneCurrent || restored.Fence.Current != handle.identity {
		if restored.Fence.Current.IsZero() && restored.Fence.Last == handle.identity {
			return CodexLeaseGenerationFence{}, staleCodexLeaseMutation("lane_current", "runtime request is no longer current")
		}
		return CodexLeaseGenerationFence{}, ErrCodexStaleTurn
	}
	current, ok := handle.runtime.restoredRecord(restored, handle.identity)
	if !ok {
		return CodexLeaseGenerationFence{}, fmt.Errorf("%w: current runtime record is absent", ErrCodexLeaseTrustLost)
	}
	fresh, err := handle.runtime.mutationFence(restored, current.Record)
	if err != nil {
		return CodexLeaseGenerationFence{}, err
	}
	if len(fresh.TouchedRecords) != len(handle.fence.TouchedRecords) {
		return CodexLeaseGenerationFence{}, staleCodexLeaseMutation("record_set", "runtime predecessor authority changed")
	}
	predecessor := CodexJournalRecordIdentity{}
	if handle.record.PredecessorTurnHash != "" {
		predecessor = CodexJournalRecordIdentity{
			LaneDigest:    handle.identity.LaneDigest,
			TurnDigest:    handle.record.PredecessorTurnHash,
			ModeEpoch:     handle.record.PredecessorModeEpoch,
			Authoritative: handle.record.PredecessorAuthoritative,
		}
	}
	for _, cachedRecord := range handle.fence.TouchedRecords {
		freshRecord, found := codexLeaseRuntimeRecordFence(&fresh, cachedRecord.Record)
		if !found {
			return CodexLeaseGenerationFence{}, staleCodexLeaseMutation("record_revision", "runtime handle no longer matches retained record authority")
		}
		if cachedRecord.Revision != freshRecord.Revision || cachedRecord.Lease != freshRecord.Lease || cachedRecord.RequestGeneration != freshRecord.RequestGeneration || cachedRecord.CurrentAttempt != freshRecord.CurrentAttempt {
			_, predecessorStillPresent := handle.runtime.restoredRecord(restored, cachedRecord.Record)
			retentionDeletedPredecessor := !predecessor.IsZero() && cachedRecord.Record == predecessor && !predecessorStillPresent && cachedRecord.Revision == handle.record.PredecessorGeneration && len(cachedRecord.TouchedAttempts) == 0 && freshRecord.Revision == 0 && freshRecord.Lease == 0 && freshRecord.RequestGeneration == 0 && freshRecord.CurrentAttempt == 0 && len(freshRecord.TouchedAttempts) == 0
			if retentionDeletedPredecessor {
				continue
			}
			return CodexLeaseGenerationFence{}, staleCodexLeaseMutation("record_revision", "runtime handle no longer matches retained record authority")
		}
		touched := make([]CodexAttemptFence, 0, len(cachedRecord.TouchedAttempts))
		for _, cachedAttempt := range cachedRecord.TouchedAttempts {
			matched := false
			for _, freshAttempt := range freshRecord.TouchedAttempts {
				if freshAttempt.RequestGeneration == cachedAttempt.RequestGeneration && freshAttempt.Generation == cachedAttempt.Generation {
					if freshAttempt.Revision != cachedAttempt.Revision {
						return CodexLeaseGenerationFence{}, staleCodexLeaseMutation("attempt_revision", "runtime handle no longer matches the current attempt")
					}
					touched = append(touched, freshAttempt)
					matched = true
					break
				}
			}
			if !matched {
				return CodexLeaseGenerationFence{}, staleCodexLeaseMutation("attempt_generation", "runtime attempt is no longer retained")
			}
		}
		freshRecord.TouchedAttempts = touched
	}
	return fresh, nil
}

func (runtime *CodexLeaseRuntime) beginLifecycleMutation() (func(), error) {
	return runtime.beginLifecycleMutationContext(context.Background())
}

func (runtime *CodexLeaseRuntime) beginLifecycleMutationContext(ctx context.Context) (func(), error) {
	if runtime == nil || runtime.coordinator == nil || runtime.store == nil || runtime.leases == nil || runtime.leases.lifecycle == nil || runtime.leases.mu == nil || runtime.coordinator.store != runtime.store || runtime.coordinator.leases != runtime.leases {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil lifecycle context", ErrCodexLeaseInvalidMutation)
	}
	lifecycle := runtime.leases.lifecycle
	for !lifecycle.persistence.TryLock() {
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		lifecycle.persistence.Unlock()
		return nil, err
	}
	runtime.leases.mu.Lock()
	unavailable := runtime.leases.writerUnavailableLocked()
	runtime.leases.mu.Unlock()
	if unavailable {
		lifecycle.persistence.Unlock()
		return nil, ErrCodexLeaseWriterUnavailable
	}
	return lifecycle.persistence.Unlock, nil
}

func (runtime *CodexLeaseRuntime) beginAccountMutation(ctx context.Context, account codex.AccountKey) (func(), error) {
	if runtime == nil || runtime.leases == nil || runtime.leases.accountGates == nil || account == "" {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	guard, err := runtime.leases.accountGates.acquire(ctx, account)
	if err != nil {
		return nil, err
	}
	releaseLifecycle, err := runtime.beginLifecycleMutationContext(ctx)
	if err != nil {
		guard.Release()
		return nil, err
	}
	return func() {
		releaseLifecycle()
		guard.Release()
	}, nil
}

func (runtime *CodexLeaseRuntime) revalidateAccountForCommit(ctx context.Context, account codex.AccountKey) error {
	if runtime == nil || runtime.revalidateAccount == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := runtime.revalidateAccount(ctx, account); err != nil {
		return &codexLeaseAccountRevalidationError{err: err}
	}
	return ctx.Err()
}

func (runtime *CodexLeaseRuntime) reserveUnseen(restored CodexRestoredLane) error {
	record := cloneCodexJournalRecordV2(restored.RequestedRecord)
	record.State = LeaseReserving
	lane := &CodexJournalLane{
		SessionHash:          record.SessionHash,
		ThreadHash:           record.ThreadHash,
		NamespaceHash:        record.NamespaceHash,
		CurrentTurnHash:      record.TurnHash,
		CurrentModeEpoch:     record.ModeEpoch,
		CurrentAuthoritative: record.Authoritative,
		LastTurnHash:         record.TurnHash,
		LastModeEpoch:        record.ModeEpoch,
		LastAuthoritative:    record.Authoritative,
	}
	fence, err := restored.MutationFence(restored.RequestedIdentity)
	if err != nil {
		return err
	}
	_, err = runtime.store.CommitLane(fence, CodexLaneMutation{Lane: lane, UpsertRecords: []CodexJournalRecordV2{record}})
	return err
}

func (runtime *CodexLeaseRuntime) reserveSuccessor(ctx context.Context, account codex.AccountKey, restored CodexRestoredLane) error {
	if restored.Fence.Last.IsZero() || restored.Fence.Last == restored.RequestedIdentity {
		return ErrCodexConcurrentTurn
	}
	predecessor, ok := runtime.restoredRecord(restored, restored.Fence.Last)
	if !ok {
		return fmt.Errorf("%w: successor predecessor is absent", ErrCodexLeaseTrustLost)
	}
	if predecessor.Record.RoutingRefs != 0 || predecessor.Record.AttemptRefs != 0 || predecessor.Record.ResponseObserverRefs != 0 {
		return ErrCodexConcurrentTurn
	}
	predecessorCurrent := restored.Fence.Current
	upserts := make([]CodexJournalRecordV2, 0, 2)
	switch {
	case predecessorCurrent.IsZero():
		if predecessor.Record.State != LeaseFailedUnadmitted && predecessor.Record.State != LeaseSuperseded && predecessor.Record.State != LeaseExpired {
			return ErrCodexConcurrentTurn
		}
		if predecessor.Record.Generation != 0 && !codexLeaseAttemptTerminalForRequest(codexLeaseCurrentAttemptState(predecessor.Record)) {
			return ErrCodexConcurrentTurn
		}
	case predecessorCurrent == restored.Fence.Last:
		if (predecessor.Record.State != LeaseContinuationPending && predecessor.Record.State != LeaseBoundQuiescent && predecessor.Record.State != LeaseOrphaned) || !codexLeaseAttemptTerminalForRequest(codexLeaseCurrentAttemptState(predecessor.Record)) || !predecessor.Record.SocketLineageExtinct {
			return ErrCodexConcurrentTurn
		}
		superseded := codexLeaseRuntimeMutationRecord(predecessor.Record)
		superseded.State = LeaseSuperseded
		upserts = append(upserts, superseded)
	default:
		return fmt.Errorf("%w: lane current and last authority diverged", ErrCodexLeaseTrustLost)
	}

	successor := cloneCodexJournalRecordV2(restored.RequestedRecord)
	successor.State = LeaseReserving
	successor.PredecessorTurnHash = restored.Fence.Last.TurnDigest
	successor.PredecessorModeEpoch = restored.Fence.Last.ModeEpoch
	successor.PredecessorAuthoritative = restored.Fence.Last.Authoritative
	upserts = append(upserts, successor)
	lane := restored.Lane
	lane.Generation = 0
	lane.CurrentTurnHash = restored.RequestedIdentity.TurnDigest
	lane.CurrentModeEpoch = restored.RequestedIdentity.ModeEpoch
	lane.CurrentAuthoritative = restored.RequestedIdentity.Authoritative
	lane.LastTurnHash = restored.RequestedIdentity.TurnDigest
	lane.LastModeEpoch = restored.RequestedIdentity.ModeEpoch
	lane.LastAuthoritative = restored.RequestedIdentity.Authoritative
	lane.LastAdmittedAccountHash = ""
	lane.LastAdmittedTurnHash = ""
	lane.LastAdmittedModeEpoch = 0
	lane.LastAdmittedAuthoritative = false
	lane.LastAdmissionJournalGeneration = 0
	lane.LastAdmittedAt = time.Time{}
	lane.LastCacheAdmittedAt = time.Time{}
	lane.LastCacheEffectiveModel = ""
	lane.LastObservedAt = time.Time{}
	fenceIdentities := []CodexJournalRecordIdentity{restored.Fence.Last, restored.RequestedIdentity}
	if predecessorCurrent == restored.Fence.Last && predecessor.Record.PredecessorTurnHash != "" {
		fenceIdentities = append(fenceIdentities, CodexJournalRecordIdentity{
			LaneDigest:    restored.Fence.Last.LaneDigest,
			TurnDigest:    predecessor.Record.PredecessorTurnHash,
			ModeEpoch:     predecessor.Record.PredecessorModeEpoch,
			Authoritative: predecessor.Record.PredecessorAuthoritative,
		})
	}
	fence, err := restored.MutationFence(fenceIdentities...)
	if err != nil {
		return err
	}
	for index := range fence.TouchedRecords {
		fence.TouchedRecords[index].TouchedAttempts = nil
	}
	if err := runtime.revalidateAccountForCommit(ctx, account); err != nil {
		return err
	}
	_, err = runtime.store.CommitLane(fence, CodexLaneMutation{Lane: &lane, UpsertRecords: upserts})
	return err
}

func (runtime *CodexLeaseRuntime) requestAfterImage(plan CodexLeaseRequestPlan) CodexCurrentRequest {
	slots := make([]CodexAttemptSlot, len(plan.Slots))
	for index, slot := range plan.Slots {
		slots[index] = CodexAttemptSlot{
			Index:         uint32(index + 1),
			AccountHash:   runtime.store.hash("account", string(slot.AccountKey)),
			CandidateHash: runtime.store.hash("candidate", slot.CandidateID),
			Kind:          slot.Kind,
		}
	}
	envelope := CodexAttemptEnvelope{
		PolicyVersion: CodexLeaseAttemptPolicyVersion,
		AttemptLimit:  uint32(len(slots)),
		Slots:         slots,
	}
	envelope.PlanDigest = codexLeaseAttemptPlanDigest(runtime.store.key, slots)
	return CodexCurrentRequest{
		RequestKind:          plan.RequestKind,
		CompactionPhase:      plan.CompactionPhase,
		RequestedModelHash:   runtime.store.hash("requested-model", plan.RequestedModel),
		EffectiveModel:       plan.EffectiveModel,
		RequiredBuckets:      append([]CapacityBucket(nil), plan.RequiredBuckets...),
		DispatchPermitDigest: plan.DispatchPermitDigest,
		AttemptEnvelope:      envelope,
		RoutingRefs:          1,
		Attempts:             []CodexJournalAttempt{{Slot: plan.InitialSlot, State: CodexAttemptPrepared}},
	}
}

// committedRequestHandle constructs an exact request result from the already
// verified installed after-image. The caller still holds the runtime's
// lifecycle mutation lock, so this must not start another owner operation.
func (runtime *CodexLeaseRuntime) committedRequestHandle(key LeaseKey, accounts []codex.AccountKey, authority CodexLeaseAuthorityPolicy, identity CodexJournalRecordIdentity, post CodexLeaseGenerationFence, record CodexJournalRecordV2, requireCurrent bool, expectedTouchedAttempts int) (*CodexLeaseRequestHandle, error) {
	if runtime == nil || runtime.store == nil {
		return nil, ErrCodexLeaseWriterUnavailable
	}
	if record.Identity() != identity || (requireCurrent && post.Current != identity) || (!requireCurrent && post.Last != identity) {
		return nil, fmt.Errorf("%w: committed request fence is not installed", ErrCodexLeaseTrustLost)
	}
	recordFence, foundFence := codexLeaseRuntimeRecordFence(&post, identity)
	if !foundFence || len(recordFence.TouchedAttempts) != expectedTouchedAttempts || record.RecordGeneration != recordFence.Revision || record.LeaseGeneration != recordFence.Lease || record.Generation != recordFence.RequestGeneration || record.CurrentAttemptGeneration != recordFence.CurrentAttempt {
		return nil, fmt.Errorf("%w: committed request record fence mismatch", ErrCodexLeaseTrustLost)
	}
	attempt, foundAttempt := codexLeaseAttemptByGeneration(record.Attempts, record.CurrentAttemptGeneration)
	if !foundAttempt {
		return nil, fmt.Errorf("%w: committed request attempt fence mismatch", ErrCodexLeaseTrustLost)
	}
	for _, attemptFence := range recordFence.TouchedAttempts {
		committedAttempt, found := codexLeaseAttemptByGeneration(record.Attempts, attemptFence.Generation)
		if !found || attemptFence.RequestGeneration != record.Generation || attemptFence.Revision != committedAttempt.Revision {
			return nil, fmt.Errorf("%w: committed request attempt fence mismatch", ErrCodexLeaseTrustLost)
		}
	}
	account, resolved := runtime.store.resolveCodexLeaseAccount(record.AccountHash, accounts)
	if !resolved {
		return nil, fmt.Errorf("%w: committed request account is unavailable", ErrCodexLeaseAuthorityMismatch)
	}
	slotAccounts := make([]codex.AccountKey, len(record.AttemptEnvelope.Slots))
	for index, slot := range record.AttemptEnvelope.Slots {
		slotAccount, resolved := runtime.store.resolveCodexLeaseAccount(slot.AccountHash, accounts)
		if !resolved {
			return nil, fmt.Errorf("%w: request slot account is unavailable", ErrCodexLeaseAuthorityMismatch)
		}
		slotAccounts[index] = slotAccount
	}
	nextFence := cloneCodexLeaseGenerationFence(post)
	for index := range nextFence.TouchedRecords {
		nextFence.TouchedRecords[index].TouchedAttempts = nil
	}
	nextRecordFence, foundFence := codexLeaseRuntimeRecordFence(&nextFence, identity)
	if !foundFence {
		return nil, fmt.Errorf("%w: committed request fence is absent", ErrCodexLeaseTrustLost)
	}
	nextRecordFence.TouchedAttempts = []CodexAttemptFence{{
		RequestGeneration: record.Generation,
		Generation:        attempt.Generation,
		Revision:          attempt.Revision,
	}}
	return &CodexLeaseRequestHandle{
		runtime:      runtime,
		key:          key,
		accounts:     append([]codex.AccountKey(nil), accounts...),
		authority:    cloneCodexLeaseAuthorityPolicy(authority),
		identity:     identity,
		record:       cloneCodexJournalRecordV2(record),
		account:      account,
		slotAccounts: slotAccounts,
		fence:        nextFence,
	}, nil
}

func (runtime *CodexLeaseRuntime) restoredRecord(restored CodexRestoredLane, identity CodexJournalRecordIdentity) (CodexRestoredRecord, bool) {
	for _, record := range restored.ResolvedRecords {
		if record.Identity == identity {
			return record, true
		}
	}
	return CodexRestoredRecord{}, false
}

func (runtime *CodexLeaseRuntime) validateRequestContinuity(restored CodexRestoredLane, requestIdentity CodexJournalRecordIdentity, selected codex.AccountKey, evidence CodexLeaseRequestEvidence) (bool, error) {
	var authority CodexRestoredRecord
	var found bool
	newTurn := restored.Classification == CodexRestoredLaneUnseen
	if newTurn {
		if !restored.Fence.Last.IsZero() {
			authority, found = runtime.restoredRecord(restored, restored.Fence.Last)
		}
	} else {
		authority, found = runtime.restoredRecord(restored, requestIdentity)
	}

	if newTurn && evidence.HasTurnState {
		return false, fmt.Errorf("%w: unexpected request turn state", ErrCodexContinuity)
	}
	if !newTurn && found {
		if authority.Record.HasTurnState != evidence.HasTurnState {
			return false, fmt.Errorf("%w: request turn state presence mismatch", ErrCodexContinuity)
		}
		if evidence.HasTurnState && !constantTimeCodexLeaseDigestEqual(authority.Record.TurnStateHash, runtime.store.hash("turn-state", evidence.TurnState)) {
			return false, fmt.Errorf("%w: request turn state mismatch", ErrCodexContinuity)
		}
	}
	if evidence.PreviousResponseID != "" {
		return false, fmt.Errorf("%w: live upstream generation unavailable for previous response", ErrCodexContinuity)
	}
	if newTurn && evidence.HasEncryptedState && (!found || (!authority.Record.EverAdmitted && !authority.Record.NonMigratable && !authority.Record.HasEncryptedState)) {
		return false, fmt.Errorf("%w: encrypted response affinity unavailable", ErrCodexContinuity)
	}
	requiresAccount := evidence.PreviousResponseID != "" || evidence.HasTurnState || evidence.HasEncryptedState || (found && (authority.Record.HasEncryptedState || (!newTurn && authority.Record.NonMigratable)))
	if requiresAccount && (!found || authority.Record.AccountHash == "" || !constantTimeCodexLeaseDigestEqual(authority.Record.AccountHash, runtime.store.hash("account", string(selected)))) {
		return requiresAccount, fmt.Errorf("%w: request account affinity mismatch", ErrCodexContinuity)
	}
	return requiresAccount, nil
}

func codexLeaseRuntimeRequestIdentity(restored CodexRestoredLane) CodexJournalRecordIdentity {
	if restored.Classification == CodexRestoredLaneCurrent && !restored.Fence.Current.IsZero() {
		return restored.Fence.Current
	}
	return restored.RequestedIdentity
}

func (handle *CodexLeaseRequestHandle) applyAdmissionEvidence(record *CodexJournalRecordV2, evidence CodexHTTPAdmissionEvidence) error {
	if handle == nil || handle.runtime == nil || record == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if evidence.HasTurnState != (evidence.TurnState != "") || len(evidence.TurnState) > codexTurnMetadataMaxBytes {
		return fmt.Errorf("%w: invalid HTTP admission evidence", ErrCodexLeaseInvalidMutation)
	}
	if !evidence.HasTurnState {
		return nil
	}
	digest := handle.runtime.store.hash("turn-state", evidence.TurnState)
	if record.HasTurnState && !constantTimeCodexLeaseDigestEqual(record.TurnStateHash, digest) {
		return fmt.Errorf("%w: admitted turn state mismatch", ErrCodexContinuity)
	}
	record.TurnStateHash = digest
	record.HasTurnState = true
	return nil
}

func (handle *CodexLeaseRequestHandle) applyResponseEvidence(record *CodexJournalRecordV2, evidence CodexHTTPResponseEvidence) error {
	if handle == nil || handle.runtime == nil || record == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if evidence.HasResponseAnchor != (evidence.ResponseAnchor != "") || len(evidence.ResponseAnchor) > codexTurnIDMaxBytes {
		return fmt.Errorf("%w: invalid HTTP response evidence", ErrCodexLeaseInvalidMutation)
	}
	if evidence.HasResponseAnchor {
		record.CorrelationHash = handle.runtime.store.hash("correlation", evidence.ResponseAnchor)
		record.HasResponseAnchor = true
	}
	if evidence.HasEncryptedState {
		record.HasEncryptedState = true
	}
	return nil
}

func (runtime *CodexLeaseRuntime) mutationFence(restored CodexRestoredLane, record CodexJournalRecordV2) (CodexLeaseGenerationFence, error) {
	identity := record.Identity()
	identities := []CodexJournalRecordIdentity{identity}
	if record.PredecessorTurnHash != "" {
		identities = append(identities, CodexJournalRecordIdentity{
			LaneDigest:    identity.LaneDigest,
			TurnDigest:    record.PredecessorTurnHash,
			ModeEpoch:     record.PredecessorModeEpoch,
			Authoritative: record.PredecessorAuthoritative,
		})
	}
	return restored.MutationFence(identities...)
}

func codexLeaseRuntimeRecordFence(fence *CodexLeaseGenerationFence, identity CodexJournalRecordIdentity) (*CodexLeaseRecordFence, bool) {
	if fence == nil {
		return nil, false
	}
	for index := range fence.TouchedRecords {
		if fence.TouchedRecords[index].Record == identity {
			return &fence.TouchedRecords[index], true
		}
	}
	return nil, false
}

func (runtime *CodexLeaseRuntime) validateAndClonePlan(plan CodexLeaseRequestPlan) (CodexLeaseRequestPlan, error) {
	if runtime == nil || runtime.store == nil || runtime.leases == nil {
		return CodexLeaseRequestPlan{}, ErrCodexLeaseWriterUnavailable
	}
	if err := plan.Key.validate(); err != nil {
		return CodexLeaseRequestPlan{}, err
	}
	if err := validateCodexLeaseAuthorityPolicy(plan.Authority); err != nil {
		return CodexLeaseRequestPlan{}, err
	}
	if !validCodexLeaseRuntimeRequest(plan.RequestKind, plan.CompactionPhase) || plan.RequestedModel == "" || plan.EffectiveModel == "" || strings.TrimSpace(plan.EffectiveModel) != plan.EffectiveModel || !validCodexLeaseBuckets(plan.RequiredBuckets, plan.EffectiveModel) || len(plan.Slots) == 0 || uint64(len(plan.Slots)) > uint64(math.MaxUint32) || plan.InitialSlot == 0 || int(plan.InitialSlot) > len(plan.Slots) {
		return CodexLeaseRequestPlan{}, fmt.Errorf("%w: incomplete request plan", ErrCodexLeaseInvalidMutation)
	}
	if plan.Evidence.HasTurnState != (plan.Evidence.TurnState != "") || len(plan.Evidence.TurnState) > codexTurnMetadataMaxBytes || len(plan.Evidence.PreviousResponseID) > codexTurnIDMaxBytes {
		return CodexLeaseRequestPlan{}, fmt.Errorf("%w: invalid request continuity evidence", ErrCodexLeaseInvalidMutation)
	}
	accounts := make(map[codex.AccountKey]struct{}, len(plan.Accounts))
	for _, account := range plan.Accounts {
		if account == "" {
			return CodexLeaseRequestPlan{}, fmt.Errorf("%w: empty resolvable account", ErrCodexLeaseInvalidMutation)
		}
		accounts[account] = struct{}{}
	}
	for _, slot := range plan.Slots {
		if slot.AccountKey == "" || slot.CandidateID == "" || (slot.Kind != CodexAttemptSlotDirect && slot.Kind != CodexAttemptSlotEligibleManagedRefresh) {
			return CodexLeaseRequestPlan{}, fmt.Errorf("%w: invalid request slot", ErrCodexLeaseInvalidMutation)
		}
		if _, ok := accounts[slot.AccountKey]; !ok {
			return CodexLeaseRequestPlan{}, fmt.Errorf("%w: request slot account is not resolvable", ErrCodexLeaseAuthorityMismatch)
		}
	}
	plan.Accounts = append([]codex.AccountKey(nil), plan.Accounts...)
	plan.RequiredBuckets = append([]CapacityBucket(nil), plan.RequiredBuckets...)
	plan.Slots = append([]CodexLeaseAttemptSlotPlan(nil), plan.Slots...)
	plan.Authority = cloneCodexLeaseAuthorityPolicy(plan.Authority)
	if plan.ExpectedBound != nil {
		if plan.ExpectedBound.Identity.IsZero() || plan.ExpectedBound.AccountKey == "" || plan.ExpectedBound.RecordGeneration == 0 {
			return CodexLeaseRequestPlan{}, fmt.Errorf("%w: invalid expected bound", ErrCodexLeaseInvalidMutation)
		}
		expected := *plan.ExpectedBound
		plan.ExpectedBound = &expected
	}
	return plan, nil
}

func (runtime *CodexLeaseRuntime) validateExpectedBound(restored CodexRestoredLane, plan CodexLeaseRequestPlan, selected codex.AccountKey) error {
	expected := plan.ExpectedBound
	if expected == nil {
		return nil
	}
	record, found := runtime.restoredRecord(restored, expected.Identity)
	if restored.Classification != CodexRestoredLaneCurrent || restored.Fence.Current != expected.Identity || !found || (!record.Record.EverAdmitted && !record.Record.NonMigratable) || record.Record.RecordGeneration != expected.RecordGeneration || record.AccountKey != expected.AccountKey || selected != expected.AccountKey || !constantTimeCodexLeaseDigestEqual(record.Record.RequestedModelHash, runtime.store.hash("requested-model", plan.RequestedModel)) || record.Record.EffectiveModel != plan.EffectiveModel || !slices.Equal(record.Record.RequiredBuckets, plan.RequiredBuckets) {
		return fmt.Errorf("%w: expected bound turn changed", ErrCodexLeaseAuthorityMismatch)
	}
	return nil
}

func codexLeaseRuntimeCanBeginRequest(record CodexJournalRecordV2) bool {
	if record.Generation == 0 {
		return record.State == LeaseReserving && codexCurrentRequestIsZero(record.CodexCurrentRequest)
	}
	if record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !codexLeaseAttemptTerminalForRequest(codexLeaseCurrentAttemptState(record)) {
		return false
	}
	return record.State == LeaseContinuationPending || record.State == LeaseBoundQuiescent || record.State == LeaseOrphaned || (record.State == LeaseProvisional && codexLeaseCurrentAttemptState(record) == CodexAttemptAbandonedBeforeDispatch)
}

func validCodexLeaseRuntimeRequest(kind CodexRequestKind, phase CodexCompactionPhase) bool {
	switch kind {
	case CodexRequestTurn:
		return phase == ""
	case CodexRequestCompaction:
		return phase == CodexCompactionStandalone || phase == CodexCompactionPreTurn || phase == CodexCompactionMidTurn
	default:
		return false
	}
}

func codexLeaseRuntimeMutationRecord(record CodexJournalRecordV2) CodexJournalRecordV2 {
	mutation := cloneCodexJournalRecordV2(record)
	mutation.RecordGeneration = 0
	mutation.LaneGeneration = 0
	mutation.PredecessorGeneration = 0
	mutation.LeaseGeneration = 0
	mutation.EverAdmitted = false
	mutation.AdmissionJournalGeneration = 0
	mutation.AdmissionRequestGeneration = 0
	mutation.AdmissionRequestKind = ""
	mutation.AdmissionCompactionPhase = ""
	mutation.AdmittedAt = time.Time{}
	mutation.CreatedAt = time.Time{}
	mutation.LastObservedAt = time.Time{}
	for index := range mutation.Attempts {
		mutation.Attempts[index].Revision = 0
		mutation.Attempts[index].CreatedAt = time.Time{}
		mutation.Attempts[index].LastObservedAt = time.Time{}
	}
	return mutation
}

func cloneCodexLeaseAuthorityPolicy(policy CodexLeaseAuthorityPolicy) CodexLeaseAuthorityPolicy {
	policy.RetainedAuthoritativeEpochs = append([]uint64(nil), policy.RetainedAuthoritativeEpochs...)
	return policy
}
