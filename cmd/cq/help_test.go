package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestRootHelpShowsFullCLISurface(t *testing.T) {
	out := &bytes.Buffer{}
	var cli CLI
	kctx, err := kong.New(&cli,
		append(cliKongOptions(), kong.Writers(out, io.Discard), kong.Exit(func(int) {}))...,
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	_, _ = kctx.Parse([]string{"--help"})
	help := out.String()
	for _, want := range []string{
		"check [<providers> ...]",
		"claude login",
		"codex login",
		"gemini accounts",
		"refresh",
		"agent install",
		"agent uninstall",
		"proxy start",
		"proxy install",
		"proxy uninstall",
		"proxy restart",
		"proxy status",
		"proxy validate-http",
		"proxy pin",
		"proxy prime",
		"proxy codex-default",
		"models list",
		"models refresh",
		"models overlay add",
		"models overlay remove",
		"models overlay prune",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("root help missing %q:\n%s", want, help)
		}
	}
}

func TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cq")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	fixture := t.TempDir()
	home := filepath.Join(fixture, "absent-home")
	xdg := filepath.Join(fixture, "absent-xdg")
	for _, tt := range []struct {
		args     []string
		exitCode int
	}{
		{args: []string{"--help"}},
		{args: []string{"--version"}},
		{args: []string{"--json", "--help"}},
		{args: []string{"--refresh", "claude", "--help"}},
		{args: []string{"claude", "--json", "login", "--help"}},
		{args: []string{"refresh", "--help"}},
		{args: []string{"agent", "install", "--help"}},
		{args: []string{"agent", "help", "install"}},
		{args: []string{"proxy", "start", "--help"}},
		{args: []string{"proxy", "help", "start"}},
		{args: []string{"models", "list", "--help"}},
		{args: []string{"models", "help", "list"}},
		{args: []string{"--json", "--version"}},
		{args: []string{"help"}, exitCode: 80},
		{args: []string{"claude"}, exitCode: 80},
	} {
		command := exec.Command(binary, tt.args...)
		command.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+xdg)
		output, err := command.CombinedOutput()
		if got := command.ProcessState.ExitCode(); got != tt.exitCode {
			t.Fatalf("cq %v exit = %d, want %d: %v\n%s", tt.args, got, tt.exitCode, err, output)
		}
	}
	for _, path := range []string{home, xdg} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only command created %s: %v", path, err)
		}
	}
}

func TestGlobalHelpAndVersionPreserveAbsentHomeAndXDGEnvironment(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cq")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	workingDirectory := t.TempDir()
	environment := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") {
			environment = append(environment, entry)
		}
	}
	for _, fixture := range []struct {
		args     []string
		exitCode int
	}{
		{args: []string{"--help"}},
		{args: []string{"--version"}},
		{args: []string{"--json", "--help"}},
		{args: []string{"--refresh", "claude", "--help"}},
		{args: []string{"claude", "--json", "login", "--help"}},
		{args: []string{"refresh", "--help"}},
		{args: []string{"agent", "install", "--help"}},
		{args: []string{"agent", "help", "install"}},
		{args: []string{"proxy", "start", "--help"}},
		{args: []string{"proxy", "help", "start"}},
		{args: []string{"models", "list", "--help"}},
		{args: []string{"models", "help", "list"}},
		{args: []string{"help"}, exitCode: 80},
		{args: []string{"claude"}, exitCode: 80},
	} {
		command := exec.Command(binary, fixture.args...)
		command.Dir = workingDirectory
		command.Env = environment
		output, err := command.CombinedOutput()
		if got := command.ProcessState.ExitCode(); got != fixture.exitCode {
			t.Fatalf("cq %v exit = %d, want %d: %v\n%s", fixture.args, got, fixture.exitCode, err, output)
		}
	}
	entries, err := os.ReadDir(workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only command created working-directory state: %v", entries)
	}
}

func TestPureGlobalInspectionPreservesBareHelpUsageError(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	handled, exitCode, err := runPureGlobalInspection([]string{"help"}, stdout, stderr)
	if !pureInspectionErrorWasRendered(err) {
		t.Fatalf("error = %v, want rendered Kong usage error", err)
	}
	if !handled || exitCode != 80 {
		t.Fatalf("handled, exitCode = %v, %d; want true, 80", handled, exitCode)
	}
	if !strings.Contains(stdout.String(), "Usage: cq check") || !strings.Contains(stderr.String(), "cq: error:") {
		t.Fatalf("bare help output changed:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

func TestPureGlobalInspectionHandlesOrdinaryUsageBeforeCompatibility(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	handled, exitCode, err := runPureGlobalInspection([]string{"claude"}, stdout, stderr)
	if !pureInspectionErrorWasRendered(err) {
		t.Fatalf("error = %v, want rendered Kong usage error", err)
	}
	if !handled {
		t.Fatal("ordinary usage error was not handled")
	}
	if exitCode != 80 {
		t.Fatalf("exit code = %d, want 80", exitCode)
	}
	if !strings.Contains(stdout.String(), "Usage: cq claude <command>") {
		t.Fatalf("usage output = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "cq: error: expected one of") {
		t.Fatalf("error output = %q", stderr.String())
	}
}

func TestPureGlobalInspectionPropagatesHelpWriteError(t *testing.T) {
	want := io.ErrClosedPipe
	handled, exitCode, err := runPureGlobalInspection([]string{"--help"}, failingWriter{err: want}, io.Discard)
	if !handled {
		t.Fatal("global help was not handled")
	}
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
}

type failingWriter struct{ err error }

func (writer failingWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestManualHelpTextDocumentsEachCommandPath(t *testing.T) {
	for _, tt := range []struct {
		name string
		path []string
		want []string
	}{
		{
			name: "refresh",
			path: []string{"refresh"},
			want: []string{"Usage: cq refresh", "Refresh stored OAuth tokens"},
		},
		{
			name: "agent",
			path: []string{"agent"},
			want: []string{"Usage: cq agent <command>", "agent install", "agent uninstall"},
		},
		{
			name: "proxy start",
			path: []string{"proxy", "start"},
			want: []string{"Usage: cq proxy start [--port PORT] [--migrate-legacy-managed]", "Start local Claude and Codex proxy", "--migrate-legacy-managed"},
		},
		{
			name: "proxy validate HTTP",
			path: []string{"proxy", "validate-http"},
			want: []string{
				"Usage: cq proxy validate-http --port PORT",
				"one-shot installed HTTP validation",
				"cannot be live port 19280",
				"does not write readiness evidence",
			},
		},
		{
			name: "proxy pin",
			path: []string{"proxy", "pin"},
			want: []string{"Usage: cq proxy pin [--clear | <email-or-account-uuid>]", "Pin Claude proxy routing"},
		},
		{
			name: "proxy prime",
			path: []string{"proxy", "prime"},
			want: []string{"Usage: cq proxy prime <command>", "prime enable", "prime disable", "prime status"},
		},
		{
			name: "proxy codex default",
			path: []string{"proxy", "codex-default"},
			want: []string{
				"Usage: cq proxy codex-default [--clear | <account-reference>]",
				"unique email, CQ alias, or opaque AccountKey",
				"independent of Codex Desktop/system identity",
				"never mutates Codex Bar or system authentication",
				"Restart proxy to apply change.",
			},
		},
		{
			name: "models list",
			path: []string{"models", "list"},
			want: []string{"Usage: cq models list [--json] [--provider PROVIDER]", "List active registry models"},
		},
		{
			name: "models overlay add",
			path: []string{"models", "overlay", "add"},
			want: []string{"Usage: cq models overlay add --provider PROVIDER --id MODEL [--clone-from MODEL]", "Add or update user model overlay"},
		},
		{
			name: "models overlay remove",
			path: []string{"models", "overlay", "remove"},
			want: []string{"Usage: cq models overlay remove --provider PROVIDER --id MODEL", "Remove user model overlay"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			help, ok := manualHelp(tt.path)
			if !ok {
				t.Fatalf("manualHelp(%v) missing entry", tt.path)
			}
			for _, want := range tt.want {
				if !strings.Contains(help, want) {
					t.Fatalf("manualHelp(%v) missing %q:\n%s", tt.path, want, help)
				}
			}
		})
	}
}

func TestRunProxyCodexDefaultHelpDoesNotCreateConfig(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	for _, args := range [][]string{
		{"codex-default", "--help"},
		{"codex-default", "-h"},
		{"help", "codex-default"},
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
