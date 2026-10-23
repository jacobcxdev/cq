package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/dots"
)

func v2DotsTestInvocation(operation string) cli.Invocation {
	return cli.Invocation{Path: "codex dots " + operation, Options: map[string][]string{}, Arguments: map[string][]string{}}
}

func TestCLIV2DotsRejectedBeforeRunnerIO(t *testing.T) {
	for _, test := range []struct {
		operation string
		options   map[string][]string
		arguments map[string][]string
	}{
		{"status", map[string][]string{"root": {"relative"}}, nil},
		{"install", map[string][]string{"mode": {"other"}}, nil},
		{"install", map[string][]string{"app": {"relative"}}, nil},
		{"install", map[string][]string{"system-auth": {"relative"}}, nil},
		{"install", map[string][]string{"cq-accounts": {"relative"}}, nil},
		{"install", map[string][]string{"account-source": {"relative"}}, nil},
		{"prime", nil, nil},
		{"prime", map[string][]string{"all": {"true"}}, map[string][]string{"dot": {"One"}}},
		{"prime", nil, map[string][]string{"dot": {" "}}},
		{"broadcast", nil, map[string][]string{"message": {"\n\t"}}},
		{"broadcast", nil, map[string][]string{"message": {strings.Repeat("x", 3001)}}},
		{"board", nil, map[string][]string{"thread": {"invalid"}}},
		{"board", map[string][]string{"clear": {"true"}}, map[string][]string{"thread": {"01a14c1f-6e3c-7622-b4a3-b279ecb2a822"}}},
	} {
		t.Run(fmt.Sprint(test), func(t *testing.T) {
			inv := v2DotsTestInvocation(test.operation)
			inv.Options, inv.Arguments = test.options, test.arguments
			runner := dotsRunner{home: func() (string, error) { t.Fatal("invalid argv accessed home"); return "", nil }}
			out := handleV2DotsWithRunner(context.Background(), inv, nil, runner)
			if out.ExitCode != 2 || len(out.Errors) != 1 || out.Errors[0].Code != "dots_invalid_argument" {
				t.Fatalf("outcome = %#v", out)
			}
			if !strings.HasPrefix(out.Errors[0].Message, "Invalid Dots argument: ") {
				t.Fatalf("invalid argument omitted its documented prefix: %q", out.Errors[0].Message)
			}
		})
	}
}

func TestCLIV2DotsInvalidArgumentMessage(t *testing.T) {
	out := handleV2DotsWithRunner(context.Background(), v2DotsTestInvocation("prime"), nil, dotsRunner{})
	if len(out.Errors) != 1 || out.Errors[0].Message != "Invalid Dots argument: Choose a stable Dot handle, an unambiguous name, or --all." {
		t.Fatalf("outcome = %#v", out)
	}
}

func TestCLIV2DotsForwardedEveryOperation(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "private dots")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"integration_mode":"isolated"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	thread := "01a14c1f-6e3c-7622-b4a3-b279ecb2a822"
	for _, operation := range []string{"install", "start", "stop", "status", "sync", "list", "prime", "broadcast", "board"} {
		t.Run(operation, func(t *testing.T) {
			inv := v2DotsTestInvocation(operation)
			inv.Options["root"] = []string{root}
			inv.Options["app"] = []string{filepath.Join(home, "ChatGPT.app")}
			inv.Options["mode"] = []string{"isolated"}
			inv.Options["account-source"] = []string{filepath.Join(home, "external accounts")}
			inv.Arguments["dot"] = []string{"Secondary"}
			inv.Arguments["message"] = []string{strings.Repeat("😀", 3000)}
			inv.Arguments["thread"] = []string{"codex://threads/" + thread}
			calls := 0
			runner := dotsTestRunner(home, func(_ context.Context, controller dots.Controller, request dots.Request) (map[string]any, error) {
				calls++
				if request.Operation != operation || controller.Root != canonicalDotsPath(root) {
					t.Fatalf("controller=%#v request=%#v", controller, request)
				}
				switch operation {
				case "install":
					if request.Install.App != inv.Options["app"][0] || request.Install.Mode != "isolated" || request.Install.AccountSource != inv.Options["account-source"][0] || request.Install.SystemAuth != filepath.Join(home, ".codex", "auth.json") || request.Install.CQAccounts != filepath.Join(home, ".codex", "accounts") {
						t.Fatalf("install = %#v", request.Install)
					}
				case "prime":
					if request.Target != "Secondary" || request.All {
						t.Fatalf("prime = %#v", request)
					}
				case "broadcast":
					if request.Target != inv.Arguments["message"][0] {
						t.Fatal("broadcast message changed")
					}
				case "board":
					if request.Target != thread || request.Clear {
						t.Fatalf("board = %#v", request)
					}
				}
				return map[string]any{"native_catalog_ready": true, "profiles": 0}, nil
			})
			out := handleV2DotsWithRunner(context.Background(), inv, nil, runner)
			if out.ExitCode != 0 || calls != 1 || !strings.Contains(string(out.Data), `"profiles":0`) || out.Terminal == nil {
				t.Fatalf("calls=%d outcome=%#v", calls, out)
			}
		})
	}
}

func TestCLIV2DotsReceiptAndFailureRemainSeparate(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code int
	}{
		{"success", nil, 0},
		{"failure", errors.New("sensitive upstream details"), 1},
		{"timeout", context.DeadlineExceeded, 7},
		{"cancelled", context.Canceled, 130},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv := v2DotsTestInvocation("prime")
			inv.Options["all"] = []string{"true"}
			runner := dotsTestRunner(t.TempDir(), func(_ context.Context, _ dots.Controller, request dots.Request) (map[string]any, error) {
				if !request.All || request.Target != "" {
					t.Fatalf("request=%#v", request)
				}
				return map[string]any{"priming": []map[string]any{{"target": "failed@example.test", "outcome": "accepted"}, {"target": "other", "outcome": "unknown", "failure_type": "TimeoutError"}}, "credential": "must not emit"}, test.err
			})
			out := handleV2DotsWithRunner(context.Background(), inv, nil, runner)
			if out.ExitCode != test.code || len(out.Warnings) != 1 || out.Warnings[0].Code != "dots_incomplete" {
				t.Fatalf("outcome=%#v", out)
			}
			var encoded bytes.Buffer
			if err := cli.WriteJSON(&encoded, inv.Path, out); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				SchemaVersion int        `json:"schema_version"`
				Command       string     `json:"command"`
				OK            bool       `json:"ok"`
				Data          v2DotsData `json:"data"`
			}
			if err := json.Unmarshal(encoded.Bytes(), &envelope); err != nil || envelope.SchemaVersion != 2 || envelope.Command != inv.Path || envelope.OK != (test.code == 0) || envelope.Data.Result == nil || len(*envelope.Data.Result.Priming) != 2 {
				t.Fatalf("envelope=%s err=%v", encoded.String(), err)
			}
			if strings.Contains(encoded.String(), "must not emit") || strings.Contains(encoded.String(), "sensitive upstream") || strings.Contains(out.Human, "\x1b") {
				t.Fatalf("unsafe output: %s", encoded.String())
			}
			styled := out.Terminal(cli.TerminalOptions{Width: 80, Colour: true})
			for _, want := range []string{"\x1b[36mfailed@example.test\x1b[0m", "\x1b[32maccepted\x1b[0m", "\x1b[33munknown\x1b[0m"} {
				if !strings.Contains(styled, want) {
					t.Fatalf("missing semantic span %q in %q", want, styled)
				}
			}
		})
	}
}

func TestCLIV2DotsMissingInvalidAndCancelledReceipt(t *testing.T) {
	for _, receipt := range []map[string]any{nil, {"profiles": "wrong type"}} {
		out := handleV2DotsWithRunner(context.Background(), v2DotsTestInvocation("status"), nil, dotsTestRunner(t.TempDir(), func(context.Context, dots.Controller, dots.Request) (map[string]any, error) { return receipt, nil }))
		if out.ExitCode != 1 || len(out.Data) != 0 {
			t.Fatalf("receipt=%v outcome=%#v", receipt, out)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := handleV2DotsWithRunner(ctx, v2DotsTestInvocation("status"), nil, dotsRunner{})
	if out.ExitCode != 130 {
		t.Fatalf("cancelled outcome=%#v", out)
	}
}

func TestCLIV2DotsBoardObservationAndClear(t *testing.T) {
	for _, clear := range []bool{false, true} {
		inv := v2DotsTestInvocation("board")
		inv.Options["clear"] = []string{fmt.Sprint(clear)}
		runner := dotsTestRunner(t.TempDir(), func(_ context.Context, _ dots.Controller, request dots.Request) (map[string]any, error) {
			if request.Clear != clear {
				t.Fatalf("clear=%t request=%#v", clear, request)
			}
			return map[string]any{"board": nil}, nil
		})
		out := handleV2DotsWithRunner(context.Background(), inv, nil, runner)
		if out.ExitCode != 0 || !strings.Contains(out.Human, "Board: Not bound") || !strings.Contains(string(out.Data), `"bound":false`) {
			t.Fatalf("outcome=%#v", out)
		}
	}
}

func TestCLIV2DotsTerminalWidthsAndPendingState(t *testing.T) {
	inv := v2DotsTestInvocation("install")
	inv.Options["app"] = []string{filepath.Join(t.TempDir(), "ChatGPT.app")}
	inv.Options["mode"] = []string{"isolated"}
	runner := dotsTestRunner(t.TempDir(), func(context.Context, dots.Controller, dots.Request) (map[string]any, error) {
		return map[string]any{"installed": true, "started": false, "restart_required": true, "system_app_modified": false, "reason": "Quit the normal desktop app, then run cq codex dots start; CQ will not interrupt running work"}, nil
	})
	out := handleV2DotsWithRunner(context.Background(), inv, nil, runner)
	if out.ExitCode != 0 || len(out.Warnings) != 1 || !strings.Contains(out.Human, "Started: No") {
		t.Fatalf("outcome=%#v", out)
	}
	for _, width := range []int{20, 40, 80, 120} {
		plain := out.Terminal(cli.TerminalOptions{Width: width})
		styled := out.Terminal(cli.TerminalOptions{Width: width, Colour: true})
		if ansi.Strip(styled) != plain {
			t.Fatalf("colour changed output at width %d", width)
		}
		for _, line := range strings.Split(plain, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d overflow: %q", width, line)
			}
		}
	}
}

func TestCLIV2DotsCanonicalParsingAndPurePresentation(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "dots", "--root", "/private/dots", "status"},
		{"codex", "dots", "status", "--root", "/private/dots"},
		{"--json", "codex", "dots", "board", "--clear"},
		{"codex", "dots", "prime", "--all"},
	} {
		inv, err := cli.Parse(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(strings.Join(args, " "), "--root") && !reflect.DeepEqual(inv.Options["root"], []string{"/private/dots"}) {
			t.Fatalf("root not retained: %#v", inv)
		}
		if inv.Path == "codex dots board" && !reflect.DeepEqual(inv.Options["clear"], []string{"true"}) {
			t.Fatalf("clear not retained: %#v", inv)
		}
	}
	for _, args := range [][]string{{"codex", "dots"}, {"codex", "dots", "broadcast", "--help"}, {"codex", "dots", "status", "--unknown"}, {"codex", "dots", "broadcast"}} {
		var out, diagnostics bytes.Buffer
		code := cli.Run(context.Background(), args, &cli.Session{Out: &out, Err: &diagnostics}, func(string) (cli.Handler, bool) {
			t.Fatal("presentation or syntax looked up runner")
			return nil, false
		})
		want := 0
		if strings.Contains(strings.Join(args, " "), "--unknown") || args[len(args)-1] == "broadcast" {
			want = 2
		}
		if code != want {
			t.Fatalf("%v: exit=%d out=%q err=%q", args, code, out.String(), diagnostics.String())
		}
	}
}

func TestCLIV2DotsPrivateABIClassification(t *testing.T) {
	if _, ok := cli.Help("__dots"); ok {
		t.Fatal("private helper has public help")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args    []string
		message string
	}{
		{[]string{"__dots", "native-executor"}, "Dots helper requires a role and installation root"},
		{[]string{"__dots", "native-executor", "relative", "--json"}, "Dots helper root must be a clean absolute path"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestCLIV2ExecutableMain$", "--"}, test.args...)...)
		root := t.TempDir()
		command.Env = []string{"CQ_T27_MAIN_HELPER=1", "HOME=" + root, "USERPROFILE=" + root, "LOCALAPPDATA=" + root, "APPDATA=" + root, "XDG_CONFIG_HOME=relative", "PATH=" + root, "SystemRoot=" + os.Getenv("SystemRoot")}
		output, err := command.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || string(output) != "cq: "+test.message+"\n" {
			t.Fatalf("private dispatch %v: output=%q err=%v", test.args, output, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("private invalid invocation accessed state: %v %v", entries, err)
		}
	}
}
