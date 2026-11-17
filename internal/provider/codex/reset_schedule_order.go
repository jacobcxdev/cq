package codex

import "time"

// NextResetAccount returns the first accepted reset that can authorise reserve
// spending. Equal dates retain stored order, matching the executor's stable sort.
// An unresolved request prevents a later entry from granting that authority.
func (s ResetAutomationState) NextResetAccount(now time.Time) (AccountKey, bool) {
	if !s.Enabled {
		return "", false
	}
	if s.Mode == ResetAutomationAdaptive {
		if !s.PlanValid || s.LastReplanAt == nil || s.LastReplanAt.After(now) ||
			s.NextReplanAt == nil || !s.NextReplanAt.After(now) || now.Sub(*s.LastReplanAt) > 5*time.Minute+45*time.Second {
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
