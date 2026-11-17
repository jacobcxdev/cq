package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestReadCodexReservePolicyDoesNotMutateState(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, test := range []struct {
		name      string
		bypass    *codexReserveBypass
		remaining float64
		want      bool
	}{
		{"configured", nil, 5, true},
		{"explicit bypass", &codexReserveBypass{Account: "system", ResetAt: now.Add(time.Hour).Unix(), Remaining: 5, ObservedAt: now.Add(-time.Minute)}, 5, false},
		{"refill rearms", &codexReserveBypass{Account: "system", ResetAt: now.Add(time.Hour).Unix(), Remaining: 5, ObservedAt: now.Add(-time.Minute)}, 100, true},
		{"changed identity rearms", &codexReserveBypass{Account: "old-system", ResetAt: now.Add(time.Hour).Unix(), Remaining: 5, ObservedAt: now.Add(-time.Minute)}, 5, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			path := "/state/reserve.json"
			before, err := json.Marshal(codexReserveDocument{LastSystemAccount: "old-system", Window: "7d", Percent: 5, Bypass: test.bypass})
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			windows := map[quota.WindowName]quota.Window{"7d": {RemainingPct: int(test.remaining), RemainingPctExact: &test.remaining, ResetAtUnix: now.Add(time.Hour).Unix()}}
			got, err := ReadCodexReservePolicy(fs, path, "system", now, windows)
			if err != nil {
				t.Fatal(err)
			}
			if (got.AccountKey != "") != test.want {
				t.Fatalf("policy = %+v; want active %t", got, test.want)
			}
			after, err := fs.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read changed service-owned state: %s, %v", after, err)
			}
		})
	}
	fs := fsutil.NewMemFS()
	if got, err := ReadCodexReservePolicy(fs, "/missing", "system", now, nil); err != nil || got.AccountKey != "" {
		t.Fatalf("missing policy = %+v, %v", got, err)
	}
	if err := fs.WriteFile("/bad", []byte(`{"window":"7d","percent":101}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCodexReservePolicy(fs, "/bad", "system", now, nil); err == nil {
		t.Fatal("accepted invalid reserve policy")
	}
}

func TestCodexReserveFollowsNextAcceptedReset(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	remaining := 5.0
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 5, RemainingPctExact: &remaining, ResetAtUnix: now.Add(24 * time.Hour).Unix()}}}})
	if _, err := reserve.Control("set", "7d", 5); err != nil {
		t.Fatal(err)
	}
	at := now.Add(time.Hour)
	state := codex.ResetAutomationState{Enabled: true, Mode: codex.ResetAutomationFixed, Entries: []codex.ResetAutomationEntry{{AccountKey: "system", At: &at, State: codex.ResetAutomationPending}}}
	var readErr error
	reserve.Schedule = func() (codex.ResetAutomationState, error) { return state, readErr }
	status := reserve.Status()
	if status.Blocked || !status.Enabled || status.Reason != "scheduled_reset" {
		t.Fatalf("next system reset did not release reserve: %+v", status)
	}
	if got := reserve.Policy(); got.AccountKey != "system" || got.Window != "7d" || got.Percent != 5 {
		t.Fatalf("automatic release removed planner policy: %+v", got)
	}
	state.Entries[0].AccountKey = "other"
	if status = reserve.Status(); !status.Blocked || status.Reason != "reserve_reached" {
		t.Fatalf("other next reset spent system reserve: %+v", status)
	}
	state.Entries[0].AccountKey = "system"
	readErr = errors.New("schedule unavailable")
	if status = reserve.Status(); !status.Blocked {
		t.Fatalf("failed schedule read released reserve: %+v", status)
	}
	readErr = nil
	state.Enabled = false
	if status = reserve.Status(); !status.Blocked {
		t.Fatalf("disabled schedule released reserve: %+v", status)
	}
	state.Enabled = true
	now = now.Add(16 * time.Second)
	if status = reserve.Status(); !status.Blocked || status.Reason != "usage_stale" {
		t.Fatalf("stale quota released reserve: %+v", status)
	}
}

func TestCodexReserveReleasesBeforeNaturalReset(t *testing.T) {
	start := time.Unix(1800000000, 0)
	now := start
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	remaining := 5.0
	reset := start.Add(800 * time.Second)
	observe := func() {
		ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: int(remaining), RemainingPctExact: &remaining, ResetAtUnix: reset.Unix()}}}})
	}
	observe()
	if _, err := reserve.Control("set", "7d", 5); err != nil {
		t.Fatal(err)
	}
	reserve.DrainRate = func(account codex.AccountKey, window quota.WindowName) float64 {
		if account != "system" || window != "7d" {
			t.Fatalf("wrong drain estimate identity: %s %s", account, window)
		}
		return .01
	}
	if status := reserve.Status(); status.Blocked || !status.Enabled || status.Reason != "natural_reset" {
		t.Fatalf("drain deadline did not release reserve: %+v", status)
	}
	reserve.DrainRate = func(codex.AccountKey, quota.WindowName) float64 { return 0 }
	if status := reserve.Status(); !status.Blocked {
		t.Fatalf("zero demand released reserve too early: %+v", status)
	}
	now = reset.Add(-5 * time.Minute)
	observe()
	if status := reserve.Status(); status.Blocked || status.Reason != "natural_reset" {
		t.Fatalf("final margin did not release reserve: %+v", status)
	}
	now = reset.Add(time.Second)
	if status := reserve.Status(); !status.Blocked || status.Reason != "usage_stale" {
		t.Fatalf("expired epoch released reserve: %+v", status)
	}
	remaining = 100
	reset = now.Add(7 * 24 * time.Hour)
	observe()
	if status := reserve.Status(); !status.Enabled || status.Reason != "" {
		t.Fatalf("refill did not rearm reserve: %+v", status)
	}
}

func TestCodexReservePolicyHonoursExplicitBypass(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 5, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	if _, err := reserve.Control("set", "7d", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := reserve.Control("disable", "", 0); err != nil {
		t.Fatal(err)
	}
	if got := reserve.Policy(); got.AccountKey != "" {
		t.Fatalf("explicit bypass kept planner reserve: %+v", got)
	}
	if _, err := reserve.Control("enable", "", 0); err != nil {
		t.Fatal(err)
	}
	if got := reserve.Policy(); got.AccountKey != "system" {
		t.Fatalf("explicit enable lost planner reserve: %+v", got)
	}
}
