package proxy

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestDefaultCodexContinuityOpenOptionsUsesConfiguredAuthority(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "cq-state")
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	routing := &CodexRoutingRuntime{
		HTTP: CodexModeStatus{
			AuthoritativeEpoch:          9,
			RetainedAuthoritativeEpochs: []uint64{3, 7},
		},
		WebSocket: CodexModeStatus{
			AuthoritativeEpoch:          11,
			RetainedAuthoritativeEpochs: []uint64{7, 10},
		},
	}

	options, err := NewCodexContinuityOpenOptions(
		fsutil.NewMemFS(),
		stateDir,
		routing,
		21*24*time.Hour,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	if options.KeyPath != filepath.Join(stateDir, "codex-turn-leases.key") || options.JournalPath != filepath.Join(stateDir, "codex-turn-leases.json") {
		t.Fatalf("authority paths = key %q journal %q", options.KeyPath, options.JournalPath)
	}
	if options.Policy.Retention != 21*24*time.Hour || options.Policy.Now() != now {
		t.Fatalf("policy = %#v", options.Policy)
	}
	wantEpochs := []uint64{3, 7, 9, 10, 11}
	if !slices.Equal(options.Modes.RecognisedAuthoritativeEpochs, wantEpochs) {
		t.Fatalf("recognised epochs = %v, want %v", options.Modes.RecognisedAuthoritativeEpochs, wantEpochs)
	}

	routing.HTTP.RetainedAuthoritativeEpochs[0] = 99
	if !slices.Equal(options.Modes.RecognisedAuthoritativeEpochs, wantEpochs) {
		t.Fatalf("options aliased routing state: %v", options.Modes.RecognisedAuthoritativeEpochs)
	}
}

func TestDefaultCodexContinuityOpenOptionsRejectsIncompleteInputs(t *testing.T) {
	t.Parallel()

	validRuntime := &CodexRoutingRuntime{}
	validClock := time.Now
	tests := []struct {
		name      string
		fs        fsutil.DurableFileSystem
		stateDir  string
		runtime   *CodexRoutingRuntime
		retention time.Duration
		now       func() time.Time
	}{
		{name: "filesystem", stateDir: "/state", runtime: validRuntime, retention: time.Hour, now: validClock},
		{name: "state directory", fs: fsutil.NewMemFS(), runtime: validRuntime, retention: time.Hour, now: validClock},
		{name: "runtime", fs: fsutil.NewMemFS(), stateDir: "/state", retention: time.Hour, now: validClock},
		{name: "retention", fs: fsutil.NewMemFS(), stateDir: "/state", runtime: validRuntime, now: validClock},
		{name: "clock", fs: fsutil.NewMemFS(), stateDir: "/state", runtime: validRuntime, retention: time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewCodexContinuityOpenOptions(test.fs, test.stateDir, test.runtime, test.retention, test.now); err == nil {
				t.Fatal("NewCodexContinuityOpenOptions unexpectedly succeeded")
			}
		})
	}
}
