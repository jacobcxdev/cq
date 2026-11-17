package codex

import (
	"testing"
	"time"
)

func TestResetAutomationNextResetAccount(t *testing.T) {
	now := time.Unix(1800000000, 0)
	earlier, later := now.Add(time.Minute), now.Add(2*time.Minute)
	last, next := now.Add(-time.Minute), now.Add(4*time.Minute)
	expired := now.Add(-time.Second)
	base := ResetAutomationState{
		Enabled: true, Mode: ResetAutomationAdaptive, PlanValid: true,
		LastReplanAt: &last, NextReplanAt: &next,
		Entries: []ResetAutomationEntry{
			{AccountKey: "other", At: &later, State: ResetAutomationPending},
			{AccountKey: "system", At: &earlier, State: ResetAutomationPending},
		},
	}
	for _, test := range []struct {
		name string
		edit func(*ResetAutomationState)
		want AccountKey
	}{
		{"earliest", nil, "system"},
		{"stable tie", func(s *ResetAutomationState) { s.Entries[0].At = &earlier }, "other"},
		{"disabled", func(s *ResetAutomationState) { s.Enabled = false }, ""},
		{"invalid adaptive", func(s *ResetAutomationState) { s.PlanValid = false }, ""},
		{"stale adaptive", func(s *ResetAutomationState) { s.NextReplanAt = &expired }, ""},
		{"old forecast with distant retry", func(s *ResetAutomationState) {
			old, distant := now.Add(-time.Hour), now.Add(time.Hour)
			s.LastReplanAt, s.NextReplanAt = &old, &distant
		}, ""},
		{"future observation", func(s *ResetAutomationState) { s.LastReplanAt = &later }, ""},
		{"missing observation", func(s *ResetAutomationState) { s.LastReplanAt = nil }, ""},
		{"fixed needs no forecast", func(s *ResetAutomationState) {
			s.Mode = ResetAutomationFixed
			s.PlanValid = false
			s.LastReplanAt = nil
			s.NextReplanAt = nil
		}, "system"},
		{"running blocks", func(s *ResetAutomationState) { s.Entries[0].State = ResetAutomationRunning }, ""},
		{"retrying blocks", func(s *ResetAutomationState) { s.Entries[0].State = ResetAutomationRetrying }, ""},
		{"completed ignored", func(s *ResetAutomationState) { s.Entries[1].State = ResetAutomationCompleted }, "other"},
		{"expired ignored", func(s *ResetAutomationState) { s.Entries[1].ExpiresAt = &expired }, "other"},
		{"undated ignored", func(s *ResetAutomationState) { s.Entries[1].At = nil }, "other"},
		{"blocked first is a barrier", func(s *ResetAutomationState) { s.Entries[1].State = ResetAutomationBlocked }, ""},
		{"blocked other cannot be skipped", func(s *ResetAutomationState) {
			s.Mode = ResetAutomationFixed
			s.Entries[0].State = ResetAutomationBlocked
			s.Entries[0].At = &expired
			s.Entries[0].NextAttemptAt = &expired
		}, ""},
		{"pending retry cannot be promised", func(s *ResetAutomationState) { s.Entries[1].NextAttemptAt = &later }, ""},
		{"no accepted entries", func(s *ResetAutomationState) { s.Entries = nil }, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := base
			state.Entries = append([]ResetAutomationEntry(nil), base.Entries...)
			if test.edit != nil {
				test.edit(&state)
			}
			got, ok := state.NextResetAccount(now)
			if got != test.want || ok != (test.want != "") {
				t.Fatalf("next = %q, %t; want %q", got, ok, test.want)
			}
		})
	}
}
