package codex

import "time"

// FreshAdaptivePlan reports whether the worker has a current adaptive plan.
func (s ResetAutomationState) FreshAdaptivePlan(now time.Time) bool {
	return s.Enabled && s.Mode == ResetAutomationAdaptive && s.PlanValid && s.PlanError == "" &&
		s.LastReplanAt != nil && !s.LastReplanAt.After(now) &&
		s.NextReplanAt != nil && s.NextReplanAt.After(now) &&
		now.Sub(*s.LastReplanAt) <= 5*time.Minute+45*time.Second
}

// AdaptiveCreditExpiries includes authorised credits waiting for the running
// balance to be spent, even when their redemption time is still unset.
func (s ResetAutomationState) AdaptiveCreditExpiries(now time.Time) map[AccountKey]time.Time {
	if !s.FreshAdaptivePlan(now) {
		return nil
	}
	expiries := make(map[AccountKey]time.Time)
	unresolved := make(map[AccountKey]bool)
	for _, entry := range s.Entries {
		switch {
		case entry.State == ResetAutomationRunning || entry.State == ResetAutomationRetrying ||
			(entry.State == ResetAutomationBlocked && entry.LastError == "consume_indeterminate"):
			unresolved[entry.AccountKey] = true
		case entry.State == ResetAutomationPending && !entry.Pinned && entry.ExpiresAt != nil && entry.ExpiresAt.After(now) &&
			(entry.LastError == "" || entry.LastError == "not_yet_useful") &&
			(entry.NextAttemptAt == nil || !entry.NextAttemptAt.After(now)):
			if previous := expiries[entry.AccountKey]; previous.IsZero() || entry.ExpiresAt.Before(previous) {
				expiries[entry.AccountKey] = *entry.ExpiresAt
			}
		}
	}
	for key := range unresolved {
		delete(expiries, key)
	}
	return expiries
}

// NextResetAccount returns the first accepted reset that can authorise reserve
// spending. Equal dates retain stored order, matching the executor's stable sort.
// An unresolved request prevents a later entry from granting that authority.
func (s ResetAutomationState) NextResetAccount(now time.Time) (AccountKey, bool) {
	if !s.Enabled {
		return "", false
	}
	if s.Mode == ResetAutomationAdaptive {
		if !s.FreshAdaptivePlan(now) {
			return "", false
		}
	} else if s.Mode != ResetAutomationFixed {
		return "", false
	}
	var first *ResetAutomationEntry
	for i := range s.Entries {
		entry := &s.Entries[i]
		if entry.State == ResetAutomationRunning || entry.State == ResetAutomationRetrying {
			return "", false
		}
		if (entry.State != ResetAutomationPending && entry.State != ResetAutomationBlocked) || entry.AccountKey == "" || entry.At == nil || entry.At.IsZero() ||
			(entry.ExpiresAt != nil && !entry.ExpiresAt.After(now)) {
			continue
		}
		if first == nil || entry.At.Before(*first.At) {
			first = entry
		}
	}
	if first == nil || first.State == ResetAutomationBlocked || (first.NextAttemptAt != nil && first.NextAttemptAt.After(now)) {
		return "", false
	}
	return first.AccountKey, true
}
