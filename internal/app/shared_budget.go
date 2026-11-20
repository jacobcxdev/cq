package app

import "github.com/jacobcxdev/cq/internal/quota"

// SharedBudgetAggregateReport summarises a provider's account allowances.
type SharedBudgetAggregateReport struct {
	AccountCount int                           `json:"account_count"`
	Windows      []SharedBudgetAggregateWindow `json:"windows"`
}

// SharedBudgetAggregateWindow describes the accounts that reported a window.
type SharedBudgetAggregateWindow struct {
	Name             quota.WindowName `json:"name"`
	MeanRemainingPct float64          `json:"mean_remaining_pct"`
	ReportedAccounts int              `json:"reported_accounts"`
	NonZeroAccounts  int              `json:"non_zero_accounts"`
}

func aggregateSharedBudgets(subscriptions []quota.SharedBudgetReport) map[string]SharedBudgetAggregateReport {
	byProvider := make(map[string][]quota.SharedBudgetReport)
	for _, subscription := range subscriptions {
		byProvider[subscription.ProviderID] = append(byProvider[subscription.ProviderID], subscription)
	}
	reports := make(map[string]SharedBudgetAggregateReport, len(byProvider))
	for providerID, accounts := range byProvider {
		if len(accounts) < 2 {
			continue
		}
		windows := aggregateSharedBudgetWindows(accounts)
		if len(windows) == 0 {
			continue
		}
		reports[providerID] = SharedBudgetAggregateReport{AccountCount: len(accounts), Windows: windows}
	}
	return reports
}

func aggregateSharedBudgetWindows(accounts []quota.SharedBudgetReport) []SharedBudgetAggregateWindow {
	type allowance struct {
		sum      float64
		reported int
		nonZero  int
	}
	totals := make(map[quota.WindowName]allowance)
	var names []quota.WindowName
	for _, account := range accounts {
		seen := make(map[quota.WindowName]bool, len(account.Windows))
		for _, window := range account.Windows {
			name := quota.WindowName(window.Name)
			remaining := window.RemainingPct
			if seen[name] || !(remaining >= 0 && remaining <= 100) {
				continue
			}
			seen[name] = true
			total := totals[name]
			if total.reported == 0 {
				names = append(names, name)
			}
			total.sum += remaining
			total.reported++
			if remaining > 0 {
				total.nonZero++
			}
			totals[name] = total
		}
	}
	var windows []SharedBudgetAggregateWindow
	for _, name := range quota.OrderedWindowNames(names) {
		total := totals[name]
		if total.reported >= 2 {
			windows = append(windows, SharedBudgetAggregateWindow{
				Name:             name,
				MeanRemainingPct: total.sum / float64(total.reported),
				ReportedAccounts: total.reported,
				NonZeroAccounts:  total.nonZero,
			})
		}
	}
	return windows
}
