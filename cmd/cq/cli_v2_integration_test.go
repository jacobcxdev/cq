package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCLIV2CompleteRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../specs/cli-v2/commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct{ Commands []struct{ Path, Kind string } }
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, command := range spec.Commands {
		if command.Kind != "command" {
			continue
		}
		count++
		if h, ok := lookupCLIV2(command.Path); !ok || h == nil {
			t.Errorf("unregistered leaf %s", command.Path)
		}
	}
	if count != 89 || len(cliV2Handlers) != count {
		t.Fatalf("leaves=%d", count)
	}
}
func TestCLIV2EveryGroupIsPure(t *testing.T) {
	raw, err := os.ReadFile("../../specs/cli-v2/commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct{ Commands []struct{ Path, Kind string } }
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, command := range spec.Commands {
		if command.Kind != "group" {
			continue
		}
		t.Run(command.Path, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			session := &cli.Session{In: strings.NewReader(""), Out: &out, Err: &diagnostics}
			exit := cli.Run(context.Background(), strings.Fields(command.Path), session, func(string) (cli.Handler, bool) { panic("group performed handler lookup") })
			want, ok := cli.Help(command.Path)
			if !ok || exit != 0 || out.String() != want || diagnostics.Len() != 0 {
				t.Fatalf("impure group help: exit=%d out=%q err=%q", exit, out.String(), diagnostics.String())
			}
		})
	}
}

func v2FixtureTempRoot() string {
	// Darwin's per-user temporary path exceeds the Unix socket address limit.
	if runtime.GOOS == "darwin" {
		return "/private/tmp"
	}
	return os.TempDir()
}

func TestCLIV2IntegratedHelpAndInvalidOptionsBeforeIO(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "invalid-relative-root")
	raw, err := os.ReadFile("../../specs/cli-v2/commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct{ Commands []struct{ Path, Kind string } }
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	paths := []string{""}
	for _, c := range spec.Commands {
		paths = append(paths, c.Path)
	}
	if len(paths) != 127 {
		t.Fatalf("help count %d", len(paths))
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			args := append(strings.Fields(path), "--help")
			if path == "help" {
				args = []string{"help", "help"}
			}
			var out, diagnostics bytes.Buffer
			session := &cli.Session{In: noAccessV2Input{}, Out: &out, Err: &diagnostics}
			exit := runCLIV2(context.Background(), args, session)
			name := strings.ReplaceAll(path, " ", "-")
			if name == "" {
				name = "cq"
			}
			want, err := os.ReadFile(filepath.Join("../../specs/cli-v2/help", name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || out.String() != string(want) || diagnostics.Len() != 0 {
				t.Fatalf("exit=%d help mismatch, err=%s", exit, diagnostics.String())
			}
			out.Reset()
			diagnostics.Reset()
			exit = runCLIV2(context.Background(), append(strings.Fields(path), "--t27-invalid", "--json"), session)
			if exit != 2 || !strings.Contains(out.String(), "Unknown option: --t27-invalid") {
				t.Fatalf("invalid option reached IO: %d %s", exit, out.String())
			}
		})
	}
}

func TestCLIV2IntegratedReservedCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "invalid-relative-root")
	for _, args := range candidateReservedArgs() {
		var out, diagnostics bytes.Buffer
		exit := runCLIV2(context.Background(), append(args, "--json"), &cli.Session{In: noAccessV2Input{}, Out: &out, Err: &diagnostics})
		if exit != 4 || !strings.Contains(out.String(), `"code":"candidate_evidence_unavailable"`) {
			t.Fatalf("reserved %v exit=%d out=%s err=%s", args, exit, out.String(), diagnostics.String())
		}
	}
}

func TestCLIV2ProductionMissingTokenIsAuthentication(t *testing.T) {
	root := t.TempDir()
	paths := proxy.PathsForRoots(userdirs.Roots{Config: root})
	raw, err := json.Marshal(&proxy.Config{Port: 19280, ClaudeUpstream: "https://api.anthropic.com", CodexUpstream: proxy.DefaultCodexUpstream})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	load := func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) }
	if _, err := load(); !errors.Is(err, proxy.ErrLocalTokenRequired) {
		t.Fatalf("actual fixture error: %v", err)
	}
	calls := 0
	doer := testDoer(func(*http.Request) (*http.Response, error) { calls++; panic("missing token contacted control") })
	_, reserveErr := requestProxyReserve(context.Background(), proxyReserveOptions{action: "status"}, proxyPolicyDependencies{LoadConfig: load, Doer: doer})
	if !errors.Is(reserveErr, proxy.ErrLocalTokenRequired) {
		t.Fatal("reserve discarded sentinel")
	}
	if out := v2ReserveFailure(reserveErr); out.ExitCode != 5 {
		t.Fatalf("reserve %+v", out)
	}
	_, policyErr := proxyPolicyRequest(context.Background(), proxyPolicyDependencies{LoadConfig: load, Doer: doer}, http.MethodGet, proxy.RuntimePolicyPath, 0, nil, "")
	if !errors.Is(policyErr, proxy.ErrLocalTokenRequired) {
		t.Fatal("policy discarded sentinel")
	}
	if out := v2PolicyFailure(policyErr, policyInvocation(t, "policy", "show")); out.ExitCode != 5 {
		t.Fatalf("policy %+v", out)
	}
	_, leaseErr := requestProxyLeaseInvalidation(context.Background(), 0, proxyLeaseDependencies{LoadConfig: load, Doer: doer})
	if out := v2LeaseFailure(leaseErr); out.ExitCode != 5 {
		t.Fatalf("lease %+v", out)
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if !bytes.Equal(raw, after) || calls != 0 {
		t.Fatal("missing authentication mutated or contacted control")
	}
}

func TestCLIV2ProductionRefreshCompletionOnce(t *testing.T) {
	oldHandler := cliV2Handlers["auth refresh"]
	oldRunner := serviceRefreshRunner
	t.Cleanup(func() { cliV2Handlers["auth refresh"] = oldHandler; serviceRefreshRunner = oldRunner })
	for _, mode := range []string{"scheduled-success", "scheduled-failure", "ordinary", "authority-failed"} {
		t.Run(mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			roots := userdirs.Roots{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state"), Cache: filepath.Join(root, "cache"), Runtime: filepath.Join(root, "runtime"), Logs: filepath.Join(root, "logs")}
			calls, hooks := 0, 0
			cliV2Handlers["auth refresh"] = scheduledV2Refresh(func(context.Context, cli.Invocation, *cli.Session) cli.Outcome {
				calls++
				if mode == "scheduled-failure" {
					return authFailure("auth_store_failed", "Synthetic failure.")
				}
				return cli.Outcome{Data: json.RawMessage(`{"providers":[]}`)}
			})
			if mode == "ordinary" {
				t.Setenv("CQ_SERVICE_REFRESH", "")
				serviceRefreshRunner = oldRunner
			} else {
				serviceRefreshRunner = func(run func() error) error {
					hooks++
					if mode == "authority-failed" {
						return errors.New("synthetic authority refused")
					}
					return recordServiceRefresh(filepath.Join(root, "cq"), roots, run, time.Now)
				}
			}
			var out, diagnostics bytes.Buffer
			exit := runCLIV2(context.Background(), []string{"refresh", "--json"}, &cli.Session{In: noAccessV2Input{}, Out: &out, Err: &diagnostics})
			wantCalls, wantExit := 1, 0
			if mode == "authority-failed" {
				wantCalls = 0
			}
			if mode == "authority-failed" || mode == "scheduled-failure" {
				wantExit = 1
			}
			if calls != wantCalls || exit != wantExit || strings.Count(out.String(), `"schema_version"`) != 1 {
				t.Fatalf("calls=%d hooks=%d exit=%d out=%s", calls, hooks, exit, out.String())
			}
			receipt, err := readServiceRefreshCompletion(filepath.Join(root, "cq"), roots, time.Now())
			if strings.HasPrefix(mode, "scheduled-") {
				if err != nil || receipt.ExitCode != wantExit || receipt.CompletedAt.Before(receipt.StartedAt) {
					t.Fatalf("receipt=%+v err=%v", receipt, err)
				}
			} else if err == nil {
				t.Fatal("unmarked or refused invocation wrote receipt")
			}
		})
	}
}

func TestCLIV2OutputWriteDoesNotReplayRefresh(t *testing.T) {
	old := cliV2Handlers["auth refresh"]
	defer func() { cliV2Handlers["auth refresh"] = old }()
	calls := 0
	cliV2Handlers["auth refresh"] = func(context.Context, cli.Invocation, *cli.Session) cli.Outcome {
		calls++
		return cli.Outcome{Data: json.RawMessage(`{}`)}
	}
	exit := runCLIV2(context.Background(), []string{"auth", "refresh", "--json"}, &cli.Session{Out: failingWriter{errors.New("full")}, Err: io.Discard})
	if exit != 1 || calls != 1 {
		t.Fatalf("exit=%d calls=%d", exit, calls)
	}
}

var _ fsutil.FileSystem = resetProductionFS{}

func TestCLIV2ExecutableMain(t *testing.T) {
	if os.Getenv("CQ_T27_MAIN_HELPER") == "1" {
		serviceLifecycleFactory = func(string) (*serviceLifecycle, error) { return nil, errors.New("frozen machine ABI selected") }
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				main()
				return
			}
		}
		t.Fatal("missing helper arguments")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, tc := range []struct {
		name     string
		args     []string
		exit     int
		contains string
	}{
		{"help", []string{"--help"}, 0, "Usage:"},
		{"version", []string{"version", "--json"}, 0, `"command":"version"`},
		{"alias-help", []string{"proxy", "reserve", "status", "--help"}, 0, "codex proxy reserve status"},
		{"invalid", []string{"service", "status", "--unknown"}, 2, "Unknown option"},
		{"internal-marker-rejected", []string{"service", "status", "--owner=homebrew"}, 2, "Unknown option"},
		{"frozen-classifier-first", []string{"service", "install", "--owner=go"}, 1, "frozen machine ABI selected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := append([]string{"-test.run=^TestCLIV2ExecutableMain$", "--"}, tc.args...)
			command := exec.CommandContext(ctx, executable, args...)
			command.Env = []string{"CQ_T27_MAIN_HELPER=1", "HOME=" + root, "USERPROFILE=" + root, "LOCALAPPDATA=" + root, "APPDATA=" + root, "XDG_CONFIG_HOME=" + root, "XDG_CACHE_HOME=" + root, "XDG_STATE_HOME=" + root, "PATH=" + root, "SystemRoot=" + os.Getenv("SystemRoot")}
			output, err := command.CombinedOutput()
			exit := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					exit = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if exit != tc.exit || !strings.Contains(string(output), tc.contains) {
				t.Fatalf("exit=%d output=%s", exit, output)
			}
		})
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("pure main wrote state: %v %v", entries, err)
	}
}
