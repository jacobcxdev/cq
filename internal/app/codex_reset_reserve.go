package app

import (
	"math"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/history"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

// Each candidate commits its first refill before treating reserve as spendable.
// The accepted schedule, not this advisory result, grants live release.
func recommendResetReserve(input aggregate.ResetScheduleInput, policy quota.ReservePolicy) aggregate.ResetSchedule {
	if policy.Window == "" {
		if input.Constraints != nil && input.Constraints.NextAccountKey != "" {
			constraints := *input.Constraints
			constraints.NextAccountKey = ""
			input.Constraints = &constraints
		}
		return aggregate.RecommendResetSchedule(input)
	}
	valid := policy.AccountKey != "" && policy.Percent > 0 && policy.Percent < 100 && !math.IsNaN(policy.Percent) && !math.IsInf(policy.Percent, 0)
	index := -1
	for i := range input.Accounts {
		if input.Accounts[i].Key == policy.AccountKey {
			if _, ok := input.Accounts[i].Windows[policy.Window]; ok {
				index = i
			}
		}
	}
	if !valid || index < 0 {
		s := aggregate.RecommendResetSchedule(input)
		s.Complete, s.Exact = false, false
		s.Blockers = append(s.Blockers, aggregate.ResetScheduleBlocker{Code: "reserve_window_unavailable"})
		return s
	}
	apply := func(next string) aggregate.ResetSchedule {
		candidate := input
		candidate.Accounts = append([]aggregate.ResetScheduleAccountInput(nil), input.Accounts...)
		windows := make(map[quota.WindowName]aggregate.ResetScheduleWindowInput, len(candidate.Accounts[index].Windows))
		for name, window := range candidate.Accounts[index].Windows {
			windows[name] = window
		}
		window := windows[policy.Window]
		window.ReservePct = policy.Percent
		window.ReserveReleased = next == policy.AccountKey
		windows[policy.Window] = window
		candidate.Accounts[index].Windows = windows
		if next != "" {
			constraints := aggregate.ResetScheduleConstraints{}
			if input.Constraints != nil {
				constraints = *input.Constraints
			} else {
				for _, account := range input.Accounts {
					for _, credit := range account.Credits {
						constraints.AllowedCredits = append(constraints.AllowedCredits, aggregate.ResetScheduleCreditRef{AccountKey: account.Key, CreditID: credit.ID})
					}
				}
			}
			constraints.NextAccountKey = next
			candidate.Constraints = &constraints
		}
		return aggregate.RecommendResetSchedule(candidate)
	}
	if input.Constraints != nil && input.Constraints.NextAccountKey != "" {
		return apply(input.Constraints.NextAccountKey)
	}
	allowed := make(map[string]bool)
	if input.Constraints != nil {
		for _, ref := range input.Constraints.AllowedCredits {
			allowed[ref.CreditID] = true
		}
	}
	var best aggregate.ResetSchedule
	have := false
	for _, account := range input.Accounts {
		eligible := false
		for _, credit := range account.Credits {
			if (input.Constraints == nil || allowed[credit.ID]) && credit.Supported && !credit.GrantedAt.After(input.Now) && (credit.ExpiresAt == nil || credit.ExpiresAt.After(input.Now)) {
				eligible = true
				break
			}
		}
		if !eligible {
			continue
		}
		candidate := apply(account.Key)
		scheduled := false
		for _, item := range candidate.Items {
			if !item.UseAt.IsZero() && ((account.AccountID != "" && item.AccountID == account.AccountID) || (account.AccountID == "" && item.AccountEmail == account.Email)) {
				scheduled = true
			}
		}
		if candidate.Complete && scheduled && (!have || aggregate.BetterResetObjective(candidate.Objective, best.Objective)) {
			best, have = candidate, true
		}
	}
	if have {
		return best
	}
	return apply("")
}

// CodexReserveDrainRate estimates the system account's share of pooled demand
// as if its reserve were available. Depleted accounts still contribute demand.
func CodexReserveDrainRate(now time.Time, policy quota.ReservePolicy, results []quota.Result, estimates history.RateEstimates) float64 {
	var demand, active float64
	systemAvailable := false
	for _, result := range results {
		if !result.IsUsable() {
			return 0
		}
		isSystem := result.AccountKey == policy.AccountKey
		account := codex.ResetAccount{AccountKey: codex.AccountKey(result.AccountKey), AccountID: result.AccountID, Email: result.Email}
		input, ok := resetScheduleAccountInput(account, result, nil, estimates)
		if !ok {
			return 0
		}
		window, ok := input.Windows[policy.Window]
		if !ok {
			return 0
		}
		rate := window.BurnPctPerS
		if window.RateSource != aggregate.ResetRateEWMA || window.Samples < 2 {
			elapsed := window.Period - window.ResetAt.Sub(now)
			if elapsed <= 0 {
				elapsed = window.Period
			}
			rate = (100 - window.RemainingPct) / elapsed.Seconds()
		}
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return 0
		}
		demand += rate * float64(input.Multiplier)
		available := true
		for _, w := range input.Windows {
			if math.IsNaN(w.RemainingPct) || math.IsInf(w.RemainingPct, 0) || w.RemainingPct < 0 || w.RemainingPct > 100 {
				return 0
			}
			if w.RemainingPct <= 0 || !w.ResetAt.After(now) {
				available = false
			}
		}
		if isSystem {
			systemAvailable = available
		}
		if available {
			active += float64(input.Multiplier)
		}
	}
	if !systemAvailable || active <= 0 {
		return 0
	}
	return demand / active
}
