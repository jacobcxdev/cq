package proxy

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexHTTPRequestRouteSnapshotter supplies all durable route state in one
// context-aware read. Implementations must return detached data.
type CodexHTTPRequestRouteSnapshotter interface {
	LoadRouteSnapshot(context.Context, LeaseKey, []codex.AccountKey, CodexLeaseAuthorityPolicy) (CodexLeaseRouteSnapshot, error)
}

// CodexHTTPRequestPlanRuntime is the one durable mutation performed by the
// factory after every other fallible preparation step has completed.
type CodexHTTPRequestPlanRuntime interface {
	BeginRequestContext(context.Context, CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error)
}

// CodexHTTPRequestPlanInput contains caller-owned native request bytes and the
// only credential revision affinity available outside durable route state.
type CodexHTTPRequestPlanInput struct {
	Encoded          []byte
	Headers          http.Header
	AcceptedRevision codex.Revision
	ExpectedBound    *CodexLeaseBoundExpectation
}

// ProbeRetained performs the read-only half of retained routing. A claimed
// result must either carry an exact bound expectation or a fail-closed error.
func (factory *CodexHTTPRequestPlanFactory) ProbeRetained(ctx context.Context, input CodexHTTPRequestPlanInput) (*CodexLeaseBoundExpectation, bool, error) {
	if factory == nil || factory.Inventory == nil || factory.Routes == nil {
		return nil, true, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanUnavailable, nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	inspection, err := factory.inspect(ctx, input.Encoded, input.Headers)
	if err != nil {
		return nil, false, nil
	}
	defer inspection.Release()
	protocol, err := inspection.Protocol()
	if err != nil || !protocol.Metadata.Found || !protocol.Metadata.Strong {
		return nil, false, nil
	}
	key := NewCodexLeaseKey(protocol.Metadata.Metadata)
	inventory, err := factory.Inventory.List(ctx)
	if err != nil {
		return nil, true, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInventory, err)
	}
	snapshot, err := factory.Routes.LoadRouteSnapshot(ctx, key, codexHTTPRequestPlanAccountKeys(inventory), factory.Authority)
	if err != nil || snapshot.JournalGeneration == 0 {
		return nil, true, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanRouteSnapshot, err)
	}
	switch snapshot.Classification {
	case CodexRestoredLaneHistorical:
		if snapshot.HistoricalAuthoritative {
			return nil, true, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanRouteSnapshot, ErrCodexStaleTurn)
		}
	case CodexRestoredLaneCurrent:
		if snapshot.BoundIdentity.Authoritative && containsCodexLeaseEpoch(factory.Authority.RetainedAuthoritativeEpochs, snapshot.BoundIdentity.ModeEpoch) && snapshot.BoundAccountKey != "" && snapshot.BoundRecordGeneration != 0 {
			return &CodexLeaseBoundExpectation{
				Identity:         snapshot.BoundIdentity,
				AccountKey:       snapshot.BoundAccountKey,
				RecordGeneration: snapshot.BoundRecordGeneration,
			}, true, nil
		}
	}
	return nil, false, nil
}

// CodexPreparedHTTPRequest transfers one frozen replay request and its durable
// prepared lifecycle handle to the request session.
type CodexPreparedHTTPRequest struct {
	Dispatch  CodexFrozenDispatchPlan
	Frozen    *CodexFrozenRequest
	Lifecycle CodexHTTPRequestLifecycle
}

// CodexHTTPRequestPlanErrorCode classifies preparation failures without
// exposing request, identity, credential, header, or dependency error text.
type CodexHTTPRequestPlanErrorCode string

const (
	CodexHTTPRequestPlanUnavailable   CodexHTTPRequestPlanErrorCode = "unavailable"
	CodexHTTPRequestPlanInspect       CodexHTTPRequestPlanErrorCode = "inspect"
	CodexHTTPRequestPlanInventory     CodexHTTPRequestPlanErrorCode = "inventory"
	CodexHTTPRequestPlanRouteSnapshot CodexHTTPRequestPlanErrorCode = "route_snapshot"
	CodexHTTPRequestPlanDispatch      CodexHTTPRequestPlanErrorCode = "dispatch_plan"
	CodexHTTPRequestPlanFreeze        CodexHTTPRequestPlanErrorCode = "freeze"
	CodexHTTPRequestPlanBegin         CodexHTTPRequestPlanErrorCode = "begin_request"
)

// CodexHTTPRequestPlanError deliberately retains only safe sentinel identity.
type CodexHTTPRequestPlanError struct {
	Code     CodexHTTPRequestPlanErrorCode
	identity error
}

func (err *CodexHTTPRequestPlanError) Error() string {
	if err == nil {
		return "Codex HTTP request preparation failed"
	}
	return "Codex HTTP request preparation failed: " + string(err.Code)
}

func (err *CodexHTTPRequestPlanError) Is(target error) bool {
	return err != nil && err.identity != nil && target == err.identity
}

func newCodexHTTPRequestPlanError(code CodexHTTPRequestPlanErrorCode, cause error) error {
	return &CodexHTTPRequestPlanError{Code: code, identity: codexHTTPRequestPlanSafeIdentity(cause)}
}

func codexHTTPRequestPlanSafeIdentity(cause error) error {
	for _, safe := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		ErrCodexFrozenRequestReleased,
		ErrCodexLeaseWriterUnavailable,
		ErrCodexLeaseTrustLost,
		ErrCodexLegacyQuarantine,
		ErrCodexStaleTurn,
		ErrCodexConcurrentTurn,
		ErrCodexContinuity,
		ErrCodexLeaseAuthorityMismatch,
		ErrCodexLeaseInvalidMutation,
		codex.ErrCredentialAuthorityUnavailable,
	} {
		if errors.Is(cause, safe) {
			return safe
		}
	}
	return nil
}

type codexHTTPRequestPlanFactoryOperations struct {
	inspect       func(context.Context, []byte, http.Header) (*CodexFrozenRequestInspection, error)
	buildDispatch func(context.Context, CodexFrozenDispatchInput) (CodexFrozenDispatchPlan, error)
	freeze        func(context.Context, *CodexFrozenRequestInspection, RouteChoice, CodexRequestHeadroom, HeadroomMode) (*CodexFrozenRequest, error)
}

// CodexHTTPRequestPlanFactory performs one inspect, inventory read, route
// snapshot, frozen dispatch build, request freeze, and durable BeginRequest.
type CodexHTTPRequestPlanFactory struct {
	Inventory         codex.CredentialInventory
	Capacity          *CodexCapacityLedger
	Routes            CodexHTTPRequestRouteSnapshotter
	Runtime           CodexHTTPRequestPlanRuntime
	DefaultAccountKey codex.AccountKey
	Authority         CodexLeaseAuthorityPolicy
	Headroom          CodexRequestHeadroom
	HeadroomMode      HeadroomMode
	Now               func() time.Time

	operations codexHTTPRequestPlanFactoryOperations
}

// Build prepares one immutable native HTTP request and commits its first
// prepared attempt. Success transfers Frozen and Lifecycle ownership.
func (factory *CodexHTTPRequestPlanFactory) Build(ctx context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	var result CodexPreparedHTTPRequest
	if factory == nil || factory.Inventory == nil || factory.Routes == nil || factory.Runtime == nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanUnavailable, nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	inspection, err := factory.inspect(ctx, input.Encoded, input.Headers)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	defer inspection.Release()

	protocol, err := inspection.Protocol()
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	metadata := protocol.Metadata.Metadata
	key := NewCodexLeaseKey(metadata)

	inventory, err := factory.Inventory.List(ctx)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInventory, err)
	}
	accounts := codexHTTPRequestPlanAccountKeys(inventory)
	snapshot, err := factory.Routes.LoadRouteSnapshot(ctx, key, accounts, factory.Authority)
	if err != nil || snapshot.JournalGeneration == 0 {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanRouteSnapshot, err)
	}
	if !codexHTTPRequestExpectedBoundMatchesSnapshot(input.ExpectedBound, snapshot) {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanRouteSnapshot, ErrCodexLeaseAuthorityMismatch)
	}

	now := time.Now()
	if factory.Now != nil {
		now = factory.Now()
	}
	requirements := codexHTTPRequestPlanRequirements(protocol)
	dispatch, err := factory.buildDispatch(ctx, CodexFrozenDispatchInput{
		Inventory:          inventory,
		Capacity:           factory.Capacity,
		Requirements:       requirements,
		Provisional:        cloneCodexHTTPRequestPlanProvisional(snapshot.Provisional),
		AffinityAccountKey: snapshot.AffinityAccountKey,
		DefaultAccountKey:  factory.DefaultAccountKey,
		BoundAccountKey:    snapshot.BoundAccountKey,
		AcceptedRevision:   input.AcceptedRevision,
		Now:                now,
	})
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, err)
	}
	dispatchAccounts := dispatch.Accounts()
	if len(dispatchAccounts) == 0 {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, dispatch.TerminalError())
	}
	choice := dispatchAccounts[0].Choice()
	if input.ExpectedBound != nil && (choice.AccountKey != input.ExpectedBound.AccountKey || choice.EffectiveModel != snapshot.BoundChoice.EffectiveModel || !slices.Equal(choice.RequiredBuckets, snapshot.BoundChoice.RequiredBuckets)) {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCodexLeaseAuthorityMismatch)
	}
	leasePlan := codexHTTPRequestLeasePlan(key, accounts, factory.Authority, protocol, choice, dispatch, input.ExpectedBound)

	frozen, err := factory.freeze(ctx, inspection, choice)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanFreeze, err)
	}
	handle, err := factory.Runtime.BeginRequestContext(ctx, leasePlan)
	if err != nil {
		if handle != nil {
			_, cleanupErr := handle.AbandonBeforeDispatchContext(context.WithoutCancel(ctx))
			err = errors.Join(err, cleanupErr)
		}
		frozen.Release()
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanBegin, err)
	}
	if handle == nil {
		frozen.Release()
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanBegin, nil)
	}

	result.Dispatch = dispatch
	result.Frozen = frozen
	result.Lifecycle = NewCodexHTTPRequestLifecycle(handle)
	return result, nil
}

func codexHTTPRequestPlanRequirements(protocol CodexProtocolRequest) CodexRouteRequirements {
	requirements := CodexRouteRequirements{RequestedModel: protocol.Model}
	metadata := protocol.Metadata.Metadata
	if metadata.RequestKind == CodexRequestCompaction && metadata.CompactionPhase == CodexCompactionPreTurn {
		requirements.RequiredModels = []string{"gpt-5.4", codexSparkModel}
	}
	return requirements
}

func (factory *CodexHTTPRequestPlanFactory) inspect(ctx context.Context, encoded []byte, headers http.Header) (*CodexFrozenRequestInspection, error) {
	if factory.operations.inspect != nil {
		return factory.operations.inspect(ctx, encoded, headers)
	}
	return InspectCodexNativeRequest(ctx, encoded, headers)
}

func (factory *CodexHTTPRequestPlanFactory) buildDispatch(ctx context.Context, input CodexFrozenDispatchInput) (CodexFrozenDispatchPlan, error) {
	if factory.operations.buildDispatch != nil {
		return factory.operations.buildDispatch(ctx, input)
	}
	return BuildCodexFrozenDispatchPlan(ctx, input)
}

func (factory *CodexHTTPRequestPlanFactory) freeze(ctx context.Context, inspection *CodexFrozenRequestInspection, choice RouteChoice) (*CodexFrozenRequest, error) {
	if factory.operations.freeze != nil {
		return factory.operations.freeze(ctx, inspection, choice, factory.Headroom, factory.HeadroomMode)
	}
	return inspection.Freeze(ctx, choice, factory.Headroom, factory.HeadroomMode)
}

func codexHTTPRequestPlanAccountKeys(inventory codex.Inventory) []codex.AccountKey {
	accounts := make([]codex.AccountKey, 0, len(inventory.Accounts))
	for _, account := range inventory.Accounts {
		if account.Key == "" {
			continue
		}
		accounts = append(accounts, account.Key)
	}
	return accounts
}

func cloneCodexHTTPRequestPlanProvisional(source map[codex.AccountKey]int) map[codex.AccountKey]int {
	if source == nil {
		return nil
	}
	clone := make(map[codex.AccountKey]int, len(source))
	for account, count := range source {
		clone[account] = count
	}
	return clone
}

func codexHTTPRequestLeasePlan(key LeaseKey, accounts []codex.AccountKey, authority CodexLeaseAuthorityPolicy, protocol CodexProtocolRequest, choice RouteChoice, dispatch CodexFrozenDispatchPlan, expected *CodexLeaseBoundExpectation) CodexLeaseRequestPlan {
	httpSlots := CodexHTTPAttemptSlots(dispatch)
	slots := make([]CodexLeaseAttemptSlotPlan, len(httpSlots))
	for index, slot := range httpSlots {
		slots[index] = CodexLeaseAttemptSlotPlan{
			AccountKey:  slot.AccountKey,
			CandidateID: string(slot.CandidateID),
			Kind:        slot.Kind,
		}
	}
	metadata := protocol.Metadata.Metadata
	var expectedClone *CodexLeaseBoundExpectation
	if expected != nil {
		clone := *expected
		expectedClone = &clone
	}
	return CodexLeaseRequestPlan{
		Key:             key,
		Accounts:        append([]codex.AccountKey(nil), accounts...),
		Authority:       cloneCodexLeaseAuthorityPolicy(authority),
		RequestKind:     metadata.RequestKind,
		CompactionPhase: metadata.CompactionPhase,
		RequestedModel:  protocol.Model,
		EffectiveModel:  choice.EffectiveModel,
		RequiredBuckets: append([]CapacityBucket(nil), choice.RequiredBuckets...),
		Slots:           slots,
		InitialSlot:     1,
		ExpectedBound:   expectedClone,
		Evidence: CodexLeaseRequestEvidence{
			PreviousResponseID: protocol.PreviousResponseID,
			TurnState:          protocol.TurnState,
			HasTurnState:       protocol.HasTurnState,
			HasEncryptedState:  protocol.HasEncryptedState,
		},
	}
}

func codexHTTPRequestExpectedBoundMatchesSnapshot(expected *CodexLeaseBoundExpectation, snapshot CodexLeaseRouteSnapshot) bool {
	if expected == nil {
		return true
	}
	return snapshot.Classification == CodexRestoredLaneCurrent &&
		snapshot.BoundIdentity == expected.Identity &&
		snapshot.BoundAccountKey == expected.AccountKey &&
		snapshot.BoundRecordGeneration == expected.RecordGeneration
}
