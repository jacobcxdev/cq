package app

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/history"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func reserveResetFixture(t *testing.T) *CodexResetApp {
	t.Helper()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	a, _, u, _ := completeResetApp(now)
	estimates := make(history.RateEstimates)
	for i := range u.results {
		remaining := 0
		if i == 0 {
			remaining = 5
		}
		u.results[i].Windows = map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: remaining, ResetAtUnix: now.Add(7 * 24 * time.Hour).Unix()}}
		estimates[history.BurnRateKey{ProviderID: "codex", AccountKey: u.results[i].AccountID, Window: "7d"}] = history.RateEstimate{RatePctPerS: 0.001, Samples: 3}
	}
	a.History = &recordingResetHistory{estimates: estimates}
	a.Reserve = func(context.Context, codex.ResetAccountSnapshot, []quota.Result, time.Time) (quota.ReservePolicy, error) {
		return quota.ReservePolicy{AccountKey: "account-a", Window: quota.Window7Day, Percent: 5}, nil
	}
	return a
}

func TestResetReserveDoesNotPostponeAnotherAccount(t *testing.T) {
	a := reserveResetFixture(t)
	c := &aggregate.ResetScheduleConstraints{NextAccountKey: "account-b", AllowedCredits: []aggregate.ResetScheduleCreditRef{{AccountKey: "account-b", CreditID: "credit-account-b"}}}
	s, err := a.recommendConstrained(context.Background(), true, c)
	if err != nil || !s.Complete || len(s.Items) != 1 || s.Items[0].Status != aggregate.ResetDueNow {
		t.Fatalf("protected quota postponed depleted account: %+v, %v", s, err)
	}
}

func TestResetReserveSystemFirstCanSpendBeforeReset(t *testing.T) {
	a := reserveResetFixture(t)
	c := &aggregate.ResetScheduleConstraints{NextAccountKey: "account-a", AllowedCredits: []aggregate.ResetScheduleCreditRef{{AccountKey: "account-a", CreditID: "credit-account-a"}}}
	s, err := a.recommendConstrained(context.Background(), true, c)
	if err != nil || !s.Complete || len(s.Items) != 1 || !s.Items[0].UseBy.After(a.now()) {
		t.Fatalf("system reserve was not available before its refill: %+v, %v", s, err)
	}
}

func TestResetReserveReadFailureBlocksRecommendation(t *testing.T) {
	a := reserveResetFixture(t)
	a.Reserve = func(context.Context, codex.ResetAccountSnapshot, []quota.Result, time.Time) (quota.ReservePolicy, error) {
		return quota.ReservePolicy{}, errors.New("unreadable policy")
	}
	s, err := a.Recommend(context.Background())
	if err == nil || s.Complete {
		t.Fatalf("unreadable reserve granted actionable forecast: %+v, %v", s, err)
	}
}

func TestReserveDrainRateUsesPoolAndHonoursOtherWindow(t *testing.T) {
	a := reserveResetFixture(t)
	u := a.Usage.(*recordingResetUsage)
	for i := range u.results {
		u.results[i].AccountKey = "account-" + string(rune('a'+i))
	}
	policy := quota.ReservePolicy{AccountKey: "account-a", Window: quota.Window7Day, Percent: 5}
	estimates := a.History.(*recordingResetHistory).estimates
	if got := CodexReserveDrainRate(a.now(), policy, u.results, estimates); math.Abs(got-0.003) > 1e-12 {
		t.Fatalf("pooled rate=%g, want .003", got)
	}
	u.results[0].Windows[quota.Window5Hour] = quota.Window{RemainingPct: 0, ResetAtUnix: a.now().Add(time.Hour).Unix()}
	w := u.results[1].Windows[quota.Window7Day]
	w.RemainingPct = 50
	u.results[1].Windows[quota.Window7Day] = w
	if got := CodexReserveDrainRate(a.now(), policy, u.results, estimates); got != 0 {
		t.Fatalf("other exhausted window permitted reserve drain: %g", got)
	}
}

func TestReserveDrainRateRejectsInvalidPhysicalQuota(t *testing.T) {
	a := reserveResetFixture(t)
	u := a.Usage.(*recordingResetUsage)
	u.results[0].AccountKey = "account-a"
	w := u.results[0].Windows[quota.Window7Day]
	nan := math.NaN()
	w.RemainingPctExact = &nan
	u.results[0].Windows[quota.Window7Day] = w
	if got := CodexReserveDrainRate(a.now(), quota.ReservePolicy{AccountKey: "account-a", Window: quota.Window7Day, Percent: 5}, u.results, a.History.(*recordingResetHistory).estimates); got != 0 {
		t.Fatalf("invalid quota permitted reserve drain: %g", got)
	}
}

func TestReserveAdaptiveReplanKeepsAcceptedFirstAccount(t *testing.T) {
	for _, first := range []codex.AccountKey{"account-a", "account-b"} {
		t.Run(string(first), func(t *testing.T) {
			a := reserveResetFixture(t)
			now := a.now()
			before, due := now.Add(-5*time.Minute), now
			aTime, bTime := now.Add(time.Hour), now.Add(2*time.Hour)
			if first == "account-b" {
				aTime, bTime = bTime, aTime
			}
			expires := now.Add(24 * time.Hour)
			s := codex.ResetAutomationState{Version: 1, Enabled: true, Mode: codex.ResetAutomationAdaptive, PlanValid: true, LastReplanAt: &before, NextReplanAt: &due,
				Entries: []codex.ResetAutomationEntry{
					{ID: "other", AccountKey: "account-b", AccountID: "acct-b", Email: "b@example.com", CreditID: "credit-account-b", At: &bTime, ExpiresAt: &expires, State: codex.ResetAutomationPending},
					{ID: "system", AccountKey: "account-a", AccountID: "acct-a", Email: "a@example.com", CreditID: "credit-account-a", At: &aTime, ExpiresAt: &expires, State: codex.ResetAutomationPending},
				}}
			a.replanAutomationState(context.Background(), &s)
			next, ok := s.NextResetAccount(now)
			if !s.PlanValid || !ok || next != first {
				t.Fatalf("replan changed accepted first account %s: next=%s, valid=%t, error=%s", first, next, s.PlanValid, s.PlanError)
			}
			if first == "account-b" && !s.Entries[0].At.Equal(now) {
				t.Fatal("protected system reserve postponed other reset")
			}
		})
	}
}

func TestReserveAdaptiveBootstrapStaysWithinAcceptedCredits(t *testing.T) {
	a := reserveResetFixture(t)
	expires := a.now().Add(24 * time.Hour)
	s := codex.ResetAutomationState{Version: 1, Enabled: true, Mode: codex.ResetAutomationAdaptive,
		Entries: []codex.ResetAutomationEntry{{ID: "accepted", AccountKey: "account-b", AccountID: "acct-b", Email: "b@example.com", CreditID: "credit-account-b", ExpiresAt: &expires, State: codex.ResetAutomationPending}}}
	a.replanAutomationState(context.Background(), &s)
	next, ok := s.NextResetAccount(a.now())
	if !s.PlanValid || !ok || next != "account-b" || len(s.Entries) != 1 || !s.Entries[0].At.Equal(a.now()) {
		t.Fatalf("undated accepted credit did not bootstrap safely: %+v", s)
	}
}

func TestReserveBlockedBarrierMatchesExecutor(t *testing.T) {
	for _, mode := range []codex.ResetAutomationMode{codex.ResetAutomationFixed, codex.ResetAutomationAdaptive} {
		t.Run(string(mode), func(t *testing.T) {
			a, backend, _, _ := automationFixture(t)
			now, later := a.now(), a.now().Add(time.Hour)
			state := automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "b@example.com", At: &now})
			otherID := state.Entries[0].ID
			automationChange(t, a, CodexResetScheduleChange{Action: "add", Reference: "a@example.com", At: &later})
			if mode == codex.ResetAutomationAdaptive {
				automationChange(t, a, CodexResetScheduleChange{Action: "mode", Mode: mode})
			}
			automationChange(t, a, CodexResetScheduleChange{Action: "enable"})
			state, err := a.Schedule.Update(context.Background(), func(s *codex.ResetAutomationState) error {
				e := s.Entry(otherID)
				e.State, e.LastError, e.NextAttemptAt = codex.ResetAutomationBlocked, "preflight_failed", &now
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if next, ok := state.NextResetAccount(now); ok {
				t.Fatalf("blocked earlier reset granted release: %s", next)
			}
			if err := a.StepSchedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err = a.Schedule.Load()
			if err != nil || state.Entry(otherID).State != codex.ResetAutomationCompleted || backend.consumeCalls != 1 {
				t.Fatalf("executor did not retry earlier blocked account: %+v, %v", state, err)
			}
		})
	}
}
