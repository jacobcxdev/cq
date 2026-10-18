package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/dots"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestDotsPreservedStockCLIExitStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"stock", &dots.CloudStdioExitError{Code: 7}, 7},
		{"wrapped", errors.Join(errors.New("helper"), &dots.CloudStdioExitError{Code: 42}), 42},
		{"invalid", &dots.CloudStdioExitError{Code: -1}, 1},
		{"ordinary", errors.New("configuration unavailable"), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := dotsExitCode(test.err); got != test.want {
				t.Fatalf("exit status = %d, want %d", got, test.want)
			}
		})
	}
}

func dotsTestRunner(home string, run func(context.Context, dots.Controller, dots.Request) (map[string]any, error)) dotsRunner {
	return dotsRunner{
		home:       func() (string, error) { return home, nil },
		executable: func() (string, error) { return "/usr/local/bin/cq", nil },
		baseURL:    func() (string, error) { return "http://127.0.0.1:19280", nil },
		run:        run,
	}
}

func TestDotsCanonicalisedInstallationRootBeforeNativeModeRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installation aliases")
	}
	home := t.TempDir()
	physical := filepath.Join(home, "physical")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(alias, "dots")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"integration_mode":"native"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := canonicalDotsPath(filepath.Join(physical, "dots"))
	runner := dotsTestRunner(home, func(_ context.Context, controller dots.Controller, request dots.Request) (map[string]any, error) {
		if controller.Root != want || controller.BaseURL == "" || request.Operation != "start" {
			t.Fatalf("noncanonical native request: %#v %#v", controller, request)
		}
		return nil, nil
	})
	if err := runner.execute(context.Background(), &CodexDotsCmd{Root: root}, "start", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestDotsLaunchOverridesBaseURLWithoutCodexConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Dots requires POSIX launchers")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("OPENAI_BASE_URL", "https://example.invalid")
	paths, err := proxy.ResolveDefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`{"port":19321,"local_token":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "dots")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"integration_mode":"native"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"install", "start", "sync"} {
		runner := dotsTestRunner(home, func(_ context.Context, controller dots.Controller, request dots.Request) (map[string]any, error) {
			if controller.BaseURL != "http://127.0.0.1:19321" || request.Install.BaseURL != controller.BaseURL {
				t.Fatalf("CQ proxy override was not forwarded: %#v %#v", controller, request)
			}
			return nil, nil
		})
		runner.baseURL = defaultDotsRunner().baseURL
		if err := runner.execute(context.Background(), &CodexDotsCmd{Root: root, Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}}, operation, false, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("Codex config was read or created: %v", err)
	}
}

func TestDotsIsolatedLaunchDoesNotRequireProxyConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Dots requires POSIX launchers")
	}
	home := t.TempDir()
	root := filepath.Join(home, "dots")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := dotsTestRunner(home, func(_ context.Context, controller dots.Controller, _ dots.Request) (map[string]any, error) {
		if controller.BaseURL != "" {
			t.Fatal("isolated launch received native proxy override")
		}
		return nil, nil
	})
	runner.baseURL = func() (string, error) { t.Fatal("isolated launch read proxy config"); return "", nil }
	for _, mode := range []string{`{"integration_mode":"isolated"}`, `{}`} {
		if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(mode), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, operation := range []string{"start", "sync"} {
			if err := runner.execute(context.Background(), &CodexDotsCmd{Root: root}, operation, false, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := runner.execute(context.Background(), &CodexDotsCmd{Root: root, Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app", Mode: "isolated"}}, "install", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestDotsCommandParsing(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"codex", "dots", "install"}, "codex dots install"},
		{[]string{"--json", "codex", "dots", "--root", "/private/dots", "status"}, "codex dots status"},
		{[]string{"codex", "dots", "prime", "Secondary"}, "codex dots prime <dot>"},
		{[]string{"codex", "dots", "prime", "--all"}, "codex dots prime"},
		{[]string{"codex", "dots", "sync"}, "codex dots sync"},
		{[]string{"codex", "dots", "broadcast", "Hello, team!"}, "codex dots broadcast <message>"},
		{[]string{"codex", "dots", "board", "codex://threads/01a14c1f-6e3c-7622-b4a3-b279ecb2a822"}, "codex dots board <thread>"},
		{[]string{"codex", "dots", "board"}, "codex dots board"},
		{[]string{"codex", "dots", "board", "--clear"}, "codex dots board"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			var cli CLI
			parser, err := kong.New(&cli, kong.Writers(io.Discard, io.Discard))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parser.Parse(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Command() != test.want {
				t.Fatalf("command %q", parsed.Command())
			}
		})
	}
}

func TestDotsInstallForwardsStructuredArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Dots requires POSIX launchers")
	}
	home := t.TempDir()
	root := filepath.Join(home, "private dots")
	var controller dots.Controller
	var request dots.Request
	runner := dotsTestRunner(home, func(_ context.Context, c dots.Controller, r dots.Request) (map[string]any, error) {
		controller, request = c, r
		return map[string]any{"installed": true}, nil
	})
	options := CodexDotsCmd{Root: root, Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app", AccountSource: "/private/account store"}}
	var output strings.Builder
	if err := runner.execute(context.Background(), &options, "install", true, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := dots.InstallOptions{Root: canonicalDotsPath(root), App: "/Applications/ChatGPT.app", SystemAuth: filepath.Join(home, ".codex", "auth.json"), CQAccounts: filepath.Join(home, ".codex", "accounts"), AccountSource: "/private/account store", Mode: "native", Executable: "/usr/local/bin/cq", BaseURL: "http://127.0.0.1:19280"}
	if request.Operation != "install" || !reflect.DeepEqual(request.Install, want) || controller.Root != want.Root || controller.Executable != want.Executable {
		t.Fatalf("controller %#v request %#v", controller, request)
	}
	var value map[string]bool
	if err := json.Unmarshal([]byte(output.String()), &value); err != nil || !value["installed"] {
		t.Fatalf("output %q error %v", output.String(), err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("CLI extracted a payload or created state: %v", err)
	}
}

func TestDotsInstallModeParsingAndPythonFlagRemoved(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"codex", "dots", "install"}, "native"},
		{[]string{"codex", "dots", "install", "--mode", "isolated"}, "isolated"},
	} {
		var cli CLI
		parser, err := kong.New(&cli, kong.Writers(io.Discard, io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parser.Parse(test.args); err != nil {
			t.Fatal(err)
		}
		if got := cli.Codex.Dots.Install.Mode; got != test.want {
			t.Fatalf("mode %q, want %q", got, test.want)
		}
	}
	var cli CLI
	parser, err := kong.New(&cli, kong.Writers(io.Discard, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse([]string{"codex", "dots", "install", "--python", "/usr/bin/python3"}); err == nil {
		t.Fatal("removed Python flag still accepted")
	}
}

func TestDotsStatusUsesGoControllerWithoutPythonInstallation(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "dots")
	calls := 0
	runner := dotsTestRunner(home, func(_ context.Context, controller dots.Controller, request dots.Request) (map[string]any, error) {
		calls++
		if controller.Root != canonicalDotsPath(root) || request.Operation != "status" {
			t.Fatalf("controller %#v request %#v", controller, request)
		}
		return nil, nil
	})
	runner.baseURL = func() (string, error) { t.Fatal("status required launch configuration"); return "", nil }
	if err := runner.execute(context.Background(), &CodexDotsCmd{Root: root}, "status", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("controller calls %d", calls)
	}
}

func TestDotsInvalidInstallNeverExecutes(t *testing.T) {
	home := t.TempDir()
	runner := dotsTestRunner(home, func(context.Context, dots.Controller, dots.Request) (map[string]any, error) {
		t.Fatal("invalid install executed")
		return nil, nil
	})
	for _, options := range []CodexDotsCmd{
		{Root: filepath.Join(home, "dots"), Install: CodexDotsInstallCmd{Mode: "other", App: "/Applications/ChatGPT.app"}},
		{Root: "relative", Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}},
		{Root: filepath.Join(home, ".codex", "dots"), Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}},
		{Root: filepath.Join(home, "dots"), Install: CodexDotsInstallCmd{App: "relative"}},
		{Root: filepath.Join(home, "custom-auth", "dots"), Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app", SystemAuth: filepath.Join(home, "custom-auth", "auth.json")}},
	} {
		if err := runner.execute(context.Background(), &options, "install", false, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid install accepted: %#v", options)
		}
	}
	for _, root := range []string{home, filepath.Dir(home), filepath.Join(home, "Library", "Application Support", "com.openai.codex", "dots"), filepath.Join(home, "Library", "Application Support", "com.openai.chat", "dots"), filepath.Join(home, "Library", "Application Support", "OpenAI", "dots")} {
		if err := runner.execute(context.Background(), &CodexDotsCmd{Root: root, Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}}, "install", false, io.Discard, io.Discard); err == nil {
			t.Fatalf("protected root accepted: %s", root)
		}
	}
}

func TestDotsEntryHelpDoesNotInstallAndHiddenHelperIsIntercepted(t *testing.T) {
	var output strings.Builder
	handled, err := runCodexDotsEntry([]string{"--json", "codex", "dots", "--help"}, &output, io.Discard)
	if !handled || err != nil {
		t.Fatalf("handled %v error %v", handled, err)
	}
	if !strings.Contains(output.String(), "install") || strings.Contains(output.String(), "python") {
		t.Fatalf("unexpected Dots help: %s", output.String())
	}
	handled, err = runCodexDotsEntry([]string{"codex", "accounts"}, io.Discard, io.Discard)
	if handled || err != nil {
		t.Fatalf("intercepted unrelated command: %v %v", handled, err)
	}
	handled, err = runCodexDotsEntry([]string{"__dots", "native"}, io.Discard, io.Discard)
	if !handled || err == nil {
		t.Fatalf("malformed helper command handled=%v error=%v", handled, err)
	}
}

func TestDotsInvalidActionsNeverExecute(t *testing.T) {
	home := t.TempDir()
	runner := dotsTestRunner(home, func(context.Context, dots.Controller, dots.Request) (map[string]any, error) {
		t.Fatal("invalid action executed")
		return nil, nil
	})
	for _, test := range []struct {
		op      string
		options CodexDotsCmd
	}{
		{"prime", CodexDotsCmd{}},
		{"prime", CodexDotsCmd{Prime: CodexDotsPrimeCmd{Dot: "Primary", All: true}}},
		{"broadcast", CodexDotsCmd{Broadcast: CodexDotsBroadcastCmd{Message: ""}}},
		{"broadcast", CodexDotsCmd{Broadcast: CodexDotsBroadcastCmd{Message: " \n"}}},
		{"broadcast", CodexDotsCmd{Broadcast: CodexDotsBroadcastCmd{Message: strings.Repeat("x", 3001)}}},
		{"board", CodexDotsCmd{Board: CodexDotsBoardCmd{Thread: "https://example.com/thread"}}},
		{"board", CodexDotsCmd{Board: CodexDotsBoardCmd{Thread: "../thread"}}},
		{"board", CodexDotsCmd{Board: CodexDotsBoardCmd{Thread: "01a14c1f-6e3c-7622-b4a3-b279ecb2a822", Clear: true}}},
	} {
		test.options.Root = filepath.Join(home, "dots")
		if err := runner.execute(context.Background(), &test.options, test.op, false, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid %s accepted: %#v", test.op, test.options)
		}
	}
}

func TestDotsActionsForwardExactTargets(t *testing.T) {
	home := t.TempDir()
	message := "--json $(do-not-run) `literal`\nHello, team!"
	for _, test := range []struct {
		op      string
		options CodexDotsCmd
		want    dots.Request
	}{
		{"broadcast", CodexDotsCmd{Broadcast: CodexDotsBroadcastCmd{Message: message}}, dots.Request{Operation: "broadcast", Target: message}},
		{"broadcast", CodexDotsCmd{Broadcast: CodexDotsBroadcastCmd{Message: strings.Repeat("🐈", 3000)}}, dots.Request{Operation: "broadcast", Target: strings.Repeat("🐈", 3000)}},
		{"prime", CodexDotsCmd{Prime: CodexDotsPrimeCmd{Dot: "dot-0123456789abcdef01234567"}}, dots.Request{Operation: "prime", Target: "dot-0123456789abcdef01234567"}},
		{"prime", CodexDotsCmd{Prime: CodexDotsPrimeCmd{All: true}}, dots.Request{Operation: "prime", All: true}},
		{"board", CodexDotsCmd{Board: CodexDotsBoardCmd{Thread: "codex://threads/01a14c1f-6e3c-7622-b4a3-b279ecb2a822"}}, dots.Request{Operation: "board", Target: "01a14c1f-6e3c-7622-b4a3-b279ecb2a822"}},
		{"board", CodexDotsCmd{Board: CodexDotsBoardCmd{Clear: true}}, dots.Request{Operation: "board", Clear: true}},
	} {
		runner := dotsTestRunner(home, func(_ context.Context, _ dots.Controller, request dots.Request) (map[string]any, error) {
			request.Install = dots.InstallOptions{}
			if !reflect.DeepEqual(request, test.want) {
				t.Fatalf("request %#v, want %#v", request, test.want)
			}
			return nil, nil
		})
		test.options.Root = filepath.Join(home, "dots")
		if err := runner.execute(context.Background(), &test.options, test.op, true, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDotsControllerFailureRetainsStructuredResult(t *testing.T) {
	home := t.TempDir()
	failure := errors.New("synthetic controller failure")
	runner := dotsTestRunner(home, func(context.Context, dots.Controller, dots.Request) (map[string]any, error) {
		return map[string]any{"error": "synthetic failure"}, failure
	})
	var output strings.Builder
	if err := runner.execute(context.Background(), &CodexDotsCmd{}, "status", false, &output, io.Discard); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	var value map[string]string
	if err := json.Unmarshal([]byte(output.String()), &value); err != nil || value["error"] != "synthetic failure" {
		t.Fatalf("output %q error %v", output.String(), err)
	}
}

func TestDotsExecutablePreservesInstalledSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew symlink behaviour requires POSIX")
	}
	directory := t.TempDir()
	version := filepath.Join(directory, "cq-version")
	if err := os.WriteFile(version, []byte("synthetic"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "cq")
	if err := os.Symlink(version, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	got, err := dotsExecutable("cq")
	if err != nil {
		t.Fatal(err)
	}
	if got != link {
		t.Fatalf("executable %q, want installed symlink %q", got, link)
	}
}

func TestDotsPreparationFailureNeverExecutes(t *testing.T) {
	home := t.TempDir()
	failure := errors.New("synthetic preparation failure")
	for _, stage := range []string{"home", "executable", "mode"} {
		runner := dotsTestRunner(home, func(context.Context, dots.Controller, dots.Request) (map[string]any, error) {
			t.Fatal("controller ran after preparation failed")
			return nil, nil
		})
		operation := "status"
		if stage == "home" {
			runner.home = func() (string, error) { return "", failure }
		}
		if stage == "executable" {
			runner.executable = func() (string, error) { return "", failure }
		}
		if stage == "mode" {
			operation = "start"
		}
		err := runner.execute(context.Background(), &CodexDotsCmd{}, operation, false, io.Discard, io.Discard)
		if err == nil || stage != "mode" && !errors.Is(err, failure) {
			t.Fatalf("stage %s error %v", stage, err)
		}
	}
}
