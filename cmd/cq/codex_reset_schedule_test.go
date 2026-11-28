package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestResetScheduleStatusShowsUpcomingBeforeHistory(t *testing.T) {
	fixture := newCodexResetCLIFixture(nil)
	store := codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	fixture.deps.App.Schedule = store
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	planned, expires, next := now.Add(time.Hour), now.Add(time.Hour+5*time.Minute), now.Add(5*time.Minute)
	_, err := store.Update(context.Background(), func(state *codex.ResetAutomationState) error {
		state.Enabled, state.Mode, state.PlanValid = true, codex.ResetAutomationAdaptive, true
		state.LastReplanAt, state.NextReplanAt = &now, &next
		state.Entries = []codex.ResetAutomationEntry{
			{ID: "completed", AccountKey: "private-key", Email: "old@example.com", CreditID: "old-credit", At: &now,
				State: codex.ResetAutomationCompleted, Outcome: codex.ConsumeReset, Warnings: []string{"redistribution_failed"}},
			{ID: "waiting", AccountKey: "private-key", Email: "waiting@example.com", CreditID: "waiting-credit",
				State: codex.ResetAutomationPending, LastError: "not_yet_useful"},
			{ID: "upcoming", AccountKey: "private-key", Email: "upcoming@example.com", CreditID: "new-credit", At: &planned,
				ExpiresAt: &expires, State: codex.ResetAutomationPending, Confidence: "low", Source: "adaptive"},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runCodexResetScheduleStatus(context.Background(), false, fixture.deps); err != nil {
		t.Fatal(err)
	}
	output := fixture.out.String()
	for _, want := range []string{"Adaptive", "New credits", "Upcoming", "History", "5m before expiry", "Low confidence", "Waiting for usage", "Reset succeeded", "routing refresh"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in schedule:\n%s", want, output)
		}
	}
	if strings.Index(output, "upcoming@example.com") > strings.Index(output, "waiting@example.com") ||
		strings.Index(output, "waiting@example.com") > strings.Index(output, "old@example.com") ||
		strings.Contains(output, "redistribution_failed") || strings.Contains(output, "private-key") {
		t.Fatalf("unexpected schedule presentation:\n%s", output)
	}
}

func TestResetScheduleHelpUsesPureInspection(t *testing.T) {
	for _, command := range []string{"", "status", "add", "set", "mode", "remove", "enable", "disable", "cancel"} {
		args := []string{"codex", "resets", "schedule"}
		if command != "" {
			args = append(args, command)
		}
		wantPath := append([]string(nil), args...)
		args = append(args, "--help")
		if path, ok := manualHelpInspectionPath(args); !ok || !reflect.DeepEqual(path, wantPath) {
			t.Fatalf("manual help path(%v) = %v, %t", args, path, ok)
		}
		out := &bytes.Buffer{}
		handled, code, err := runPureGlobalInspection(args, out, io.Discard)
		if !handled || code != 0 || err != nil || !strings.Contains(out.String(), "Usage: cq codex resets schedule") {
			t.Fatalf("help(%v) = %t, %d, %v: %s", args, handled, code, err, out.String())
		}
	}
}

func TestAdaptiveScheduleDisplaysJointPace(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	pace := &quota.ResetPacePhase{From: now, Until: now.Add(time.Hour), Percent: 41}
	state := codex.ResetAutomationState{Enabled: true, PlanValid: true, Mode: codex.ResetAutomationAdaptive, NextPace: pace}
	var out bytes.Buffer
	if err := renderCodexAutomation(&out, state, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Slow down to 41% of observed burn") ||
		!strings.Contains(out.String(), "until 28 Nov 2026 05:00 UTC") || publicCodexAutomation(state).NextPace != pace {
		t.Fatalf("pace missing from adaptive plan output:\n%s", out.String())
	}
	state.PlanValid = false
	if publicCodexAutomation(state).NextPace != nil {
		t.Fatal("invalid plan exposed its pace")
	}
}

func TestAdaptiveScheduleStatusHidesStalePace(t *testing.T) {
	fixture := newCodexResetCLIFixture(nil)
	now := fixture.deps.App.Clock.Now()
	last, next := now.Add(-5*time.Minute), now
	store := codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	fixture.deps.App.Schedule = store
	fixture.deps.ExecutorStatus = func(context.Context) proxy.CodexResetSchedulerStatus {
		return proxy.CodexResetSchedulerStatus{Running: true, State: "running", CheckedAt: now.Add(-time.Second)}
	}
	if _, err := store.Update(context.Background(), func(state *codex.ResetAutomationState) error {
		state.Enabled, state.Mode, state.PlanValid = true, codex.ResetAutomationAdaptive, true
		state.LastReplanAt, state.NextReplanAt = &last, &next
		state.NextPace = &quota.ResetPacePhase{From: last, Until: now.Add(time.Hour), Percent: 41}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := runCodexResetScheduleStatus(context.Background(), true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fixture.out.String(), "next_pace") {
		t.Fatalf("stale pace exposed in schedule status: %s", fixture.out)
	}
}

func TestCLIParsesResetAutomationCommands(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "resets", "recommend", "--schedule", "--mode", "adaptive", "--yes"},
		{"codex", "resets", "schedule"}, {"codex", "resets", "schedule", "status"},
		{"codex", "resets", "schedule", "add", "user@example.com", "--at", "2026-10-28T18:00:00Z"},
		{"codex", "resets", "schedule", "set", "entry", "--follow-mode"},
		{"codex", "resets", "schedule", "mode", "fixed"},
		{"codex", "resets", "schedule", "remove", "entry"},
		{"codex", "resets", "schedule", "enable", "--yes"},
		{"codex", "resets", "schedule", "disable"}, {"codex", "resets", "schedule", "cancel"},
	} {
		if _, _, err := parseCodexResetCLI(t, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
func TestResetAutomationCLIPreviewDefaultNoAndPublicJSON(t *testing.T) {
	fixture := newCodexResetCLIFixture(strings.NewReader("\n"))
	fixture.deps.App.Schedule = codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	// Acceptance is opt-in; declining must not persist any schedule.
	if err := runCodexResetsRecommendOptions(context.Background(), CodexResetsRecommendCmd{Schedule: true}, true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.deps.App.Schedule.Load()
	if err != nil || state.Revision != 0 {
		t.Fatal("decline persisted schedule")
	}
	fixture.out.Reset()
	fixture.deps.In = panicReader{}
	if err := runCodexResetsRecommendOptions(context.Background(), CodexResetsRecommendCmd{Schedule: true, Yes: true}, true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	state, _ = fixture.deps.App.Schedule.Load()
	if !state.Enabled || state.Mode != codex.ResetAutomationFixed || len(state.Entries) == 0 {
		t.Fatalf("bad acceptance %+v", state)
	}
	if strings.Contains(fixture.out.String(), "account_key") {
		t.Fatal("private account key leaked")
	}
	fixture.out.Reset()
	if err := runCodexResetScheduleChange(context.Background(), app.CodexResetScheduleChange{Action: "disable"}, false, true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	state, _ = fixture.deps.App.Schedule.Load()
	if state.Enabled {
		t.Fatal("disable failed")
	}
}

func TestResetAutomationCLIAdaptivePreviewExplainsFutureCredits(t *testing.T) {
	fixture := newCodexResetCLIFixture(strings.NewReader("\n"))
	fixture.deps.App.Schedule = codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	mode := "adaptive"
	if err := runCodexResetsRecommendOptions(context.Background(), CodexResetsRecommendCmd{Schedule: true, Mode: &mode}, false, fixture.deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fixture.out.String(), "enrol and redeem eligible current and future credits") {
		t.Fatalf("adaptive consent did not explain future redemptions:\n%s", fixture.out)
	}
	state, err := fixture.deps.App.Schedule.Load()
	if err != nil || state.Enabled || state.Revision != 0 {
		t.Fatalf("declined preview changed the schedule: %+v, %v", state, err)
	}
}

func TestResetAutomationCLIRequiresYesForPipedConfirmation(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := output.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	output.Close()
	fixture := newCodexResetCLIFixture(input)
	fixture.deps.App.Schedule = codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	if err := runCodexResetsRecommendOptions(context.Background(), CodexResetsRecommendCmd{Schedule: true}, true, fixture.deps); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("piped confirmation accepted: %v", err)
	}
	state, err := fixture.deps.App.Schedule.Load()
	if err != nil || state.Revision != 0 {
		t.Fatal("piped confirmation persisted schedule")
	}
	if err := runCodexResetsRecommendOptions(context.Background(), CodexResetsRecommendCmd{Schedule: true, Yes: true}, true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	state, err = fixture.deps.App.Schedule.Load()
	if err != nil || !state.Enabled {
		t.Fatalf("explicit acceptance failed: %+v, %v", state, err)
	}
}
