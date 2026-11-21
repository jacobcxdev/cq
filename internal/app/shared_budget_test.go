package app

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestAggregateSharedBudgetsDiscountsBlockedAccounts(t *testing.T) {
	account := func(fiveHour, weekly, monthly float64) quota.SharedBudgetReport {
		return quota.SharedBudgetReport{
			ProviderID: "opencode-go",
			Windows: []quota.SharedBudgetWindow{
				{Name: "5h", RemainingPct: fiveHour, ResetAtUnix: 2000},
				{Name: "7d", RemainingPct: weekly, ResetAtUnix: 3000},
				{Name: "1mo", RemainingPct: monthly, ResetAtUnix: 4000},
			},
		}
	}
	accounts := make([]quota.SharedBudgetReport, 0, 8)
	for range 6 {
		accounts = append(accounts, account(100, 0, 50))
	}
	accounts = append(accounts, account(100, 4, 52), account(0, 18, 59))
	got := aggregateSharedBudgets(accounts, 1000)["opencode-go"].Windows
	want := []SharedBudgetAggregateWindow{
		{Name: "5h", MeanRemainingPct: 12.5, ReportedAccounts: 8, NonZeroAccounts: 7, NextResetAtUnix: 2000},
		{Name: "7d", MeanRemainingPct: 0.5, ReportedAccounts: 8, NonZeroAccounts: 2, NextResetAtUnix: 3000},
		{Name: "1mo", MeanRemainingPct: 6.5, ReportedAccounts: 8, NonZeroAccounts: 8, NextResetAtUnix: 4000},
	}
	if len(got) != len(want) {
		t.Fatalf("aggregate windows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("aggregate window %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAggregateSharedBudgetsIgnoresInvalidAndDuplicateWindows(t *testing.T) {
	accounts := []quota.SharedBudgetReport{
		{ProviderID: "opencode-go", Windows: []quota.SharedBudgetWindow{
			{Name: "5h", RemainingPct: 100},
			{Name: "7d", RemainingPct: 101},
			{Name: "7d", RemainingPct: 20},
			{Name: "7d", RemainingPct: 0},
		}},
		{ProviderID: "opencode-go", Windows: []quota.SharedBudgetWindow{
			{Name: "5h", RemainingPct: 50},
			{Name: "7d", RemainingPct: 0},
		}},
	}
	got := aggregateSharedBudgets(accounts, 1000)["opencode-go"].Windows
	if len(got) != 2 || got[0].MeanRemainingPct != 50 || got[1].MeanRemainingPct != 10 {
		t.Fatalf("aggregate windows = %+v, want 5h 50%% and 7d 10%%", got)
	}
}
