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

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

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
