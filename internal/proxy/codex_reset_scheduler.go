package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const RuntimeCodexResetScheduleStatusPath = "/_cq/control/codex/resets/status"
const CodexResetSchedulerHeartbeatMaxAge = 60 * time.Second

type CodexResetSchedulerStatus struct {
	Running   bool      `json:"running"`
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checked_at"`
	LastError string    `json:"last_error,omitempty"`
}

// CodexResetScheduler stays inert until the supervisor selects this worker.
// Its heartbeat is process-local; an idle loop never fsyncs schedule state.
type CodexResetScheduler struct {
	mu      sync.Mutex
	step    func(context.Context) error
	now     func() time.Time
	gate    *RuntimeUpgradeAdmission
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
	status  CodexResetSchedulerStatus
}

func NewCodexResetScheduler(step func(context.Context) error, now func() time.Time) *CodexResetScheduler {
	if now == nil {
		now = time.Now
	}
	return &CodexResetScheduler{step: step, now: now, status: CodexResetSchedulerStatus{State: "inactive"}}
}
func (s *CodexResetScheduler) Activate(ctx context.Context, gate *RuntimeUpgradeAdmission) error {
	if s == nil || ctx == nil || gate == nil || s.step == nil {
		return errors.New("reset scheduler unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("reset scheduler stopped")
	}
	if s.cancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.gate = gate
	s.done = make(chan struct{})
	go s.run(runCtx)
	return nil
}

// Claim serialises a durable reset claim with admission pause. Preflight work
// can finish after a pause, but it cannot start a new consume request.
func (s *CodexResetScheduler) Claim(ctx context.Context, claim func() error) error {
	if s == nil || ctx == nil || claim == nil {
		return errors.New("reset scheduler unavailable")
	}
	s.mu.Lock()
	gate, active := s.gate, s.cancel != nil && !s.stopped
	s.mu.Unlock()
	if gate == nil || !active {
		return errors.New("reset scheduler inactive")
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if gate.paused {
		return ErrRuntimeUpgradePaused
	}
	return claim()
}

func (s *CodexResetScheduler) run(ctx context.Context) {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status.Running = false
		s.status.CheckedAt = s.now().UTC()
		if recover() != nil {
			s.status.State = "failed"
			s.status.LastError = "executor_panic"
		} else {
			s.status.State = "stopped"
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		release, err := s.gate.BeginRequest()
		s.mu.Lock()
		s.status = CodexResetSchedulerStatus{Running: err == nil, State: "running", CheckedAt: s.now().UTC()}
		if err != nil {
			s.status.State = "paused"
		}
		s.mu.Unlock()
		if err == nil {
			func() {
				defer release()
				stepCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				stepErr := s.step(stepCtx)
				s.mu.Lock()
				defer s.mu.Unlock()
				s.status.CheckedAt = s.now().UTC()
				if stepErr != nil {
					s.status.LastError = "execution_failed"
					if errors.Is(stepErr, fsutil.ErrExclusiveLockHeld) {
						s.status.Running = false
						s.status.State = "executor_busy"
					}
				} else {
					s.status.LastError = ""
				}
			}()
		}
		timer.Reset(5 * time.Second)
	}
}
func (s *CodexResetScheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	cancel, done := s.cancel, s.done
	s.status.Running = false
	s.status.State = "stopped"
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
func (s *CodexResetScheduler) Status() CodexResetSchedulerStatus {
	if s == nil {
		return CodexResetSchedulerStatus{State: "unavailable"}
	}
	s.mu.Lock()
	status := s.status
	gate := s.gate
	now := s.now()
	s.mu.Unlock()
	if gate != nil {
		gate.mu.Lock()
		paused := gate.paused
		gate.mu.Unlock()
		if paused && status.Running {
			status.Running = false
			status.State = "paused"
		}
	}
	if status.Running && (status.CheckedAt.IsZero() || status.CheckedAt.After(now) || now.Sub(status.CheckedAt) > CodexResetSchedulerHeartbeatMaxAge) {
		status.Running = false
		status.State = "stale"
	}
	return status
}
func (s *Server) handleCodexResetScheduleStatus(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.CodexResetScheduler == nil {
		http.Error(w, "reset scheduler unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.CodexResetScheduler.Status())
}
