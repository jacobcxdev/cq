package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexPrewarmAttemptSlot is a secret-free slot in the first real request's
// frozen attempt envelope. The store hashes both identifiers before writing.
type CodexPrewarmAttemptSlot struct {
	AccountKey  codex.AccountKey
	CandidateID codex.CandidateID
	Kind        CodexAttemptSlotKind
}

type CodexPrewarmAdoptionFence struct {
	ReservationGeneration      uint64
	DownstreamSocketGeneration uint64
	UpstreamSocketGeneration   uint64
}

// CodexPrewarmAdoptionRevalidator must verify that the selected account is
// still routable and both physical socket generations are still live. It runs
// under the account gate, coordinator lifecycle lock, and reservation lock;
// implementations must not re-enter those owners.
type CodexPrewarmAdoptionRevalidator func(context.Context, codex.AccountKey, CodexPrewarmAdoptionFence) error

// CodexPrewarmAdoptionRequest carries the exact in-memory reservation fence
// and the first real request. Raw correlation and identity values are never
// copied into the durable journal.
type CodexPrewarmAdoptionRequest struct {
	Key                        LeaseKey
	Policy                     CodexLeaseAuthorityPolicy
	Choice                     RouteChoice
	AttemptSlots               []CodexPrewarmAttemptSlot
	RequestKind                CodexRequestKind
	CompactionPhase            CodexCompactionPhase
	Correlation                string
	ResponseAnchor             string
	TurnState                  string
	ReservationGeneration      uint64
	DownstreamSocketGeneration uint64
	UpstreamSocketGeneration   uint64
	Revalidate                 CodexPrewarmAdoptionRevalidator
}

type CodexPrewarmAdoptionResult struct {
	Record CodexJournalRecordV2
	Fence  CodexLeaseGenerationFence
}

func (coordinator *CodexContinuityCoordinator) AdoptPrewarm(ctx context.Context, request CodexPrewarmAdoptionRequest) (CodexPrewarmAdoptionResult, error) {
	if ctx == nil {
		return CodexPrewarmAdoptionResult{}, fmt.Errorf("%w: nil prewarm adoption context", ErrCodexLeaseInvalidMutation)
	}
	request = cloneCodexPrewarmAdoptionRequest(request)
	if err := validateCodexPrewarmAdoptionRequest(coordinator, request); err != nil {
		return CodexPrewarmAdoptionResult{}, err
	}
	gate, err := coordinator.leases.accountGates.acquire(ctx, request.Choice.AccountKey)
	if err != nil {
		return CodexPrewarmAdoptionResult{}, err
	}
	defer gate.Release()

	coordinator.leases.lifecycle.persistence.Lock()
	defer coordinator.leases.lifecycle.persistence.Unlock()
	if coordinator.leases.writerUnavailable() {
		return CodexPrewarmAdoptionResult{}, ErrCodexLeaseWriterUnavailable
	}

	manager := coordinator.prewarms
	manager.mu.Lock()
	defer manager.mu.Unlock()
	reservation := manager.reservations[request.Key.Lane]
	if !matchesCodexPrewarmAdoptionReservation(reservation, request) {
		return CodexPrewarmAdoptionResult{}, fmt.Errorf("%w: prewarm adoption fence mismatch", ErrCodexContinuity)
	}
	if err := ctx.Err(); err != nil {
		return CodexPrewarmAdoptionResult{}, err
	}
	fence := CodexPrewarmAdoptionFence{
		ReservationGeneration:      request.ReservationGeneration,
		DownstreamSocketGeneration: request.DownstreamSocketGeneration,
		UpstreamSocketGeneration:   request.UpstreamSocketGeneration,
	}
	if err := request.Revalidate(ctx, request.Choice.AccountKey, fence); err != nil {
		return CodexPrewarmAdoptionResult{}, fmt.Errorf("revalidate prewarm adoption: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return CodexPrewarmAdoptionResult{}, err
	}

	operation, err := coordinator.store.beginOperation()
	if err != nil {
		return CodexPrewarmAdoptionResult{}, err
	}
	defer operation.Release()
	coordinator.store.mu.Lock()
	defer coordinator.store.mu.Unlock()

	result, err := coordinator.store.commitCodexPrewarmAdoptionLocked(request)
	if err != nil {
		return CodexPrewarmAdoptionResult{}, err
	}
	reservation.State = CodexPrewarmAdopted
	reservation.Generation++
	reservation.LastSeen = manager.now().UTC()
	return result, nil
}

func cloneCodexPrewarmAdoptionRequest(request CodexPrewarmAdoptionRequest) CodexPrewarmAdoptionRequest {
	clone := request
	clone.Policy.RetainedAuthoritativeEpochs = append([]uint64(nil), request.Policy.RetainedAuthoritativeEpochs...)
	clone.Choice.RequiredBuckets = append([]CapacityBucket(nil), request.Choice.RequiredBuckets...)
	clone.AttemptSlots = append([]CodexPrewarmAttemptSlot(nil), request.AttemptSlots...)
	return clone
}

func validateCodexPrewarmAdoptionRequest(coordinator *CodexContinuityCoordinator, request CodexPrewarmAdoptionRequest) error {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil || coordinator.leases.lifecycle == nil || coordinator.leases.accountGates == nil || coordinator.prewarms == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if err := request.Key.validate(); err != nil {
		return err
	}
	if err := validateCodexLeaseAuthorityPolicy(request.Policy); err != nil {
		return err
	}
	if !request.Policy.Authoritative {
		return fmt.Errorf("%w: adoption requires authoritative mode", ErrCodexLeaseAuthorityMismatch)
	}
	if request.RequestKind != CodexRequestTurn && !(request.RequestKind == CodexRequestCompaction && request.CompactionPhase == CodexCompactionPreTurn) {
		return fmt.Errorf("%w: adoption requires turn or pre-turn compaction", ErrCodexLeaseInvalidMutation)
	}
	if request.RequestKind == CodexRequestTurn && request.CompactionPhase != "" {
		return fmt.Errorf("%w: turn adoption has compaction phase", ErrCodexLeaseInvalidMutation)
	}
	if request.Choice.AccountKey == "" || strings.TrimSpace(request.Choice.RequestedModel) != request.Choice.RequestedModel || request.Choice.RequestedModel == "" || strings.TrimSpace(request.Choice.EffectiveModel) != request.Choice.EffectiveModel || request.Choice.EffectiveModel == "" || !validCodexLeaseBuckets(request.Choice.RequiredBuckets, request.Choice.EffectiveModel) {
		return fmt.Errorf("%w: incomplete adoption route", ErrCodexLeaseInvalidMutation)
	}
	if len(request.AttemptSlots) == 0 || len(request.AttemptSlots) > math.MaxUint32 {
		return fmt.Errorf("%w: empty adoption attempt plan", ErrCodexLeaseInvalidMutation)
	}
	for _, slot := range request.AttemptSlots {
		if slot.AccountKey != request.Choice.AccountKey || slot.CandidateID == "" || (slot.Kind != CodexAttemptSlotDirect && slot.Kind != CodexAttemptSlotEligibleManagedRefresh) {
			return fmt.Errorf("%w: foreign or invalid adoption attempt slot", ErrCodexLeaseInvalidMutation)
		}
	}
	if request.Correlation == "" || request.ResponseAnchor == "" || request.ReservationGeneration == 0 || request.DownstreamSocketGeneration == 0 || request.UpstreamSocketGeneration == 0 || request.Revalidate == nil {
		return fmt.Errorf("%w: incomplete adoption fence", ErrCodexLeaseInvalidMutation)
	}
	return nil
}

func matchesCodexPrewarmAdoptionReservation(reservation *CodexPrewarmReservation, request CodexPrewarmAdoptionRequest) bool {
	return reservation != nil && reservation.State == CodexPrewarmReady && reservation.Lane == request.Key.Lane &&
		reservation.AccountKey == request.Choice.AccountKey && reservation.Correlation == request.Correlation &&
		reservation.ResponseAnchor == request.ResponseAnchor && reservation.TurnState == request.TurnState &&
		reservation.Generation == request.ReservationGeneration &&
		reservation.DownstreamSocketGeneration == request.DownstreamSocketGeneration &&
		reservation.UpstreamSocketGeneration == request.UpstreamSocketGeneration
}

func (store *CodexLeaseStore) commitCodexPrewarmAdoptionLocked(request CodexPrewarmAdoptionRequest) (CodexPrewarmAdoptionResult, error) {
	if store.closed {
		return CodexPrewarmAdoptionResult{}, ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return CodexPrewarmAdoptionResult{}, fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if store.v2 == nil {
		return CodexPrewarmAdoptionResult{}, fmt.Errorf("%w: schema-v2 journal unavailable", ErrCodexLeaseTrustLost)
	}
	if err := store.revalidateV2InstalledLocked(); err != nil {
		store.poisoned = err
		return CodexPrewarmAdoptionResult{}, err
	}
	if store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return CodexPrewarmAdoptionResult{}, ErrCodexLegacyQuarantine
	}
	if !containsCodexLeaseEpoch(store.modes.RecognisedAuthoritativeEpochs, request.Policy.ModeEpoch) {
		return CodexPrewarmAdoptionResult{}, fmt.Errorf("%w: unrecognised adoption epoch", ErrCodexLeaseAuthorityMismatch)
	}
	if store.v2.Generation == math.MaxUint64 {
		return CodexPrewarmAdoptionResult{}, fmt.Errorf("%w: journal generation overflow", ErrCodexLeaseInvalidMutation)
	}

	next := cloneCodexLeaseV2Envelope(*store.v2)
	sessionHash := store.hash("session", request.Key.Lane.Session)
	threadHash := store.hash("thread", request.Key.Lane.Thread)
	namespaceHash := store.hash("namespace", request.Key.Lane.Namespace)
	turnHash := store.hash("turn", request.Key.Turn)
	laneDigest := codexJournalLaneDigest(sessionHash, threadHash, namespaceHash)
	var laneIndex = -1
	for index, lane := range next.Lanes {
		if codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash) == laneDigest {
			laneIndex = index
			if !codexLaneTupleIdentity(lane, true).IsZero() {
				return CodexPrewarmAdoptionResult{}, ErrCodexConcurrentTurn
			}
		}
	}
	for _, record := range next.Records {
		if record.Identity().LaneDigest == laneDigest && record.TurnHash == turnHash && record.ModeEpoch == request.Policy.ModeEpoch && record.Authoritative {
			return CodexPrewarmAdoptionResult{}, ErrCodexStaleTurn
		}
	}

	commitGeneration := next.Generation + 1
	wallNow := store.policy.Now().UTC()
	lane := CodexJournalLane{SessionHash: sessionHash, ThreadHash: threadHash, NamespaceHash: namespaceHash}
	if laneIndex >= 0 {
		lane = next.Lanes[laneIndex]
	}
	now := monotonicCodexLeaseTime(wallNow, lane.LastObservedAt)
	lane.Generation++
	lane.CurrentTurnHash = turnHash
	lane.CurrentModeEpoch = request.Policy.ModeEpoch
	lane.CurrentAuthoritative = true
	lane.LastTurnHash = turnHash
	lane.LastModeEpoch = request.Policy.ModeEpoch
	lane.LastAuthoritative = true
	lane.LastObservedAt = now

	accountHash := store.hash("account", string(request.Choice.AccountKey))
	slots := make([]CodexAttemptSlot, len(request.AttemptSlots))
	for index, input := range request.AttemptSlots {
		slots[index] = CodexAttemptSlot{Index: uint32(index + 1), AccountHash: accountHash, CandidateHash: store.hash("candidate", string(input.CandidateID)), Kind: input.Kind}
	}
	record := CodexJournalRecordV2{
		SessionHash: sessionHash, ThreadHash: threadHash, NamespaceHash: namespaceHash, TurnHash: turnHash,
		AccountHash: accountHash, CorrelationHash: store.hash("correlation", request.ResponseAnchor),
		RecordGeneration: 1, LaneGeneration: lane.Generation, LeaseGeneration: 1,
		ModeEpoch: request.Policy.ModeEpoch, DownstreamSocketGeneration: request.DownstreamSocketGeneration, UpstreamSocketGeneration: request.UpstreamSocketGeneration,
		State: LeaseProvisional, ProtocolSchema: CurrentCodexLeaseSchema, Authoritative: true,
		HasResponseAnchor: true, HasTurnState: request.TurnState != "", NonMigratable: true,
		AdoptedPrewarm: true, PrewarmAdoptionJournalGeneration: commitGeneration,
		CreatedAt: now, LastObservedAt: now,
		CodexCurrentRequest: CodexCurrentRequest{
			Generation: 1, RequestKind: request.RequestKind, CompactionPhase: request.CompactionPhase,
			RequestedModelHash: store.hash("requested-model", request.Choice.RequestedModel), EffectiveModel: request.Choice.EffectiveModel,
			RequiredBuckets:          append([]CapacityBucket(nil), request.Choice.RequiredBuckets...),
			AttemptEnvelope:          CodexAttemptEnvelope{PolicyVersion: CodexLeaseAttemptPolicyVersion, AttemptLimit: uint32(len(slots)), Slots: slots},
			CurrentAttemptGeneration: 1,
			RoutingRefs:              1,
			Attempts:                 []CodexJournalAttempt{{Generation: 1, Revision: 1, Slot: 1, State: CodexAttemptPrepared, CreatedAt: now, LastObservedAt: now}},
		},
	}
	if request.TurnState != "" {
		record.TurnStateHash = store.hash("turn-state", request.TurnState)
	}
	record.AttemptEnvelope.PlanDigest = codexLeaseAttemptPlanDigest(store.key, record.AttemptEnvelope.Slots)
	if laneIndex >= 0 {
		next.Lanes[laneIndex] = lane
	} else {
		next.Lanes = append(next.Lanes, lane)
	}
	next.Records = append(next.Records, record)
	if err := store.commitV2Locked(next.Generation, next); err != nil {
		return CodexPrewarmAdoptionResult{}, err
	}
	for _, installed := range store.v2.Records {
		if installed.Identity() == record.Identity() {
			fence := CodexLeaseGenerationFence{Journal: store.v2.Generation, Lane: lane.Generation, Current: installed.Identity(), Last: installed.Identity(), TouchedRecords: []CodexLeaseRecordFence{{Record: installed.Identity(), Revision: installed.RecordGeneration, Lease: installed.LeaseGeneration, RequestGeneration: installed.Generation, CurrentAttempt: installed.CurrentAttemptGeneration, TouchedAttempts: []CodexAttemptFence{{RequestGeneration: installed.Generation, Generation: 1, Revision: 1}}}}}
			return CodexPrewarmAdoptionResult{Record: cloneCodexJournalRecordV2(installed), Fence: fence}, nil
		}
	}
	return CodexPrewarmAdoptionResult{}, errors.New("committed prewarm adoption record missing")
}
