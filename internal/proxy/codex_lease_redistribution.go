package proxy

import (
	"context"
	"fmt"
	"math"
	"sort"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexLeaseRedistributionResult describes durable next-request scheduling.
type CodexLeaseRedistributionResult struct {
	ScheduledLeases   int    `json:"scheduled_leases"`
	DeferredLeases    int    `json:"deferred_leases"`
	JournalGeneration uint64 `json:"journal_generation"`
}

// RedistributeTaskAffinities schedules existing chats for a fresh route at a
// safe request boundary. It never changes currently dispatched work.
func (coordinator *CodexContinuityCoordinator) RedistributeTaskAffinities(ctx context.Context) (CodexLeaseRedistributionResult, error) {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil {
		return CodexLeaseRedistributionResult{}, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return CodexLeaseRedistributionResult{}, fmt.Errorf("%w: nil redistribution context", ErrCodexLeaseInvalidMutation)
	}
	release, err := coordinator.beginCodexLeasePersistenceContext(ctx)
	if err != nil {
		return CodexLeaseRedistributionResult{}, err
	}
	defer release()
	return coordinator.store.redistributeTaskAffinities("", "")
}

func (store *CodexLeaseStore) redistributeTaskAffinities(account codex.AccountKey, eventID string) (CodexLeaseRedistributionResult, error) {
	operation, err := store.beginOperation()
	if err != nil {
		return CodexLeaseRedistributionResult{}, err
	}
	defer operation.Release()
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.v2 == nil {
		return CodexLeaseRedistributionResult{}, ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return CodexLeaseRedistributionResult{}, fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return CodexLeaseRedistributionResult{}, ErrCodexLegacyQuarantine
	}
	if store.v2.Generation == math.MaxUint64 {
		return CodexLeaseRedistributionResult{}, fmt.Errorf("%w: journal generation overflow", ErrCodexLeaseInvalidMutation)
	}
	result := CodexLeaseRedistributionResult{JournalGeneration: store.v2.Generation}
	next := cloneCodexLeaseV2Envelope(*store.v2)
	changed := false
	if account != "" {
		accountHash, eventHash := store.hash("account", string(account)), store.hash("reset-redistribution", eventID)
		found := false
		for index := range next.ResetRedistributionEvents {
			event := &next.ResetRedistributionEvents[index]
			if event.AccountHash != accountHash {
				continue
			}
			if event.EventHash == eventHash {
				return result, nil
			}
			*event = codexResetRedistributionEvent{AccountHash: accountHash, EventHash: eventHash, Generation: next.Generation + 1}
			found = true
			break
		}
		if !found {
			if len(next.ResetRedistributionEvents) == 128 {
				sort.Slice(next.ResetRedistributionEvents, func(i, j int) bool {
					return next.ResetRedistributionEvents[i].Generation < next.ResetRedistributionEvents[j].Generation
				})
				next.ResetRedistributionEvents = next.ResetRedistributionEvents[1:]
			}
			next.ResetRedistributionEvents = append(next.ResetRedistributionEvents, codexResetRedistributionEvent{AccountHash: accountHash, EventHash: eventHash, Generation: next.Generation + 1})
		}
		changed = true
	}
	for index := range next.Lanes {
		lane := &next.Lanes[index]
		if codexLaneAffinityIsZero(*lane) && lane.CurrentTurnHash == "" && lane.LastTurnHash == "" {
			continue
		}
		result.ScheduledLeases++
		if codexLeaseLaneAffinityRequiresAccount(next, *lane) {
			result.DeferredLeases++
		}
		if pending := codexLaneRedistributionPending(*lane); pending != 0 {
			inUse := false
			for _, record := range next.Records {
				if codexLeaseRecordInLane(record, *lane) && codexLeaseLaneCurrentMatchesRecord(*lane, record) && record.RedistributionGeneration == pending && !codexLeaseRuntimeCanBeginRequest(record) {
					inUse = true
					break
				}
			}
			if !inUse {
				continue
			}
		}
		lane.RedistributionRequestedGeneration = next.Generation + 1
		changed = true
	}
	if !changed {
		return result, nil
	}
	if err := store.commitV2Locked(next.Generation, next); err != nil {
		return CodexLeaseRedistributionResult{}, err
	}
	result.JournalGeneration = store.v2.Generation
	return result, nil
}

func codexLaneRedistributionPending(lane CodexJournalLane) uint64 {
	if lane.RedistributionRequestedGeneration > lane.RedistributionCompletedGeneration {
		return lane.RedistributionRequestedGeneration
	}
	return 0
}

func (runtime *CodexLeaseRuntime) validateRedistributionPlan(restored CodexRestoredLane, identity CodexJournalRecordIdentity, plan CodexLeaseRequestPlan) (bool, error) {
	if plan.RedistributionGeneration == 0 {
		return false, nil
	}
	current, found := runtime.restoredRecord(restored, identity)
	if restored.Classification == CodexRestoredLaneUnseen {
		current, found = runtime.restoredRecord(restored, restored.Fence.Last)
	}
	_, failedHead := codexRestoredLaneRestartableFailedHead(restored)
	if !found || (restored.Classification != CodexRestoredLaneCurrent && restored.Classification != CodexRestoredLaneUnseen && !failedHead) || plan.RedistributionGeneration != codexLaneRedistributionPending(restored.Lane) ||
		plan.RequestKind != CodexRequestTurn || plan.Evidence.PreviousResponseID != "" || plan.Evidence.HasTurnState || plan.RequiresAccountContinuity || plan.authenticatedCallerContinuity ||
		!codexLeaseSafeRedistributionSource(current.Record) {
		return false, fmt.Errorf("%w: unsafe redistribution request", ErrCodexLeaseInvalidMutation)
	}
	return true, nil
}

func codexLeaseRedistributionBeginRequest(old, desired CodexJournalRecordV2, lane CodexJournalLane) bool {
	return desired.RedistributionGeneration != 0 && desired.RedistributionGeneration == codexLaneRedistributionPending(lane) &&
		(codexLeaseSafeRedistributionSource(old) || (old.Generation == 0 && old.State == LeaseReserving)) && desired.RequestKind == CodexRequestTurn && !desired.NonMigratable
}

// Unadmitted work can move only after a definitive rejection or abandonment;
// indeterminate dispatch remains owned by its original account.
func codexLeaseSafeRedistributionSource(record CodexJournalRecordV2) bool {
	return codexLeaseRuntimeCanBeginRequest(record) &&
		(record.EverAdmitted || codexLeaseRestartableFailedHead(record) || codexLeaseAbandonedUnadmittedHead(record))
}

// RedistributeTaskAffinitiesForReset deduplicates confirmed reset events before
// scheduling the same safe next-request route refresh used by manual control.
func (coordinator *CodexContinuityCoordinator) RedistributeTaskAffinitiesForReset(ctx context.Context, account codex.AccountKey, eventID string) (CodexLeaseRedistributionResult, error) {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil {
		return CodexLeaseRedistributionResult{}, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil || account == "" || eventID == "" {
		return CodexLeaseRedistributionResult{}, fmt.Errorf("%w: incomplete reset redistribution", ErrCodexLeaseInvalidMutation)
	}
	release, err := coordinator.beginCodexLeasePersistenceContext(ctx)
	if err != nil {
		return CodexLeaseRedistributionResult{}, err
	}
	defer release()
	return coordinator.store.redistributeTaskAffinities(account, eventID)
}

type codexResetRedistributionEvent struct {
	AccountHash string `json:"account_hash"`
	EventHash   string `json:"event_hash"`
	Generation  uint64 `json:"generation"`
}

// redistributionTurnState recognises the current account's latched state only
// after its request has drained and an explicit redistribution is pending.
func (runtime *CodexLeaseRuntime) redistributionTurnState(ctx context.Context, key LeaseKey, authority CodexLeaseAuthorityPolicy, state string) (bool, error) {
	if runtime == nil || runtime.store == nil {
		return false, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return false, fmt.Errorf("%w: nil redistribution context", ErrCodexLeaseInvalidMutation)
	}
	restored, err := runtime.store.loadLaneForIngress(key, authority)
	if err != nil {
		return false, err
	}
	current, found := runtime.restoredRecord(restored, restored.Fence.Current)
	return ctx.Err() == nil && codexLaneRedistributionPending(restored.Lane) != 0 && found && current.Record.HasTurnState && codexLeaseRuntimeCanBeginRequest(current.Record) && constantTimeCodexLeaseDigestEqual(current.Record.TurnStateHash, runtime.store.hash("turn-state", state)), ctx.Err()
}
