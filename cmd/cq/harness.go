package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// CodexHarnessCmd stores opt-in GLM harness profiles independently of Codex config.
type CodexHarnessCmd struct {
	Status  CodexHarnessStatusCmd `cmd:"" default:"1" help:"Show saved harness policy; preview selection with --model"`
	Enable  struct{}              `cmd:"" help:"Enable GLM minimal defaults with automatically detected profile support"`
	Disable struct{}              `cmd:"" help:"Disable policy while preserving project and chat preferences"`
	Project struct {
		Directory string `arg:"" help:"Project directory prefix (normalised to an absolute path)"`
		Profile   string `arg:"" enum:"minimal,full,auto" help:"Harness profile; auto removes this override and inherits"`
	} `cmd:"" help:"Save a project harness default without implicitly enabling policy"`
	Chat struct {
		Thread  string `arg:"" help:"Canonical lower-case chat UUID or codex://threads/ link"`
		Profile string `arg:"" enum:"minimal,full,auto" help:"Harness profile; auto removes this override and inherits"`
	} `cmd:"" help:"Save a chat harness override without implicitly enabling policy"`
}

type CodexHarnessStatusCmd struct {
	Model   string `help:"Exact model ID for a policy preview (for example glm-5.3); omitted means no model"`
	Project string `help:"Project directory for preview (normalised to an absolute path)"`
	Chat    string `help:"Chat UUID or codex://threads/ link for preview"`
}

const harnessApplicationNote = "CQ automatically detects profile support. With the CQ launcher, profiles control plugins, standalone MCP servers and local skills. With a standard ChatGPT launch, profiles control plugins only; standalone MCP servers and local skills stay unchanged. For standard launches, policy applies at the next eligible turn after CQ observes a GLM request; first-turn application is not guaranteed."

func (CodexHarnessCmd) Help() string       { return harnessApplicationNote }
func (CodexHarnessStatusCmd) Help() string { return harnessApplicationNote }

var harnessChatIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type codexHarnessRunner struct {
	fs      fsutil.FileSystem
	path    func() (string, error)
	receipt func(string) (harness.Receipt, error)
}

type codexHarnessStatus struct {
	Policy          harness.Policy     `json:"policy"`
	Selection       *harness.Selection `json:"selection,omitempty"`
	Application     string             `json:"application"`
	LastApplication *harness.Receipt   `json:"last_application,omitempty"`
}

func defaultCodexHarnessRunner() codexHarnessRunner {
	return codexHarnessRunner{
		fs: fsutil.OSFileSystem{},
		receipt: func(chatID string) (harness.Receipt, error) {
			roots, err := userdirs.Default()
			if err != nil {
				return harness.Receipt{}, err
			}
			return (harness.Receipts{FS: fsutil.OSFileSystem{}, Dir: filepath.Join(roots.State, "harness")}).Load(chatID)
		},
		path: func() (string, error) {
			roots, err := userdirs.Default()
			if err != nil {
				return "", err
			}
			return filepath.Join(roots.Config, "harness.json"), nil
		},
	}
}

func (runner codexHarnessRunner) execute(operation string, cmd CodexHarnessCmd, jsonOutput bool, output io.Writer) error {
	var update func(*harness.Policy) error
	var project, chat string
	switch operation {
	case "status":
		var err error
		if cmd.Status.Project != "" {
			project, err = normaliseHarnessProject(cmd.Status.Project)
			if err != nil {
				return err
			}
		}
		if cmd.Status.Chat != "" {
			chat, err = normaliseHarnessChat(cmd.Status.Chat)
			if err != nil {
				return err
			}
		}
	case "enable", "disable":
		update = func(policy *harness.Policy) error {
			policy.Enabled = operation == "enable"
			return nil
		}
	case "project <directory> <profile>":
		if err := validateHarnessProfile(cmd.Project.Profile); err != nil {
			return err
		}
		var err error
		project, err = normaliseHarnessProject(cmd.Project.Directory)
		if err != nil {
			return err
		}
		update = func(policy *harness.Policy) error {
			if cmd.Project.Profile == "auto" {
				delete(policy.Projects, project)
			} else {
				policy.Projects[project] = harness.Profile(cmd.Project.Profile)
			}
			return nil
		}
	case "chat <thread> <profile>":
		if err := validateHarnessProfile(cmd.Chat.Profile); err != nil {
			return err
		}
		var err error
		chat, err = normaliseHarnessChat(cmd.Chat.Thread)
		if err != nil {
			return err
		}
		update = func(policy *harness.Policy) error {
			if cmd.Chat.Profile == "auto" {
				delete(policy.Chats, chat)
			} else {
				policy.Chats[chat] = harness.Profile(cmd.Chat.Profile)
			}
			return nil
		}
	default:
		return fmt.Errorf("unknown harness operation: %s", operation)
	}
	path, err := runner.path()
	if err != nil {
		return err
	}
	var policy harness.Policy
	if update == nil {
		policy, err = harness.Load(runner.fs, path)
	} else {
		policy, err = harness.Update(runner.fs, path, update)
	}
	if err != nil {
		return err
	}
	status := codexHarnessStatus{Policy: policy, Application: harnessApplicationNote}
	if operation == "status" && chat != "" && runner.receipt != nil {
		receipt, readErr := runner.receipt(chat)
		if readErr == nil {
			status.LastApplication = &receipt
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	}
	if operation == "status" && (cmd.Status.Model != "" || project != "" || chat != "") {
		selection := policy.Resolve(cmd.Status.Model, project, chat)
		status.Selection = &selection
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(status)
	}
	var text strings.Builder
	if update != nil {
		text.WriteString("Harness policy saved.\n")
	}
	fmt.Fprintf(&text, "Codex GLM harness policy\n  enabled: %t\n  GLM default when enabled: minimal\n  other models: unmanaged\n", policy.Enabled)
	for _, project := range slices.Sorted(maps.Keys(policy.Projects)) {
		fmt.Fprintf(&text, "  project %s: %s\n", project, policy.Projects[project])
	}
	for _, chat := range slices.Sorted(maps.Keys(policy.Chats)) {
		fmt.Fprintf(&text, "  chat %s: %s\n", chat, policy.Chats[chat])
	}
	if status.Selection != nil {
		fmt.Fprintf(&text, "  preview: %s (managed: %t, source: %s", status.Selection.Profile, status.Selection.Managed, status.Selection.Source)
		if status.Selection.Scope != "" {
			fmt.Fprintf(&text, ", scope: %s", status.Selection.Scope)
		}
		text.WriteString(")\n")
	}
	if receipt := status.LastApplication; receipt != nil {
		fmt.Fprintf(&text, "  last application: %s (%s, state: %s, source: %s, recorded: %s)\n", receipt.Profile, receipt.Mode, receipt.State, receipt.Source, receipt.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	fmt.Fprintf(&text, "\n%s\n", status.Application)
	_, err = io.WriteString(output, text.String())
	return err
}

func validateHarnessProfile(profile string) error {
	if profile != string(harness.Minimal) && profile != string(harness.Full) && profile != "auto" {
		return fmt.Errorf("harness profile must be minimal, full, or auto")
	}
	return nil
}

func normaliseHarnessProject(directory string) (string, error) {
	if directory == "" || strings.ContainsRune(directory, '\x00') {
		return "", fmt.Errorf("harness project directory is empty or invalid")
	}
	path, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve harness project directory: %w", err)
	}
	return filepath.Clean(path), nil
}

func normaliseHarnessChat(thread string) (string, error) {
	id := strings.TrimPrefix(thread, "codex://threads/")
	if !harnessChatIDPattern.MatchString(id) {
		return "", fmt.Errorf("harness chat ID must be a canonical lower-case UUID or codex://threads/ link")
	}
	return id, nil
}

// Harness commands only read or update their private policy file. They must not
// initialise compatibility state as a side effect of a policy inspection.
func isCodexHarnessCommand(args []string) bool {
	var command []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		command = append(command, arg)
		if len(command) == 2 {
			return command[0] == "codex" && command[1] == "harness"
		}
	}
	return false
}
