package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexResetForecastSelectsProspectiveRefills(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 500_000_000, time.UTC)
	first, second := now.Add(time.Hour), now.Add(27*time.Hour)
	firstInSameSecond := first.Add(time.Millisecond)
	expiresAfterSecond := second.Add(2 * time.Millisecond)
	pastInSameSecond := now.Add(-time.Millisecond)
	backoff := first.Add(time.Minute)
	entries := []codex.ResetAutomationEntry{
		{ID: "second", CreditID: "second", AccountKey: "key-a", State: codex.ResetAutomationPending, At: &second, ExpiresAt: &expiresAfterSecond},
		{ID: "first", CreditID: "first", AccountKey: "key-a", State: codex.ResetAutomationPending, At: &first},
		{ID: "coincident", CreditID: "coincident", AccountKey: "key-a", State: codex.ResetAutomationPending, At: &firstInSameSecond},
		{ID: "immediate", CreditID: "immediate", AccountKey: "other-key", State: codex.ResetAutomationPending, At: &now},
		{ID: "overdue", CreditID: "overdue", AccountKey: "key-a", State: codex.ResetAutomationPending, At: &pastInSameSecond},
		{ID: "at-expiry", CreditID: "at-expiry", AccountKey: "no-credit", State: codex.ResetAutomationPending, At: &first, ExpiresAt: &first},
		{ID: "after-expiry", CreditID: "after-expiry", AccountKey: "no-credit", State: codex.ResetAutomationPending, At: &second, ExpiresAt: &first},
		{ID: "undated", CreditID: "undated", AccountKey: "undated", State: codex.ResetAutomationPending},
		{ID: "backoff", CreditID: "backoff", AccountKey: "blocked-retry", State: codex.ResetAutomationPending, At: &first, NextAttemptAt: &backoff},
		{ID: "completed", CreditID: "completed", AccountKey: "key-a", State: codex.ResetAutomationCompleted, At: &now},
		{ID: "cancelled", CreditID: "cancelled", AccountKey: "cancelled", State: codex.ResetAutomationCancelled, At: &first},
		{ID: "retrying", CreditID: "retrying", AccountKey: "retry-account", State: codex.ResetAutomationRetrying, At: &first},
		{ID: "retry-later", CreditID: "retry-later", AccountKey: "retry-account", State: codex.ResetAutomationPending, At: &second},
		{ID: "running", CreditID: "running", AccountKey: "running-account", State: codex.ResetAutomationRunning, At: &now},
		{ID: "run-later", CreditID: "run-later", AccountKey: "running-account", State: codex.ResetAutomationPending, At: &second},
		{ID: "indeterminate", CreditID: "indeterminate", AccountKey: "indeterminate-account", State: codex.ResetAutomationBlocked, LastError: "consume_indeterminate", At: &first},
		{ID: "indeterminate-later", CreditID: "indeterminate-later", AccountKey: "indeterminate-account", State: codex.ResetAutomationPending, At: &second},
		{ID: "blocked", CreditID: "blocked", AccountKey: "blocked", State: codex.ResetAutomationBlocked, At: &first},
	}
	store := codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	if _, err := store.Update(context.Background(), func(state *codex.ResetAutomationState) error {
		state.Enabled, state.Entries = true, entries
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := codexResetForecastFrom(context.Background(), now, store, func(context.Context) proxy.CodexResetSchedulerStatus {
		return proxy.CodexResetSchedulerStatus{Running: true, State: "running", CheckedAt: now}
	})
	want := map[string]quota.BankedResetForecast{
		"key-a":     {ScheduledAtUnix: []int64{first.Unix(), second.Unix()}},
		"other-key": {ScheduledAtUnix: []int64{now.Unix()}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forecast = %v, want %v", got, want)
	}
}

func TestCodexResetForecastRequiresEnabledScheduleAndHealthyExecutor(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := now.Add(time.Hour)
	valid := proxy.CodexResetSchedulerStatus{Running: true, State: "running", CheckedAt: now}
	for _, test := range []struct {
		name       string
		file       string
		status     proxy.CodexResetSchedulerStatus
		wantProbes int
		wantReset  bool
	}{
		{name: "missing file", file: "missing", status: valid},
		{name: "corrupt file", file: "corrupt", status: valid},
		{name: "paused schedule", file: "paused", status: valid},
		{name: "empty schedule", file: "empty", status: valid},
		{name: "running", status: valid, wantProbes: 1, wantReset: true},
		{name: "paused executor", status: proxy.CodexResetSchedulerStatus{State: "paused", CheckedAt: now}, wantProbes: 1},
		{name: "stale executor", status: proxy.CodexResetSchedulerStatus{State: "stale", CheckedAt: now.Add(-2 * time.Minute)}, wantProbes: 1},
		{name: "execution error", status: proxy.CodexResetSchedulerStatus{Running: true, State: "running", CheckedAt: now, LastError: "execution_failed"}, wantProbes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := fsutil.OSFileSystem{}
			store := codex.NewResetAutomationStore(fs, filepath.Join(t.TempDir(), "state", "schedule.json"))
			switch test.file {
			case "missing":
			case "corrupt":
				if err := fsutil.EnsureSecureDirectory(fs, filepath.Dir(store.Path)); err != nil {
					t.Fatal(err)
				}
				if err := fsutil.SecureAtomicWrite(fs, store.Path, []byte("{")); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := store.Update(context.Background(), func(state *codex.ResetAutomationState) error {
					state.Enabled = test.file != "paused"
					if test.file != "empty" {
						state.Entries = []codex.ResetAutomationEntry{{ID: "entry", CreditID: "credit", AccountKey: "key", State: codex.ResetAutomationPending, At: &at}}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			probes := 0
			got := codexResetForecastFrom(context.Background(), now, store, func(context.Context) proxy.CodexResetSchedulerStatus {
				probes++
				return test.status
			})
			if (len(got["key"].ScheduledAtUnix) == 1) != test.wantReset || probes != test.wantProbes {
				t.Errorf("forecast = %v, probed executor %d times, want reset %v and %d probes", got, probes, test.wantReset, test.wantProbes)
			}
		})
	}
}

func TestCodexResetForecastRequiresFreshAdaptivePlanExceptPins(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at, pin := now.Add(time.Hour), now.Add(2*time.Hour)
	for _, test := range []struct {
		name         string
		valid        bool
		last         time.Time
		next         time.Time
		wantUnpinned bool
	}{
		{name: "fresh", valid: true, last: now.Add(-time.Minute), next: now.Add(time.Minute), wantUnpinned: true},
		{name: "invalid", last: now.Add(-time.Minute), next: now.Add(time.Minute)},
		{name: "missing replan", valid: true, next: now.Add(time.Minute)},
		{name: "future replan", valid: true, last: now.Add(time.Minute), next: now.Add(time.Minute)},
		{name: "stale replan", valid: true, last: now.Add(-6 * time.Minute), next: now.Add(time.Minute)},
		{name: "replan due", valid: true, last: now.Add(-time.Minute), next: now},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
			if _, err := store.Update(context.Background(), func(state *codex.ResetAutomationState) error {
				state.Enabled, state.Mode, state.PlanValid = true, codex.ResetAutomationAdaptive, test.valid
				if !test.last.IsZero() {
					state.LastReplanAt = &test.last
				}
				state.NextReplanAt = &test.next
				state.Entries = []codex.ResetAutomationEntry{
					{ID: "entry", CreditID: "credit", AccountKey: "adaptive-key", State: codex.ResetAutomationPending, At: &at},
					{ID: "pin", CreditID: "pinned-credit", AccountKey: "pinned-key", State: codex.ResetAutomationPending, At: &pin, Pinned: true},
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got := codexResetForecastFrom(context.Background(), now, store, func(context.Context) proxy.CodexResetSchedulerStatus {
				return proxy.CodexResetSchedulerStatus{Running: true, State: "running", CheckedAt: now}
			})
			want := map[string]quota.BankedResetForecast{"pinned-key": {ScheduledAtUnix: []int64{pin.Unix()}}}
			if test.wantUnpinned {
				want["adaptive-key"] = quota.BankedResetForecast{ScheduledAtUnix: []int64{at.Unix()}}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("forecast = %v, want %v", got, want)
			}
		})
	}
}

func TestCodexResetForecastKeepsLastConfirmedRedemptionWhenPaused(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	first, latest, future := now.Add(-2*time.Hour), now.Add(-time.Hour), now.Add(time.Hour)
	store := codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	if _, err := store.Update(context.Background(), func(state *codex.ResetAutomationState) error {
		state.Enabled = false
		state.Entries = []codex.ResetAutomationEntry{
			{ID: "latest", CreditID: "latest", AccountKey: "key", State: codex.ResetAutomationCompleted, Outcome: codex.ConsumeReset, UpdatedAt: latest},
			{ID: "first", CreditID: "first", AccountKey: "key", State: codex.ResetAutomationCompleted, Outcome: codex.ConsumeReset, UpdatedAt: first},
			{ID: "already", CreditID: "already", AccountKey: "key", State: codex.ResetAutomationCompleted, Outcome: codex.ConsumeAlreadyRedeemed, UpdatedAt: now},
			{ID: "future", CreditID: "future", AccountKey: "key", State: codex.ResetAutomationCompleted, Outcome: codex.ConsumeReset, UpdatedAt: future},
			{ID: "next", CreditID: "next", AccountKey: "key", State: codex.ResetAutomationPending, At: &future},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := codexResetForecastFrom(context.Background(), now, store, func(context.Context) proxy.CodexResetSchedulerStatus {
		t.Error("paused forecast probed executor")
		return proxy.CodexResetSchedulerStatus{}
	})
	want := map[string]quota.BankedResetForecast{"key": {LastRedeemedAtUnix: latest.Unix()}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forecast = %v, want %v", got, want)
	}
}
