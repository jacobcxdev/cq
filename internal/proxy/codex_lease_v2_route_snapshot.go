package proxy

import (
	"context"
	"fmt"
	"sort"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexLeaseRouteSnapshot is a detached, journal-generation-fenced view of
// lease-owned routing state. The accepted credential revision is deliberately
// absent: callers own that inventory revision and must combine it explicitly.
type CodexLeaseRouteSnapshot struct {
	Classification            CodexRestoredLaneClassification
	BoundAccountKey           codex.AccountKey
	BoundIdentity             CodexJournalRecordIdentity
	BoundRecordGeneration     uint64
	BoundChoice               RouteChoice
	BoundRequiresAccount      bool
	HistoricalAuthoritative   bool
	RestartableFailedHead     bool
	AffinityPresent           bool
	AffinityInvalidated       bool
	AffinityAccountKey        codex.AccountKey
	AffinityCacheAdmittedAt   time.Time
	AffinityEffectiveModel    string
	AffinityRequiresAccount   bool
	UnavailableAccountKeys    []codex.AccountKey
	QuotaExhaustedAccountKeys []codex.AccountKey
	Provisional               map[codex.AccountKey]int
	JournalGeneration         uint64
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
	policy = cloneCodexLeaseAuthorityPolicy(policy)
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
			Classification:    restored.Classification,
			Provisional:       provisional,
			JournalGeneration: restored.Fence.Journal,
		}
		_, snapshot.RestartableFailedHead = codexRestoredLaneRestartableFailedHead(restored)
		unavailable := make(map[codex.AccountKey]struct{})
		quotaUnavailable := make(map[codex.AccountKey]struct{})
		if restored.RequestedIdentity == codexLaneRequestScopeIdentity(restored.Lane) {
			for _, accountHash := range restored.Lane.RequestUnavailableAccountHashes {
				account, resolved := coordinator.store.resolveCodexLeaseAccount(accountHash, accounts)
				if !resolved {
					continue
				}
				unavailable[account] = struct{}{}
				snapshot.UnavailableAccountKeys = append(snapshot.UnavailableAccountKeys, account)
			}
		}
		for _, accountHash := range restored.Lane.QuotaExhaustedAccountHashes {
			account, resolved := coordinator.store.resolveCodexLeaseAccount(accountHash, accounts)
			if !resolved {
				continue
			}
			if _, found := unavailable[account]; !found {
				unavailable[account] = struct{}{}
				snapshot.UnavailableAccountKeys = append(snapshot.UnavailableAccountKeys, account)
			}
			quotaUnavailable[account] = struct{}{}
			snapshot.QuotaExhaustedAccountKeys = append(snapshot.QuotaExhaustedAccountKeys, account)
		}
		sort.Slice(snapshot.QuotaExhaustedAccountKeys, func(i, j int) bool {
			return snapshot.QuotaExhaustedAccountKeys[i] < snapshot.QuotaExhaustedAccountKeys[j]
		})
		sort.Slice(snapshot.UnavailableAccountKeys, func(i, j int) bool { return snapshot.UnavailableAccountKeys[i] < snapshot.UnavailableAccountKeys[j] })
		if restored.Affinity != nil {
			snapshot.AffinityPresent = true
			snapshot.AffinityCacheAdmittedAt = restored.Affinity.CacheAdmittedAt
			snapshot.AffinityEffectiveModel = restored.Affinity.CacheEffectiveModel
			sourceFound := false
			for _, record := range restored.ResolvedRecords {
				if record.Identity == restored.Affinity.Source {
					snapshot.AffinityRequiresAccount = codexLeaseRecordRequiresAccount(record.Record)
					sourceFound = true
					break
				}
			}
			if !sourceFound {
				snapshot.AffinityRequiresAccount = true
			}
			if restored.Affinity.Resolved {
				snapshot.AffinityAccountKey = restored.Affinity.AccountKey
				if _, blocked := quotaUnavailable[snapshot.AffinityAccountKey]; blocked {
					snapshot.AffinityAccountKey = ""
					snapshot.AffinityRequiresAccount = false
				}
			}
			snapshot.AffinityInvalidated = codexLaneAffinityJournalGeneration(restored.Lane) <= restored.AffinityInvalidationGeneration && !snapshot.AffinityRequiresAccount
			if snapshot.AffinityInvalidated {
				snapshot.AffinityAccountKey = ""
			}
		}
		if restored.Classification == CodexRestoredLaneCurrent {
			for _, record := range restored.ResolvedRecords {
				if record.Identity != restored.Fence.Current || (!record.Record.EverAdmitted && !record.Record.NonMigratable) {
					continue
				}
				boundAccount := record.AccountKey
				if codexLeaseCurrentAttemptState(record.Record) == CodexAttemptAccountUnavailable {
					if _, blocked := quotaUnavailable[boundAccount]; blocked {
						continue
					}
				}
				attempt, found := codexLeaseAttemptByGeneration(record.Record.Attempts, record.Record.CurrentAttemptGeneration)
				if found && !record.Record.NonMigratable && (attempt.State == CodexAttemptPrepared || attempt.State == CodexAttemptDispatched) && attempt.Slot > 0 && int(attempt.Slot) <= len(record.Record.AttemptEnvelope.Slots) && !constantTimeCodexLeaseDigestEqual(record.Record.AttemptEnvelope.Slots[attempt.Slot-1].AccountHash, record.Record.AccountHash) {
					boundAccount, _ = coordinator.store.resolveCodexLeaseAccount(record.Record.AttemptEnvelope.Slots[attempt.Slot-1].AccountHash, accounts)
				}
				if boundAccount == "" {
					return CodexLeaseRouteSnapshot{}, fmt.Errorf("%w: persisted bound account is unavailable", ErrCodexLeaseAuthorityMismatch)
				}
				snapshot.BoundAccountKey = boundAccount
				snapshot.BoundIdentity = record.Identity
				snapshot.BoundRecordGeneration = record.Record.RecordGeneration
				snapshot.BoundChoice = cloneRouteChoice(record.Choice)
				snapshot.BoundRequiresAccount = codexLeaseRecordRequiresAccount(record.Record)
				break
			}
		}
		if restored.Classification == CodexRestoredLaneHistorical {
			for _, record := range restored.ResolvedRecords {
				if record.Identity.TurnDigest == restored.RequestedIdentity.TurnDigest && record.Identity.Authoritative && codexLeaseRecordAllowedByPolicy(record.Record, policy) {
					snapshot.HistoricalAuthoritative = true
					break
				}
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
