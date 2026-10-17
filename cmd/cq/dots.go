package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alecthomas/kong"
	dotspayload "github.com/jacobcxdev/cq/experiments/dots"
)

// CodexDotsCmd manages the Dots adapter and its private state.
type CodexDotsCmd struct {
	Root       string              `help:"Private Dots installation directory (default ~/.local/share/cq/dots)" placeholder:"ABS_PATH"`
	Install    CodexDotsInstallCmd `cmd:"" help:"Install multi-account Dots for the normal native app"`
	Start      CodexDotsActionCmd  `cmd:"" help:"Start Dots in the configured native app"`
	Stop       CodexDotsActionCmd  `cmd:"" help:"Stop the Dots adapter after quitting the app"`
	Status     CodexDotsActionCmd  `cmd:"" help:"Show Dots status"`
	Sync       CodexDotsActionCmd  `cmd:"" help:"Refresh Dots account discovery"`
	List       CodexDotsActionCmd  `cmd:"" help:"List discovered Dots"`
	Prime      CodexDotsPrimeCmd   `cmd:"" help:"Prime a Dot or all discovered Dots"`
	RestoreApp CodexDotsActionCmd  `cmd:"" help:"Restore the original app signature after quitting the app"`
}

type CodexDotsInstallCmd struct {
	Mode          string `help:"Patch and locally re-sign normal app, or use isolated research profile" default:"native" enum:"native,isolated"`
	App           string `help:"Native app bundle" default:"/Applications/ChatGPT.app" placeholder:"ABS_PATH"`
	SystemAuth    string `help:"Read-only native Codex authentication path" placeholder:"ABS_PATH"`
	CQAccounts    string `help:"Read-only CQ Codex accounts directory" placeholder:"ABS_PATH"`
	AccountSource string `help:"Optional read-only CodexBar account directory" placeholder:"ABS_PATH"`
	Python        string `help:"Python 3.14 interpreter (default python3.14)" placeholder:"ABS_PATH"`
}
type CodexDotsActionCmd struct{}
type CodexDotsPrimeCmd struct {
	Dot string `arg:"" optional:"" help:"Stable Dot handle or exact unambiguous display name"`
	All bool   `help:"Prime all discovered Dots"`
}

type dotsRunner struct {
	home    func() (string, error)
	extract func(string) error
	run     func(context.Context, string, []string, io.Writer, io.Writer) error
}

func defaultDotsRunner() dotsRunner {
	return dotsRunner{home: os.UserHomeDir, extract: dotspayload.Extract,
		run: func(ctx context.Context, python string, args []string, stdout, stderr io.Writer) error {
			command := exec.CommandContext(ctx, python, args...)
			command.Stdout, command.Stderr = stdout, stderr
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "PYTHONPATH=") && !strings.HasPrefix(value, "PYTHONHOME=") && !strings.HasPrefix(value, "CQ_DOTS_CONFIG=") && !strings.HasPrefix(value, "PYTHONDONTWRITEBYTECODE=") {
					command.Env = append(command.Env, value)
				}
			}
			command.Env = append(command.Env, "PYTHONDONTWRITEBYTECODE=1")
			if err := command.Run(); err != nil {
				return fmt.Errorf("Dots controller failed: %w", err)
			}
			return nil
		}}
}

func runCodexDotsEntry(args []string, stdout, stderr io.Writer) (bool, error) {
	filtered := make([]string, 0, len(args))
	jsonOutput := false
	for _, arg := range args {
		if arg == "--json" || arg == "-j" {
			jsonOutput = true
		} else if strings.HasPrefix(arg, "--json=") || strings.HasPrefix(arg, "-j=") {
			value := strings.SplitN(arg, "=", 2)[1]
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				filtered = append(filtered, arg)
			} else {
				jsonOutput = parsed
			}
		} else {
			filtered = append(filtered, arg)
		}
	}
	if len(filtered) < 2 || filtered[0] != "codex" || filtered[1] != "dots" {
		return false, nil
	}
	var options struct{ CodexDotsCmd }
	parser, err := kong.New(&options, kong.Name("cq codex dots"), kong.Writers(stdout, stderr), kong.Exit(func(int) {}))
	if err != nil {
		return true, err
	}
	parsed, err := parser.Parse(filtered[2:])
	if helpRequested(filtered[2:]) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	return true, defaultDotsRunner().execute(context.Background(), &options.CodexDotsCmd, parsed.Command(), jsonOutput, stdout, stderr)
}

func (runner dotsRunner) execute(ctx context.Context, options *CodexDotsCmd, command string, jsonOutput bool, stdout, stderr io.Writer) error {
	home, err := runner.home()
	if err != nil {
		return err
	}
	root := options.Root
	if root == "" {
		root = filepath.Join(home, ".local", "share", "cq", "dots")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("Dots root must be a clean absolute path")
	}
	if dotsContains(canonicalDotsPath(root), canonicalDotsPath(home)) {
		return fmt.Errorf("Dots root must not contain the home directory")
	}
	protectedState := []string{filepath.Join(home, ".codex"), "/Applications/ChatGPT.app"}
	for _, name := range []string{"Codex", "ChatGPT", "com.openai.codex", "com.openai.chat", "OpenAI"} {
		protectedState = append(protectedState, filepath.Join(home, "Library", "Application Support", name))
	}
	for _, path := range protectedState {
		if overlapsDotsRoot(root, path) {
			return fmt.Errorf("Dots root overlaps an app or credential path")
		}
	}
	operation := strings.Fields(command)
	if len(operation) == 0 {
		return fmt.Errorf("missing Dots command")
	}
	op := operation[0]
	if op == "prime" && (options.Prime.All == (options.Prime.Dot != "")) {
		return fmt.Errorf("choose a stable Dot handle, an unambiguous name, or --all")
	}
	args := []string{filepath.Join(root, "payload", "cli.py"), op, "--root", root}
	python := filepath.Join(root, "runtime", "venv", "bin", "python")
	if op == "install" {
		install := options.Install
		if install.Mode == "" {
			install.Mode = "native"
		}
		if install.Mode != "native" && install.Mode != "isolated" {
			return fmt.Errorf("Dots mode must be native or isolated")
		}
		if install.SystemAuth == "" {
			install.SystemAuth = filepath.Join(home, ".codex", "auth.json")
		}
		if install.CQAccounts == "" {
			install.CQAccounts = filepath.Join(home, ".codex", "accounts")
		}
		inputs := []string{install.App, install.SystemAuth, install.CQAccounts}
		if install.AccountSource != "" {
			inputs = append(inputs, install.AccountSource)
		}
		for _, path := range inputs {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return fmt.Errorf("Dots install paths must be clean and absolute")
			}
		}
		protected := append(inputs, filepath.Join(home, ".codex"), filepath.Dir(install.SystemAuth))
		protected = append(protected, filepath.Join(home, "Library", "Application Support", "Codex"), filepath.Join(home, "Library", "Application Support", "ChatGPT"))
		for _, path := range protected {
			if overlapsDotsRoot(root, path) {
				return fmt.Errorf("Dots root overlaps an app or credential path")
			}
		}
		python = install.Python
		if python == "" {
			python = "python3.14"
		} else if !filepath.IsAbs(python) {
			return fmt.Errorf("Dots Python path must be absolute")
		}
		args = append(args, "--mode", install.Mode, "--app", install.App, "--system-auth", install.SystemAuth, "--cq-accounts", install.CQAccounts)
		if install.AccountSource != "" {
			args = append(args, "--account-source", install.AccountSource)
		}
		if _, err := os.Lstat(filepath.Join(root, "settings.json")); err == nil {
			if err := dotspayload.ValidateInstallation(root); err != nil {
				return err
			}
			if err := runner.run(ctx, filepath.Join(root, "runtime", "venv", "bin", "python"),
				[]string{filepath.Join(root, "payload", "cli.py"), "preinstall", "--root", root}, io.Discard, stderr); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := runner.extract(root); err != nil {
			return err
		}
	} else {
		if err := dotspayload.ValidateInstallation(root); err != nil {
			return fmt.Errorf("Dots installation unavailable: %w", err)
		}
	}

	if op == "prime" {
		if options.Prime.All && options.Prime.Dot != "" {
			return fmt.Errorf("choose a Dot or --all")
		}
		if options.Prime.Dot != "" {
			args = append(args, options.Prime.Dot)
		}
		if options.Prime.All {
			args = append(args, "--all")
		}
	}
	if jsonOutput {
		args = append(args, "--json")
	}
	return runner.run(ctx, python, args, stdout, stderr)
}

func canonicalDotsPath(value string) string {
	for current := value; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			suffix, _ := filepath.Rel(current, value)
			return filepath.Join(resolved, suffix)
		}
		if filepath.Dir(current) == current {
			return value
		}
	}
}

func dotsContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func overlapsDotsRoot(root, path string) bool {
	root, path = canonicalDotsPath(root), canonicalDotsPath(path)
	return dotsContains(root, path) || dotsContains(path, root)
}
