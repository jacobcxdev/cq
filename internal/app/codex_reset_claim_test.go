package app

import (
	"context"
	"errors"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestResetAutomationAdmissionPauseDuringPreflightPreventsClaim(t *testing.T) {
	a, backend, usage, attempts := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	paused := false
	errPaused := errors.New("runtime admissions paused")
	a.ScheduleClaim = func(ctx context.Context, claim func() error) error {
		if paused {
			return errPaused
		}
		return claim()
	}
	a.Usage = resetAutomationUsageHook{results: usage.results, hook: func() { paused = true }}
	if err := a.StepSchedule(context.Background()); !errors.Is(err, errPaused) {
		t.Fatalf("paused preflight = %v, want paused error", err)
	}
	state, err := a.Schedule.Load()
	if err != nil {
		t.Fatal(err)
	}
	if backend.consumeCalls != 0 || attempts.ensureCalls != 0 || state.Entries[0].State != codex.ResetAutomationPending {
		t.Fatalf("paused preflight claimed or consumed: %+v, POSTs=%d, attempts=%d", state.Entries[0], backend.consumeCalls, attempts.ensureCalls)
	}
	paused = false
	a.Usage = usage
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.consumeCalls != 1 {
		t.Fatalf("resumed executor consumed %d times", backend.consumeCalls)
	}
}
