package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const CodexResponsesNamespace = "codex-responses"

var (
	ErrCodexStaleTurn       = errors.New("stale Codex turn")
	ErrCodexConcurrentTurn  = errors.New("concurrent Codex turn")
	ErrCodexContinuity      = errors.New("Codex turn continuity unavailable")
	ErrCodexLeaseTransition = errors.New("invalid Codex lease transition")
)

type LaneKey struct {
	Session   string
	Thread    string
	Namespace string
}

type LeaseKey struct {
	Lane LaneKey
	Turn string
}

func NewCodexLeaseKey(metadata CodexTurnMetadata) LeaseKey {
	return LeaseKey{Lane: LaneKey{Session: metadata.SessionID, Thread: metadata.ThreadID, Namespace: CodexResponsesNamespace}, Turn: metadata.TurnID}
}

func (key LaneKey) validate() error {
	if key.Session == "" || key.Thread == "" || key.Namespace == "" {
		return errors.New("Codex lane requires session, thread, and namespace")
	}
	return nil
}

func (key LeaseKey) validate() error {
	if err := key.Lane.validate(); err != nil {
		return err
	}
	if key.Turn == "" {
		return errors.New("Codex lease requires turn")
	}
	return nil
}

type LeaseState uint8

const (
	LeaseReserving LeaseState = iota + 1
	LeaseProvisional
	LeaseBoundActive
	LeaseContinuationPending
	LeaseBoundQuiescent
	LeaseOrphaned
	LeaseSuperseded
	LeaseExpired
	LeaseFailedUnadmitted
)

func (state LeaseState) String() string {
	switch state {
	case LeaseReserving:
		return "reserving"
	case LeaseProvisional:
		return "provisional"
	case LeaseBoundActive:
		return "bound_active"
	case LeaseContinuationPending:
		return "continuation_pending"
	case LeaseBoundQuiescent:
		return "bound_quiescent"
	case LeaseOrphaned:
		return "orphaned"
	case LeaseSuperseded:
		return "superseded"
	case LeaseExpired:
		return "expired"
	case LeaseFailedUnadmitted:
		return "failed_unadmitted"
	default:
		return "unknown"
	}
}

func validLeaseTransition(from, to LeaseState) bool {
	if from == LeaseProvisional && to == LeaseProvisional {
		return true
	}
	switch from {
	case LeaseReserving:
		return to == LeaseProvisional || to == LeaseFailedUnadmitted
	case LeaseProvisional:
		return to == LeaseBoundActive || to == LeaseFailedUnadmitted
	case LeaseBoundActive:
		return to == LeaseContinuationPending || to == LeaseBoundQuiescent || to == LeaseOrphaned
	case LeaseContinuationPending, LeaseBoundQuiescent, LeaseOrphaned:
		return to == LeaseBoundActive || to == LeaseOrphaned || to == LeaseSuperseded || to == LeaseExpired
	default:
		return false
	}
}

type CodexAttemptState uint8

const (
	CodexAttemptPrepared CodexAttemptState = iota + 1
	CodexAttemptDispatched
	CodexAttemptStreaming
	CodexAttemptProviderCompleted
	CodexAttemptProviderFailed
	CodexAttemptIndeterminate
)

func validCodexAttemptTransition(from, to CodexAttemptState) bool {
	switch from {
	case CodexAttemptPrepared:
		return to == CodexAttemptDispatched
	case CodexAttemptDispatched:
		return to == CodexAttemptStreaming || to == CodexAttemptProviderFailed || to == CodexAttemptIndeterminate
	case CodexAttemptStreaming:
		return to == CodexAttemptProviderCompleted || to == CodexAttemptProviderFailed || to == CodexAttemptIndeterminate
	default:
		return false
	}
}

type CodexTurnLease struct {
	Key                      LeaseKey
	State                    LeaseState
	AccountKey               codex.AccountKey
	Choice                   RouteChoice
	Generation               uint64
	ModeEpoch                uint64
	Authoritative            bool
	RoutingRefs              int
	ActiveAttempts           int
	UpstreamSocketGeneration uint64
	ResponseAnchor           string
	TurnState                string
	TurnStateUnavailable     bool
	HasEncryptedState        bool
	NonMigratable            bool
	LastSeen                 time.Time
}

func (lease CodexTurnLease) CheckContinuation(account codex.AccountKey, socketGeneration uint64, previousResponseID string, encryptedState bool) error {
	if (previousResponseID != "" || encryptedState || lease.HasEncryptedState) && account != lease.AccountKey {
		return fmt.Errorf("%w: account mismatch", ErrCodexContinuity)
	}
	if previousResponseID != "" && (socketGeneration == 0 || socketGeneration != lease.UpstreamSocketGeneration) {
		return fmt.Errorf("%w: upstream socket generation mismatch", ErrCodexContinuity)
	}
	return nil
}

type codexManagedLease struct {
	lease CodexTurnLease
	ready chan struct{}
}

type CodexTurnLeaseManager struct {
	mu            sync.Mutex
	current       map[LaneKey]LeaseKey
	leases        map[LeaseKey]*codexManagedLease
	now           func() time.Time
	modeEpoch     uint64
	authoritative bool
}

func NewCodexTurnLeaseManager(modeEpoch uint64, authoritative bool, now func() time.Time) *CodexTurnLeaseManager {
	if now == nil {
		now = time.Now
	}
	return &CodexTurnLeaseManager{
		current:       make(map[LaneKey]LeaseKey),
		leases:        make(map[LeaseKey]*codexManagedLease),
		now:           now,
		modeEpoch:     modeEpoch,
		authoritative: authoritative,
	}
}

func (manager *CodexTurnLeaseManager) Acquire(ctx context.Context, key LeaseKey, selectAccount func(context.Context) (codex.AccountKey, error)) (CodexTurnLease, error) {
	if selectAccount == nil {
		return CodexTurnLease{}, errors.New("Codex account selector unavailable")
	}
	return manager.AcquireRoute(ctx, key, func(ctx context.Context) (RouteChoice, error) {
		account, err := selectAccount(ctx)
		return RouteChoice{AccountKey: account}, err
	})
}

func (manager *CodexTurnLeaseManager) AcquireRoute(ctx context.Context, key LeaseKey, selectRoute func(context.Context) (RouteChoice, error)) (CodexTurnLease, error) {
	if err := key.validate(); err != nil {
		return CodexTurnLease{}, err
	}
	if selectRoute == nil {
		return CodexTurnLease{}, errors.New("Codex route selector unavailable")
	}
	for {
		manager.mu.Lock()
		if existing := manager.leases[key]; existing != nil {
			if manager.current[key.Lane] != key {
				manager.mu.Unlock()
				return CodexTurnLease{}, ErrCodexStaleTurn
			}
			if existing.lease.State == LeaseReserving {
				ready := existing.ready
				manager.mu.Unlock()
				select {
				case <-ctx.Done():
					return CodexTurnLease{}, ctx.Err()
				case <-ready:
					continue
				}
			}
			if existing.lease.State == LeaseFailedUnadmitted || existing.lease.State == LeaseExpired || existing.lease.State == LeaseSuperseded {
				manager.mu.Unlock()
				return CodexTurnLease{}, ErrCodexStaleTurn
			}
			existing.lease.RoutingRefs++
			existing.lease.LastSeen = manager.now()
			result := existing.lease
			manager.mu.Unlock()
			return result, nil
		}

		if currentKey, ok := manager.current[key.Lane]; ok {
			predecessor := manager.leases[currentKey]
			if predecessor != nil {
				if predecessor.lease.RoutingRefs != 0 || predecessor.lease.ActiveAttempts != 0 ||
					predecessor.lease.State == LeaseReserving || predecessor.lease.State == LeaseProvisional || predecessor.lease.State == LeaseBoundActive {
					manager.mu.Unlock()
					return CodexTurnLease{}, ErrCodexConcurrentTurn
				}
				if !validLeaseTransition(predecessor.lease.State, LeaseSuperseded) {
					manager.mu.Unlock()
					return CodexTurnLease{}, ErrCodexConcurrentTurn
				}
				predecessor.lease.State = LeaseSuperseded
				predecessor.lease.Generation++
				predecessor.lease.LastSeen = manager.now()
			}
		}

		managed := &codexManagedLease{
			lease: CodexTurnLease{
				Key:           key,
				State:         LeaseReserving,
				Generation:    1,
				ModeEpoch:     manager.modeEpoch,
				Authoritative: manager.authoritative,
				RoutingRefs:   1,
				LastSeen:      manager.now(),
			},
			ready: make(chan struct{}),
		}
		manager.leases[key] = managed
		manager.current[key.Lane] = key
		manager.mu.Unlock()

		choice, err := selectRoute(ctx)
		manager.mu.Lock()
		if err != nil || choice.AccountKey == "" {
			managed.lease.State = LeaseFailedUnadmitted
			managed.lease.RoutingRefs = 0
			managed.lease.Generation++
			managed.lease.LastSeen = manager.now()
			close(managed.ready)
			manager.mu.Unlock()
			if err == nil {
				err = errors.New("Codex account selector returned empty account")
			}
			return CodexTurnLease{}, err
		}
		managed.lease.AccountKey = choice.AccountKey
		managed.lease.Choice = cloneRouteChoice(choice)
		managed.lease.State = LeaseProvisional
		managed.lease.Generation++
		managed.lease.LastSeen = manager.now()
		close(managed.ready)
		result := managed.lease
		manager.mu.Unlock()
		return result, nil
	}
}

func cloneRouteChoice(choice RouteChoice) RouteChoice {
	choice.RequiredBuckets = append([]CapacityBucket(nil), choice.RequiredBuckets...)
	return choice
}

func (manager *CodexTurnLeaseManager) ReplaceProvisionalRoute(key LeaseKey, choice RouteChoice) (CodexTurnLease, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil || manager.current[key.Lane] != key || managed.lease.State != LeaseProvisional || choice.AccountKey == "" {
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	managed.lease.AccountKey = choice.AccountKey
	managed.lease.Choice = cloneRouteChoice(choice)
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return managed.lease, nil
}

func (manager *CodexTurnLeaseManager) ReleaseRouting(key LeaseKey) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil || managed.lease.RoutingRefs == 0 {
		return errors.New("Codex routing reference unavailable")
	}
	managed.lease.RoutingRefs--
	managed.lease.LastSeen = manager.now()
	return nil
}

func (manager *CodexTurnLeaseManager) Admit(key LeaseKey, account codex.AccountKey, socketGeneration uint64, persist func([]CodexTurnLease) error) (CodexTurnLease, error) {
	manager.mu.Lock()
	managed := manager.leases[key]
	if managed == nil || manager.current[key.Lane] != key {
		manager.mu.Unlock()
		return CodexTurnLease{}, ErrCodexStaleTurn
	}
	if managed.lease.AccountKey != account {
		manager.mu.Unlock()
		return CodexTurnLease{}, fmt.Errorf("%w: admission account mismatch", ErrCodexContinuity)
	}
	if !validLeaseTransition(managed.lease.State, LeaseBoundActive) {
		manager.mu.Unlock()
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	managed.lease.State = LeaseBoundActive
	managed.lease.ActiveAttempts++
	managed.lease.UpstreamSocketGeneration = socketGeneration
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	snapshot := manager.snapshotLocked()
	result := managed.lease
	manager.mu.Unlock()

	if persist != nil {
		if err := persist(snapshot); err != nil {
			manager.mu.Lock()
			managed.lease.NonMigratable = true
			managed.lease.Generation++
			result = managed.lease
			manager.mu.Unlock()
			return result, fmt.Errorf("persist admitted Codex lease: %w", err)
		}
	}
	return result, nil
}

func (manager *CodexTurnLeaseManager) ObserveCompleted(key LeaseKey, endTurn *bool) (CodexTurnLease, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil || managed.lease.State != LeaseBoundActive || managed.lease.ActiveAttempts == 0 {
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	next := LeaseBoundQuiescent
	if endTurn != nil && !*endTurn {
		next = LeaseContinuationPending
	}
	managed.lease.State = next
	managed.lease.ActiveAttempts--
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return managed.lease, nil
}

func (manager *CodexTurnLeaseManager) ObserveIndeterminate(key LeaseKey) (CodexTurnLease, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil || managed.lease.State != LeaseBoundActive || managed.lease.ActiveAttempts == 0 {
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	managed.lease.State = LeaseOrphaned
	managed.lease.ActiveAttempts--
	managed.lease.UpstreamSocketGeneration = 0
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return managed.lease, nil
}

func (manager *CodexTurnLeaseManager) ObserveProviderFailed(key LeaseKey) (CodexTurnLease, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil || managed.lease.State != LeaseBoundActive || managed.lease.ActiveAttempts == 0 {
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	managed.lease.State = LeaseBoundQuiescent
	managed.lease.ActiveAttempts--
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return managed.lease, nil
}

func (manager *CodexTurnLeaseManager) FailUnadmitted(key LeaseKey) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil {
		return ErrCodexStaleTurn
	}
	if managed.lease.State != LeaseProvisional && managed.lease.State != LeaseReserving {
		return nil
	}
	managed.lease.State = LeaseFailedUnadmitted
	managed.lease.RoutingRefs = 0
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return nil
}

func (manager *CodexTurnLeaseManager) SetTurnState(key LeaseKey, state string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil {
		return ErrCodexStaleTurn
	}
	if managed.lease.TurnState != "" && managed.lease.TurnState != state {
		return fmt.Errorf("%w: turn state changed", ErrCodexContinuity)
	}
	if managed.lease.TurnState == "" && state != "" {
		managed.lease.TurnState = state
		managed.lease.Generation++
		managed.lease.LastSeen = manager.now()
	}
	return nil
}

func (manager *CodexTurnLeaseManager) SetResponseAnchor(key LeaseKey, anchor string, encrypted bool) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil {
		return ErrCodexStaleTurn
	}
	changed := false
	if anchor != "" && managed.lease.ResponseAnchor != anchor {
		managed.lease.ResponseAnchor = anchor
		changed = true
	}
	if encrypted && !managed.lease.HasEncryptedState {
		managed.lease.HasEncryptedState = true
		changed = true
	}
	if changed {
		managed.lease.Generation++
		managed.lease.LastSeen = manager.now()
	}
	return nil
}

func (manager *CodexTurnLeaseManager) MarkNonMigratable(key LeaseKey) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil {
		return ErrCodexStaleTurn
	}
	if !managed.lease.NonMigratable {
		managed.lease.NonMigratable = true
		managed.lease.Generation++
		managed.lease.LastSeen = manager.now()
	}
	return nil
}

func (manager *CodexTurnLeaseManager) Get(key LeaseKey) (CodexTurnLease, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.leases[key]
	if managed == nil {
		return CodexTurnLease{}, false
	}
	return managed.lease, true
}

func (manager *CodexTurnLeaseManager) Snapshot() []CodexTurnLease {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.snapshotLocked()
}

func (manager *CodexTurnLeaseManager) Mode() (uint64, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.modeEpoch, manager.authoritative
}

func (manager *CodexTurnLeaseManager) snapshotLocked() []CodexTurnLease {
	result := make([]CodexTurnLease, 0, len(manager.leases))
	for _, managed := range manager.leases {
		result = append(result, managed.lease)
	}
	return result
}

func (manager *CodexTurnLeaseManager) Restore(leases []CodexTurnLease) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, lease := range leases {
		if lease.ModeEpoch != manager.modeEpoch || lease.Authoritative != manager.authoritative {
			continue
		}
		if lease.State == LeaseReserving || lease.State == LeaseProvisional || lease.State == LeaseBoundActive || lease.State == LeaseContinuationPending || lease.State == LeaseBoundQuiescent {
			lease.State = LeaseOrphaned
			lease.ActiveAttempts = 0
			lease.RoutingRefs = 0
			lease.UpstreamSocketGeneration = 0
			lease.Generation++
		}
		managed := &codexManagedLease{lease: lease, ready: make(chan struct{})}
		close(managed.ready)
		manager.leases[lease.Key] = managed
		if lease.State != LeaseSuperseded && lease.State != LeaseExpired && lease.State != LeaseFailedUnadmitted {
			manager.current[lease.Key.Lane] = lease.Key
		}
	}
}
