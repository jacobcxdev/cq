package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

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
	home := t.TempDir()
	root := filepath.Join(home, "private dots")
	var program string
	var arguments []string
	var extracted string
	runner := dotsRunner{home: func() (string, error) { return home, nil }, extract: func(path string) error { extracted = path; return nil }, run: func(_ context.Context, p string, args []string, _, _ io.Writer) error {
		program = p
		arguments = args
		return nil
	}}
	options := CodexDotsCmd{Root: root, Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app", Python: "/custom/python3.14", AccountSource: "/private/account store"}}
	if err := runner.execute(context.Background(), &options, "install", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "payload", "cli.py"), "install", "--root", root, "--app", "/Applications/ChatGPT.app", "--system-auth", filepath.Join(home, ".codex", "auth.json"), "--cq-accounts", filepath.Join(home, ".codex", "accounts"), "--account-source", "/private/account store", "--json"}
	if program != "/custom/python3.14" || extracted != root || !reflect.DeepEqual(arguments, want) {
		t.Fatalf("program %q extract %q args %#v", program, extracted, arguments)
	}
}

func TestDotsStatusUsesInstalledInterpreterWithoutExtraction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Dots requires POSIX private file modes")
	}
	home := t.TempDir()
	root := filepath.Join(home, "dots")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"payload/runtime", "runtime/venv/bin"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "payload", "cli.py"), []byte("controller"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		mode os.FileMode
	}{{"settings.json", 0o600}, {"runtime/venv/bin/python", 0o700}} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file.name)), []byte("fixture"), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	var program string
	var arguments []string
	calls := 0
	runner := dotsRunner{home: func() (string, error) { return home, nil }, extract: func(string) error { t.Fatal("status extracted payload"); return nil }, run: func(_ context.Context, p string, args []string, _, _ io.Writer) error {
		calls++
		program = p
		arguments = args
		return nil
	}}
	if err := runner.execute(context.Background(), &CodexDotsCmd{Root: root}, "status", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || program != filepath.Join(root, "runtime", "venv", "bin", "python") {
		t.Fatalf("wrong interpreter %q calls %d", program, calls)
	}
	if !reflect.DeepEqual(arguments, []string{filepath.Join(root, "payload", "cli.py"), "status", "--root", root, "--json"}) {
		t.Fatal(arguments)
	}
}

func TestDotsInvalidInstallNeverExtractsOrExecutes(t *testing.T) {
	home := t.TempDir()
	calls := 0
	runner := dotsRunner{home: func() (string, error) { return home, nil }, extract: func(string) error { calls++; return nil }, run: func(context.Context, string, []string, io.Writer, io.Writer) error { calls++; return nil }}
	for _, options := range []CodexDotsCmd{
		{Root: "relative", Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}},
		{Root: filepath.Join(home, ".codex", "dots"), Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}},
		{Root: filepath.Join(home, "dots"), Install: CodexDotsInstallCmd{App: "relative"}},
		{Root: filepath.Join(home, "dots"), Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app", Python: "python"}},
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
	if calls != 0 {
		t.Fatal("invalid install performed work")
	}
}

func TestDotsExtractionFailureNeverExecutes(t *testing.T) {
	home := t.TempDir()
	failure := errors.New("extract failed")
	runner := dotsRunner{home: func() (string, error) { return home, nil }, extract: func(string) error { return failure }, run: func(context.Context, string, []string, io.Writer, io.Writer) error {
		t.Fatal("executed after extraction failed")
		return nil
	}}
	err := runner.execute(context.Background(), &CodexDotsCmd{Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}}, "install", false, io.Discard, io.Discard)
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestDotsEntryHelpDoesNotInstall(t *testing.T) {
	var output strings.Builder
	handled, err := runCodexDotsEntry([]string{"--json", "codex", "dots", "--help"}, &output, io.Discard)
	if !handled || err != nil {
		t.Fatalf("handled %v error %v", handled, err)
	}
	if !strings.Contains(output.String(), "install") {
		t.Fatalf("missing Dots help: %s", output.String())
	}
	handled, err = runCodexDotsEntry([]string{"codex", "accounts"}, io.Discard, io.Discard)
	if handled || err != nil {
		t.Fatalf("intercepted unrelated command: %v %v", handled, err)
	}
}

func TestDotsPrimeRequiresExactlyOneTargetBeforeExecuting(t *testing.T) {
	home := t.TempDir()
	runner := dotsRunner{home: func() (string, error) { return home, nil }, run: func(context.Context, string, []string, io.Writer, io.Writer) error {
		t.Fatal("executed invalid prime")
		return nil
	}}
	for _, prime := range []CodexDotsPrimeCmd{{}, {Dot: "Primary", All: true}} {
		if err := runner.execute(context.Background(), &CodexDotsCmd{Root: filepath.Join(home, "dots"), Prime: prime}, "prime", false, io.Discard, io.Discard); err == nil {
			t.Fatal("invalid prime accepted")
		}
	}
}

func TestDotsReinstallChecksExistingRuntimeBeforeExtraction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Dots requires POSIX private modes")
	}
	home := t.TempDir()
	root := filepath.Join(home, "dots")
	for _, name := range []string{"payload/runtime", "runtime/venv/bin"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []struct {
		name string
		mode os.FileMode
	}{{"settings.json", 0o600}, {"payload/cli.py", 0o600}, {"runtime/venv/bin/python", 0o700}} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file.name)), []byte("fixture"), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	stopped := errors.New("live isolated processes")
	extracted := false
	calls := 0
	runner := dotsRunner{home: func() (string, error) { return home, nil }, extract: func(string) error { extracted = true; return nil }, run: func(_ context.Context, python string, args []string, stdout, stderr io.Writer) error {
		calls++
		if python != filepath.Join(root, "runtime/venv/bin/python") || !reflect.DeepEqual(args, []string{filepath.Join(root, "payload/cli.py"), "preinstall", "--root", root}) {
			t.Fatalf("wrong preinstall command %s %#v", python, args)
		}
		if stdout != io.Discard {
			t.Fatal("preinstall output was not suppressed")
		}
		return stopped
	}}
	options := CodexDotsCmd{Root: root, Install: CodexDotsInstallCmd{App: "/Applications/ChatGPT.app"}}
	if err := runner.execute(context.Background(), &options, "install", false, io.Discard, io.Discard); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
	if extracted || calls != 1 {
		t.Fatalf("live reinstall extracted=%v calls=%d", extracted, calls)
	}
	if err := os.Chmod(filepath.Join(root, "settings.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runner.execute(context.Background(), &options, "install", false, io.Discard, io.Discard); err == nil {
		t.Fatal("non-private config accepted")
	}
	if calls != 1 || extracted {
		t.Fatal("unsafe installation executed or extracted")
	}
}
