package proxy

import (
	"context"
	"fmt"
	"sync"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexAccountRemovalGuard holds the account admission exclusion until the
// credential coordinator has durably decided the removal operation.
type CodexAccountRemovalGuard interface {
	Release()
}

// CodexBoundAuthoritySummary is a privacy-safe view of authenticated lease
// authority for one account. BoundCount is the exact sum of the five state
// counters. While its guard is held, ordinary admission and restore cannot
// increase BoundCount. Existing authority may move between categories, and
// unrelated-account transactions may advance JournalGeneration.
type CodexBoundAuthoritySummary struct {
	JournalGeneration   uint64
	BoundCount          int
	BoundActive         int
	ContinuationPending int
	BoundQuiescent      int
	OrphanedOrRestored  int
	AdoptedPrewarm      int
}

type codexAccountGateSet struct {
	mu      sync.Mutex
	entries map[codex.AccountKey]*codexAccountGateEntry
	closed  bool
	done    chan struct{}
}

type codexAccountGateEntry struct {
	token chan struct{}
	refs  int
}

type codexAccountGateGuard struct {
	set     *codexAccountGateSet
	account codex.AccountKey
	entry   *codexAccountGateEntry
	once    sync.Once
}

func newCodexAccountGateSet() *codexAccountGateSet {
	return &codexAccountGateSet{
		entries: make(map[codex.AccountKey]*codexAccountGateEntry),
		done:    make(chan struct{}),
	}
}

func (set *codexAccountGateSet) acquire(ctx context.Context, account codex.AccountKey) (*codexAccountGateGuard, error) {
	if set == nil || account == "" {
		return nil, fmt.Errorf("%w: invalid account admission gate", ErrCodexLeaseAuthorityMismatch)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil account admission context", ErrCodexLeaseAuthorityMismatch)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	set.mu.Lock()
	if set.closed {
		set.mu.Unlock()
		return nil, ErrCodexLeaseWriterUnavailable
	}
	entry := set.entries[account]
	if entry == nil {
		entry = &codexAccountGateEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		set.entries[account] = entry
	}
	entry.refs++
	done := set.done
	set.mu.Unlock()

	select {
	case <-ctx.Done():
		set.releaseReference(account, entry)
		return nil, ctx.Err()
	case <-done:
		set.releaseReference(account, entry)
		return nil, ErrCodexLeaseWriterUnavailable
	case <-entry.token:
		set.mu.Lock()
		closed := set.closed
		set.mu.Unlock()
		if closed {
			set.releaseReference(account, entry)
			return nil, ErrCodexLeaseWriterUnavailable
		}
		return &codexAccountGateGuard{set: set, account: account, entry: entry}, nil
	}
}

func (set *codexAccountGateSet) close() {
	if set == nil {
		return
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return
	}
	set.closed = true
	close(set.done)
	clear(set.entries)
}

func (guard *codexAccountGateGuard) Release() {
	if guard == nil {
		return
	}
	guard.once.Do(func() {
		if guard.set == nil || guard.entry == nil {
			return
		}
		select {
		case guard.entry.token <- struct{}{}:
		default:
		}
		guard.set.releaseReference(guard.account, guard.entry)
	})
}

func (set *codexAccountGateSet) releaseReference(account codex.AccountKey, entry *codexAccountGateEntry) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if entry.refs > 0 {
		entry.refs--
	}
	if entry.refs == 0 && set.entries[account] == entry {
		delete(set.entries, account)
	}
}

type codexBoundAuthorityCategory uint8

const (
	codexBoundAuthorityNone codexBoundAuthorityCategory = iota
	codexBoundAuthorityActive
	codexBoundAuthorityContinuation
	codexBoundAuthorityQuiescent
	codexBoundAuthorityOrphaned
	codexBoundAuthorityAdoptedPrewarm
)

// BeginAccountRemoval excludes new admission, restore, and prewarm-adoption
// authority while its returned guard is held and summarises the authenticated
// durable/live union. Only a signed store-owned adoption marker contributes to
// AdoptedPrewarm; legacy live prewarm state cannot invent that authority.
func (coordinator *CodexContinuityCoordinator) BeginAccountRemoval(ctx context.Context, account codex.AccountKey) (CodexAccountRemovalGuard, CodexBoundAuthoritySummary, error) {
	if coordinator == nil || coordinator.store == nil || coordinator.leases == nil || coordinator.leases.mu == nil || coordinator.leases.accountGates == nil || coordinator.leases.writerUnavailable() {
		return nil, CodexBoundAuthoritySummary{}, ErrCodexLeaseWriterUnavailable
	}
	gate, err := coordinator.leases.accountGates.acquire(ctx, account)
	if err != nil {
		return nil, CodexBoundAuthoritySummary{}, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			gate.Release()
		}
	}()

	store := coordinator.store
	operation, err := store.beginOperation()
	if err != nil {
		return nil, CodexBoundAuthoritySummary{}, err
	}
	defer operation.Release()

	coordinator.leases.mu.Lock()
	defer coordinator.leases.mu.Unlock()
	if coordinator.leases.writerUnavailableLocked() {
		return nil, CodexBoundAuthoritySummary{}, ErrCodexLeaseWriterUnavailable
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	summary, err := coordinator.boundAuthoritySummaryLocked(account)
	if err != nil {
		return nil, CodexBoundAuthoritySummary{}, err
	}
	succeeded = true
	return gate, summary, nil
}

func (coordinator *CodexContinuityCoordinator) boundAuthoritySummaryLocked(account codex.AccountKey) (CodexBoundAuthoritySummary, error) {
	store := coordinator.store
	if store.closed {
		return CodexBoundAuthoritySummary{}, ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return CodexBoundAuthoritySummary{}, fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if store.v2 == nil {
		return CodexBoundAuthoritySummary{}, fmt.Errorf("%w: schema-v2 journal unavailable", ErrCodexLeaseTrustLost)
	}
	if err := store.revalidateV2InstalledLocked(); err != nil {
		store.poisoned = err
		return CodexBoundAuthoritySummary{}, err
	}
	if store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return CodexBoundAuthoritySummary{}, ErrCodexLegacyQuarantine
	}
	if err := validateCodexLeaseRepresentedModes(representedCodexLeaseAuthoritativeEpochs(store.v2.Records), store.modes); err != nil {
		return CodexBoundAuthoritySummary{}, err
	}

	targetHash := store.hash("account", string(account))
	categories := make(map[CodexJournalRecordIdentity]codexBoundAuthorityCategory)
	for _, record := range store.v2.Records {
		if !record.Authoritative || !constantTimeCodexLeaseDigestEqual(record.AccountHash, targetHash) {
			continue
		}
		category := codexLeaseBoundAuthorityCategory(record.State)
		if category == codexBoundAuthorityNone && record.AdoptedPrewarm && (record.State == LeaseProvisional || record.State == LeaseFailedUnadmitted) {
			category = codexBoundAuthorityAdoptedPrewarm
		} else if record.RequestKind == CodexRequestPrewarm && !record.EverAdmitted {
			// A generic prewarm request-kind row is not proof that its handoff
			// completed. Only the signed store-owned marker carries authority.
			category = codexBoundAuthorityNone
		}
		if category != codexBoundAuthorityNone {
			categories[record.Identity()] = category
		}
	}

	for _, managed := range coordinator.leases.leases {
		lease := managed.lease
		if !lease.Authoritative || lease.AccountKey != account {
			continue
		}
		if !containsCodexLeaseEpoch(store.modes.RecognisedAuthoritativeEpochs, lease.ModeEpoch) {
			return CodexBoundAuthoritySummary{}, fmt.Errorf("%w: live authoritative epoch %d is not retained", ErrCodexLeaseAuthorityMismatch, lease.ModeEpoch)
		}
		identity := codexJournalIdentityForLiveLease(store, lease)
		if lease.AdoptedPrewarm && categories[identity] != codexBoundAuthorityAdoptedPrewarm {
			continue
		}
		category := codexLeaseBoundAuthorityCategory(lease.State)
		if category == codexBoundAuthorityNone {
			continue
		}
		if categories[identity] != codexBoundAuthorityAdoptedPrewarm {
			categories[identity] = category
		}
	}

	summary := CodexBoundAuthoritySummary{JournalGeneration: store.v2.Generation}
	for _, category := range categories {
		switch category {
		case codexBoundAuthorityActive:
			summary.BoundActive++
		case codexBoundAuthorityContinuation:
			summary.ContinuationPending++
		case codexBoundAuthorityQuiescent:
			summary.BoundQuiescent++
		case codexBoundAuthorityOrphaned:
			summary.OrphanedOrRestored++
		case codexBoundAuthorityAdoptedPrewarm:
			summary.AdoptedPrewarm++
		}
	}
	summary.BoundCount = summary.BoundActive + summary.ContinuationPending + summary.BoundQuiescent + summary.OrphanedOrRestored + summary.AdoptedPrewarm
	return summary, nil
}

func codexLeaseBoundAuthorityCategory(state LeaseState) codexBoundAuthorityCategory {
	switch state {
	case LeaseBoundActive:
		return codexBoundAuthorityActive
	case LeaseContinuationPending:
		return codexBoundAuthorityContinuation
	case LeaseBoundQuiescent:
		return codexBoundAuthorityQuiescent
	case LeaseOrphaned:
		return codexBoundAuthorityOrphaned
	default:
		return codexBoundAuthorityNone
	}
}

func codexJournalIdentityForLiveLease(store *CodexLeaseStore, lease CodexTurnLease) CodexJournalRecordIdentity {
	sessionHash := store.hash("session", lease.Key.Lane.Session)
	threadHash := store.hash("thread", lease.Key.Lane.Thread)
	namespaceHash := store.hash("namespace", lease.Key.Lane.Namespace)
	return CodexJournalRecordIdentity{
		LaneDigest:    codexJournalLaneDigest(sessionHash, threadHash, namespaceHash),
		TurnDigest:    store.hash("turn", lease.Key.Turn),
		ModeEpoch:     lease.ModeEpoch,
		Authoritative: lease.Authoritative,
	}
}
