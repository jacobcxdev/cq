package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/kong"
	dotspayload "github.com/jacobcxdev/cq/experiments/dots"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// CodexDotsCmd manages the Dots adapter and its private state.
type CodexDotsCmd struct {
	Root      string                `help:"Private Dots installation directory (default ~/.local/share/cq/dots)" placeholder:"ABS_PATH"`
	Install   CodexDotsInstallCmd   `cmd:"" help:"Install multi-account Dots for the normal native app"`
	Start     CodexDotsActionCmd    `cmd:"" help:"Start Dots in the configured native app"`
	Stop      CodexDotsActionCmd    `cmd:"" help:"Stop the Dots adapter after quitting the app"`
	Status    CodexDotsActionCmd    `cmd:"" help:"Show Dots status"`
	Sync      CodexDotsActionCmd    `cmd:"" help:"Refresh Dots account discovery"`
	List      CodexDotsActionCmd    `cmd:"" help:"List discovered Dots"`
	Prime     CodexDotsPrimeCmd     `cmd:"" help:"Prime a Dot or all discovered Dots"`
	Broadcast CodexDotsBroadcastCmd `cmd:"" help:"Send a user message to every discovered Dot"`
	Board     CodexDotsBoardCmd     `cmd:"" help:"Show or assign one local chat as the shared Dot message board"`
}

type CodexDotsInstallCmd struct {
	Mode          string `help:"Use normal native profile or isolated research profile" default:"native" enum:"native,isolated"`
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
type CodexDotsBroadcastCmd struct {
	Message string `arg:"" help:"Message to send once to every discovered Dot (1 to 3000 characters)"`
}
type CodexDotsBoardCmd struct {
	Thread string `arg:"" optional:"" help:"Existing local thread ID or codex://threads/ID link"`
	Clear  bool   `help:"Remove the shared message board binding"`
}

var dotsBoardThread = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

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
			baseURL, cqPath := "", ""
			native, err := dotsNativeLaunch(args)
			if err != nil {
				return err
			}
			if native {
				cfg, err := proxy.LoadExistingConfig()
				if err != nil {
					return fmt.Errorf("CQ proxy configuration unavailable: %w", err)
				}
				baseURL = "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
				cqPath, err = exec.LookPath(os.Args[0])
				if err != nil {
					return fmt.Errorf("CQ launcher executable unavailable: %w", err)
				}
				cqPath, err = filepath.Abs(cqPath)
				if err != nil {
					return err
				}
			}
			command.Env = dotsProcessEnvironment(os.Environ(), baseURL, cqPath)
			if err := command.Run(); err != nil {
				return fmt.Errorf("Dots controller failed: %w", err)
			}
			return nil
		}}
}

func dotsNativeLaunch(args []string) (bool, error) {
	if len(args) < 2 {
		return false, nil
	}
	switch args[1] {
	case "install":
		for index := 2; index+1 < len(args); index++ {
			if args[index] == "--mode" {
				return args[index+1] == "native", nil
			}
		}
		return true, nil
	case "start", "sync":
		for index := 2; index+1 < len(args); index++ {
			if args[index] != "--root" {
				continue
			}
			file, err := os.Open(filepath.Join(args[index+1], "settings.json"))
			if err != nil {
				return false, err
			}
			defer file.Close()
			var settings struct {
				Mode string `json:"integration_mode"`
			}
			if err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&settings); err != nil {
				return false, fmt.Errorf("invalid Dots integration mode: %w", err)
			}
			return settings.Mode == "native", nil
		}
		return false, fmt.Errorf("Dots launch requires an installation root")
	default:
		return false, nil
	}
}

func dotsProcessEnvironment(inherited []string, baseURL, cqPath string) []string {
	environment := make([]string, 0, len(inherited)+3)
	for _, value := range inherited {
		key, _, _ := strings.Cut(value, "=")
		if key == "PYTHONPATH" || key == "PYTHONHOME" || key == "CQ_DOTS_CONFIG" || key == "PYTHONDONTWRITEBYTECODE" || key == "CQ_DOTS_CQ_PATH" || baseURL != "" && key == "OPENAI_BASE_URL" {
			continue
		}
		environment = append(environment, value)
	}
	environment = append(environment, "PYTHONDONTWRITEBYTECODE=1")
	if baseURL != "" {
		environment = append(environment, "OPENAI_BASE_URL="+baseURL, "CQ_DOTS_CQ_PATH="+cqPath)
	}
	return environment
}

func runCodexDotsEntry(args []string, stdout, stderr io.Writer) (bool, error) {
	filtered := make([]string, 0, len(args))
	jsonOutput := false
	positional := false
	for _, arg := range args {
		if positional {
			filtered = append(filtered, arg)
			continue
		}
		if arg == "--" {
			positional = true
			filtered = append(filtered, arg)
			continue
		}
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
	for index, arg := range filtered[2:] {
		if arg == "--" {
			break
		}
		if arg == "--help" || arg == "-h" || index == 0 && arg == "help" {
			return true, nil
		}
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
	if op == "broadcast" && (strings.TrimSpace(options.Broadcast.Message) == "" || utf8.RuneCountInString(options.Broadcast.Message) > 3000) {
		return fmt.Errorf("broadcast message must contain 1 to 3000 characters")
	}
	if op == "board" {
		if options.Board.Clear && options.Board.Thread != "" {
			return fmt.Errorf("choose a board thread or --clear")
		}
		if options.Board.Thread != "" && !dotsBoardThread.MatchString(strings.TrimPrefix(options.Board.Thread, "codex://threads/")) {
			return fmt.Errorf("board must be an exact local thread ID or codex://threads/ID link")
		}
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
	if op == "broadcast" {
		args = append(args, "--", options.Broadcast.Message)
	}
	if op == "board" {
		if options.Board.Thread != "" {
			args = append(args, options.Board.Thread)
		}
		if options.Board.Clear {
			args = append(args, "--clear")
		}
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
