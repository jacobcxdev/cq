package aggregate

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestAdaptiveResetStartsAtDepletionThenSlowsToPinnedRefill(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	first := now.Add(time.Hour)
	second := first.Add(4 * time.Hour)
	account := scheduleTestAccount(now, "account", 20, 50, 50/time.Hour.Seconds(),
		ResetScheduleCreditInput{ID: "first", Supported: true})
	account.Credits = append(account.Credits, ResetScheduleCreditInput{ID: "second", Supported: true})
	delete(account.Windows, quota.Window5Hour)
	account.Windows[quota.Window7Day] = resetTestWindow(50, now.Add(8*time.Hour), 7*24*time.Hour, 50/time.Hour.Seconds())
	plan := RecommendResetSchedule(ResetScheduleInput{
		Now: now, Accounts: []ResetScheduleAccountInput{account}, AdaptivePace: true,
		Constraints: &ResetScheduleConstraints{
			AllowedCredits: []ResetScheduleCreditRef{{AccountKey: "account", CreditID: "first"}, {AccountKey: "account", CreditID: "second"}},
			FixedUses:      []ResetScheduleFixedUse{{AccountKey: "account", CreditID: "second", At: second}},
		},
	})
	if !plan.Complete || len(plan.Items) != 2 || plan.Items[0].UseAt.Round(time.Second) != first || !plan.Items[1].UseAt.Equal(second) {
		t.Fatalf("expected depletion and pin, got %+v", plan)
	}
	if plan.NextPace == nil || plan.NextPace.From.Round(time.Second) != first || !plan.NextPace.Until.Equal(second) ||
		plan.NextPace.Percent != 50 || plan.Objective.GapDurationSeconds != 0 {
		t.Fatalf("expected full first refill to stretch at half pace to the pin: %+v", plan)
	}
}

func TestAdaptiveResetWaitsOutSessionGateWithoutWastingWeeklyAllowance(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	account := scheduleTestAccount(now, "account", 20, 50, 50/time.Hour.Seconds(),
		ResetScheduleCreditInput{ID: "credit", Supported: true})
	account.Windows[quota.Window5Hour] = resetTestWindow(25, now.Add(2*time.Hour), 5*time.Hour, 50/time.Hour.Seconds())
	account.Windows[quota.Window7Day] = resetTestWindow(80, now.Add(48*time.Hour), 7*24*time.Hour, 20/time.Hour.Seconds())
	plan := RecommendResetSchedule(ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{account}, AdaptivePace: true})
	if !plan.Complete || plan.NextPace == nil || !plan.NextPace.From.Equal(now) ||
		!plan.NextPace.Until.Equal(now.Add(2*time.Hour)) || plan.NextPace.Percent != 25 {
		t.Fatalf("expected existing session quota to stretch to the natural refill: %+v", plan)
	}
	if item := plan.Items[0]; item.UseAt.Round(time.Second) != now.Add(8*time.Hour+30*time.Minute) || item.RestoredPct[quota.Window7Day] != 100 {
		t.Fatalf("redeemed while weekly quota could still be spent: %+v", item)
	}
}

func TestAdaptiveResetCanUseCreditAfterExpiryReminderIfQuotaRunsOut(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour + 2*time.Minute)
	account := scheduleTestAccount(now, "account", 20, 10, 10/time.Hour.Seconds(),
		ResetScheduleCreditInput{ID: "credit", Supported: true, ExpiresAt: &expiry})
	delete(account.Windows, quota.Window5Hour)
	plan := RecommendResetSchedule(ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{account}, AdaptivePace: true})
	if item := plan.Items[0]; !plan.Complete || item.UseAt.Round(time.Second) != now.Add(time.Hour) || !item.UseAt.Before(expiry) {
		t.Fatalf("should use the credit after spending the remaining quota and before expiry: %+v", plan)
	}
}

func TestAdaptiveResetBridgesSimultaneousDepletion(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	refill := now.Add(2 * time.Hour)
	var accounts []ResetScheduleAccountInput
	for _, key := range []string{"a", "b"} {
		accounts = append(accounts, ResetScheduleAccountInput{
			Key: key, Multiplier: 20, Windows: map[quota.WindowName]ResetScheduleWindowInput{
				quota.Window5Hour: resetTestWindow(100, refill, 5*time.Hour, 700/time.Hour.Seconds()),
			},
		})
	}
	plan := RecommendResetSchedule(ResetScheduleInput{Now: now, Accounts: accounts, AdaptivePace: true})
	if pace := plan.NextPace; !plan.Complete || plan.Objective.GapDurationSeconds != 0 ||
		pace == nil || pace.Percent != 7 || !pace.From.Equal(now) || !pace.Until.Equal(refill) {
		t.Fatalf("should bridge both accounts running out together: %+v", plan)
	}
}

func TestAdaptiveResetDoesNotRedeemAtExpiryDuringAnotherAccountsRefill(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Second)
	refill := now.Add(3 * time.Second)
	account := scheduleTestAccount(now, "a", 20, 50, 50,
		ResetScheduleCreditInput{ID: "credit", Supported: true, ExpiresAt: &expiry})
	delete(account.Windows, quota.Window5Hour)
	account.Windows[quota.Window7Day] = resetTestWindow(50, refill, 7*24*time.Hour, 50)
	gated := ResetScheduleAccountInput{
		Key: "b", Multiplier: 20, Windows: map[quota.WindowName]ResetScheduleWindowInput{
			quota.Window7Day:  resetTestWindow(0, refill.Add(time.Hour), 7*24*time.Hour, 0),
			quota.Window5Hour: resetTestWindow(0, expiry, 5*time.Hour, 0),
		},
	}
	plan := RecommendResetSchedule(ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{account, gated}, AdaptivePace: true})
	if pace := plan.NextPace; !plan.Complete || !plan.Items[0].UseAt.IsZero() || plan.Objective.GapDurationSeconds != 0 ||
		pace == nil || pace.Percent != 33 || !pace.From.Equal(now) || !pace.Until.Equal(refill) {
		t.Fatalf("should stretch the remaining quota across expiry and the gated refill: %+v", plan)
	}
}

func TestAdaptiveResetSpreadsRequiredSlowdownAfterFirstRedemption(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	account := scheduleTestAccount(now, "account", 20, 40, 40/time.Hour.Seconds(),
		ResetScheduleCreditInput{ID: "first", Supported: true})
	account.Credits = append(account.Credits, ResetScheduleCreditInput{ID: "second", Supported: true})
	delete(account.Windows, quota.Window5Hour)
	account.Windows[quota.Window7Day] = resetTestWindow(40, now.Add(7*time.Hour), 7*24*time.Hour, 40/time.Hour.Seconds())
	plan := RecommendResetSchedule(ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{account}, AdaptivePace: true})
	if !plan.Complete || !plan.Items[1].UseAt.After(now.Add(4*time.Hour)) || plan.Objective.GapDurationSeconds != 0 {
		t.Fatalf("expected the slowdown to move the second reset and avoid gaps: %+v", plan)
	}
	if p := plan.NextPace; p == nil || p.Percent != 83 || p.From.Round(time.Second) != now.Add(time.Hour) || p.Until != now.Add(7*time.Hour) {
		t.Fatalf("expected the slower pace to begin at the first redemption, got %+v", p)
	}
}
