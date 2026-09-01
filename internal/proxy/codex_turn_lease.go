package proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const CodexResponsesNamespace = "codex-responses"

var (
	ErrCodexStaleTurn        = errors.New("stale Codex turn")
	ErrCodexConcurrentTurn   = errors.New("concurrent Codex turn")
	ErrCodexContinuity       = errors.New("Codex turn continuity unavailable")
	ErrCodexLeaseTransition  = errors.New("invalid Codex lease transition")
	ErrCodexNoAuthorityFence = errors.New("no retained Codex authority fence")
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
	CodexAttemptAbandonedBeforeDispatch
	CodexAttemptAccountUnavailable
)

func validCodexAttemptTransition(from, to CodexAttemptState) bool {
	switch from {
	case CodexAttemptPrepared:
		return to == CodexAttemptDispatched || to == CodexAttemptAccountUnavailable
	case CodexAttemptDispatched:
		return to == CodexAttemptStreaming || to == CodexAttemptProviderFailed || to == CodexAttemptIndeterminate || to == CodexAttemptAccountUnavailable
	case CodexAttemptStreaming:
		return to == CodexAttemptProviderCompleted || to == CodexAttemptProviderFailed || to == CodexAttemptIndeterminate || to == CodexAttemptAccountUnavailable
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
	AdoptedPrewarm           bool
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

type codexTurnLeaseManagerLifecycle struct {
	closed      bool
	persistence sync.Mutex
}

type CodexTurnLeaseManager struct {
	mu            *sync.Mutex
	current       map[LaneKey]LeaseKey
	leases        map[LeaseKey]*codexManagedLease
	accountGates  *codexAccountGateSet
	lifecycle     *codexTurnLeaseManagerLifecycle
	now           func() time.Time
	modeEpoch     uint64
	authoritative bool
}

func NewCodexTurnLeaseManager(modeEpoch uint64, authoritative bool, now func() time.Time) *CodexTurnLeaseManager {
	if now == nil {
		now = time.Now
	}
	mu := &sync.Mutex{}
	return &CodexTurnLeaseManager{
		mu:            mu,
		current:       make(map[LaneKey]LeaseKey),
		leases:        make(map[LeaseKey]*codexManagedLease),
		accountGates:  newCodexAccountGateSet(),
		lifecycle:     &codexTurnLeaseManagerLifecycle{},
		now:           now,
		modeEpoch:     modeEpoch,
		authoritative: authoritative,
	}
}

// ForMode returns a mode-specific view over one shared turn-state core.
// HTTP and WebSocket routing must use views from the same core so an exact
// Responses turn cannot acquire different accounts across transports.
func (manager *CodexTurnLeaseManager) ForMode(modeEpoch uint64, authoritative bool) *CodexTurnLeaseManager {
	if manager == nil || manager.mu == nil {
		closed := NewCodexTurnLeaseManager(modeEpoch, authoritative, nil)
		closed.revoke()
		return closed
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	view := &CodexTurnLeaseManager{
		mu:            manager.mu,
		current:       manager.current,
		leases:        manager.leases,
		accountGates:  manager.accountGates,
		lifecycle:     manager.lifecycle,
		now:           manager.now,
		modeEpoch:     modeEpoch,
		authoritative: authoritative,
	}
	if manager.writerUnavailableLocked() {
		view.modeEpoch = 0
		view.authoritative = false
	}
	return view
}

// revoke permanently closes every mode view over this manager core and clears
// the in-memory lease authority it owned. The continuity coordinator calls it
// before closing the durable store.
func (manager *CodexTurnLeaseManager) revoke() {
	if manager == nil || manager.mu == nil {
		return
	}
	if manager.lifecycle == nil {
		return
	}
	manager.lifecycle.persistence.Lock()
	defer manager.lifecycle.persistence.Unlock()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !manager.lifecycle.closed {
		manager.lifecycle.closed = true
		if manager.accountGates != nil {
			manager.accountGates.close()
		}
		for _, managed := range manager.leases {
			if managed == nil {
				continue
			}
			if managed.ready != nil {
				select {
				case <-managed.ready:
				default:
					close(managed.ready)
				}
			}
			clear(managed.lease.Choice.RequiredBuckets)
			managed.lease = CodexTurnLease{}
			managed.ready = nil
		}
		clear(manager.current)
		clear(manager.leases)
	}
}

func (manager *CodexTurnLeaseManager) writerUnavailable() bool {
	if manager == nil || manager.mu == nil {
		return true
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.writerUnavailableLocked()
}

// writerUnavailableLocked reports lifecycle liveness while manager.mu is held.
// Receiver methods outside this file must use this guard before touching the
// shared maps.
func (manager *CodexTurnLeaseManager) writerUnavailableLocked() bool {
	return manager == nil || manager.lifecycle == nil || manager.lifecycle.closed || manager.current == nil || manager.leases == nil
}

func (manager *CodexTurnLeaseManager) Acquire(ctx context.Context, key LeaseKey, selectAccount func(context.Context) (codex.AccountKey, error)) (CodexTurnLease, error) {
	if manager.writerUnavailable() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	if selectAccount == nil {
		return CodexTurnLease{}, errors.New("Codex account selector unavailable")
	}
	return manager.AcquireRoute(ctx, key, func(ctx context.Context) (RouteChoice, error) {
		account, err := selectAccount(ctx)
		return RouteChoice{AccountKey: account}, err
	})
}

func (manager *CodexTurnLeaseManager) AcquireRoute(ctx context.Context, key LeaseKey, selectRoute func(context.Context) (RouteChoice, error)) (CodexTurnLease, error) {
	if manager == nil || manager.mu == nil || manager.writerUnavailable() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	if err := key.validate(); err != nil {
		return CodexTurnLease{}, err
	}
	if selectRoute == nil {
		return CodexTurnLease{}, errors.New("Codex route selector unavailable")
	}
	for {
		manager.mu.Lock()
		if manager.writerUnavailableLocked() {
			manager.mu.Unlock()
			return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
		}
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
			result := cloneCodexTurnLease(existing.lease)
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
		if manager.writerUnavailableLocked() {
			manager.mu.Unlock()
			return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
		}
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
		result := cloneCodexTurnLease(managed.lease)
		manager.mu.Unlock()
		return result, nil
	}
}

func cloneRouteChoice(choice RouteChoice) RouteChoice {
	choice.RequiredBuckets = append([]CapacityBucket(nil), choice.RequiredBuckets...)
	return choice
}

func cloneCodexTurnLease(lease CodexTurnLease) CodexTurnLease {
	lease.Choice = cloneRouteChoice(lease.Choice)
	return lease
}

func (manager *CodexTurnLeaseManager) ReplaceProvisionalRoute(key LeaseKey, choice RouteChoice) (CodexTurnLease, error) {
	if manager == nil || manager.mu == nil {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	managed := manager.leases[key]
	if managed == nil || manager.current[key.Lane] != key || managed.lease.State != LeaseProvisional || choice.AccountKey == "" {
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	managed.lease.AccountKey = choice.AccountKey
	managed.lease.Choice = cloneRouteChoice(choice)
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return cloneCodexTurnLease(managed.lease), nil
}

func (manager *CodexTurnLeaseManager) ReleaseRouting(key LeaseKey) error {
	if manager == nil || manager.mu == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return ErrCodexLeaseWriterUnavailable
	}
	managed := manager.leases[key]
	if managed == nil || managed.lease.RoutingRefs == 0 {
		return errors.New("Codex routing reference unavailable")
	}
	managed.lease.RoutingRefs--
	managed.lease.LastSeen = manager.now()
	return nil
}

func (manager *CodexTurnLeaseManager) Admit(key LeaseKey, account codex.AccountKey, socketGeneration uint64, persist func([]CodexTurnLease) error) (CodexTurnLease, error) {
	return manager.AdmitContext(context.Background(), key, account, socketGeneration, nil, persist)
}

// AdmitContext serialises the provisional-to-bound transition with account
// removal. When supplied, revalidate runs while the account gate is held and
// before memory becomes bound; Task 16D uses it to reject durable-pending
// removals that completed while this admission waited for the gate.
func (manager *CodexTurnLeaseManager) AdmitContext(ctx context.Context, key LeaseKey, account codex.AccountKey, socketGeneration uint64, revalidate func(context.Context, codex.AccountKey) error, persist func([]CodexTurnLease) error) (CodexTurnLease, error) {
	if manager == nil || manager.mu == nil || manager.accountGates == nil || manager.writerUnavailable() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	guard, err := manager.accountGates.acquire(ctx, account)
	if err != nil {
		return CodexTurnLease{}, err
	}
	defer guard.Release()
	if manager.writerUnavailable() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	var revalidateErr error
	if revalidate != nil {
		revalidateErr = revalidate(ctx, account)
	}
	if manager.writerUnavailable() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	if revalidateErr != nil {
		return CodexTurnLease{}, fmt.Errorf("revalidate Codex account admission: %w", revalidateErr)
	}

	lifecycle := manager.lifecycle
	lifecycle.persistence.Lock()
	defer lifecycle.persistence.Unlock()
	manager.mu.Lock()
	if manager.writerUnavailableLocked() {
		manager.mu.Unlock()
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
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
	result := cloneCodexTurnLease(managed.lease)

	if persist != nil {
		manager.mu.Unlock()
		persistErr := runCodexAdmissionPersist(snapshot, persist)
		manager.mu.Lock()
		if manager.writerUnavailableLocked() {
			manager.mu.Unlock()
			return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
		}
		if persistErr != nil {
			managed.lease.NonMigratable = true
			managed.lease.Generation++
			result = cloneCodexTurnLease(managed.lease)
			manager.mu.Unlock()
			return result, fmt.Errorf("persist admitted Codex lease: %w", persistErr)
		}
	}
	if manager.writerUnavailableLocked() {
		manager.mu.Unlock()
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Unlock()
	return result, nil
}

func runCodexAdmissionPersist(snapshot []CodexTurnLease, persist func([]CodexTurnLease) error) (err error) {
	defer clearCodexTurnLeaseSnapshot(snapshot)
	return persist(snapshot)
}

func clearCodexTurnLeaseSnapshot(snapshot []CodexTurnLease) {
	for index := range snapshot {
		clear(snapshot[index].Choice.RequiredBuckets)
		snapshot[index] = CodexTurnLease{}
	}
	clear(snapshot)
}

func (manager *CodexTurnLeaseManager) ObserveCompleted(key LeaseKey, endTurn *bool) (CodexTurnLease, error) {
	if manager == nil || manager.mu == nil {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
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
	return cloneCodexTurnLease(managed.lease), nil
}

func (manager *CodexTurnLeaseManager) ObserveIndeterminate(key LeaseKey) (CodexTurnLease, error) {
	if manager == nil || manager.mu == nil {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	managed := manager.leases[key]
	if managed == nil || managed.lease.State != LeaseBoundActive || managed.lease.ActiveAttempts == 0 {
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	managed.lease.State = LeaseOrphaned
	managed.lease.ActiveAttempts--
	managed.lease.UpstreamSocketGeneration = 0
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return cloneCodexTurnLease(managed.lease), nil
}

func (manager *CodexTurnLeaseManager) ObserveProviderFailed(key LeaseKey) (CodexTurnLease, error) {
	if manager == nil || manager.mu == nil {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
	}
	managed := manager.leases[key]
	if managed == nil || managed.lease.State != LeaseBoundActive || managed.lease.ActiveAttempts == 0 {
		return CodexTurnLease{}, ErrCodexLeaseTransition
	}
	managed.lease.State = LeaseBoundQuiescent
	managed.lease.ActiveAttempts--
	managed.lease.Generation++
	managed.lease.LastSeen = manager.now()
	return cloneCodexTurnLease(managed.lease), nil
}

func (manager *CodexTurnLeaseManager) FailUnadmitted(key LeaseKey) error {
	if manager == nil || manager.mu == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return ErrCodexLeaseWriterUnavailable
	}
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
	if manager == nil || manager.mu == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return ErrCodexLeaseWriterUnavailable
	}
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
	if manager == nil || manager.mu == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return ErrCodexLeaseWriterUnavailable
	}
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
	if manager == nil || manager.mu == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return ErrCodexLeaseWriterUnavailable
	}
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
	if manager == nil || manager.mu == nil {
		return CodexTurnLease{}, false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return CodexTurnLease{}, false
	}
	managed := manager.leases[key]
	if managed == nil {
		return CodexTurnLease{}, false
	}
	return cloneCodexTurnLease(managed.lease), true
}

func (manager *CodexTurnLeaseManager) ObservedRouteChoice(key LeaseKey) (RouteChoice, bool, error) {
	if manager == nil || manager.mu == nil {
		return RouteChoice{}, false, ErrCodexLeaseWriterUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return RouteChoice{}, false, ErrCodexLeaseWriterUnavailable
	}
	managed := manager.leases[key]
	if managed == nil {
		if currentKey, found := manager.current[key.Lane]; found {
			current := manager.leases[currentKey]
			if current != nil && (current.lease.RoutingRefs != 0 || current.lease.ActiveAttempts != 0 ||
				current.lease.State == LeaseReserving || current.lease.State == LeaseProvisional || current.lease.State == LeaseBoundActive) {
				return RouteChoice{}, false, ErrCodexConcurrentTurn
			}
		}
		return RouteChoice{}, false, nil
	}
	if manager.current[key.Lane] != key {
		return RouteChoice{}, false, ErrCodexStaleTurn
	}
	switch managed.lease.State {
	case LeaseProvisional, LeaseBoundActive, LeaseContinuationPending, LeaseBoundQuiescent, LeaseOrphaned:
	default:
		return RouteChoice{}, false, ErrCodexStaleTurn
	}
	choice := cloneRouteChoice(managed.lease.Choice)
	if choice.AccountKey == "" {
		choice.AccountKey = managed.lease.AccountKey
	}
	if choice.AccountKey == "" || choice.RequestedModel == "" || choice.EffectiveModel == "" {
		return RouteChoice{}, false, fmt.Errorf("%w: observed route choice unavailable", ErrCodexContinuity)
	}
	return choice, true, nil
}

func (manager *CodexTurnLeaseManager) Snapshot() []CodexTurnLease {
	if manager == nil || manager.mu == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return nil
	}
	return manager.snapshotLocked()
}

func (manager *CodexTurnLeaseManager) Compact(retention time.Duration) {
	if manager == nil || manager.mu == nil {
		return
	}
	if retention <= 0 {
		retention = DefaultCodexLeaseRetention
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return
	}
	now := manager.now()
	for key, managed := range manager.leases {
		if managed.lease.RoutingRefs != 0 || managed.lease.ActiveAttempts != 0 || now.Sub(managed.lease.LastSeen) <= retention {
			continue
		}
		delete(manager.leases, key)
		if manager.current[key.Lane] == key {
			delete(manager.current, key.Lane)
		}
	}
}

func (manager *CodexTurnLeaseManager) Mode() (uint64, bool) {
	if manager == nil || manager.mu == nil {
		return 0, false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return 0, false
	}
	return manager.modeEpoch, manager.authoritative
}

func (manager *CodexTurnLeaseManager) snapshotLocked() []CodexTurnLease {
	result := make([]CodexTurnLease, 0, len(manager.leases))
	for _, managed := range manager.leases {
		result = append(result, cloneCodexTurnLease(managed.lease))
	}
	return result
}

func (manager *CodexTurnLeaseManager) Restore(leases []CodexTurnLease) {
	if manager == nil || manager.mu == nil || manager.accountGates == nil || manager.writerUnavailable() {
		return
	}
	accountSet := make(map[codex.AccountKey]struct{})
	for _, lease := range leases {
		if lease.Authoritative && lease.AccountKey != "" && codexLeaseRestoreCreatesBoundAuthority(lease.State) {
			accountSet[lease.AccountKey] = struct{}{}
		}
	}
	accounts := make([]codex.AccountKey, 0, len(accountSet))
	for account := range accountSet {
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
	guards := make([]*codexAccountGateGuard, 0, len(accounts))
	for _, account := range accounts {
		guard, err := manager.accountGates.acquire(context.Background(), account)
		if err != nil {
			for index := len(guards) - 1; index >= 0; index-- {
				guards[index].Release()
			}
			return
		}
		guards = append(guards, guard)
	}
	defer func() {
		for index := len(guards) - 1; index >= 0; index-- {
			guards[index].Release()
		}
	}()

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.writerUnavailableLocked() {
		return
	}
	for _, lease := range leases {
		lease = cloneCodexTurnLease(lease)
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

func codexLeaseRestoreCreatesBoundAuthority(state LeaseState) bool {
	switch state {
	case LeaseReserving, LeaseProvisional, LeaseBoundActive, LeaseContinuationPending, LeaseBoundQuiescent, LeaseOrphaned:
		return true
	default:
		return false
	}
}
