//go:build unix

package proxy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestOpenCodexRoutingRuntimePreservesLegacyAuthorityAfterStateRootMove(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	configDir := filepath.Join(base, "cq")
	stateDir := filepath.Join(configDir, "state")
	httpRequirements, wsRequirements := DefaultCodexRoutingRequirements("build", "client")
	legacy := codexRoutingJournal{
		Version:   CodexRoutingJournalVersion,
		NextEpoch: 151,
		HTTP: codexModeTrack{
			CodexModeStatus: CodexModeStatus{
				Configured:                  CodexRoutingEnforce,
				Effective:                   CodexRoutingEnforce,
				ModeEpoch:                   149,
				AuthoritativeEpoch:          149,
				RetainedAuthoritativeEpochs: []uint64{29, 31, 46, 51, 53, 56, 59, 61, 63, 79, 82, 84},
			},
			RuntimeFingerprint: requirementsFingerprint(httpRequirements),
		},
		WebSocket: codexModeTrack{
			CodexModeStatus: CodexModeStatus{
				Configured:                  CodexRoutingEnforce,
				Effective:                   CodexRoutingEnforce,
				ModeEpoch:                   151,
				AuthoritativeEpoch:          151,
				RetainedAuthoritativeEpochs: []uint64{132, 140, 146},
			},
			RuntimeFingerprint: requirementsFingerprint(wsRequirements),
		},
	}
	if err := saveJSONFile(filepath.Join(configDir, "codex-routing-mode.json"), &legacy); err != nil {
		t.Fatal(err)
	}
	fresh := codexRoutingJournal{
		Version:   CodexRoutingJournalVersion,
		NextEpoch: 2,
		HTTP: codexModeTrack{
			CodexModeStatus: CodexModeStatus{
				Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce,
				ModeEpoch: 1, AuthoritativeEpoch: 1,
			},
			RuntimeFingerprint: requirementsFingerprint(httpRequirements),
		},
		WebSocket: codexModeTrack{
			CodexModeStatus: CodexModeStatus{
				Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce,
				ModeEpoch: 2, AuthoritativeEpoch: 2,
			},
			RuntimeFingerprint: requirementsFingerprint(wsRequirements),
		},
	}
	if err := saveJSONFile(filepath.Join(stateDir, "codex-routing-mode.json"), &fresh); err != nil {
		t.Fatal(err)
	}

	runtime, err := OpenCodexRoutingRuntime(&Config{
		CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingEnforce,
	}, "build", "client")
	if err != nil {
		t.Fatal(err)
	}
	wantHTTPRetained := []uint64{29, 31, 46, 51, 53, 56, 59, 61, 63, 79, 82, 84}
	if runtime.HTTP.ModeEpoch != 149 || !slices.Equal(runtime.HTTP.RetainedAuthoritativeEpochs, wantHTTPRetained) {
		t.Fatalf("HTTP authority = %+v, want legacy epoch 149 retaining %v", runtime.HTTP, wantHTTPRetained)
	}
	if runtime.WebSocket.ModeEpoch != 151 || !slices.Equal(runtime.WebSocket.RetainedAuthoritativeEpochs, []uint64{132, 140, 146}) {
		t.Fatalf("WebSocket authority = %+v, want legacy epoch 151 retaining 132/140/146", runtime.WebSocket)
	}
	options, err := NewCodexContinuityOpenOptions(fsutil.NewMemFS(), "/state", runtime, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	represented := []uint64{56, 59, 63, 79, 82, 132, 140, 146, 149, 151}
	if err := validateCodexLeaseRepresentedModes(represented, options.Modes); err != nil {
		t.Fatalf("migrated routing authority rejected installed continuity epochs %v: %v", represented, err)
	}
	migrated, err := loadCodexRoutingJournal(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.NextEpoch != 151 || migrated.HTTP.ModeEpoch != 149 || migrated.WebSocket.ModeEpoch != 151 {
		t.Fatalf("migrated journal = %+v", migrated)
	}
}

func TestOpenCodexRoutingRuntimeFreshInstallUsesStateRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)

	runtime, err := OpenCodexRoutingRuntime(&Config{
		CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingEnforce,
	}, "build", "client")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.HTTP.ModeEpoch != 1 || runtime.WebSocket.ModeEpoch != 2 {
		t.Fatalf("fresh routing authority = HTTP %d WebSocket %d, want 1/2", runtime.HTTP.ModeEpoch, runtime.WebSocket.ModeEpoch)
	}
	configDir := filepath.Join(base, "cq")
	if _, err := os.Stat(filepath.Join(configDir, "state", "codex-routing-mode.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "codex-routing-mode.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy journal created on fresh install: %v", err)
	}
}

func TestOpenCodexRoutingRuntimeDoesNotReplaceEstablishedStateWithLegacyJournal(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	configDir := filepath.Join(base, "cq")
	stateDir := filepath.Join(configDir, "state")
	httpRequirements, wsRequirements := DefaultCodexRoutingRequirements("build", "client")
	legacy := codexRoutingJournal{
		Version: CodexRoutingJournalVersion, NextEpoch: 151,
		HTTP: codexModeTrack{CodexModeStatus: CodexModeStatus{
			Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce,
			ModeEpoch: 149, AuthoritativeEpoch: 149,
		}, RuntimeFingerprint: requirementsFingerprint(httpRequirements)},
		WebSocket: codexModeTrack{CodexModeStatus: CodexModeStatus{
			Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce,
			ModeEpoch: 151, AuthoritativeEpoch: 151,
		}, RuntimeFingerprint: requirementsFingerprint(wsRequirements)},
	}
	if err := saveJSONFile(filepath.Join(configDir, "codex-routing-mode.json"), &legacy); err != nil {
		t.Fatal(err)
	}
	current := legacy
	current.NextEpoch = 153
	current.HTTP.ModeEpoch = 152
	current.HTTP.AuthoritativeEpoch = 152
	current.HTTP.RetainedAuthoritativeEpochs = []uint64{149}
	current.WebSocket.ModeEpoch = 153
	current.WebSocket.AuthoritativeEpoch = 153
	current.WebSocket.RetainedAuthoritativeEpochs = []uint64{151}
	if err := saveJSONFile(filepath.Join(stateDir, "codex-routing-mode.json"), &current); err != nil {
		t.Fatal(err)
	}

	runtime, err := OpenCodexRoutingRuntime(&Config{
		CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingEnforce,
	}, "build", "client")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.HTTP.ModeEpoch != 152 || runtime.WebSocket.ModeEpoch != 153 {
		t.Fatalf("routing authority = HTTP %d WebSocket %d, want established state 152/153", runtime.HTTP.ModeEpoch, runtime.WebSocket.ModeEpoch)
	}
}

func TestOpenCodexRoutingRuntimeFailsClosedWhenResetRecoveryLegacyIsMalformed(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	configDir := filepath.Join(base, "cq")
	stateDir := filepath.Join(configDir, "state")
	httpRequirements, wsRequirements := DefaultCodexRoutingRequirements("build", "client")
	fresh := codexRoutingJournal{
		Version: CodexRoutingJournalVersion, NextEpoch: 2,
		HTTP: codexModeTrack{CodexModeStatus: CodexModeStatus{
			Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce,
			ModeEpoch: 1, AuthoritativeEpoch: 1,
		}, RuntimeFingerprint: requirementsFingerprint(httpRequirements)},
		WebSocket: codexModeTrack{CodexModeStatus: CodexModeStatus{
			Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce,
			ModeEpoch: 2, AuthoritativeEpoch: 2,
		}, RuntimeFingerprint: requirementsFingerprint(wsRequirements)},
	}
	if err := saveJSONFile(filepath.Join(stateDir, "codex-routing-mode.json"), &fresh); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "codex-routing-mode.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := OpenCodexRoutingRuntime(&Config{
		CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingEnforce,
	}, "build", "client")
	if err == nil || !strings.Contains(err.Error(), "parse Codex routing mode journal") {
		t.Fatalf("error = %v, want malformed legacy journal failure", err)
	}
}
