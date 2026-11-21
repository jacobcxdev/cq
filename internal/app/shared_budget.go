package app

import "github.com/jacobcxdev/cq/internal/quota"

// SharedBudgetAggregateReport summarises a provider's account allowances.
type SharedBudgetAggregateReport struct {
	AccountCount int                           `json:"account_count"`
	Windows      []SharedBudgetAggregateWindow `json:"windows"`
}

// SharedBudgetAggregateWindow describes the accounts that reported a window.
// MeanRemainingPct treats accounts blocked by another reported window as zero.
// NonZeroAccounts counts the raw non-zero reports for this window.
type SharedBudgetAggregateWindow struct {
	Name             quota.WindowName `json:"name"`
	MeanRemainingPct float64          `json:"mean_remaining_pct"`
	ReportedAccounts int              `json:"reported_accounts"`
	NonZeroAccounts  int              `json:"non_zero_accounts"`
	NextResetAtUnix  int64            `json:"next_reset_at_unix,omitempty"`
}

func aggregateSharedBudgets(subscriptions []quota.SharedBudgetReport, nowEpoch int64) map[string]SharedBudgetAggregateReport {
	byProvider := make(map[string][]quota.SharedBudgetReport)
	for _, subscription := range subscriptions {
		byProvider[subscription.ProviderID] = append(byProvider[subscription.ProviderID], subscription)
	}
	reports := make(map[string]SharedBudgetAggregateReport, len(byProvider))
	for providerID, accounts := range byProvider {
		if len(accounts) < 2 {
			continue
		}
		windows := aggregateSharedBudgetWindows(accounts, nowEpoch)
		if len(windows) == 0 {
			continue
		}
		reports[providerID] = SharedBudgetAggregateReport{AccountCount: len(accounts), Windows: windows}
	}
	return reports
}

func aggregateSharedBudgetWindows(accounts []quota.SharedBudgetReport, nowEpoch int64) []SharedBudgetAggregateWindow {
	type allowance struct {
		sum         float64
		reported    int
		nonZero     int
		nextResetAt int64
	}
	totals := make(map[quota.WindowName]allowance)
	var names []quota.WindowName
	for _, account := range accounts {
		seen := make(map[quota.WindowName]bool, len(account.Windows))
		valid := make([]quota.SharedBudgetWindow, 0, len(account.Windows))
		blocked := false
		for _, window := range account.Windows {
			name := quota.WindowName(window.Name)
			remaining := window.RemainingPct
			if seen[name] || !(remaining >= 0 && remaining <= 100) {
				continue
			}
			seen[name] = true
			valid = append(valid, window)
			blocked = blocked || remaining == 0
		}
		for _, window := range valid {
			name := quota.WindowName(window.Name)
			remaining := window.RemainingPct
			total := totals[name]
			if total.reported == 0 {
				names = append(names, name)
			}
			if !blocked {
				total.sum += remaining
			}
			total.reported++
			if remaining > 0 {
				total.nonZero++
			}
			if reset := window.ResetAtUnix; reset > max(nowEpoch, 0) && (total.nextResetAt == 0 || reset < total.nextResetAt) {
				total.nextResetAt = reset
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
				NextResetAtUnix:  total.nextResetAt,
			})
		}
	}
	return windows
}
