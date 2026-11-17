package main

import (
	"errors"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestReadResetReserveUsesCurrentSystemIdentity(t *testing.T) {
	fs := fsutil.NewMemFS()
	path := "/state/reserve.json"
	if err := fs.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"last_system_account":"old","window":"7d","percent":5}`)
	if err := fs.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := codex.ResetAccountSnapshot{Accounts: []codex.ResetAccount{{AccountKey: "current", AccountID: "id", Active: true}}}
	now := time.Unix(1800000000, 0)
	usage := []quota.Result{{AccountKey: "current", AccountID: "id", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 5, ResetAtUnix: now.Add(time.Hour).Unix()}}}}
	policy, err := readCodexResetReserve(fs, path, snapshot, usage, now)
	if err != nil || policy.AccountKey != "current" || policy.Percent != 5 {
		t.Fatalf("policy=%+v, err=%v", policy, err)
	}
	after, err := fs.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("recommendation rewrote service reserve state")
	}
	snapshot.Accounts = append(snapshot.Accounts, codex.ResetAccount{AccountKey: "ambiguous", Active: true})
	if _, err := readCodexResetReserve(fs, path, snapshot, usage, now); err == nil {
		t.Fatal("ambiguous identity accepted")
	}
}

func TestReserveScheduleRejectsDurableAttempts(t *testing.T) {
	now := time.Unix(1800000000, 0)
	at, expired := now.Add(time.Minute), now.Add(-time.Minute)
	base := codex.ResetAutomationState{Enabled: true, Mode: codex.ResetAutomationFixed, Entries: []codex.ResetAutomationEntry{
		{AccountKey: "other", At: &expired, ExpiresAt: &expired, State: codex.ResetAutomationPending},
		{AccountKey: "system", At: &at, State: codex.ResetAutomationPending},
	}}
	for _, tt := range []struct {
		name       string
		unresolved codex.AccountKey
		readErr    error
	}{
		{"pending system attempt", "system", nil},
		{"expired other attempt", "other", nil},
		{"unreadable attempt state", "", errors.New("read failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, err := readCodexReserveSchedule(func() (codex.ResetAutomationState, error) { return base, nil }, func(account codex.AccountKey) ([]codex.ResetAttempt, error) {
				if tt.readErr != nil {
					return nil, tt.readErr
				}
				if account == tt.unresolved {
					return []codex.ResetAttempt{{AccountKey: account, CreditID: "unresolved"}}, nil
				}
				return nil, nil
			})
			if err == nil {
				t.Fatalf("unresolved refill granted authority: %+v", s)
			}
		})
	}
	s, err := readCodexReserveSchedule(func() (codex.ResetAutomationState, error) { return base, nil }, func(codex.AccountKey) ([]codex.ResetAttempt, error) { return nil, nil })
	if next, ok := s.NextResetAccount(now); err != nil || !ok || next != "system" {
		t.Fatalf("resolved schedule blocked: %+v %v", s, err)
	}
}
