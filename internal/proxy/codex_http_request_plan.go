package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
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

type codexWebSocketPrewarmRuntime interface {
	adoptWebSocketPrewarmContext(context.Context, []codex.AccountKey, CodexPrewarmAdoptionRequest) (*CodexLeaseRequestHandle, error)
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

	leaseHandle *CodexLeaseRequestHandle
	receipt     *codexTurnReceiptHandle
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

// CodexRequestFailureReason is a credential-free reason suitable for route
// diagnostics and stderr. Unknown dependency text is never retained.
type CodexRequestFailureReason string

const (
	CodexRequestFailureUnknown                        CodexRequestFailureReason = "unknown"
	CodexRequestFailureContextCanceled                CodexRequestFailureReason = "context_canceled"
	CodexRequestFailureDeadlineExceeded               CodexRequestFailureReason = "deadline_exceeded"
	CodexRequestFailureFrozenRequestReleased          CodexRequestFailureReason = "frozen_request_released"
	CodexRequestFailureLeaseWriterUnavailable         CodexRequestFailureReason = "lease_writer_unavailable"
	CodexRequestFailureLeaseTrustLost                 CodexRequestFailureReason = "lease_trust_lost"
	CodexRequestFailureLegacyQuarantine               CodexRequestFailureReason = "legacy_quarantine"
	CodexRequestFailureStaleTurn                      CodexRequestFailureReason = "stale_turn"
	CodexRequestFailureConcurrentTurn                 CodexRequestFailureReason = "concurrent_turn"
	CodexRequestFailureContinuity                     CodexRequestFailureReason = "continuity"
	CodexRequestFailureLeaseAuthorityMismatch         CodexRequestFailureReason = "lease_authority_mismatch"
	CodexRequestFailureLeaseInvalidMutation           CodexRequestFailureReason = "lease_invalid_mutation"
	CodexRequestFailureCredentialAuthorityUnavailable CodexRequestFailureReason = "credential_authority_unavailable"
	CodexRequestFailureSessionPolicyUnavailable       CodexRequestFailureReason = "session_policy_unavailable"
	CodexRequestFailureSessionPolicyContinuity        CodexRequestFailureReason = "session_policy_continuity"
	CodexRequestFailureCapabilityRouteUnavailable     CodexRequestFailureReason = "capability_route_unavailable"
	CodexRequestFailureDispatchPermitInvalid          CodexRequestFailureReason = "dispatch_permit_invalid"
	CodexRequestFailureDispatchPermitReplayed         CodexRequestFailureReason = "dispatch_permit_replayed"
)

type CodexHTTPRequestPlanFailure struct {
	Stage  CodexHTTPRequestPlanErrorCode
	Reason CodexRequestFailureReason
}

// CodexHTTPRequestPlanError deliberately retains only safe sentinel identity.
type CodexHTTPRequestPlanError struct {
	Code     CodexHTTPRequestPlanErrorCode
	Reason   CodexRequestFailureReason
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
	return &CodexHTTPRequestPlanError{
		Code:     code,
		Reason:   codexRequestFailureReason(cause),
		identity: codexHTTPRequestPlanSafeIdentity(cause),
	}
}

func codexRequestFailureReason(cause error) CodexRequestFailureReason {
	var continuityErr *codexContinuityError
	if errors.As(cause, &continuityErr) {
		return safeCodexRequestFailureReason(CodexRequestFailureReason(continuityErr.reason))
	}
	switch {
	case errors.Is(cause, context.Canceled):
		return CodexRequestFailureContextCanceled
	case errors.Is(cause, context.DeadlineExceeded):
		return CodexRequestFailureDeadlineExceeded
	case errors.Is(cause, ErrCodexFrozenRequestReleased):
		return CodexRequestFailureFrozenRequestReleased
	case errors.Is(cause, ErrCodexLeaseWriterUnavailable):
		return CodexRequestFailureLeaseWriterUnavailable
	case errors.Is(cause, ErrCodexLeaseTrustLost):
		return CodexRequestFailureLeaseTrustLost
	case errors.Is(cause, ErrCodexLegacyQuarantine):
		return CodexRequestFailureLegacyQuarantine
	case errors.Is(cause, ErrCodexStaleTurn):
		return CodexRequestFailureStaleTurn
	case errors.Is(cause, ErrCodexConcurrentTurn):
		return CodexRequestFailureConcurrentTurn
	case errors.Is(cause, ErrCodexContinuity):
		return CodexRequestFailureContinuity
	case errors.Is(cause, ErrCodexLeaseAuthorityMismatch):
		return CodexRequestFailureLeaseAuthorityMismatch
	case errors.Is(cause, ErrCodexLeaseInvalidMutation):
		return CodexRequestFailureLeaseInvalidMutation
	case errors.Is(cause, codex.ErrCredentialAuthorityUnavailable):
		return CodexRequestFailureCredentialAuthorityUnavailable
	case errors.Is(cause, ErrSessionPolicyUnavailable):
		return CodexRequestFailureSessionPolicyUnavailable
	case errors.Is(cause, ErrSessionPolicyContinuity):
		return CodexRequestFailureSessionPolicyContinuity
	case errors.Is(cause, ErrCapabilityRouteUnavailable):
		return CodexRequestFailureCapabilityRouteUnavailable
	case errors.Is(cause, ErrCallerDispatchPermitInvalid):
		return CodexRequestFailureDispatchPermitInvalid
	case errors.Is(cause, ErrCallerDispatchPermitReplayed):
		return CodexRequestFailureDispatchPermitReplayed
	}
	var routeErr *CodexRoutePolicyError
	if errors.As(cause, &routeErr) {
		return safeCodexRequestFailureReason(CodexRequestFailureReason(routeErr.Status))
	}
	return CodexRequestFailureUnknown
}

func safeCodexRequestFailureReason(reason CodexRequestFailureReason) CodexRequestFailureReason {
	switch reason {
	case CodexRequestFailureUnknown,
		CodexRequestFailureContextCanceled,
		CodexRequestFailureDeadlineExceeded,
		CodexRequestFailureFrozenRequestReleased,
		CodexRequestFailureLeaseWriterUnavailable,
		CodexRequestFailureLeaseTrustLost,
		CodexRequestFailureLegacyQuarantine,
		CodexRequestFailureStaleTurn,
		CodexRequestFailureConcurrentTurn,
		CodexRequestFailureContinuity,
		CodexRequestFailureLeaseAuthorityMismatch,
		CodexRequestFailureLeaseInvalidMutation,
		CodexRequestFailureCredentialAuthorityUnavailable,
		CodexRequestFailureSessionPolicyUnavailable,
		CodexRequestFailureSessionPolicyContinuity,
		CodexRequestFailureCapabilityRouteUnavailable,
		CodexRequestFailureDispatchPermitInvalid,
		CodexRequestFailureDispatchPermitReplayed,
		CodexRequestFailureReason(codexContinuityUnexpectedTurnState),
		CodexRequestFailureReason(codexContinuityTurnStatePresenceMismatch),
		CodexRequestFailureReason(codexContinuityTurnStateMismatch),
		CodexRequestFailureReason(codexContinuityPreviousResponseMismatch),
		CodexRequestFailureReason(codexContinuityEncryptedAffinityMissing),
		CodexRequestFailureReason(codexContinuityAccountAffinityMismatch),
		CodexRequestFailureReason(CodexRoutePlanDefaultMissing),
		CodexRequestFailureReason(CodexRoutePlanDefaultUnresolved),
		CodexRequestFailureReason(CodexRoutePlanDefaultIncompatible),
		CodexRequestFailureReason(CodexRoutePlanDefaultUnroutable),
		CodexRequestFailureReason(CodexRoutePlanBoundUnresolved),
		CodexRequestFailureReason(CodexRoutePlanBoundIncompatible),
		CodexRequestFailureReason(CodexRoutePlanBoundUnroutable),
		CodexRequestFailureReason(CodexRoutePlanAffinityUnresolved),
		CodexRequestFailureReason(CodexRoutePlanAffinityIncompatible),
		CodexRequestFailureReason(CodexRoutePlanAffinityUnroutable),
		CodexRequestFailureReason(CodexRoutePlanCanceled),
		CodexRequestFailureReason(CodexRoutePlanInvalidCandidate):
		return reason
	default:
		return CodexRequestFailureUnknown
	}
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
	PinnedAccountKey  codex.AccountKey
	Authority         CodexLeaseAuthorityPolicy
	Headroom          CodexRequestHeadroom
	HeadroomMode      HeadroomMode
	Now               func() time.Time
	SessionPolicy     *SessionPolicyResolver
	DispatchPermits   CallerDispatchPermitAuthority
	TransportKind     string
	TurnReceipts      *CodexTurnReceiptStore

	operations codexHTTPRequestPlanFactoryOperations
}

const (
	codexHTTPRequestSuccessorDrainRetryInterval = 2 * time.Millisecond
	codexHTTPRequestSuccessorDrainWait          = 250 * time.Millisecond
)

// Build prepares one immutable native HTTP request and commits its first
// prepared attempt. Success transfers Frozen and Lifecycle ownership.
func (factory *CodexHTTPRequestPlanFactory) Build(ctx context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(codexHTTPRequestSuccessorDrainWait)
	for {
		result, err := factory.buildOnce(ctx, input)
		if !errors.Is(err, ErrCodexConcurrentTurn) {
			return result, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return result, err
		}
		wait := min(remaining, codexHTTPRequestSuccessorDrainRetryInterval)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanBegin, ctx.Err())
		case <-timer.C:
		}
	}
}

func (factory *CodexHTTPRequestPlanFactory) buildOnce(ctx context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	var result CodexPreparedHTTPRequest
	if factory == nil || factory.Inventory == nil || factory.Routes == nil || factory.Runtime == nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanUnavailable, nil)
	}

	inspection, err := factory.inspect(ctx, input.Encoded, input.Headers)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	defer inspection.Release()

	protocol, err := inspection.Protocol()
	if factory.TransportKind == "http" {
		replaceCodexRequestShapeObservation(ctx, classifyCodexRequestShape(protocol, err))
	}
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	metadata := protocol.Metadata.Metadata
	key := NewCodexLeaseKey(metadata)

	inventory, err := factory.Inventory.List(ctx)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInventory, err)
	}
	unfilteredInventory := inventory
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
	caller, callerOK := runtimeCallerAuthority(ctx)
	policyDecision := SessionPolicyDecision{Allowed: sortedAccountKeys(accounts), Status: PolicyDecisionUnbound}
	if factory.SessionPolicy != nil {
		policyDecision, err = enforceSessionPolicy(factory.SessionPolicy, caller, []byte(metadata.SessionID), accounts, snapshot.BoundAccountKey, now)
		if err != nil {
			return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, err)
		}
		if policyDecision.Status == PolicyDecisionSelected {
			inventory = filterCodexHTTPRequestInventory(inventory, policyDecision.Allowed)
		}
	}
	requirements := codexHTTPRequestPlanRequirements(protocol)
	affinityAccountKey, continuityAccountKey, err := codexHTTPRequestTaskAffinityAccounts(snapshot, protocol, now)
	authenticatedCallerAccount := codex.AccountKey("")
	if callerOK {
		identity, _ := runtimeCallerIdentity(ctx)
		if caller.Domain == NormalCallerCodex && codexAuthenticatedCallerAccount(unfilteredInventory, caller, identity) == "" {
			return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCodexLeaseAuthorityMismatch)
		}
		authenticatedCallerAccount = codexAuthenticatedCallerAccount(inventory, caller, identity)
	}
	if errors.Is(err, ErrCodexLeaseAuthorityMismatch) && authenticatedCallerAccount != "" {
		continuityAccountKey = authenticatedCallerAccount
		err = nil
	}
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, err)
	}
	authenticatedBoundContinuation := authenticatedCallerAccount != "" &&
		snapshot.Classification == CodexRestoredLaneCurrent &&
		snapshot.BoundAccountKey != "" && snapshot.BoundIdentity.Authoritative && snapshot.BoundRecordGeneration != 0
	authenticatedCallerContinuity := authenticatedBoundContinuation ||
		(authenticatedCallerAccount != "" && continuityAccountKey != "" &&
			(protocol.PreviousResponseID != "" || protocol.HasTurnState || protocol.HasEncryptedState))
	expectedBound := input.ExpectedBound
	if expectedBound == nil && authenticatedBoundContinuation {
		expectedBound = &CodexLeaseBoundExpectation{
			Identity:         snapshot.BoundIdentity,
			AccountKey:       snapshot.BoundAccountKey,
			RecordGeneration: snapshot.BoundRecordGeneration,
		}
	}
	boundAccountKey := snapshot.BoundAccountKey
	if continuityAccountKey != "" {
		if boundAccountKey != "" && boundAccountKey != continuityAccountKey {
			return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCodexLeaseAuthorityMismatch)
		}
		boundAccountKey = continuityAccountKey
	}
	if boundAccountKey == "" {
		boundAccountKey = factory.PinnedAccountKey
	}
	dispatchInput := CodexFrozenDispatchInput{
		Inventory:              inventory,
		Capacity:               factory.Capacity,
		Requirements:           requirements,
		Provisional:            cloneCodexHTTPRequestPlanProvisional(snapshot.Provisional),
		AffinityAccountKey:     affinityAccountKey,
		AffinityEffectiveModel: snapshot.AffinityEffectiveModel,
		DefaultAccountKey:      factory.DefaultAccountKey,
		BoundAccountKey:        boundAccountKey,
		AcceptedRevision:       input.AcceptedRevision,
		Now:                    now,
	}
	dispatch, err := factory.buildDispatch(ctx, dispatchInput)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, err)
	}
	dispatchAccounts := dispatch.Accounts()
	if len(dispatchAccounts) == 0 {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, dispatch.TerminalError())
	}
	choice := dispatchAccounts[0].Choice()
	if policyDecision.Status == PolicyDecisionSelected {
		capabilityPolicy, capabilityEvidence, active := factory.SessionPolicy.capabilityPolicy(policyDecision.Pool, policyDecision.PolicyRevision)
		if active {
			if !callerOK {
				return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCapabilityRouteUnavailable)
			}
			allowed := policyDecision.Allowed
			if boundAccountKey != "" {
				allowed = []codex.AccountKey{boundAccountKey}
			}
			finalChoice, routeErr := ResolveCapabilityRoute(capabilityPolicy, capabilityEvidence, CallerRequestAuthorityV1{
				SchemaVersion: 1, AllowedAccounts: allowed, PreferredAccount: choice.AccountKey, AccountWorkspaces: codexCapabilityAccountWorkspaces(inventory, allowed), EvaluatedAt: now,
				FinalScope: codexCapabilityFinalScope(factory.TransportKind, input.Encoded, protocol, choice, caller),
			})
			if routeErr != nil {
				return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, routeErr)
			}
			policyDecision.Allowed = append([]codex.AccountKey(nil), finalChoice.AllowedAccounts...)
			inventory = filterCodexHTTPRequestInventory(inventory, finalChoice.AllowedAccounts)
			dispatchInput.Inventory = inventory
			dispatch, err = factory.buildDispatch(ctx, dispatchInput)
			if err != nil {
				return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, err)
			}
			dispatchAccounts = dispatch.Accounts()
			if len(dispatchAccounts) == 0 {
				return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCapabilityRouteUnavailable)
			}
			choice = dispatchAccounts[0].Choice()
			finalChoice, routeErr = ResolveCapabilityRoute(capabilityPolicy, capabilityEvidence, CallerRequestAuthorityV1{
				SchemaVersion: 1, AllowedAccounts: finalChoice.AllowedAccounts, PreferredAccount: choice.AccountKey, AccountWorkspaces: codexCapabilityAccountWorkspaces(inventory, finalChoice.AllowedAccounts), EvaluatedAt: now,
				FinalScope: codexCapabilityFinalScope(factory.TransportKind, input.Encoded, protocol, choice, caller),
			})
			if routeErr != nil || finalChoice.AccountKey != choice.AccountKey {
				return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCapabilityRouteUnavailable)
			}
		}
	}
	if input.ExpectedBound != nil && (choice.AccountKey != input.ExpectedBound.AccountKey || choice.EffectiveModel != snapshot.BoundChoice.EffectiveModel || !slices.Equal(choice.RequiredBuckets, snapshot.BoundChoice.RequiredBuckets)) {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCodexLeaseAuthorityMismatch)
	}
	shadowAdvice := codexNoAffinityShadowResult{Comparison: CodexTurnReceiptShadowNotApplicable}
	if factory.TurnReceipts != nil && codexTurnReceiptEligible(protocol) {
		shadowAdvice = codexNoAffinityShadowAdvice(ctx, dispatch, dispatchInput.DefaultAccountKey, dispatchInput.BoundAccountKey, dispatchAccounts[0])
	}
	frozen, err := factory.freeze(ctx, inspection, choice)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanFreeze, err)
	}
	permitDigest := ""
	if policyDecision.Status == PolicyDecisionSelected {
		if !callerOK || factory.DispatchPermits == nil {
			frozen.Release()
			return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCallerDispatchPermitInvalid)
		}
		permit, permitErr := factory.DispatchPermits.IssueAndConsume(ctx, CallerDispatchPermitRequestV1{
			CallerAdmissionDigest: caller.ConsumptionDigest, CallerDomain: caller.Domain, CallerSubjectID: caller.SubjectID,
			SessionDigest: policyDecision.SessionDigest, Pool: policyDecision.Pool, RoutingGeneration: policyDecision.PolicyRevision,
			AllowedAccounts: policyDecision.Allowed, SelectedAccount: choice.AccountKey,
		})
		if permitErr != nil {
			frozen.Release()
			return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, permitErr)
		}
		permitDigest = permit.Digest
	}
	leasePlan := codexHTTPRequestLeasePlan(key, accounts, factory.Authority, protocol, choice, dispatch, expectedBound, continuityAccountKey != "" || authenticatedBoundContinuation, authenticatedCallerContinuity, permitDigest)
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
	result.leaseHandle = handle
	result.Lifecycle = NewCodexHTTPRequestLifecycle(handle)
	if trace := codexInstalledHTTPTraceFromContext(ctx); trace != nil {
		requestGeneration, attemptGeneration, durableV2 := codexInstalledHTTPV2Generations(result.Lifecycle)
		trace.plannedRequest(codexInstalledHTTPPlanFacts{
			strongTurn:        protocol.Metadata.Strong && metadata.RequestKind == CodexRequestTurn && codexInstalledHTTPV2NewTurn(result.Lifecycle),
			strongRequest:     protocol.Metadata.Strong,
			zstd:              strings.EqualFold(strings.TrimSpace(input.Headers.Get("Content-Encoding")), "zstd"),
			headroom:          factory.Headroom != nil,
			initialAdmitted:   result.Lifecycle.EverAdmitted(),
			durableV2:         durableV2,
			requestGeneration: requestGeneration,
			attemptGeneration: attemptGeneration,
			dispatch:          dispatch.probe,
		})
		result.Lifecycle = trace.wrapLifecycle(result.Lifecycle)
	}
	result.receipt = factory.registerCodexTurnReceipt(protocol, policyDecision, boundAccountKey, dispatchAccounts[0], shadowAdvice)
	result.Lifecycle = wrapCodexTurnReceiptLifecycle(result.Lifecycle, result.receipt)
	return result, nil
}

func (factory *CodexHTTPRequestPlanFactory) registerCodexTurnReceipt(protocol CodexProtocolRequest, policy SessionPolicyDecision, boundAccountKey codex.AccountKey, account CodexFrozenDispatchAccount, shadow codexNoAffinityShadowResult) *codexTurnReceiptHandle {
	if factory == nil || factory.TurnReceipts == nil || !codexTurnReceiptEligible(protocol) {
		return nil
	}
	metadata := protocol.Metadata.Metadata
	transport := CodexTurnReceiptTransportHTTP
	if factory.TransportKind == "websocket" {
		transport = CodexTurnReceiptTransportWebSocket
	}
	routeReason := CodexTurnReceiptRouteUnknown
	if boundAccountKey != "" {
		routeReason = CodexTurnReceiptRouteBound
	} else {
		switch account.decision {
		case codexRuntimeDecisionAffinityReuse:
			routeReason = CodexTurnReceiptRouteAffinityReuse
		case codexRuntimeDecisionFairnessSelect:
			routeReason = CodexTurnReceiptRouteFairnessSelect
		case codexRuntimeDecisionTerminalDefault:
			routeReason = CodexTurnReceiptRouteTerminalDefault
		}
	}
	pool := ""
	if policy.Status == PolicyDecisionSelected {
		pool = policy.Pool
	}
	shape := classifyCodexRequestShape(protocol, nil)
	choice := account.Choice()
	session := []byte(metadata.SessionID)
	turn := []byte(metadata.TurnID)
	defer zeroRuntimeBytes(session)
	defer zeroRuntimeBytes(turn)
	shadowAlternativeAccountHint := ""
	if shadow.Comparison == CodexTurnReceiptShadowAlternativeAccount && shadow.AlternativeAccountKey != "" {
		shadowAlternativeAccountHint = redactedAccountHint("codex", string(shadow.AlternativeAccountKey))
	}
	return factory.TurnReceipts.register(session, turn, CodexTurnReceiptV2{
		CodexTurnReceiptV1: CodexTurnReceiptV1{
			State:                    CodexTurnReceiptPlanned,
			Transport:                transport,
			RequestKind:              shape.RequestKind,
			RequestLineage:           shape.RequestLineage,
			RequestedModelClass:      shape.RequestedModelClass,
			RequestedReasoningEffort: shape.RequestedReasoningEffort,
			CompactionPhase:          shape.CompactionPhase,
			Pool:                     pool,
			RouteReason:              routeReason,
			PlannedAccountHint:       redactedAccountHint("codex", string(choice.AccountKey)),
		},
		ShadowComparison:             shadow.Comparison,
		ShadowAlternativeAccountHint: shadowAlternativeAccountHint,
	})
}

func codexTurnReceiptEligible(protocol CodexProtocolRequest) bool {
	if !protocol.Metadata.Strong {
		return false
	}
	metadata := protocol.Metadata.Metadata
	return metadata.RequestKind == CodexRequestTurn && metadata.SessionID != "" && metadata.TurnID != ""
}

func codexCapabilityFinalScope(transport string, encoded []byte, protocol CodexProtocolRequest, choice RouteChoice, caller RuntimeCallerAuthorityV1) CapabilityFinalScopeCoreV1 {
	if transport == "" {
		transport = "http"
	}
	encodedDigest := sha256.Sum256(encoded)
	transformationDigest := sha256.Sum256([]byte(protocol.Model + "\x00" + choice.EffectiveModel))
	originDigest := sha256.Sum256([]byte(string(caller.Domain) + "\x00" + caller.SubjectID + "\x00" + caller.ConsumptionDigest + "\x00" + string(choice.AccountKey)))
	return CapabilityFinalScopeCoreV1{
		SchemaVersion: 1, RouteID: "responses", Provider: "codex", TransportKind: transport,
		ProductSurface: string(caller.Domain), AccessPath: "responses", AuthMode: "oauth",
		RequestedModel: protocol.Model, EffectiveModel: choice.EffectiveModel, OutboundModel: choice.EffectiveModel,
		TransformationDigest: hex.EncodeToString(transformationDigest[:]), EncodedRequestDigest: hex.EncodeToString(encodedDigest[:]),
		NormalCredentialOriginBindingDigest: hex.EncodeToString(originDigest[:]),
	}
}

func codexCapabilityAccountWorkspaces(inventory codex.Inventory, allowed []codex.AccountKey) []CapabilityAccountWorkspaceV1 {
	set := make(map[codex.AccountKey]struct{}, len(allowed))
	for _, account := range allowed {
		set[account] = struct{}{}
	}
	workspaces := make([]CapabilityAccountWorkspaceV1, 0, len(allowed))
	for _, account := range inventory.Accounts {
		if _, ok := set[account.Key]; ok && account.Identity.AccountID != "" {
			workspaces = append(workspaces, CapabilityAccountWorkspaceV1{AccountKey: account.Key, Workspace: account.Identity.AccountID})
		}
	}
	sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].AccountKey < workspaces[j].AccountKey })
	return workspaces
}

func filterCodexHTTPRequestInventory(inventory codex.Inventory, allowed []codex.AccountKey) codex.Inventory {
	set := make(map[codex.AccountKey]struct{}, len(allowed))
	for _, account := range allowed {
		set[account] = struct{}{}
	}
	filtered := inventory
	filtered.Accounts = nil
	for _, account := range inventory.Accounts {
		if _, ok := set[account.Key]; ok {
			filtered.Accounts = append(filtered.Accounts, account)
		}
	}
	return filtered
}

// planWebSocketPrewarm selects one memory-only account order. Prewarm carries
// no turn identity and therefore must not create or mutate durable lease state.
func (factory *CodexHTTPRequestPlanFactory) planWebSocketPrewarm(ctx context.Context, input CodexHTTPRequestPlanInput) (CodexFrozenDispatchPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if factory == nil || factory.Inventory == nil {
		return CodexFrozenDispatchPlan{}, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanUnavailable, nil)
	}
	protocol, err := ParseCodexProtocolRequest(input.Encoded, "", nil)
	if err != nil {
		return CodexFrozenDispatchPlan{}, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	protocol, err = validateCodexWSPrewarmRequest(input.Encoded, protocol)
	if err != nil {
		return CodexFrozenDispatchPlan{}, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	inventory, err := factory.Inventory.List(ctx)
	if err != nil {
		return CodexFrozenDispatchPlan{}, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInventory, err)
	}
	now := time.Now()
	if factory.Now != nil {
		now = factory.Now()
	}
	accounts := codexHTTPRequestPlanAccountKeys(inventory)
	caller, _ := runtimeCallerAuthority(ctx)
	decision, policyErr := enforceSessionPolicy(factory.SessionPolicy, caller, []byte(protocol.Metadata.Metadata.SessionID), accounts, factory.PinnedAccountKey, now)
	if policyErr != nil {
		return CodexFrozenDispatchPlan{}, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, policyErr)
	}
	if decision.Status == PolicyDecisionSelected {
		inventory = filterCodexHTTPRequestInventory(inventory, decision.Allowed)
	}
	dispatch, err := factory.buildDispatch(ctx, CodexFrozenDispatchInput{
		Inventory:         inventory,
		Capacity:          factory.Capacity,
		Requirements:      codexHTTPRequestPlanRequirements(protocol),
		DefaultAccountKey: factory.DefaultAccountKey,
		BoundAccountKey:   factory.PinnedAccountKey,
		AcceptedRevision:  input.AcceptedRevision,
		Now:               now,
	})
	if err != nil {
		return dispatch, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, err)
	}
	if len(dispatch.Accounts()) == 0 {
		return dispatch, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, dispatch.TerminalError())
	}
	return dispatch, nil
}

func (factory *CodexHTTPRequestPlanFactory) beginWebSocketPrewarm(input CodexHTTPRequestPlanInput) (CodexPrewarmReservation, error) {
	if factory == nil {
		return CodexPrewarmReservation{}, ErrCodexLeaseWriterUnavailable
	}
	coordinator, ok := factory.Routes.(*CodexContinuityCoordinator)
	if !ok || coordinator == nil || coordinator.prewarms == nil {
		return CodexPrewarmReservation{}, ErrCodexLeaseWriterUnavailable
	}
	protocol, err := ParseCodexProtocolRequest(input.Encoded, "", nil)
	if err != nil {
		return CodexPrewarmReservation{}, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	protocol, err = validateCodexWSPrewarmRequest(input.Encoded, protocol)
	if err != nil {
		return CodexPrewarmReservation{}, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInspect, err)
	}
	digest := sha256.Sum256(input.Encoded)
	return coordinator.prewarms.Create(protocol.Metadata.Metadata, hex.EncodeToString(digest[:]))
}

func (factory *CodexHTTPRequestPlanFactory) bindWebSocketPrewarm(reservation CodexPrewarmReservation, account codex.AccountKey, downstreamGeneration, upstreamGeneration uint64) (CodexPrewarmReservation, error) {
	if factory == nil {
		return CodexPrewarmReservation{}, ErrCodexLeaseWriterUnavailable
	}
	coordinator, ok := factory.Routes.(*CodexContinuityCoordinator)
	if !ok || coordinator == nil || coordinator.prewarms == nil || reservation.State != CodexPrewarmCreating {
		return CodexPrewarmReservation{}, ErrCodexLeaseWriterUnavailable
	}
	return coordinator.prewarms.BindSockets(reservation.Lane, account, downstreamGeneration, upstreamGeneration)
}

func (factory *CodexHTTPRequestPlanFactory) readyWebSocketPrewarm(reservation CodexPrewarmReservation, responseAnchor, turnState string) (CodexPrewarmReservation, error) {
	if factory == nil {
		return CodexPrewarmReservation{}, ErrCodexLeaseWriterUnavailable
	}
	coordinator, ok := factory.Routes.(*CodexContinuityCoordinator)
	if !ok || coordinator == nil || coordinator.prewarms == nil || reservation.State != CodexPrewarmBoundActive {
		return CodexPrewarmReservation{}, ErrCodexLeaseWriterUnavailable
	}
	return coordinator.prewarms.Ready(reservation.Lane, responseAnchor, turnState)
}

func (factory *CodexHTTPRequestPlanFactory) cancelWebSocketPrewarm(reservation CodexPrewarmReservation) error {
	if factory == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	coordinator, ok := factory.Routes.(*CodexContinuityCoordinator)
	if !ok || coordinator == nil || coordinator.prewarms == nil || reservation.Lane == (LaneKey{}) || reservation.Correlation == "" {
		return ErrCodexLeaseWriterUnavailable
	}
	return coordinator.prewarms.cancel(reservation.Lane, reservation.Correlation)
}

func (factory *CodexHTTPRequestPlanFactory) adoptWebSocketPrewarm(ctx context.Context, input CodexHTTPRequestPlanInput, reservation CodexPrewarmReservation, revalidate CodexPrewarmAdoptionRevalidator) (CodexPreparedHTTPRequest, error) {
	var result CodexPreparedHTTPRequest
	if factory == nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanUnavailable, nil)
	}
	runtime, ok := factory.Runtime.(codexWebSocketPrewarmRuntime)
	if factory.Inventory == nil || !ok || runtime == nil || reservation.State != CodexPrewarmReady || revalidate == nil {
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
	if !protocol.Metadata.Strong || key.Lane != reservation.Lane || protocol.PreviousResponseID != reservation.ResponseAnchor ||
		(metadata.RequestKind != CodexRequestTurn && !(metadata.RequestKind == CodexRequestCompaction && metadata.CompactionPhase == CodexCompactionPreTurn)) {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCodexContinuity)
	}
	inventory, err := factory.Inventory.List(ctx)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanInventory, err)
	}
	accounts := codexHTTPRequestPlanAccountKeys(inventory)
	now := time.Now()
	if factory.Now != nil {
		now = factory.Now()
	}
	dispatch, err := factory.buildDispatch(ctx, CodexFrozenDispatchInput{
		Inventory:         inventory,
		Capacity:          factory.Capacity,
		Requirements:      codexHTTPRequestPlanRequirements(protocol),
		DefaultAccountKey: factory.DefaultAccountKey,
		BoundAccountKey:   reservation.AccountKey,
		AcceptedRevision:  input.AcceptedRevision,
		Now:               now,
	})
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, err)
	}
	dispatchAccounts := dispatch.Accounts()
	if len(dispatchAccounts) != 1 || dispatchAccounts[0].Choice().AccountKey != reservation.AccountKey {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, ErrCodexContinuity)
	}
	choice := dispatchAccounts[0].Choice()
	frozen, err := factory.freeze(ctx, inspection, choice)
	if err != nil {
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanFreeze, err)
	}
	httpSlots := CodexHTTPAttemptSlots(dispatch)
	slots := make([]CodexPrewarmAttemptSlot, len(httpSlots))
	for index, slot := range httpSlots {
		slots[index] = CodexPrewarmAttemptSlot{AccountKey: slot.AccountKey, CandidateID: slot.CandidateID, Kind: slot.Kind}
	}
	handle, err := runtime.adoptWebSocketPrewarmContext(ctx, accounts, CodexPrewarmAdoptionRequest{
		Key:                        key,
		Policy:                     factory.Authority,
		Choice:                     choice,
		AttemptSlots:               slots,
		RequestKind:                metadata.RequestKind,
		CompactionPhase:            metadata.CompactionPhase,
		Correlation:                reservation.Correlation,
		ResponseAnchor:             reservation.ResponseAnchor,
		TurnState:                  reservation.TurnState,
		ReservationGeneration:      reservation.Generation,
		DownstreamSocketGeneration: reservation.DownstreamSocketGeneration,
		UpstreamSocketGeneration:   reservation.UpstreamSocketGeneration,
		Revalidate:                 revalidate,
	})
	if err != nil || handle == nil {
		frozen.Release()
		return result, newCodexHTTPRequestPlanError(CodexHTTPRequestPlanBegin, err)
	}
	result.Dispatch = dispatch
	result.Frozen = frozen
	result.leaseHandle = handle
	result.Lifecycle = NewCodexHTTPRequestLifecycle(handle)
	result.receipt = factory.registerCodexTurnReceipt(protocol, SessionPolicyDecision{}, reservation.AccountKey, dispatchAccounts[0], codexNoAffinityShadowResult{Comparison: CodexTurnReceiptShadowNotApplicable})
	result.Lifecycle = wrapCodexTurnReceiptLifecycle(result.Lifecycle, result.receipt)
	return result, nil
}

const codexGPT56PromptCacheFairnessFloor = 30 * time.Minute

func codexHTTPRequestTaskAffinityAccounts(snapshot CodexLeaseRouteSnapshot, protocol CodexProtocolRequest, now time.Time) (codex.AccountKey, codex.AccountKey, error) {
	affinityPresent := snapshot.AffinityPresent || snapshot.AffinityAccountKey != ""
	requiresContinuity := snapshot.AffinityRequiresAccount || protocol.PreviousResponseID != "" || protocol.HasTurnState || protocol.HasEncryptedState
	if requiresContinuity {
		account := snapshot.BoundAccountKey
		if account == "" {
			account = snapshot.AffinityAccountKey
		}
		if account == "" {
			return "", "", ErrCodexLeaseAuthorityMismatch
		}
		return "", account, nil
	}
	if !affinityPresent {
		return "", "", nil
	}
	if codexGPT56PromptCacheFairnessEligible(snapshot.AffinityEffectiveModel, snapshot.AffinityCacheAdmittedAt, now) {
		return "", "", nil
	}
	if snapshot.AffinityAccountKey == "" {
		return "", "", nil
	}
	return snapshot.AffinityAccountKey, "", nil
}

func codexAuthenticatedCallerAccount(inventory codex.Inventory, caller RuntimeCallerAuthorityV1, identity string) codex.AccountKey {
	if caller.Domain != NormalCallerCodex {
		return ""
	}
	parts := strings.Split(identity, "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	accountKey := codex.AccountKey(parts[0])
	candidateID := codex.CandidateID(parts[1])
	for _, account := range inventory.Accounts {
		if account.Key != accountKey || !account.Routable || account.Unstable {
			continue
		}
		for _, candidate := range account.Candidates {
			if candidate.Ref.AccountKey == accountKey && candidate.Ref.CandidateID == candidateID && candidate.Routable && !candidate.DispatchBlocked {
				return accountKey
			}
		}
	}
	return ""
}

func codexGPT56PromptCacheModel(model string) bool {
	return ParseModel(model) == "gpt-5.6-sol"
}

func codexGPT56PromptCacheFairnessEligible(model string, admittedAt, now time.Time) bool {
	return codexGPT56PromptCacheModel(model) && !admittedAt.IsZero() && !now.Before(admittedAt) && !now.Before(admittedAt.Add(codexGPT56PromptCacheFairnessFloor))
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

func codexHTTPRequestLeasePlan(key LeaseKey, accounts []codex.AccountKey, authority CodexLeaseAuthorityPolicy, protocol CodexProtocolRequest, choice RouteChoice, dispatch CodexFrozenDispatchPlan, expected *CodexLeaseBoundExpectation, requiresAccountContinuity, authenticatedCallerContinuity bool, dispatchPermitDigest string) CodexLeaseRequestPlan {
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
		Key:                           key,
		Accounts:                      append([]codex.AccountKey(nil), accounts...),
		Authority:                     cloneCodexLeaseAuthorityPolicy(authority),
		RequestKind:                   metadata.RequestKind,
		CompactionPhase:               metadata.CompactionPhase,
		RequestedModel:                protocol.Model,
		EffectiveModel:                choice.EffectiveModel,
		RequiredBuckets:               append([]CapacityBucket(nil), choice.RequiredBuckets...),
		Slots:                         slots,
		InitialSlot:                   1,
		ExpectedBound:                 expectedClone,
		RequiresAccountContinuity:     requiresAccountContinuity,
		authenticatedCallerContinuity: authenticatedCallerContinuity,
		DispatchPermitDigest:          dispatchPermitDigest,
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
