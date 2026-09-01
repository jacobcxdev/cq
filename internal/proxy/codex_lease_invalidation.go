package proxy

import (
	"context"
	"fmt"
	"math"
)

type CodexLeaseInvalidationResult struct {
	InvalidatedLeases int    `json:"invalidated_leases"`
	JournalGeneration uint64 `json:"journal_generation"`
}

// InvalidateTaskAffinities durably suppresses reusable account affinity from
// earlier admissions. Active request authority and required continuity remain
// intact; later portable requests select from current account capacity again.
func (coordinator *CodexContinuityCoordinator) InvalidateTaskAffinities(ctx context.Context) (CodexLeaseInvalidationResult, error) {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil {
		return CodexLeaseInvalidationResult{}, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return CodexLeaseInvalidationResult{}, fmt.Errorf("%w: nil affinity invalidation context", ErrCodexLeaseInvalidMutation)
	}
	release, err := coordinator.beginCodexLeaseRouteSnapshot(ctx)
	if err != nil {
		return CodexLeaseInvalidationResult{}, err
	}
	defer release()
	return coordinator.store.invalidateTaskAffinities()
}

func (store *CodexLeaseStore) invalidateTaskAffinities() (CodexLeaseInvalidationResult, error) {
	operation, err := store.beginOperation()
	if err != nil {
		return CodexLeaseInvalidationResult{}, err
	}
	defer operation.Release()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.v2 == nil {
		return CodexLeaseInvalidationResult{}, ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return CodexLeaseInvalidationResult{}, fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return CodexLeaseInvalidationResult{}, ErrCodexLegacyQuarantine
	}
	if store.v2.Generation == math.MaxUint64 {
		return CodexLeaseInvalidationResult{}, fmt.Errorf("%w: journal generation overflow", ErrCodexLeaseInvalidMutation)
	}

	invalidated := 0
	for _, lane := range store.v2.Lanes {
		if codexLaneAffinityIsZero(lane) || codexLaneAffinityJournalGeneration(lane) <= store.v2.AffinityInvalidationGeneration || codexLeaseLaneAffinityRequiresAccount(*store.v2, lane) {
			continue
		}
		invalidated++
	}
	next := cloneCodexLeaseV2Envelope(*store.v2)
	next.AffinityInvalidationGeneration = next.Generation + 1
	if err := store.commitV2Locked(next.Generation, next); err != nil {
		return CodexLeaseInvalidationResult{}, err
	}
	return CodexLeaseInvalidationResult{InvalidatedLeases: invalidated, JournalGeneration: store.v2.Generation}, nil
}

func codexLeaseLaneAffinityRequiresAccount(envelope codexLeaseJournalEnvelopeV2, lane CodexJournalLane) bool {
	if codexLaneAffinityIsZero(lane) {
		return false
	}
	source := CodexJournalRecordIdentity{
		LaneDigest:    codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash),
		TurnDigest:    lane.LastAdmittedTurnHash,
		ModeEpoch:     lane.LastAdmittedModeEpoch,
		Authoritative: lane.LastAdmittedAuthoritative,
	}
	for _, record := range envelope.Records {
		if record.Identity() == source {
			return record.HasEncryptedState
		}
	}
	return true
}
