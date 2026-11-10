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
	RedistributionGeneration  uint64
	Classification            CodexRestoredLaneClassification
	BoundAccountKey           codex.AccountKey
	BoundIdentity             CodexJournalRecordIdentity
	BoundRecordGeneration     uint64
	BoundChoice               RouteChoice
	BoundRequiresAccount      bool
	BoundRequestCompleted     bool
	BoundPreTurnCompleted     bool
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
// together with global provisional route counts from the same authenticated
// in-memory journal generation. A later durable mutation revalidates installed
// bytes before replacement, so route probes never queue behind journal I/O.
func (coordinator *CodexContinuityCoordinator) LoadRouteSnapshot(ctx context.Context, key LeaseKey, accounts []codex.AccountKey, policy CodexLeaseAuthorityPolicy) (CodexLeaseRouteSnapshot, error) {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil || coordinator.leases.lifecycle == nil || coordinator.leases.mu == nil {
		return CodexLeaseRouteSnapshot{}, ErrCodexLeaseWriterUnavailable
	}
	if ctx == nil {
		return CodexLeaseRouteSnapshot{}, fmt.Errorf("%w: nil route snapshot context", ErrCodexLeaseInvalidMutation)
	}
	policy = cloneCodexLeaseAuthorityPolicy(policy)

	for {
		if err := ctx.Err(); err != nil {
			return CodexLeaseRouteSnapshot{}, err
		}
		restored, err := coordinator.store.loadLaneSnapshot(key, accounts, policy)
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
			Classification:           restored.Classification,
			RedistributionGeneration: codexLaneRedistributionPending(restored.Lane),
			Provisional:              provisional,
			JournalGeneration:        restored.Fence.Journal,
		}
		accountResolver := newCodexLeaseAccountResolver(coordinator.store, accounts)
		_, snapshot.RestartableFailedHead = codexRestoredLaneRestartableFailedHead(restored)
		unavailable := make(map[codex.AccountKey]struct{})
		quotaUnavailable := make(map[codex.AccountKey]struct{})
		if restored.RequestedIdentity == codexLaneRequestScopeIdentity(restored.Lane) {
			for _, accountHash := range restored.Lane.RequestUnavailableAccountHashes {
				account, resolved := accountResolver.resolve(accountHash)
				if !resolved {
					continue
				}
				unavailable[account] = struct{}{}
				snapshot.UnavailableAccountKeys = append(snapshot.UnavailableAccountKeys, account)
			}
		}
		for _, accountHash := range restored.Lane.QuotaExhaustedAccountHashes {
			account, resolved := accountResolver.resolve(accountHash)
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
					boundAccount, _ = accountResolver.resolve(record.Record.AttemptEnvelope.Slots[attempt.Slot-1].AccountHash)
				}
				if boundAccount == "" {
					return CodexLeaseRouteSnapshot{}, fmt.Errorf("%w: persisted bound account is unavailable", ErrCodexLeaseAuthorityMismatch)
				}
				snapshot.BoundAccountKey = boundAccount
				snapshot.BoundIdentity = record.Identity
				snapshot.BoundRecordGeneration = record.Record.RecordGeneration
				snapshot.BoundChoice = cloneRouteChoice(record.Choice)
				snapshot.BoundRequiresAccount = codexLeaseRecordRequiresAccount(record.Record)
				snapshot.BoundRequestCompleted = codexLeaseCurrentAttemptState(record.Record) == CodexAttemptProviderCompleted
				snapshot.BoundPreTurnCompleted = codexLeasePreTurnCompactionCanBeginTurn(record.Record, CodexRequestTurn)
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

func (coordinator *CodexContinuityCoordinator) beginCodexLeasePersistenceContext(ctx context.Context) (func(), error) {
	lifecycle := coordinator.leases.lifecycle
	finishWait := codexProcessRuntimeObservability.beginPersistenceWait()
	defer finishWait()
	if err := lifecycle.persistence.LockContext(ctx); err != nil {
		return nil, err
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
	// loadLaneSnapshot already read the last authenticated in-memory state. A
	// generation comparison keeps this separate calculation coherent without
	// holding lifecycle.persistence across either read.
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

	provisional := make(map[codex.AccountKey]int)
	accountResolver := newCodexLeaseAccountResolver(store, accounts)
	for index := range store.v2.Records {
		record := &store.v2.Records[index]
		if record.EverAdmitted || record.State != LeaseProvisional || record.RoutingRefs != 1 {
			continue
		}
		attempt, found := codexLeaseAttemptByGeneration(record.Attempts, record.CurrentAttemptGeneration)
		if !found || (attempt.State != CodexAttemptPrepared && attempt.State != CodexAttemptDispatched) {
			continue
		}
		// Installed snapshots are canonical. Locate only candidate provisionals'
		// lane heads instead of allocating a map of every retained lane per read.
		laneIndex := sort.Search(len(store.v2.Lanes), func(index int) bool {
			lane := &store.v2.Lanes[index]
			return compareCodexLeaseLaneScope(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash, record.SessionHash, record.ThreadHash, record.NamespaceHash) >= 0
		})
		if laneIndex == len(store.v2.Lanes) {
			continue
		}
		lane := &store.v2.Lanes[laneIndex]
		if lane.SessionHash != record.SessionHash || lane.ThreadHash != record.ThreadHash || lane.NamespaceHash != record.NamespaceHash || lane.CurrentTurnHash == "" || lane.CurrentTurnHash != record.TurnHash || lane.CurrentModeEpoch != record.ModeEpoch || lane.CurrentAuthoritative != record.Authoritative {
			continue
		}
		account, resolved := accountResolver.resolve(record.AccountHash)
		if !resolved {
			return nil, false, fmt.Errorf("%w: active provisional account is unavailable", ErrCodexLeaseAuthorityMismatch)
		}
		provisional[account]++
	}
	return provisional, true, nil
}
