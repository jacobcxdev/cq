package proxy

import (
	"errors"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type CodexPrewarmState uint8

const (
	CodexPrewarmCreating CodexPrewarmState = iota + 1
	CodexPrewarmBoundActive
	CodexPrewarmReady
	CodexPrewarmFailed
	CodexPrewarmDisconnected
	CodexPrewarmAdopted
	CodexPrewarmExpired
	CodexPrewarmCancelled
)

type CodexPrewarmReservation struct {
	Lane                       LaneKey
	Correlation                string
	State                      CodexPrewarmState
	AccountKey                 codex.AccountKey
	SocketGeneration           uint64
	DownstreamSocketGeneration uint64
	UpstreamSocketGeneration   uint64
	ResponseAnchor             string
	TurnState                  string
	Generation                 uint64
	LastSeen                   time.Time
}

type CodexPrewarmManager struct {
	mu           sync.Mutex
	reservations map[LaneKey]*CodexPrewarmReservation
	leases       *CodexTurnLeaseManager
	now          func() time.Time
}

func NewCodexPrewarmManager(leases *CodexTurnLeaseManager, now func() time.Time) *CodexPrewarmManager {
	if now == nil {
		now = time.Now
	}
	return &CodexPrewarmManager{reservations: make(map[LaneKey]*CodexPrewarmReservation), leases: leases, now: now}
}

func (manager *CodexPrewarmManager) Create(metadata CodexTurnMetadata, correlation string) (CodexPrewarmReservation, error) {
	if err := validateCodexTurnMetadata(metadata); err != nil {
		return CodexPrewarmReservation{}, err
	}
	if metadata.RequestKind != CodexRequestPrewarm || metadata.TurnID != "" {
		return CodexPrewarmReservation{}, errors.New("prewarm reservation requires typed empty turn")
	}
	lane := NewCodexLeaseKey(metadata).Lane
	if correlation == "" {
		return CodexPrewarmReservation{}, errors.New("prewarm correlation required")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if existing := manager.reservations[lane]; existing != nil && existing.State != CodexPrewarmFailed && existing.State != CodexPrewarmExpired && existing.State != CodexPrewarmCancelled && existing.State != CodexPrewarmAdopted {
		return CodexPrewarmReservation{}, errors.New("prewarm reservation already exists")
	}
	reservation := &CodexPrewarmReservation{Lane: lane, Correlation: correlation, State: CodexPrewarmCreating, Generation: 1, LastSeen: manager.now()}
	manager.reservations[lane] = reservation
	return *reservation, nil
}

func (manager *CodexPrewarmManager) Bind(lane LaneKey, account codex.AccountKey, socketGeneration uint64) (CodexPrewarmReservation, error) {
	return manager.BindSockets(lane, account, socketGeneration, socketGeneration)
}

func (manager *CodexPrewarmManager) BindSockets(lane LaneKey, account codex.AccountKey, downstreamSocketGeneration, upstreamSocketGeneration uint64) (CodexPrewarmReservation, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	reservation := manager.reservations[lane]
	if reservation == nil || reservation.State != CodexPrewarmCreating || account == "" || downstreamSocketGeneration == 0 || upstreamSocketGeneration == 0 {
		return CodexPrewarmReservation{}, errors.New("prewarm cannot bind")
	}
	reservation.State = CodexPrewarmBoundActive
	reservation.AccountKey = account
	reservation.SocketGeneration = upstreamSocketGeneration
	reservation.DownstreamSocketGeneration = downstreamSocketGeneration
	reservation.UpstreamSocketGeneration = upstreamSocketGeneration
	reservation.Generation++
	reservation.LastSeen = manager.now()
	return *reservation, nil
}

func (manager *CodexPrewarmManager) Ready(lane LaneKey, responseAnchor, turnState string) (CodexPrewarmReservation, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	reservation := manager.reservations[lane]
	if reservation == nil || reservation.State != CodexPrewarmBoundActive || responseAnchor == "" {
		return CodexPrewarmReservation{}, errors.New("prewarm cannot become ready")
	}
	reservation.State = CodexPrewarmReady
	reservation.ResponseAnchor = responseAnchor
	reservation.TurnState = turnState
	reservation.Generation++
	reservation.LastSeen = manager.now()
	return *reservation, nil
}

func (manager *CodexPrewarmManager) ReplaceCorrelation(lane LaneKey, expected, replacement string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	reservation := manager.reservations[lane]
	if reservation == nil || reservation.Correlation != expected || replacement == "" || reservation.State != CodexPrewarmBoundActive {
		return errors.New("prewarm correlation cannot change")
	}
	reservation.Correlation = replacement
	reservation.Generation++
	reservation.LastSeen = manager.now()
	return nil
}

func (manager *CodexPrewarmManager) Disconnect(lane LaneKey) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	reservation := manager.reservations[lane]
	if reservation == nil || (reservation.State != CodexPrewarmBoundActive && reservation.State != CodexPrewarmReady) {
		return errors.New("prewarm cannot disconnect")
	}
	reservation.State = CodexPrewarmDisconnected
	reservation.SocketGeneration = 0
	reservation.DownstreamSocketGeneration = 0
	reservation.UpstreamSocketGeneration = 0
	reservation.Generation++
	reservation.LastSeen = manager.now()
	return nil
}

func (manager *CodexPrewarmManager) cancel(lane LaneKey, correlation string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	reservation := manager.reservations[lane]
	if reservation == nil || reservation.Correlation != correlation ||
		(reservation.State != CodexPrewarmCreating && reservation.State != CodexPrewarmBoundActive && reservation.State != CodexPrewarmReady) {
		return errors.New("prewarm cannot cancel")
	}
	reservation.State = CodexPrewarmCancelled
	reservation.SocketGeneration = 0
	reservation.DownstreamSocketGeneration = 0
	reservation.UpstreamSocketGeneration = 0
	reservation.Generation++
	reservation.LastSeen = manager.now()
	return nil
}

// Adopt is retained only as a fail-closed compatibility seam. Durable
// promotion must use CodexContinuityCoordinator.AdoptPrewarm.
func (manager *CodexPrewarmManager) Adopt(LeaseKey, string) (CodexTurnLease, error) {
	return CodexTurnLease{}, ErrCodexLeaseWriterUnavailable
}

func (manager *CodexPrewarmManager) Restore(reservations []CodexPrewarmReservation) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, reservation := range reservations {
		if reservation.State == CodexPrewarmAdopted {
			delete(manager.reservations, reservation.Lane)
			continue
		}
		if reservation.State == CodexPrewarmCreating || reservation.State == CodexPrewarmBoundActive || reservation.State == CodexPrewarmReady {
			reservation.State = CodexPrewarmDisconnected
			reservation.SocketGeneration = 0
			reservation.DownstreamSocketGeneration = 0
			reservation.UpstreamSocketGeneration = 0
			reservation.Generation++
		}
		copy := reservation
		manager.reservations[copy.Lane] = &copy
	}
}

func (manager *CodexPrewarmManager) snapshot(lane LaneKey) CodexPrewarmReservation {
	if manager == nil {
		return CodexPrewarmReservation{}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	reservation := manager.reservations[lane]
	if reservation == nil {
		return CodexPrewarmReservation{}
	}
	return *reservation
}
