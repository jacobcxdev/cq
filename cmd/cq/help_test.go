package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestRootHelpShowsFullCLISurface(t *testing.T) {
	out := &bytes.Buffer{}
	handled, exitCode, err := runPureGlobalInspection([]string{"--help"}, out, io.Discard)
	if !handled || exitCode != 0 || err != nil {
		t.Fatalf("root help = %t, %d, %v", handled, exitCode, err)
	}
	help := out.String()
	for _, want := range []string{
		"check",
		"claude",
		"codex",
		"gemini",
		"refresh",
		"agent",
		"service",
		"proxy",
		"models",
		"operation",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("root help missing %q:\n%s", want, help)
		}
	}
	for _, unwanted := range []string{"claude login", "codex login", "gemini accounts", "agent install", "proxy start", "models list", "operation status"} {
		if strings.Contains(help, unwanted) {
			t.Fatalf("root help contains nested command %q:\n%s", unwanted, help)
		}
	}
}

func TestCodexHelpShowsImmediateCommands(t *testing.T) {
	out := &bytes.Buffer{}
	handled, exitCode, err := runPureGlobalInspection([]string{"codex", "--help"}, out, io.Discard)
	if !handled || exitCode != 0 || err != nil {
		t.Fatalf("codex help = %t, %d, %v", handled, exitCode, err)
	}
	help := out.String()
	for _, want := range []string{"codex login", "codex accounts", "codex switch", "codex remove", "codex resets", "codex validate", "codex canary"} {
		if !strings.Contains(help, want) {
			t.Fatalf("codex help missing %q:\n%s", want, help)
		}
	}
}

func TestCodexResetsHelpDocumentsSafetyAndScope(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{
			args: []string{"codex", "resets", "--help"},
			want: []string{"portfolio-wide", "advisory-only", "resets list", "resets recommend", "resets use"},
		},
		{
			args: []string{"codex", "resets", "list", "--help"},
			want: []string{"[account-reference]", "every account visible through cq codex accounts"},
		},
		{
			args: []string{"codex", "resets", "recommend", "--help"},
			want: []string{"whole visible portfolio", "never consumes"},
		},
		{
			args: []string{"codex", "resets", "use", "--help"},
			want: []string{"--credit CREDIT_ID", "next expiry", "default is No", "--yes"},
		},
	}

	for _, test := range tests {
		out := &bytes.Buffer{}
		handled, exitCode, err := runPureGlobalInspection(test.args, out, io.Discard)
		if !handled || exitCode != 0 || err != nil {
			t.Fatalf("help(%v) = %t, %d, %v", test.args, handled, exitCode, err)
		}
		for _, want := range test.want {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("help(%v) missing %q:\n%s", test.args, want, out.String())
			}
		}
	}
}

func TestProxyDefaultHelpShowsImmediateProviders(t *testing.T) {
	help, ok := manualHelp([]string{"proxy", "default"})
	if !ok {
		t.Fatal("manualHelp(proxy default) missing entry")
	}
	for _, want := range []string{
		"Usage: cq proxy default <provider>",
		"proxy default codex",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("proxy default help missing %q:\n%s", want, help)
		}
	}
}

func TestProxyHelpShowsEveryImmediateCommand(t *testing.T) {
	help, ok := manualHelp([]string{"proxy"})
	if !ok {
		t.Fatal("manualHelp(proxy) missing entry")
	}
	for _, command := range stringCommandsForSelector(t, "proxy.go", "runProxy", "args[0]") {
		want := "proxy " + command
		if !strings.Contains(help, want) {
			t.Fatalf("proxy help missing %q:\n%s", want, help)
		}
	}
}

func TestProxyHookCommandReachesHandler(t *testing.T) {
	handled, exitCode, err := runPureGlobalInspection([]string{"proxy", "hook", "codex-stop"}, io.Discard, io.Discard)
	if handled || exitCode != 0 || err != nil {
		t.Fatalf("proxy hook pre-dispatch = %t, %d, %v; want handler fallthrough", handled, exitCode, err)
	}
}

func TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState(t *testing.T) {
	if os.Getenv("CQ_TEST_BARE_PROXY_STATUS") == "1" {
		proxyStatusGet = func(string) (*http.Response, error) {
			return &http.Response{Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
		}
		if err := runProxyStatus(proxyCommandOptions{}); err != nil {
			t.Fatal(err)
		}
		return
	}
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
		helper   bool
	}{
		{args: []string{"--help"}},
		{args: []string{"--version"}},
		{args: []string{"--json", "--help"}},
		{args: []string{"--refresh", "claude", "--help"}},
		{args: []string{"claude", "--json", "login", "--help"}},
		{args: []string{"codex", "validate", "--help"}},
		{args: []string{"codex", "canary", "--help"}},
		{args: []string{"codex", "resets", "--help"}},
		{args: []string{"codex", "resets", "list", "--help"}},
		{args: []string{"codex", "resets", "recommend", "--help"}},
		{args: []string{"codex", "resets", "use", "--help"}},
		{args: []string{"codex", "validate"}},
		{args: []string{"codex", "canary"}},
		{args: []string{"codex", "validate", "capture", "ignored", "--help"}},
		{args: []string{"codex", "canary", "status", "ignored", "--help"}},
		{args: []string{"refresh", "--help"}},
		{args: []string{"agent", "install", "--help"}},
		{args: []string{"agent", "install", "ignored", "--help"}},
		{args: []string{"agent", "help", "install"}},
		{args: []string{"agent", "unknown", "--help"}, exitCode: 1},
		{args: []string{"proxy", "start", "--help"}},
		{args: []string{"proxy", "status"}, helper: true},
		{args: []string{"proxy", "status", "--json"}},
		{args: []string{"proxy", "start", "--port", "29280", "--help"}},
		{args: []string{"proxy", "help", "start"}},
		{args: []string{"proxy", "hook", "--help"}},
		{args: []string{"proxy", "hook", "codex-stop", "--help"}},
		{args: []string{"proxy", "help", "hook"}},
		{args: []string{"models", "list", "--help"}},
		{args: []string{"models", "list", "--provider", "codex", "--help"}},
		{args: []string{"models", "help", "list"}},
		{args: []string{"proxy", "endpoint", "unknown", "--help"}},
		{args: []string{"proxy", "endpoint"}},
		{args: []string{"--json", "--version"}},
		{args: []string{"agent"}, exitCode: 1},
		{args: []string{"proxy"}, exitCode: 1},
		{args: []string{"models"}, exitCode: 1},
		{args: []string{"models", "overlay"}, exitCode: 1},
		{args: []string{"proxy", "prime"}, exitCode: 1},
		{args: []string{"codex", "help", "validate"}, exitCode: 80},
		{args: []string{"agent", "help", "install", "extra"}, exitCode: 1},
		{args: []string{"proxy", "prime", "enable", "ignored", "--help"}, exitCode: 1},
		{args: []string{"proxy", "prime", "unknown", "--help"}, exitCode: 1},
		{args: []string{"models", "overlay", "unknown", "--help"}, exitCode: 1},
		{args: []string{"models", "overlay", "help", "unknown"}, exitCode: 1},
		{args: []string{"proxy", "prime", "--", "--help"}, exitCode: 1},
		{args: []string{"proxy", "prime", "help", "enable", "extra"}, exitCode: 1},
		{args: []string{"help"}, exitCode: 80},
		{args: []string{"claude"}, exitCode: 80},
		{args: []string{"refresh", "unexpected"}, exitCode: 1},
		{args: []string{"agent", "unknown"}, exitCode: 1},
		{args: []string{"proxy", "prime", "unknown"}, exitCode: 1},
		{args: []string{"proxy", "pin", "--bad"}, exitCode: 1},
		{args: []string{"models", "list", "--provider", "invalid"}, exitCode: 1},
		{args: []string{"models", "overlay", "add", "--provider"}, exitCode: 1},
		{args: []string{"codex", "validate", "capture", "--input"}, exitCode: 1},
		{args: []string{"codex", "canary", "unknown"}, exitCode: 1},
	} {
		command := exec.Command(binary, tt.args...)
		if tt.helper {
			command = exec.Command(os.Args[0], "-test.run=^TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState$")
		}
		command.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+xdg)
		if tt.helper {
			command.Env = append(command.Env, "CQ_TEST_BARE_PROXY_STATUS=1")
		}
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

func TestProxyStatusPreDispatchBoundaryUsesOnlyInjectedCollectors(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "absent-home")
	xdg := filepath.Join(root, "absent-xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	calls := 0
	deadlineSeen := false
	target := ProxyInspectionTarget{
		Inspector: func(ctx context.Context) proxy.Fact[proxy.InspectorIdentity] {
			calls++
			deadline, ok := ctx.Deadline()
			remaining := time.Until(deadline)
			deadlineSeen = ok && remaining > 9*time.Second && remaining <= 10*time.Second
			return proxy.UnavailableFact[proxy.InspectorIdentity]("inspector_unavailable")
		},
		Desired: func(context.Context) proxy.Fact[proxy.DesiredProxyState] {
			calls++
			return proxy.UnavailableFact[proxy.DesiredProxyState]("config_unavailable")
		},
		Service: func(context.Context) proxy.Fact[proxy.ServiceState] {
			calls++
			return proxy.UnavailableFact[proxy.ServiceState]("service_unavailable")
		},
		Listener: func(context.Context) proxy.Fact[proxy.ListenerState] {
			calls++
			return proxy.UnavailableFact[proxy.ListenerState]("listener_unavailable")
		},
		Process: func(context.Context) proxy.Fact[proxy.ProcessState] {
			calls++
			return proxy.UnavailableFact[proxy.ProcessState]("process_unavailable")
		},
		Runtime: func(context.Context) proxy.Fact[proxy.RuntimeIdentity] {
			calls++
			return proxy.UnavailableFact[proxy.RuntimeIdentity]("runtime_unavailable")
		},
		DataPlane: func(context.Context) proxy.Fact[proxy.DataPlaneProof] {
			calls++
			return proxy.UnavailableFact[proxy.DataPlaneProof]("data_plane_unavailable")
		},
	}
	var stdout bytes.Buffer
	handled, exitCode, err := runPureGlobalInspectionWithTarget([]string{"proxy", "status", "--json"}, &stdout, io.Discard, target)
	if err != nil || !handled || exitCode != 0 || calls != 7 || !deadlineSeen {
		t.Fatalf("pre-dispatch status = handled %t exit %d calls %d err %v", handled, exitCode, calls, err)
	}
	if !strings.Contains(stdout.String(), `"kind":"proxy_snapshot"`) {
		t.Fatalf("status output = %q", stdout.String())
	}
	for _, path := range []string{home, xdg} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("read-only status created %s: %v", path, statErr)
		}
	}
}

func TestProxyStatusPreDispatchPreservesFrozenBareStatus(t *testing.T) {
	authority, err := ClassifyProxyCommand([]string{"proxy", "status"})
	if err != nil || authority.Row != "proxy_status_frozen" {
		t.Fatalf("bare status authority = %+v, %v", authority, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "absent-xdg"))
	originalGet := proxyStatusGet
	var requestedAddress string
	proxyStatusGet = func(address string) (*http.Response, error) {
		requestedAddress = address
		return &http.Response{Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
	}
	t.Cleanup(func() { proxyStatusGet = originalGet })
	handled, exitCode, err := runPureGlobalInspectionWithTarget([]string{"proxy", "status"}, io.Discard, io.Discard, ProxyInspectionTarget{})
	if err != nil || !handled || exitCode != 0 {
		t.Fatalf("bare status pre-dispatch = handled %t exit %d err %v", handled, exitCode, err)
	}
	if requestedAddress != "http://127.0.0.1:19280/health" {
		t.Fatalf("bare status address = %q", requestedAddress)
	}
	if _, statErr := os.Stat(os.Getenv("XDG_CONFIG_HOME")); !os.IsNotExist(statErr) {
		t.Fatalf("bare status created config home: %v", statErr)
	}

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "cq", "proxy.json"), []byte(`{"port":1234}`), 0o600); err != nil {
		t.Fatal(err)
	}
	requestedAddress = ""
	handled, exitCode, err = runPureGlobalInspectionWithTarget([]string{"proxy", "status"}, io.Discard, io.Discard, ProxyInspectionTarget{})
	if !handled || exitCode != 1 || err == nil || err.Error() != "load config: local_token is required" || requestedAddress != "" {
		t.Fatalf("invalid existing config = handled %t exit %d address %q err %v", handled, exitCode, requestedAddress, err)
	}

	handled, exitCode, err = runPureGlobalInspectionWithTarget([]string{"proxy", "status", "--port", "invalid"}, io.Discard, io.Discard, ProxyInspectionTarget{})
	if !handled || exitCode != 1 || err == nil || err.Error() != `proxy status: invalid port "invalid"` || requestedAddress != "" {
		t.Fatalf("invalid frozen port = handled %t exit %d address %q err %v", handled, exitCode, requestedAddress, err)
	}
	for _, args := range [][]string{
		{"proxy", "status", "--port", "1234", "--json"},
		{"proxy", "status", "--port", "1234", "--human"},
		{"proxy", "status", "--port", "1234", "--strict"},
		{"proxy", "status", "--port", "1234", "--timeout", "5s"},
		{"proxy", "status", "--port", "1234", "--instance-state-root", "/tmp/instance"},
	} {
		handled, exitCode, err = runPureGlobalInspectionWithTarget(args, io.Discard, io.Discard, ProxyInspectionTarget{})
		if !handled || exitCode != 64 || err == nil || err.Error() != "proxy status usage" || requestedAddress != "" {
			t.Fatalf("mixed status selectors %v = handled %t exit %d address %q err %v", args, handled, exitCode, requestedAddress, err)
		}
	}
}

func TestReconciledProxyStatusStrictControlsVerdictExit(t *testing.T) {
	target := ProxyInspectionTarget{}
	for _, test := range []struct {
		name string
		args []string
		want int
	}{
		{name: "non-strict", args: []string{"proxy", "status", "--human"}, want: 0},
		{name: "strict", args: []string{"proxy", "status", "--human", "--strict"}, want: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			handled, exitCode, err := runPureGlobalInspectionWithTarget(test.args, io.Discard, io.Discard, target)
			if err != nil || !handled || exitCode != test.want {
				t.Fatalf("status = handled %t exit %d err %v, want exit %d", handled, exitCode, err, test.want)
			}
		})
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
		{args: []string{"codex", "validate", "--help"}},
		{args: []string{"codex", "canary", "--help"}},
		{args: []string{"codex", "validate"}},
		{args: []string{"codex", "canary"}},
		{args: []string{"codex", "validate", "capture", "ignored", "--help"}},
		{args: []string{"codex", "canary", "status", "ignored", "--help"}},
		{args: []string{"refresh", "--help"}},
		{args: []string{"agent", "install", "--help"}},
		{args: []string{"agent", "install", "ignored", "--help"}},
		{args: []string{"agent", "help", "install"}},
		{args: []string{"proxy", "start", "--help"}},
		{args: []string{"proxy", "start", "--port", "29280", "--help"}},
		{args: []string{"proxy", "help", "start"}},
		{args: []string{"models", "list", "--help"}},
		{args: []string{"models", "list", "--provider", "codex", "--help"}},
		{args: []string{"models", "help", "list"}},
		{args: []string{"proxy", "endpoint", "unknown", "--help"}},
		{args: []string{"proxy", "endpoint"}},
		{args: []string{"agent"}, exitCode: 1},
		{args: []string{"proxy"}, exitCode: 1},
		{args: []string{"models"}, exitCode: 1},
		{args: []string{"models", "overlay"}, exitCode: 1},
		{args: []string{"proxy", "prime"}, exitCode: 1},
		{args: []string{"codex", "help", "validate"}, exitCode: 80},
		{args: []string{"agent", "help", "install", "extra"}, exitCode: 1},
		{args: []string{"proxy", "prime", "enable", "ignored", "--help"}, exitCode: 1},
		{args: []string{"proxy", "prime", "unknown", "--help"}, exitCode: 1},
		{args: []string{"models", "overlay", "unknown", "--help"}, exitCode: 1},
		{args: []string{"models", "overlay", "help", "unknown"}, exitCode: 1},
		{args: []string{"proxy", "prime", "--", "--help"}, exitCode: 1},
		{args: []string{"proxy", "prime", "help", "enable", "extra"}, exitCode: 1},
		{args: []string{"help"}, exitCode: 80},
		{args: []string{"claude"}, exitCode: 80},
		{args: []string{"refresh", "unexpected"}, exitCode: 1},
		{args: []string{"agent", "unknown"}, exitCode: 1},
		{args: []string{"proxy", "prime", "unknown"}, exitCode: 1},
		{args: []string{"proxy", "pin", "--bad"}, exitCode: 1},
		{args: []string{"models", "list", "--provider", "invalid"}, exitCode: 1},
		{args: []string{"models", "overlay", "add", "--provider"}, exitCode: 1},
		{args: []string{"codex", "validate", "capture", "--input"}, exitCode: 1},
		{args: []string{"codex", "canary", "unknown"}, exitCode: 1},
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

func TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		args      []string
		wantHelp  []string
		wantError string
	}{
		{name: "agent", args: []string{"agent"}, wantHelp: []string{"Usage: cq agent <command>", "agent install"}, wantError: "missing subcommand"},
		{name: "proxy", args: []string{"proxy"}, wantHelp: []string{"Usage: cq proxy <command>", "proxy start"}, wantError: "missing subcommand"},
		{name: "models", args: []string{"models"}, wantHelp: []string{"Usage: cq models <command>", "models list"}, wantError: "models: missing subcommand"},
		{name: "models overlay", args: []string{"models", "overlay"}, wantHelp: []string{"Usage: cq models overlay <command>", "overlay add"}, wantError: "models overlay: missing subcommand"},
		{name: "proxy prime", args: []string{"proxy", "prime"}, wantError: "usage: cq proxy prime <status|enable|disable>"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			handled, exitCode, err := runPureGlobalInspection(fixture.args, stdout, stderr)
			if !handled || exitCode != 1 || err == nil || err.Error() != fixture.wantError {
				t.Fatalf("inspection = %t/%d/%v, want true/1/%q", handled, exitCode, err, fixture.wantError)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			for _, want := range fixture.wantHelp {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q", stderr.String(), want)
				}
			}
		})
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

func TestPureGlobalInspectionPreservesInvalidHelpUsage(t *testing.T) {
	for _, fixture := range []struct {
		args []string
		want string
	}{
		{args: []string{"codex", "help", "validate"}, want: "unexpected argument help"},
		{args: []string{"proxy", "unknown", "--help"}, want: "unknown proxy command: unknown"},
		{args: []string{"agent", "help", "unknown"}, want: "no help for command path: agent unknown"},
		{args: []string{"models", "unknown", "help"}, want: "unknown models command: unknown"},
		{args: []string{"agent", "help", "install", "extra"}, want: "no help for command path: agent install extra"},
		{args: []string{"proxy", "prime", "enable", "ignored", "--help"}, want: "usage: cq proxy prime <status|enable|disable>"},
	} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		handled, exitCode, err := runPureGlobalInspection(fixture.args, stdout, stderr)
		combined := stdout.String() + stderr.String()
		if !handled || exitCode == 0 || (err == nil && !strings.Contains(combined, fixture.want)) || (err != nil && !strings.Contains(err.Error(), fixture.want) && !strings.Contains(combined, fixture.want)) {
			t.Fatalf("runPureGlobalInspection(%v) = %t, %d, %v, %q; want nonzero usage containing %q", fixture.args, handled, exitCode, err, combined, fixture.want)
		}
	}
}

func TestPureGlobalInspectionMatchesNestedHandlerGrammar(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		args       []string
		wantExit   int
		wantOutput string
		wantError  string
	}{
		{name: "codex validate implicit help", args: []string{"codex", "validate"}, wantOutput: "Usage: cq codex validate capture"},
		{name: "codex canary implicit help", args: []string{"codex", "canary"}, wantOutput: "Usage: cq codex canary start|status|stop"},
		{name: "proxy endpoint implicit help", args: []string{"proxy", "endpoint"}, wantOutput: "Usage: cq proxy endpoint <command>"},
		{name: "proxy prime unknown help", args: []string{"proxy", "prime", "unknown", "--help"}, wantExit: 1, wantError: "no help for command path: proxy prime unknown"},
		{name: "models overlay unknown help", args: []string{"models", "overlay", "unknown", "--help"}, wantExit: 1, wantError: "unknown models overlay command: unknown"},
		{name: "models overlay help unknown", args: []string{"models", "overlay", "help", "unknown"}, wantExit: 1, wantError: "no help for command path: models overlay unknown"},
		{name: "proxy prime option terminator help", args: []string{"proxy", "prime", "--", "--help"}, wantExit: 1, wantError: "no help for command path: proxy prime --"},
		{name: "proxy prime nested help extra", args: []string{"proxy", "prime", "help", "enable", "extra"}, wantExit: 1, wantError: "no help for command path: proxy prime enable extra"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			handled, exitCode, err := runPureGlobalInspection(fixture.args, stdout, stderr)
			if !handled || exitCode != fixture.wantExit {
				t.Fatalf("inspection = handled %t exit %d, want true/%d", handled, exitCode, fixture.wantExit)
			}
			if fixture.wantError != "" {
				if err == nil || err.Error() != fixture.wantError {
					t.Fatalf("error = %v, want %q", err, fixture.wantError)
				}
				return
			}
			if err != nil || !strings.Contains(stdout.String(), fixture.wantOutput) || stderr.Len() != 0 {
				t.Fatalf("output = %q/%q, error %v; want stdout containing %q", stdout, stderr, err, fixture.wantOutput)
			}
		})
	}
}

func TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors(t *testing.T) {
	for _, fixture := range []struct {
		name string
		args []string
		want string
	}{
		{name: "refresh arguments", args: []string{"refresh", "unexpected"}, want: "refresh: unexpected arguments"},
		{name: "agent command", args: []string{"agent", "unknown"}, want: "unknown agent command: unknown"},
		{name: "proxy command", args: []string{"proxy", "unknown"}, want: "unknown proxy command: unknown"},
		{name: "proxy prime command", args: []string{"proxy", "prime", "unknown"}, want: "unknown proxy prime command: unknown"},
		{name: "proxy prime arity", args: []string{"proxy", "prime", "status", "extra"}, want: "usage: cq proxy prime <status|enable|disable>"},
		{name: "proxy start port", args: []string{"proxy", "start", "--port", "invalid"}, want: "proxy start: invalid port \"invalid\""},
		{name: "proxy status flag", args: []string{"proxy", "status", "--migrate-legacy-managed"}, want: "proxy status: unknown argument --migrate-legacy-managed"},
		{name: "proxy validate port", args: []string{"proxy", "validate-http", "--port"}, want: "proxy validate-http: --port requires a value"},
		{name: "proxy validate live port", args: []string{"proxy", "validate-http", "--port", "19280"}, want: "proxy validate-http: live proxy port is forbidden"},
		{name: "proxy pin flag", args: []string{"proxy", "pin", "--bad"}, want: "unknown flag \"--bad\""},
		{name: "proxy pin arity", args: []string{"proxy", "pin", "one", "two"}, want: "unexpected arguments"},
		{name: "proxy codex default", args: []string{"proxy", "default", "codex", "--bad"}, want: proxyCodexDefaultUsageMessage},
		{name: "proxy endpoint inspect arity", args: []string{"proxy", "endpoint", "inspect-legacy", "extra"}, want: "usage: cq proxy endpoint inspect-legacy"},
		{name: "proxy endpoint command", args: []string{"proxy", "endpoint", "unknown"}, want: "unknown proxy endpoint command: unknown"},
		{name: "models command", args: []string{"models", "unknown"}, want: "unknown models command: unknown"},
		{name: "models list provider value", args: []string{"models", "list", "--provider"}, want: "models list: --provider requires a value"},
		{name: "models list provider", args: []string{"models", "list", "--provider", "invalid"}, want: "models: unknown provider \"invalid\" (want anthropic or codex)"},
		{name: "models list flag", args: []string{"models", "list", "--bad"}, want: "models list: unknown argument --bad"},
		{name: "models overlay command", args: []string{"models", "overlay", "unknown"}, want: "unknown models overlay command: unknown"},
		{name: "models overlay add flags", args: []string{"models", "overlay", "add", "--provider"}, want: "--provider requires a value"},
		{name: "models overlay remove clone", args: []string{"models", "overlay", "remove", "--provider", "codex", "--id", "x", "--clone-from", "y"}, want: "--clone-from is not supported here"},
		{name: "codex validate command", args: []string{"codex", "validate", "unknown"}, want: "unknown Codex validation command: unknown"},
		{name: "codex capture value", args: []string{"codex", "validate", "capture", "--input"}, want: "--input requires a value"},
		{name: "codex capture required", args: []string{"codex", "validate", "capture", "--metadata", "x"}, want: "Codex capture requires --input and --output"},
		{name: "codex http value", args: []string{"codex", "validate", "http", "--client-build"}, want: "--client-build requires a value"},
		{name: "codex websocket flag", args: []string{"codex", "validate", "websocket", "--bad", "x"}, want: "unknown Codex WebSocket validation argument: --bad"},
		{name: "codex canary command", args: []string{"codex", "canary", "unknown"}, want: "unknown Codex canary command: unknown"},
		{name: "codex canary arity", args: []string{"codex", "canary", "status", "extra"}, want: "Codex canary status takes no arguments"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			handled, exitCode, err := runPureGlobalInspection(fixture.args, stdout, stderr)
			if !handled || exitCode != 1 || err == nil || err.Error() != fixture.want {
				t.Fatalf("inspection = %t/%d/%v, want true/1/%q", handled, exitCode, err, fixture.want)
			}
		})
	}
}

func TestPureGlobalInspectionLeavesValidCaptureWhitespacePathForHandler(t *testing.T) {
	handled, exitCode, err := runPureGlobalInspection(
		[]string{"codex", "validate", "capture", "--input", " ", "--output", "out"},
		io.Discard,
		io.Discard,
	)
	if handled || exitCode != 0 || err != nil {
		t.Fatalf("valid lexical capture path = %t/%d/%v, want deferred to handler", handled, exitCode, err)
	}
}

func TestPureGlobalInspectionLexicalErrorsMatchHandlers(t *testing.T) {
	modelDeps := modelsDeps{FS: &fsutil.MemFS{}, HomeDir: "/home", Roots: testCQRoots(), Stdout: io.Discard, Stderr: io.Discard}
	for _, fixture := range []struct {
		name    string
		args    []string
		handler func() error
	}{
		{name: "refresh", args: []string{"refresh", "unexpected"}, handler: func() error { return runRefreshCommand([]string{"unexpected"}) }},
		{name: "agent", args: []string{"agent", "unknown"}, handler: func() error { return runAgent([]string{"unknown"}) }},
		{name: "proxy", args: []string{"proxy", "unknown"}, handler: func() error { return runProxy([]string{"unknown"}) }},
		{name: "proxy start", args: []string{"proxy", "start", "--port", "bad"}, handler: func() error { _, err := parseProxyCommandOptions([]string{"--port", "bad"}); return err }},
		{name: "proxy status", args: []string{"proxy", "status", "--migrate-legacy-managed"}, handler: func() error {
			_, err := parseProxyCommandOptionsFor("proxy status", []string{"--migrate-legacy-managed"})
			return err
		}},
		{name: "proxy codex default", args: []string{"proxy", "default", "codex", "--bad"}, handler: func() error {
			return runProxyCodexDefaultWithDependencies(nil, []string{"--bad"}, proxyCodexDefaultDependencies{})
		}},
		{name: "models list", args: []string{"models", "list", "--provider"}, handler: func() error { return runModelsList([]string{"--provider"}, modelDeps) }},
		{name: "models overlay", args: []string{"models", "overlay", "add", "--provider"}, handler: func() error { return runModelsOverlay([]string{"add", "--provider"}, modelDeps) }},
		{name: "codex validate", args: []string{"codex", "validate", "unknown"}, handler: func() error { return runCodexValidate([]string{"unknown"}) }},
		{name: "codex canary", args: []string{"codex", "canary", "unknown"}, handler: func() error { return runCodexCanary([]string{"unknown"}) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			want := fixture.handler()
			if want == nil {
				t.Fatal("handler unexpectedly accepted invalid grammar")
			}
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			handled, exitCode, got := runPureGlobalInspection(fixture.args, stdout, stderr)
			if !handled || exitCode != 1 || got == nil || got.Error() != want.Error() {
				t.Fatalf("inspection = %t/%d/%v, handler = %v", handled, exitCode, got, want)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("inspection output = %q/%q, want empty handler output", stdout, stderr)
			}
		})
	}
}

func TestManualHelpInspectionPreservesEndpointHelpGrammar(t *testing.T) {
	stdout := &bytes.Buffer{}
	handled, exitCode, err := runPureGlobalInspection([]string{"proxy", "endpoint", "unknown", "--help"}, stdout, io.Discard)
	want, ok := manualHelp([]string{"proxy", "endpoint"})
	if !ok || err != nil || !handled || exitCode != 0 || stdout.String() != want {
		t.Fatalf("endpoint help = %t, %d, %v, %q; want exact help %q", handled, exitCode, err, stdout.String(), want)
	}
}

func TestManualHelpInspectionAllowsLeafArgumentsBeforeHelp(t *testing.T) {
	for _, fixture := range []struct {
		args []string
		path []string
	}{
		{args: []string{"proxy", "start", "--port", "29280", "--help"}, path: []string{"proxy", "start"}},
		{args: []string{"agent", "install", "ignored", "--help"}, path: []string{"agent", "install"}},
		{args: []string{"models", "list", "--provider", "codex", "--help"}, path: []string{"models", "list"}},
		{args: []string{"codex", "validate", "capture", "ignored", "--help"}, path: []string{"codex", "validate"}},
		{args: []string{"codex", "canary", "status", "ignored", "--help"}, path: []string{"codex", "canary"}},
	} {
		stdout := &bytes.Buffer{}
		handled, exitCode, err := runPureGlobalInspection(fixture.args, stdout, io.Discard)
		want, ok := manualHelp(fixture.path)
		if !ok || err != nil || !handled || exitCode != 0 || stdout.String() != want {
			t.Fatalf("runPureGlobalInspection(%v) = %t, %d, %v, %q; want exact help %q", fixture.args, handled, exitCode, err, stdout.String(), want)
		}
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
			name: "codex validate",
			path: []string{"codex", "validate"},
			want: []string{"Usage: cq codex validate capture", "websocket --client-build BUILD"},
		},
		{
			name: "codex canary",
			path: []string{"codex", "canary"},
			want: []string{"Usage: cq codex canary start|status|stop"},
		},
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
			name: "proxy hook",
			path: []string{"proxy", "hook"},
			want: []string{"Usage: cq proxy hook codex-stop", "privacy-safe turn receipt", "does not change routing"},
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
			want: []string{
				"Usage: cq proxy pin [<provider> [--clear | <account-reference>]]",
				"claude or codex",
				"Codex pin applies to new and unbound work",
				"Hard-bound Codex continuity remains on its existing account",
				"Restart proxy to apply a Codex pin change.",
			},
		},
		{
			name: "proxy prime",
			path: []string{"proxy", "prime"},
			want: []string{"Usage: cq proxy prime <command>", "prime enable", "prime disable", "prime status"},
		},
		{
			name: "proxy codex default",
			path: []string{"proxy", "default", "codex"},
			want: []string{
				"Usage: cq proxy default codex [--clear | <account-reference>]",
				"unique email, CQ alias, or opaque AccountKey",
				"independent of system identity",
				"Restart proxy to apply changes.",
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
