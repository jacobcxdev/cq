package proxy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCodexCanaryRecordsOnlyPrivacySafeCounters(t *testing.T) {
	fsys := fsutil.NewMemFS()
	_ = fsys.WriteFile("/home/.codex/auth.json", []byte("system-secret"), 0o600)
	_ = fsys.WriteFile("/home/.codex/accounts/registry.json", []byte("registry-secret"), 0o600)
	start := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", []string{"/home/.codex/auth.json", "/home/.codex/accounts/registry.json"}, CodexCanaryTuple{CQBuild: "build", ClientBuild: "client", ParserSchema: 1, LeaseSchema: 1, FixtureHash: "fixture"}, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAdmitted(start.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = fsys.WriteFile("/home/.codex/auth.json", []byte("changed-secret"), 0o600)
	if err := recorder.RecordAdmitted(start.Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	state := recorder.State()
	if state.AdmittedTurns != 2 || state.AutomaticHashChanges != 1 || state.ConsecutiveCalendarDays != 3 {
		t.Fatalf("state = %+v", state)
	}
	data, _ := fsys.ReadFile("/state/canary.json")
	for _, secret := range []string{"system-secret", "changed-secret", "registry-secret", "/home/"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("canary leaked %q", secret)
		}
	}
}

func TestCodexCanaryExplicitSwitchResetsHashBaseline(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/.codex/auth.json"
	_ = fsys.WriteFile(path, []byte("before"), 0o600)
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", []string{path}, CodexCanaryTuple{CQBuild: "build", ClientBuild: "client", ParserSchema: 1, LeaseSchema: 1, FixtureHash: "fixture"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = fsys.WriteFile(path, []byte("explicit"), 0o600)
	if err := recorder.AcknowledgeExplicitSwitch(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAdmitted(time.Now()); err != nil {
		t.Fatal(err)
	}
	if recorder.State().AutomaticHashChanges != 0 {
		t.Fatalf("state = %+v", recorder.State())
	}
}

func TestCodexCanaryConsecutiveDaysResetAfterGap(t *testing.T) {
	start := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsutil.NewMemFS(), "/state/canary.json", nil, CodexCanaryTuple{CQBuild: "build", ClientBuild: "client", ParserSchema: 1, LeaseSchema: 1, FixtureHash: "fixture"}, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordHeartbeat(start.Add(72 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().ConsecutiveCalendarDays; got != 1 {
		t.Fatalf("days after gap = %d", got)
	}
	if err := recorder.RecordHeartbeat(start.Add(96 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().ConsecutiveCalendarDays; got != 2 {
		t.Fatalf("days after next observation = %d", got)
	}
}

func TestCodexCanaryRecordsSecretLeakCounter(t *testing.T) {
	recorder, err := StartCodexCanary(fsutil.NewMemFS(), "/state/canary.json", nil, CodexCanaryTuple{CQBuild: "build", ClientBuild: "client", ParserSchema: 1, LeaseSchema: 1, FixtureHash: "fixture"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordSecretLeak(); err != nil {
		t.Fatal(err)
	}
	if recorder.State().SecretLeaks != 1 {
		t.Fatalf("state = %+v", recorder.State())
	}
}

func TestCodexCanaryRefusesToReplaceActiveRun(t *testing.T) {
	fsys := fsutil.NewMemFS()
	tuple := CodexCanaryTuple{CQBuild: "build", ClientBuild: "client", ParserSchema: 1, LeaseSchema: 1, FixtureHash: "fixture"}
	if _, err := StartCodexCanary(fsys, "/state/canary.json", nil, tuple, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := StartCodexCanary(fsys, "/state/canary.json", nil, tuple, time.Now()); !errors.Is(err, ErrCodexCanaryActive) {
		t.Fatalf("second start error = %v", err)
	}
}
