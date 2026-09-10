package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cli"
)

func TestCLIV2PresentationNeedsNoHomeOrHandlers(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-home")
	t.Setenv("HOME", missing)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(missing, "config"))
	emptyRegistry := cli.Lookup(func(string) (cli.Handler, bool) { return nil, false })
	if _, ok := emptyRegistry("codex account list"); ok {
		t.Fatal("empty operational registry returned a handler")
	}

	help, ok := cli.Help("codex account list")
	if !ok || !strings.HasPrefix(help, "List locally configured Codex accounts.") {
		t.Fatalf("pure help unavailable: ok=%v", ok)
	}
	version := cli.VersionOutcome(cli.BuildInfo{})
	if version.ExitCode != 0 || !strings.HasPrefix(version.Human, "cq dev\n") {
		t.Fatalf("pure version unavailable: %#v", version)
	}
	completion := cli.CompletionOutcome("bash")
	if completion.ExitCode != 0 || completion.Human == "" {
		t.Fatalf("pure completion unavailable: %#v", completion)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("presentation accessed home: %v", err)
	}
}
