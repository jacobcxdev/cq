package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func automationFixture(t *testing.T) (*CodexResetApp, *recordingResetBackend, *recordingResetUsage, *recordingResetAttempts) {
	t.Helper()
	a, b, u, p := completeResetApp(time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC))
	b.consume = codex.ConsumeResetResult{Outcome: codex.ConsumeReset, WindowsReset: 2}
	a.Schedule = codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	return a, b, u, p
}
func automationChange(t *testing.T, a *CodexResetApp, c CodexResetScheduleChange) codex.ResetAutomationState {
	t.Helper()
	p, err := a.PrepareSchedule(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.ApplySchedule(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestResetAutomationManualDefaultsAndRevisionFence(t *testing.T) {
	a, b, _, _ := automationFixture(t)
	at := a.now().Add(time.Hour)
	p, err := a.PrepareSchedule(context.Background(), CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	if err != nil {
		t.Fatal(err)
	}
	if p.State.Enabled || p.State.Mode != codex.ResetAutomationFixed || len(p.State.Entries) != 1 || !p.State.Entries[0].Pinned || b.consumeCalls != 0 {
		t.Fatalf("bad preview: %+v", p.State)
	}
	if s, _ := a.Schedule.Load(); s.Revision != 0 {
		t.Fatal("preview persisted")
	}
	s, err := a.ApplySchedule(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ApplySchedule(context.Background(), p); err == nil {
		t.Fatal("stale preview accepted")
	}
	s = automationChange(t, a, CodexResetScheduleChange{Action: "mode", Mode: codex.ResetAutomationAdaptive})
	if s.Mode != codex.ResetAutomationAdaptive || !s.Entries[0].At.Equal(at) {
		t.Fatal("mode moved pin")
	}
	s = automationChange(t, a, CodexResetScheduleChange{Action: "remove", EntryID: s.Entries[0].ID})
	if s.Entries[0].State != codex.ResetAutomationCancelled {
		t.Fatal("entry not cancelled")
	}
}

func TestResetAutomationRejectsPastManualTimes(t *testing.T) {
	a, backend, _, _ := automationFixture(t)
	past := a.now().Add(-time.Second)
	if _, err := a.PrepareSchedule(context.Background(), CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &past}); err == nil {
		t.Fatal("accepted past manual addition")
	}
	future := a.now().Add(time.Hour)
	state := automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &future})
	if _, err := a.PrepareSchedule(context.Background(), CodexResetScheduleChange{Action: "set", EntryID: state.Entries[0].ID, At: &past}); err == nil {
		t.Fatal("accepted past manual edit")
	}
	current, err := a.Schedule.Load()
	if err != nil || current.Revision != state.Revision || !current.Entries[0].At.Equal(future) || backend.consumeCalls != 0 {
		t.Fatalf("invalid manual time changed schedule: %+v, %v", current, err)
	}
}

func TestResetAutomationDueExecutionAndCancellationMerge(t *testing.T) {
	a, b, _, attempts := automationFixture(t)
	at := a.now()
	s := automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.consumeCalls != 0 {
		t.Fatal("disabled consumed")
	}
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	attempts.ensureHook = func() {
		claimed, err := a.Schedule.Load()
		if err != nil || claimed.Entries[0].State != codex.ResetAutomationRunning {
			t.Error("attempt persisted before schedule claim")
		}
	}

	b.consumeHook = func() {
		running, _ := a.Schedule.Load()
		if running.Entries[0].State != codex.ResetAutomationRunning || attempts.ensureCalls == 0 {
			t.Error("POST before durable claim")
		}
		automationChange(t, a, CodexResetScheduleChange{Action: "cancel"})
	}
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ = a.Schedule.Load()
	if b.consumeCalls != 1 || s.Enabled || s.Entries[0].State != codex.ResetAutomationCompleted {
		t.Fatalf("bad completion: %+v calls=%d", s, b.consumeCalls)
	}
}
func TestResetAutomationFullAndExpiredNeverConsume(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "expired"}[expired], func(t *testing.T) {
			a, b, u, _ := automationFixture(t)
			at := a.now()
			automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
			automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
			if expired {
				a.Clock = resetTestClock{a.now().Add(25 * time.Hour)}
			} else {
				for name, w := range u.results[0].Windows {
					w.RemainingPct = 100
					u.results[0].Windows[name] = w
				}
			}
			if err := a.StepSchedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			s, _ := a.Schedule.Load()
			want := codex.ResetAutomationSkipped
			if expired {
				want = codex.ResetAutomationMissed
			}
			if b.consumeCalls != 0 || s.Entries[0].State != want {
				t.Fatalf("state %+v calls %d", s, b.consumeCalls)
			}
		})
	}
}

func TestResetAutomationExecutorLockExcludesAnotherStore(t *testing.T) {
	a, backend, _, _ := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	other := codex.NewResetAutomationStore(fsutil.OSFileSystem{}, a.Schedule.Path)
	lock, err := other.AcquireExecutor()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := a.StepSchedule(context.Background()); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("competing executor = %v", err)
	}
	if backend.consumeCalls != 0 {
		t.Fatal("competing executor consumed")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.consumeCalls != 1 {
		t.Fatalf("released executor consumed %d times", backend.consumeCalls)
	}
}

func TestResetAutomationExecutorLockExcludesManualUse(t *testing.T) {
	a, backend, _, attempts := automationFixture(t)
	plan, err := a.PrepareUse(context.Background(), "a@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	other := codex.NewResetAutomationStore(fsutil.OSFileSystem{}, a.Schedule.Path)
	lock, err := other.AcquireExecutor()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := a.ExecuteUse(context.Background(), plan); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("manual use bypassed executor: %v", err)
	}
	if backend.consumeCalls != 0 || attempts.ensureCalls != 0 {
		t.Fatal("competing manual use persisted or consumed an attempt")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExecuteUse(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if backend.consumeCalls != 1 {
		t.Fatalf("released manual use consumed %d times", backend.consumeCalls)
	}
}

func TestResetAutomationUnknownResponseReplaysExactCreditAfterExpiry(t *testing.T) {
	a, b, u, p := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	b.consumeErr = errors.New("lost response")
	if err := a.StepSchedule(context.Background()); err == nil {
		t.Fatal("lost response hidden")
	}
	s, _ := a.Schedule.Load()
	if s.Entries[0].State != codex.ResetAutomationRetrying || len(p.pending["account-a"]) != 1 {
		t.Fatal("attempt lost")
	}
	b.consumeErr = nil
	b.consume.Outcome = codex.ConsumeAlreadyRedeemed
	b.credits["account-a"] = codex.ResetCreditInventory{}
	a.Clock = resetTestClock{a.now().Add(25 * time.Hour)}
	u.results = resetAppUsage(a.now(), "a", "b", "c")
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ = a.Schedule.Load()
	if s.Entries[0].State != codex.ResetAutomationCompleted || b.consumeCalls != 2 {
		t.Fatalf("retry failed %+v", s)
	}
}
func TestResetAutomationCancellationDuringPreflightWins(t *testing.T) {
	a, b, u, _ := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	a.Usage = resetAutomationUsageHook{results: u.results, hook: func() { automationChange(t, a, CodexResetScheduleChange{Action: "disable"}) }}
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.consumeCalls != 0 {
		t.Fatal("cancelled preflight consumed")
	}
}

func TestResetAutomationManualRetryCompletesScheduledEntry(t *testing.T) {
	a, b, _, attempts := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	b.consumeErr = errors.New("lost response")
	if err := a.StepSchedule(context.Background()); err == nil {
		t.Fatal("lost response hidden")
	}
	b.consumeErr = nil
	b.consume.Outcome = codex.ConsumeAlreadyRedeemed
	b.credits["account-a"] = codex.ResetCreditInventory{}
	plan, err := a.PrepareUse(context.Background(), "a@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	attempts.removeCalls = 0
	b.consumeHook = func() { automationChange(t, a, CodexResetScheduleChange{Action: "disable"}) }
	result, err := a.ExecuteUse(context.Background(), plan)
	if err != nil || result.Outcome != codex.ConsumeAlreadyRedeemed {
		t.Fatalf("manual retry = %+v, %v", result, err)
	}
	state, err := a.Schedule.Load()
	if err != nil || state.Entries[0].State != codex.ResetAutomationCompleted || state.Entries[0].Outcome != codex.ConsumeAlreadyRedeemed || state.Entries[0].NextAttemptAt != nil || state.PlanValid || state.Enabled {
		t.Fatalf("manual outcome not merged: %+v, %v", state, err)
	}
	if len(attempts.pending["account-a"]) != 0 || attempts.removeCalls != 1 || b.consumeCalls != 2 {
		t.Fatal("manual completion did not clean exactly its resolved attempt")
	}
	if err := a.StepSchedule(context.Background()); err != nil || b.consumeCalls != 2 {
		t.Fatal("resolved manual retry consumed again")
	}
}

func TestResetAutomationClaimWithoutAttemptReplaysMissingCredit(t *testing.T) {
	for _, outcome := range []codex.ConsumeResetOutcome{codex.ConsumeAlreadyRedeemed, codex.ConsumeNoCredit} {
		t.Run(string(outcome), func(t *testing.T) {
			a, b, _, attempts := automationFixture(t)
			requests := &automationRequestBackend{recordingResetBackend: b}
			a.Backend = requests
			at := a.now()
			automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
			automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
			_, err := a.Schedule.Update(context.Background(), func(s *codex.ResetAutomationState) error {
				s.Entries[0].State = codex.ResetAutomationRetrying
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			b.credits["account-a"] = codex.ResetCreditInventory{}
			b.consume.Outcome = outcome
			if err := a.StepSchedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, _ := a.Schedule.Load()
			want := codex.ResetAutomationCompleted
			if outcome == codex.ConsumeNoCredit {
				want = codex.ResetAutomationMissed
			}
			if state.Entries[0].State != want || state.Entries[0].Outcome != outcome || b.consumeCalls != 1 || len(attempts.pending["account-a"]) != 0 {
				t.Fatalf("claim not reconciled: %+v calls=%d", state, b.consumeCalls)
			}
			if requests.creditIDs[0] != "credit-account-a" || requests.keys[0] != codex.ResetIdempotencyKey("account-a", "credit-account-a") {
				t.Fatal("claim recovery changed credit or request key")
			}
		})
	}
}

func TestResetAutomationManualOutcomeSurvivesScheduleSaveFailure(t *testing.T) {
	a, b, _, attempts := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	plan, err := a.PrepareUse(context.Background(), "a@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	originalPath := a.Schedule.Path
	b.consumeHook = func() { a.Schedule.Path = "invalid-relative-path" }
	result, err := a.ExecuteUse(context.Background(), plan)
	if err != nil || result.Outcome != codex.ConsumeReset || len(result.Warnings) != 1 || result.Warnings[0].Code != "schedule_update_failed" {
		t.Fatalf("manual success lost: %+v, %v", result, err)
	}
	if len(attempts.pending["account-a"]) != 1 || attempts.removeCalls != 0 {
		t.Fatal("retry record removed before schedule outcome saved")
	}
	a.Schedule.Path = originalPath
	b.consumeHook = nil
	b.credits["account-a"] = codex.ResetCreditInventory{}
	b.consume.Outcome = codex.ConsumeAlreadyRedeemed
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := a.Schedule.Load()
	if state.Entries[0].State != codex.ResetAutomationCompleted || len(attempts.pending["account-a"]) != 0 {
		t.Fatalf("manual success not recovered: %+v", state)
	}
}

func TestResetAutomationClaimRecoveryDoesNotReplaceUnrelatedAttempt(t *testing.T) {
	a, b, _, attempts := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	_, err := a.Schedule.Update(context.Background(), func(s *codex.ResetAutomationState) error {
		s.Entries[0].State = codex.ResetAutomationRunning
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = attempts.Ensure("account-a", "unrelated-credit", a.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StepSchedule(context.Background()); err == nil {
		t.Fatal("unrelated unresolved attempt ignored")
	}
	if b.consumeCalls != 0 || len(attempts.pending["account-a"]) != 1 || attempts.pending["account-a"][0].CreditID != "unrelated-credit" {
		t.Fatal("recovery replaced unrelated pending attempt")
	}
}

func TestResetAutomationAcceptDrainedWeeklyPortfolioRunsInitialReset(t *testing.T) {
	a, b, u, _ := automationFixture(t)
	for _, result := range u.results {
		delete(result.Windows, quota.Window5Hour)
		w := result.Windows[quota.Window7Day]
		w.RemainingPct = 0
		result.Windows[quota.Window7Day] = w
	}
	state := automationChange(t, a, CodexResetScheduleChange{Action: "accept", Mode: codex.ResetAutomationAdaptive})
	if !state.Enabled || b.consumeCalls != 0 {
		t.Fatal("acceptance did not leave execution to executor")
	}
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = a.Schedule.Load()
	completed := 0
	for _, e := range state.Entries {
		if e.State == codex.ResetAutomationCompleted {
			completed++
		}
	}
	if b.consumeCalls != 1 || completed != 1 {
		t.Fatalf("initial due reset did not execute: %+v calls=%d", state, b.consumeCalls)
	}
}

type resetAutomationUsageHook struct {
	results []quota.Result
	hook    func()
}

func (u resetAutomationUsageHook) Fetch(context.Context, time.Time) ([]quota.Result, error) {
	u.hook()
	return u.results, nil
}

func TestResetAutomationAdaptiveFailureBlocksFollowersButNotPins(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "following", true: "pinned"}[pinned], func(t *testing.T) {
			a, b, u, _ := automationFixture(t)
			at := a.now()
			s := automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
			automationChange(t, a, CodexResetScheduleChange{Action: "mode", Mode: codex.ResetAutomationAdaptive})
			if !pinned {
				automationChange(t, a, CodexResetScheduleChange{Action: "set", EntryID: s.Entries[0].ID, FollowMode: true})
				_, err := a.Schedule.Update(context.Background(), func(s *codex.ResetAutomationState) error { s.Entries[0].At = &at; return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			u.results = u.results[:1] // Missing other portfolio accounts makes adaptive forecast incomplete.
			automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
			if err := a.StepSchedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, _ := a.Schedule.Load()
			if pinned {
				if b.consumeCalls != 1 || state.Entries[0].State != codex.ResetAutomationCompleted {
					t.Fatalf("pin blocked by unrelated forecast: %+v", state)
				}
			} else {
				if b.consumeCalls != 0 || state.PlanValid || state.Entries[0].State != codex.ResetAutomationBlocked || !state.Entries[0].At.Equal(at) {
					t.Fatalf("stale plan executed or lost date: %+v", state)
				}
				state = automationChange(t, a, CodexResetScheduleChange{Action: "mode", Mode: codex.ResetAutomationFixed})
				if state.Entries[0].At != nil || state.Entries[0].LastError != "time_required" {
					t.Fatal("stale date resurrected in fixed mode")
				}
			}
		})
	}
}
func TestResetAutomationAdaptiveKeepsUndatedAuthorisationAndNeverEnrolsCredits(t *testing.T) {
	a, b, u, _ := automationFixture(t)
	at := a.now()
	s := automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "mode", Mode: codex.ResetAutomationAdaptive})
	automationChange(t, a, CodexResetScheduleChange{Action: "set", EntryID: s.Entries[0].ID, FollowMode: true})
	for _, result := range u.results {
		for name, w := range result.Windows {
			w.RemainingPct = 100
			result.Windows[name] = w
		}
	}
	state := automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	if !state.PlanValid || state.Entries[0].At != nil || state.Entries[0].LastError != "not_yet_useful" {
		t.Fatalf("unused authorisation lost %+v", state)
	}
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.consumeCalls != 0 {
		t.Fatal("full adaptive entry consumed")
	}
	a.Clock = resetTestClock{a.now().Add(6 * time.Minute)}
	u.results = resetAppUsage(a.now(), "a", "b", "c")
	inventory := b.credits["account-a"]
	inventory.Credits = append(inventory.Credits, resetAppCredit("new-unaccepted", a.now().Add(24*time.Hour)))
	b.credits["account-a"] = inventory
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = a.Schedule.Load()
	if len(state.Entries) != 1 || state.Entries[0].CreditID != "credit-account-a" || state.Entries[0].At == nil {
		t.Fatalf("reconsideration failed %+v", state)
	}
}
func TestResetAutomationAttemptSurvivesClaimSaveGap(t *testing.T) {
	a, b, u, p := automationFixture(t)
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	// A durable attempt exists, but the process died before its schedule claim saved.
	_, err := p.Ensure("account-a", "credit-account-a", a.now())
	if err != nil {
		t.Fatal(err)
	}
	a.Clock = resetTestClock{a.now().Add(25 * time.Hour)}
	u.results = resetAppUsage(a.now(), "a", "b", "c")
	b.credits["account-a"] = codex.ResetCreditInventory{}
	b.consume.Outcome = codex.ConsumeAlreadyRedeemed
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := a.Schedule.Load()
	if b.consumeCalls != 1 || state.Entries[0].State != codex.ResetAutomationCompleted {
		t.Fatalf("durable attempt discarded %+v", state)
	}
}
func TestResetAutomationAcceptReplacementPreservesDisabledMode(t *testing.T) {
	a, b, _, _ := automationFixture(t)
	state := automationChange(t, a, CodexResetScheduleChange{Action: "accept"})
	if !state.Enabled || state.Mode != codex.ResetAutomationFixed || b.consumeCalls != 0 {
		t.Fatal("acceptance defaults wrong")
	}
	if _, err := a.PrepareSchedule(context.Background(), CodexResetScheduleChange{Action: "accept"}); err == nil {
		t.Fatal("replaced without flag")
	}
	automationChange(t, a, CodexResetScheduleChange{Action: "disable"})
	automationChange(t, a, CodexResetScheduleChange{Action: "mode", Mode: codex.ResetAutomationAdaptive})
	state = automationChange(t, a, CodexResetScheduleChange{Action: "accept", Replace: true})
	if state.Enabled || state.Mode != codex.ResetAutomationAdaptive {
		t.Fatal("replacement changed enabled or mode")
	}
	cancelled := 0
	for _, entry := range state.Entries {
		if entry.State == codex.ResetAutomationCancelled {
			cancelled++
		}
	}
	if cancelled == 0 || b.consumeCalls != 0 {
		t.Fatal("replacement lost history or consumed")
	}
}

func TestResetAutomationRestartAfterResultSaveFailureReusesDurableRequest(t *testing.T) {
	a, b, u, _ := automationFixture(t)
	requests := &automationRequestBackend{recordingResetBackend: b}
	a.Backend = requests
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	attempts, err := codex.NewResetAttemptStore(fsutil.OSFileSystem{}, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	a.Attempts = attempts
	at := a.now()
	automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	originalPath := a.Schedule.Path
	// The POST succeeds, but the result cannot be saved. Keep its retry record.
	b.consumeHook = func() { a.Schedule.Path = "relative-invalid-path" }
	if err := a.StepSchedule(context.Background()); err == nil {
		t.Fatal("result-save failure hidden")
	}
	a.Schedule = codex.NewResetAutomationStore(fsutil.OSFileSystem{}, originalPath)
	state, err := a.Schedule.Load()
	if err != nil || state.Entries[0].State != codex.ResetAutomationRunning {
		t.Fatalf("durable claim lost %+v %v", state, err)
	}
	a.Attempts, err = codex.NewResetAttemptStore(fsutil.OSFileSystem{}, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := a.Attempts.Pending("account-a")
	if err != nil || len(pending) != 1 {
		t.Fatal("attempt removed before result saved")
	}
	b.consumeHook = nil
	b.consume.Outcome = codex.ConsumeAlreadyRedeemed
	b.credits["account-a"] = codex.ResetCreditInventory{}
	a.Clock = resetTestClock{a.now().Add(25 * time.Hour)}
	u.results = resetAppUsage(a.now(), "a", "b", "c")
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = a.Schedule.Load()
	if state.Entries[0].State != codex.ResetAutomationCompleted || len(requests.keys) != 2 || requests.keys[0] != requests.keys[1] || requests.creditIDs[0] != requests.creditIDs[1] {
		t.Fatalf("restart changed request: %+v %v %v", state, requests.keys, requests.creditIDs)
	}
	pending, err = a.Attempts.Pending("account-a")
	if err != nil || len(pending) != 0 {
		t.Fatal("completed attempt not cleaned")
	}
}

type automationRequestBackend struct {
	*recordingResetBackend
	keys, creditIDs []string
}

func (b *automationRequestBackend) Consume(ctx context.Context, account codex.ResetAccount, creditID, key string) (codex.ConsumeResetResult, error) {
	b.keys = append(b.keys, key)
	b.creditIDs = append(b.creditIDs, creditID)
	return b.recordingResetBackend.Consume(ctx, account, creditID, key)
}

func TestResetAutomationAdaptiveDueNowUsesClockAfterFreshPlan(t *testing.T) {
	a, b, u, _ := automationFixture(t)
	for _, result := range u.results {
		for name, w := range result.Windows {
			w.RemainingPct = 0
			result.Windows[name] = w
		}
	}
	at := a.now()
	state := automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &at})
	automationChange(t, a, CodexResetScheduleChange{Action: "mode", Mode: codex.ResetAutomationAdaptive})
	automationChange(t, a, CodexResetScheduleChange{Action: "set", EntryID: state.Entries[0].ID, FollowMode: true})
	automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
	a.Clock = &automationAdvancingClock{at: a.now()}
	if err := a.StepSchedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.consumeCalls != 1 {
		t.Fatal("fresh due-now date was deferred behind the pre-fetch clock")
	}
}

type automationAdvancingClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *automationAdvancingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(time.Second)
	return c.at
}
