package aggregate

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func constrainedResetInput() ResetScheduleInput {
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	expiry := now.Add(24 * time.Hour)
	return ResetScheduleInput{Now: now, Accounts: []ResetScheduleAccountInput{{Key: "a", AccountID: "acct-a", Multiplier: 1,
		Windows: map[quota.WindowName]ResetScheduleWindowInput{quota.Window7Day: resetTestWindow(0, now.Add(7*24*time.Hour), 7*24*time.Hour, .001)},
		Credits: []ResetScheduleCreditInput{{ID: "accepted", GrantedAt: now.Add(-time.Hour), ExpiresAt: &expiry, Supported: true}, {ID: "unaccepted", GrantedAt: now.Add(-time.Hour), ExpiresAt: &expiry, Supported: true}},
	}}}
}

func TestResetConstraintsExcludeUnacceptedAndEmptySet(t *testing.T) {
	in := constrainedResetInput()
	in.Constraints = &ResetScheduleConstraints{AllowedCredits: []ResetScheduleCreditRef{{AccountKey: "a", CreditID: "accepted"}}}
	got := RecommendResetSchedule(in)
	if !got.Complete || len(got.Items) != 1 || got.Items[0].CreditID != "accepted" {
		t.Fatalf("allowed = %+v", got)
	}
	in.Constraints = &ResetScheduleConstraints{}
	got = RecommendResetSchedule(in)
	if !got.Complete || len(got.Items) != 0 || got.Objective.RestoredPct != 0 {
		t.Fatalf("empty = %+v", got)
	}
}

func TestResetConstraintsFixedUseRestoresOnlyAtPinnedTime(t *testing.T) {
	in := constrainedResetInput()
	pin := in.Now.Add(time.Hour)
	in.Constraints = &ResetScheduleConstraints{AllowedCredits: []ResetScheduleCreditRef{{AccountKey: "a", CreditID: "accepted"}}, FixedUses: []ResetScheduleFixedUse{{AccountKey: "a", CreditID: "accepted", At: pin}}}
	got := RecommendResetSchedule(in)
	if !got.Complete || len(got.Items) != 1 || !got.Items[0].UseAt.Equal(pin) || got.Objective.GapDurationSeconds < 3600 || got.Objective.RestoredPct != 100 {
		t.Fatalf("pin = %+v", got)
	}
	in.Constraints.FixedUses[0].At = in.Now.Add(-time.Minute)
	got = RecommendResetSchedule(in)
	if !got.Complete || !got.Items[0].UseAt.Equal(in.Now) {
		t.Fatalf("overdue = %+v", got)
	}
}

func TestResetConstraintsRejectUnknownAndDuplicateAllowedCredits(t *testing.T) {
	for _, refs := range [][]ResetScheduleCreditRef{{{AccountKey: "wrong", CreditID: "accepted"}}, {{AccountKey: "a", CreditID: "missing"}}, {{AccountKey: "a", CreditID: "accepted"}, {AccountKey: "a", CreditID: "accepted"}}} {
		in := constrainedResetInput()
		in.Constraints = &ResetScheduleConstraints{AllowedCredits: refs}
		if got := RecommendResetSchedule(in); got.Complete || len(got.Blockers) == 0 {
			t.Fatalf("accepted invalid constraint: %+v", got)
		}
	}
}

func TestResetConstraintsRejectInvalidPins(t *testing.T) {
	in := constrainedResetInput()
	pin := ResetScheduleFixedUse{AccountKey: "a", CreditID: "accepted", At: in.Now.Add(time.Hour)}
	for _, pins := range [][]ResetScheduleFixedUse{
		{{AccountKey: "wrong", CreditID: pin.CreditID, At: pin.At}},
		{{AccountKey: "a", CreditID: "missing", At: pin.At}},
		{{AccountKey: "a", CreditID: pin.CreditID}},
		{{AccountKey: "a", CreditID: pin.CreditID, At: *in.Accounts[0].Credits[0].ExpiresAt}},
		{pin, pin},
	} {
		in.Constraints = &ResetScheduleConstraints{AllowedCredits: []ResetScheduleCreditRef{{AccountKey: "a", CreditID: "accepted"}}, FixedUses: pins}
		if got := RecommendResetSchedule(in); got.Complete || len(got.Blockers) == 0 {
			t.Fatalf("accepted invalid pins: %+v", got)
		}
	}
}

func TestResetConstraintsHorizonIgnoresUnacceptedExpiries(t *testing.T) {
	in := constrainedResetInput()
	far := in.Now.Add(365 * 24 * time.Hour)
	in.Accounts[0].Credits[1].ExpiresAt = &far
	in.Constraints = &ResetScheduleConstraints{AllowedCredits: []ResetScheduleCreditRef{{AccountKey: "a", CreditID: "accepted"}}}
	got := RecommendResetSchedule(in)
	if !got.Horizon.Equal(in.Now.Add(8 * 24 * time.Hour)) {
		t.Fatalf("unaccepted credit extended horizon: %s", got.Horizon)
	}
}
