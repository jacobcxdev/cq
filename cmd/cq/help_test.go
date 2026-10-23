package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingWriter struct{ err error }

func (writer failingWriter) Write([]byte) (int, error) { return 0, writer.err }
func TestRunProxyCodexDefaultHelpDoesNotCreateConfig(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	for _, args := range [][]string{
		{"default", "--help"},
		{"default", "codex", "--help"},
		{"default", "codex", "-h"},
		{"help", "default", "codex"},
	} {
		if err := runProxy(args); err != nil {
			t.Fatalf("runProxy(%v) error = %v", args, err)
		}
	}

	path := filepath.Join(configHome, "cq", "proxy.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("help config stat error = %v, want not exist", err)
	}
}

func TestRunModelsHelpDoesNotRefresh(t *testing.T) {
	_, stdout, _, deps := testModelsDeps()
	refreshCalls := 0
	deps.Refresh = func() error {
		refreshCalls++
		return nil
	}

	for _, args := range [][]string{
		{"--help"},
		{"list", "--help"},
		{"overlay", "add", "--help"},
	} {
		stdout.Reset()
		if err := runModels(args, deps); err != nil {
			t.Fatalf("runModels(%v): %v", args, err)
		}
		if refreshCalls != 0 {
			t.Fatalf("runModels(%v) called Refresh %d time(s), want 0", args, refreshCalls)
		}
		if !strings.Contains(stdout.String(), "Usage: cq models") {
			t.Fatalf("runModels(%v) did not print models help:\n%s", args, stdout.String())
		}
	}
}
