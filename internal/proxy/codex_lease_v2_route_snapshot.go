package proxy

import (
	"context"
	"fmt"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexLeaseRouteSnapshot is a detached, journal-generation-fenced view of
// lease-owned routing state. The accepted credential revision is deliberately
// absent: callers own that inventory revision and must combine it explicitly.
type CodexLeaseRouteSnapshot struct {
	BoundAccountKey    codex.AccountKey
	AffinityAccountKey codex.AccountKey
	Provisional        map[codex.AccountKey]int
	JournalGeneration  uint64
}

// LoadRouteSnapshot returns the requested lane's bound and affinity accounts
// together with global provisional route counts from the same signed journal
// generation.
func (coordinator *CodexContinuityCoordinator) LoadRouteSnapshot(ctx context.Context, key LeaseKey, accounts []codex.AccountKey, policy CodexLeaseAuthorityPolicy) (CodexLeaseRouteSnapshot, error) {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil || coordinator.leases.lifecycle == nil || coordinator.leases.mu == nil {
		return CodexLeaseRouteSnapshot{}, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return CodexLeaseRouteSnapshot{}, fmt.Errorf("%w: nil route snapshot context", ErrCodexLeaseInvalidMutation)
	}
	release, err := coordinator.beginCodexLeaseRouteSnapshot(ctx)
	if err != nil {
		return CodexLeaseRouteSnapshot{}, err
	}
	defer release()

	for {
		if err := ctx.Err(); err != nil {
			return CodexLeaseRouteSnapshot{}, err
		}
		restored, err := coordinator.store.LoadLane(key, accounts, policy)
		if err != nil {
			return CodexLeaseRouteSnapshot{}, err
		}
		provisional, matched, err := coordinator.store.loadCodexLeaseProvisionalRouteCounts(restored.Fence.Journal, accounts)
		if err != nil {
			return CodexLeaseRouteSnapshot{}, err
		}
		if !matched {
			continue
		}
		if err := ctx.Err(); err != nil {
			return CodexLeaseRouteSnapshot{}, err
		}

		snapshot := CodexLeaseRouteSnapshot{
			Provisional:       provisional,
			JournalGeneration: restored.Fence.Journal,
		}
		if restored.Affinity != nil && restored.Affinity.Resolved {
			snapshot.AffinityAccountKey = restored.Affinity.AccountKey
		}
		if restored.Classification == CodexRestoredLaneCurrent {
			for _, record := range restored.ResolvedRecords {
				if record.Identity != restored.Fence.Current || !record.Record.EverAdmitted {
					continue
				}
				if record.AccountKey == "" {
					return CodexLeaseRouteSnapshot{}, fmt.Errorf("%w: persisted bound account is unavailable", ErrCodexLeaseAuthorityMismatch)
				}
				snapshot.BoundAccountKey = record.AccountKey
				break
			}
		}
		return snapshot, nil
	}
}

func (coordinator *CodexContinuityCoordinator) beginCodexLeaseRouteSnapshot(ctx context.Context) (func(), error) {
	lifecycle := coordinator.leases.lifecycle
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
	coordinator.leases.mu.Lock()
	unavailable := coordinator.leases.writerUnavailableLocked()
	coordinator.leases.mu.Unlock()
	if unavailable {
		lifecycle.persistence.Unlock()
		return nil, ErrCodexLeaseWriterUnavailable
	}
	return lifecycle.persistence.Unlock, nil
}

func (store *CodexLeaseStore) loadCodexLeaseProvisionalRouteCounts(expectedGeneration uint64, accounts []codex.AccountKey) (map[codex.AccountKey]int, bool, error) {
	operation, err := store.beginOperation()
	if err != nil {
		return nil, false, err
	}
	defer operation.Release()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil, false, ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if err := store.revalidateV2InstalledLocked(); err != nil {
		store.poisoned = err
		return nil, false, err
	}
	if store.v2 == nil {
		return nil, false, fmt.Errorf("%w: schema-v2 journal unavailable", ErrCodexLeaseTrustLost)
	}
	if store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return nil, false, ErrCodexLegacyQuarantine
	}
	if err := validateCodexLeaseRepresentedModes(representedCodexLeaseAuthoritativeEpochs(store.v2.Records), store.modes); err != nil {
		return nil, false, err
	}
	if store.v2.Generation != expectedGeneration {
		return nil, false, nil
	}

	current := make(map[CodexJournalRecordIdentity]struct{}, len(store.v2.Lanes))
	for _, lane := range store.v2.Lanes {
		identity := codexLaneTupleIdentity(lane, true)
		if !identity.IsZero() {
			current[identity] = struct{}{}
		}
	}
	provisional := make(map[codex.AccountKey]int)
	for _, record := range store.v2.Records {
		if _, ok := current[record.Identity()]; !ok || record.EverAdmitted || record.State != LeaseProvisional || record.RoutingRefs != 1 {
			continue
		}
		attempt := codexLeaseCurrentAttemptState(record)
		if attempt != CodexAttemptPrepared && attempt != CodexAttemptDispatched {
			continue
		}
		account, resolved := store.resolveCodexLeaseAccount(record.AccountHash, accounts)
		if !resolved {
			return nil, false, fmt.Errorf("%w: active provisional account is unavailable", ErrCodexLeaseAuthorityMismatch)
		}
		provisional[account]++
	}
	return provisional, true, nil
}
