//go:build unix

package proxy

import (
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestDefaultCQStateAndRuntimeConsumersUseResolvedRoots(t *testing.T) {
	configBase := filepath.Join(t.TempDir(), "config")
	cacheBase := filepath.Join(t.TempDir(), "cache")
	t.Setenv("XDG_CONFIG_HOME", configBase)
	t.Setenv("XDG_CACHE_HOME", cacheBase)

	stateDir := filepath.Join(configBase, "cq", "state")
	wantState := map[string]string{
		"canary":    filepath.Join(stateDir, "codex-routing-canary.json"),
		"admission": filepath.Join(stateDir, "normal-caller-admissions-v1"),
	}
	canary, err := DefaultCodexCanaryPath()
	if err != nil {
		t.Fatalf("DefaultCodexCanaryPath() error = %v", err)
	}
	admission, err := DefaultNormalCallerAdmissionPath()
	if err != nil {
		t.Fatalf("DefaultNormalCallerAdmissionPath() error = %v", err)
	}
	if canary != wantState["canary"] || admission != wantState["admission"] {
		t.Fatalf("state paths = %q / %q, want %#v", canary, admission, wantState)
	}
	lifecycle, err := DefaultRuntimeLifecyclePath()
	if err != nil {
		t.Fatalf("DefaultRuntimeLifecyclePath() error = %v", err)
	}
	if want := filepath.Join(stateDir, ".cq-instance-cq.lifecycle.lock"); lifecycle != want {
		t.Fatalf("lifecycle path = %q, want %q", lifecycle, want)
	}

	fsys := fsutil.NewMemFS()
	lease, err := OpenDefaultCodexLeaseStore(fsys)
	if err != nil {
		t.Fatalf("OpenDefaultCodexLeaseStore() error = %v", err)
	}
	if lease.path != filepath.Join(stateDir, "codex-turn-leases.json") || lease.keyPath != filepath.Join(stateDir, "codex-turn-leases.key") {
		t.Fatalf("lease paths = %q / %q", lease.path, lease.keyPath)
	}
	primer, err := OpenDefaultCodexPrimerStore(fsys)
	if err != nil {
		t.Fatalf("OpenDefaultCodexPrimerStore() error = %v", err)
	}
	if primer.path != filepath.Join(stateDir, "codex-window-primer.json") || primer.keyPath != filepath.Join(stateDir, "codex-window-primer.key") {
		t.Fatalf("primer paths = %q / %q", primer.path, primer.keyPath)
	}
}

func TestCodexCanaryPathUsesExplicitStateRoot(t *testing.T) {
	stateDir := filepath.Join(string(filepath.Separator), "cq", "state")
	if got, want := CodexCanaryPath(stateDir), filepath.Join(stateDir, "codex-routing-canary.json"); got != want {
		t.Fatalf("CodexCanaryPath() = %q, want %q", got, want)
	}
	if got, want := NormalCallerAdmissionPath(stateDir), filepath.Join(stateDir, "normal-caller-admissions-v1"); got != want {
		t.Fatalf("NormalCallerAdmissionPath() = %q, want %q", got, want)
	}
	runtimeDir := filepath.Join(string(filepath.Separator), "cq", "runtime")
	if got, want := RuntimeLifecyclePath(runtimeDir), filepath.Join(runtimeDir, ".cq-instance-cq.lifecycle.lock"); got != want {
		t.Fatalf("RuntimeLifecyclePath() = %q, want %q", got, want)
	}
}
