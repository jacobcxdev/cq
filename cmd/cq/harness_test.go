package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCodexHarnessCommandParsing(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"codex", "harness"}, "codex harness status"},
		{[]string{"codex", "harness", "status", "--model", "glm-5.3", "--project", ".", "--chat", "01a198eb-f8e9-72a1-856c-0e986d10f6f7"}, "codex harness status"},
		{[]string{"--json", "codex", "harness", "enable"}, "codex harness enable"},
		{[]string{"codex", "harness", "disable", "--json"}, "codex harness disable"},
		{[]string{"codex", "harness", "project", ".", "minimal"}, "codex harness project <directory> <profile>"},
		{[]string{"codex", "harness", "project", ".", "full"}, "codex harness project <directory> <profile>"},
		{[]string{"codex", "harness", "project", ".", "auto"}, "codex harness project <directory> <profile>"},
		{[]string{"codex", "harness", "chat", "codex://threads/01a198eb-f8e9-72a1-856c-0e986d10f6f7", "auto"}, "codex harness chat <thread> <profile>"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			_, parsed, err := parseCodexResetCLI(t, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Command() != test.want {
				t.Fatalf("command = %q, want %q", parsed.Command(), test.want)
			}
		})
	}
}

func TestCodexHarnessHelpUsesPureInspection(t *testing.T) {
	for _, operation := range []string{"", "status", "enable", "disable", "project", "chat"} {
		args := []string{"codex", "harness"}
		if operation != "" {
			args = append(args, operation)
		}
		args = append(args, "--help")
		var output bytes.Buffer
		handled, code, err := runPureGlobalInspection(args, &output, io.Discard)
		if !handled || code != 0 || err != nil || !strings.Contains(output.String(), "Usage: cq codex harness") {
			t.Fatalf("help %v = %t, %d, %v: %s", args, handled, code, err, &output)
		}
		if operation == "" || operation == "status" {
			rendered := strings.Join(strings.Fields(output.String()), " ")
			for _, detail := range []string{"automatically", "CQ launcher", "standard ChatGPT launch", "standalone MCP", "local skills", "For standard launches", "not guaranteed", "observes a GLM request"} {
				if !strings.Contains(rendered, detail) {
					t.Fatalf("help %v omitted %q: %s", args, detail, &output)
				}
			}
		}
	}
}

func harnessTestRunner() (codexHarnessRunner, *fsutil.MemFS) {
	fsys := fsutil.NewMemFS()
	return codexHarnessRunner{fs: fsys, path: func() (string, error) { return "/cq/harness.json", nil }}, fsys
}

func runHarnessTestCLI(t *testing.T, runner codexHarnessRunner, output io.Writer, args ...string) error {
	t.Helper()
	cli, parsed, err := parseCodexResetCLI(t, append([]string{"codex", "harness"}, args...)...)
	if err != nil {
		return err
	}
	return runner.execute(strings.TrimPrefix(parsed.Command(), "codex harness "), cli.Codex.Harness, cli.JSON, output)
}

func TestCodexHarnessStatusMissingPolicyIsReadOnly(t *testing.T) {
	runner, fsys := harnessTestRunner()
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "--json"); err != nil {
		t.Fatal(err)
	}
	var status codexHarnessStatus
	decoder := json.NewDecoder(&output)
	if err := decoder.Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Policy.Enabled || status.Policy.Version != 1 || status.Selection != nil {
		t.Fatalf("missing policy = %#v", status)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("JSON contains extra output: %v", err)
	}
	if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created config directory: %v", err)
	}
}

func TestCodexHarnessSavedPreferencesAndInheritance(t *testing.T) {
	runner, fsys := harnessTestRunner()
	chat := "01a198eb-f8e9-72a1-856c-0e986d10f6f7"
	otherChat := "01a198eb-f8e9-72a1-856c-0e986d10f6f8"
	project := filepath.Join(t.TempDir(), "project")
	for _, args := range [][]string{{"project", filepath.Join(project, "sub", ".."), "full"}, {"chat", "codex://threads/" + chat, "minimal"}, {"chat", otherChat, "full"}} {
		if err := runHarnessTestCLI(t, runner, io.Discard, args...); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || policy.Enabled || policy.Projects[project] != harness.Full || policy.Chats[chat] != harness.Minimal || policy.Chats[otherChat] != harness.Full {
		t.Fatalf("saved overrides = %#v, %v", policy, err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "enable"); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "chat", chat, "auto"); err != nil {
		t.Fatal(err)
	}
	policy, _ = harness.Load(fsys, "/cq/harness.json")
	if _, exists := policy.Chats[chat]; exists || policy.Resolve("glm-5.3", project, chat).Profile != harness.Full {
		t.Fatalf("chat auto did not inherit project: %#v", policy)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "project", project, "auto"); err != nil {
		t.Fatal(err)
	}
	policy, _ = harness.Load(fsys, "/cq/harness.json")
	if len(policy.Projects) != 0 || policy.Resolve("glm-5.3", project, chat).Profile != harness.Minimal || policy.Chats[otherChat] != harness.Full {
		t.Fatalf("project auto did not inherit GLM default: %#v", policy)
	}
	for _, operation := range []string{"disable", "enable"} {
		if err := runHarnessTestCLI(t, runner, io.Discard, operation); err != nil {
			t.Fatal(err)
		}
		policy, _ = harness.Load(fsys, "/cq/harness.json")
		if policy.Enabled != (operation == "enable") || policy.Chats[otherChat] != harness.Full {
			t.Fatalf("%s discarded preferences: %#v", operation, policy)
		}
	}
}

func TestCodexHarnessStatusPreviewAndSavedJSON(t *testing.T) {
	runner, fsys := harnessTestRunner()
	chat := "01a198eb-f8e9-72a1-856c-0e986d10f6f7"
	project, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"project", ".", "full"}, {"chat", chat, "minimal"}, {"enable"}} {
		if err := runHarnessTestCLI(t, runner, io.Discard, args...); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := fsys.ReadFile("/cq/harness.json")
	for _, test := range []struct {
		model string
		want  harness.Selection
	}{
		{"glm-5.3", harness.Selection{Managed: true, Profile: harness.Minimal, Source: "chat", Scope: chat}},
		{"gpt-6.1", harness.Selection{Profile: harness.Full, Source: "model"}},
	} {
		var output bytes.Buffer
		if err := runHarnessTestCLI(t, runner, &output, "status", "--json", "--model", test.model, "--project", ".", "--chat", "codex://threads/"+chat); err != nil {
			t.Fatal(err)
		}
		var status codexHarnessStatus
		if err := json.Unmarshal(output.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.Selection == nil || *status.Selection != test.want || !status.Policy.Enabled || status.Policy.Projects[project] != harness.Full || status.Policy.Chats[chat] != harness.Minimal {
			t.Fatalf("preview = %#v", status)
		}
		if !strings.Contains(status.Application, "automatically") || !strings.Contains(status.Application, "CQ launcher") || !strings.Contains(status.Application, "standard ChatGPT launch") || !strings.Contains(status.Application, "For standard launches") || !strings.Contains(status.Application, "not guaranteed") {
			t.Fatalf("status omitted scope or application limits: %s", &output)
		}
	}
	after, _ := fsys.ReadFile("/cq/harness.json")
	if !bytes.Equal(before, after) {
		t.Fatal("preview changed saved policy")
	}
}

func TestCodexHarnessInvalidInputDoesNotCreateConfig(t *testing.T) {
	chat := "01a198eb-f8e9-72a1-856c-0e986d10f6f7"
	for _, args := range [][]string{
		{"chat", "not-a-chat", "auto"},
		{"chat", strings.ToUpper(chat), "full"},
		{"chat", "codex://threads/" + chat + "/extra", "minimal"},
		{"chat", chat, "invalid"},
		{"project", ".", "invalid"},
		{"project", "", "auto"},
		{"project", "bad\x00path", "auto"},
		{"status", "--chat", "not-a-chat"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			runner, fsys := harnessTestRunner()
			var output bytes.Buffer
			if err := runHarnessTestCLI(t, runner, &output, args...); err == nil {
				t.Fatal("invalid input was accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("invalid input emitted success output: %s", &output)
			}
			if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid input created config directory: %v", err)
			}
		})
	}
}

func TestCodexHarnessFailedWritePreservesSavedPolicy(t *testing.T) {
	runner, fsys := harnessTestRunner()
	if err := runHarnessTestCLI(t, runner, io.Discard, "enable"); err != nil {
		t.Fatal(err)
	}
	before, _ := fsys.ReadFile("/cq/harness.json")
	runner.fs = harnessRenameFailureFS{fsys}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "disable"); err == nil || !strings.Contains(err.Error(), "test rename failed") {
		t.Fatalf("failed write = %v", err)
	}
	after, _ := fsys.ReadFile("/cq/harness.json")
	if !bytes.Equal(before, after) || output.Len() != 0 {
		t.Fatalf("failed write changed policy or emitted success: %s", &output)
	}
}

type harnessRenameFailureFS struct{ *fsutil.MemFS }

func (fsys harnessRenameFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return harnessRenameFailureDirectory{directory, directory.(fsutil.IdentityBoundRenamer), directory.(fsutil.IdentityBoundRemover)}, nil
}

type harnessRenameFailureDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
}

func (harnessRenameFailureDirectory) RenameChecked(string, string, fsutil.SecureFileIdentity) error {
	return errors.New("test rename failed")
}

func TestCodexHarnessOnlyChangesPrivatePolicy(t *testing.T) {
	runner, fsys := harnessTestRunner()
	untouched := map[string][]byte{
		"/codex/config.toml":      []byte("native configuration"),
		"/codex/plugins.json":     []byte("global plugin preferences"),
		"/codex/history.jsonl":    []byte("existing chat history"),
		"/codex/auth.json":        []byte("source account credentials"),
		"/cq/codex-accounts.json": []byte("CQ accounts"),
	}
	for path, data := range untouched {
		if err := fsys.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range []string{"enable", "disable", "status"} {
		var output bytes.Buffer
		if err := runHarnessTestCLI(t, runner, &output, operation); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "standalone MCP") || !strings.Contains(output.String(), "local skills") || !strings.Contains(output.String(), "next eligible turn") {
			t.Fatalf("%s scope or timing is unclear: %s", operation, &output)
		}
	}
	for path, want := range untouched {
		got, err := fsys.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("changed unrelated file %s: %v", path, err)
		}
	}
	info, err := fsys.Stat("/cq/harness.json")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("policy permissions = %v, %v", info, err)
	}
}

func TestCodexHarnessMainStatusBypassesCompatibilityWrites(t *testing.T) {
	if os.Getenv("CQ_HARNESS_STATUS_HELPER") == "1" {
		os.Args = []string{"cq", "--json", "codex", "harness", "status"}
		main()
		os.Exit(0)
	}
	root := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestCodexHarnessMainStatusBypassesCompatibilityWrites$")
	command.Env = append(os.Environ(), "CQ_HARNESS_STATUS_HELPER=1", "XDG_CONFIG_HOME="+root, "HOME="+root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("status subprocess: %v: %s", err, output)
	}
	var status codexHarnessStatus
	if err := json.Unmarshal(output, &status); err != nil || status.Policy.Enabled {
		t.Fatalf("subprocess status = %s, %v", output, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("status changed config root: %v, %v", entries, err)
	}
}

func TestCodexHarnessHumanStatusSortsOverrides(t *testing.T) {
	runner, _ := harnessTestRunner()
	for _, args := range [][]string{{"project", "/z-project", "minimal"}, {"project", "/a-project", "full"}} {
		if err := runHarnessTestCLI(t, runner, io.Discard, args...); err != nil {
			t.Fatal(err)
		}
	}
	var first, second bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &first); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessTestCLI(t, runner, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) || strings.Index(first.String(), "/a-project") < 0 || strings.Index(first.String(), "/a-project") > strings.Index(first.String(), "/z-project") {
		t.Fatalf("unordered output: %s", &first)
	}
}

func TestCodexHarnessRunnerValidatesBeforeResolvingConfig(t *testing.T) {
	for _, operation := range []string{"project <directory> <profile>", "chat <thread> <profile>", "unknown"} {
		t.Run(operation, func(t *testing.T) {
			runner, fsys := harnessTestRunner()
			runner.path = func() (string, error) {
				t.Fatal("invalid command reached config resolution")
				return "", nil
			}
			cmd := CodexHarnessCmd{}
			cmd.Project.Directory, cmd.Project.Profile = ".", "invalid"
			cmd.Chat.Thread, cmd.Chat.Profile = "01a198eb-f8e9-72a1-856c-0e986d10f6f7", "invalid"
			if err := runner.execute(operation, cmd, true, io.Discard); err == nil {
				t.Fatal("invalid operation or profile was accepted")
			}
			if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid command wrote state: %v", err)
			}
		})
	}
}

func TestCodexHarnessStatusPropagatesBoundaryErrors(t *testing.T) {
	t.Run("config resolution", func(t *testing.T) {
		runner, _ := harnessTestRunner()
		failure := errors.New("config resolution failed")
		runner.path = func() (string, error) { return "", failure }
		if err := runHarnessTestCLI(t, runner, io.Discard); !errors.Is(err, failure) {
			t.Fatalf("config failure = %v", err)
		}
	})
	t.Run("invalid saved policy", func(t *testing.T) {
		runner, fsys := harnessTestRunner()
		if err := runHarnessTestCLI(t, runner, io.Discard, "enable"); err != nil {
			t.Fatal(err)
		}
		invalid := []byte(`{"version":999,"enabled":true}`)
		if err := fsys.WriteFile("/cq/harness.json", invalid, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, operation := range []string{"status", "disable"} {
			var output bytes.Buffer
			if err := runHarnessTestCLI(t, runner, &output, operation, "--json"); err == nil || output.Len() != 0 {
				t.Fatalf("%s swallowed invalid policy or printed success: %v, %s", operation, err, &output)
			}
		}
		after, _ := fsys.ReadFile("/cq/harness.json")
		if !bytes.Equal(after, invalid) {
			t.Fatal("invalid saved policy was overwritten")
		}
	})
	for _, jsonOutput := range []bool{false, true} {
		t.Run("output", func(t *testing.T) {
			runner, _ := harnessTestRunner()
			if err := runner.execute("status", CodexHarnessCmd{}, jsonOutput, harnessErrorWriter{}); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("output failure = %v", err)
			}
		})
	}
}

type harnessErrorWriter struct{}

func (harnessErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCodexHarnessStatusSeparatesReceiptFromSelectionPreview(t *testing.T) {
	chat := "01a198eb-f8e9-72a1-856c-0e986d10f6f7"
	for _, mode := range []string{"plugins", "full"} {
		t.Run(mode, func(t *testing.T) {
			runner, fsys := harnessTestRunner()
			for _, args := range [][]string{{"enable"}, {"chat", chat, "full"}} {
				if err := runHarnessTestCLI(t, runner, io.Discard, args...); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := fsys.ReadFile("/cq/harness.json")
			receipt := harness.Receipt{
				Version: 1, ChatID: chat, Mode: mode, Profile: harness.Minimal, State: "applied", Source: "default",
				BaselinePlugins: []string{"original-disabled"}, AppliedPlugins: []string{"original-disabled", "plugin"},
				UpdatedAt: time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC),
			}
			loads := 0
			runner.receipt = func(id string) (harness.Receipt, error) {
				loads++
				if id != chat {
					t.Fatalf("receipt lookup used %q instead of canonical chat ID", id)
				}
				return receipt, nil
			}
			var output bytes.Buffer
			if err := runHarnessTestCLI(t, runner, &output, "status", "--json", "--model", "glm-5.3", "--chat", "codex://threads/"+chat); err != nil {
				t.Fatal(err)
			}
			var status codexHarnessStatus
			if err := json.Unmarshal(output.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Selection == nil || status.Selection.Profile != harness.Full || status.Selection.Source != "chat" || status.LastApplication == nil || status.LastApplication.Profile != harness.Minimal || status.LastApplication.Mode != mode || status.LastApplication.State != "applied" || status.LastApplication.ChatID != chat {
				t.Fatalf("receipt and preview were conflated: %#v", status)
			}
			output.Reset()
			if err := runHarnessTestCLI(t, runner, &output, "status", "--model", "glm-5.3", "--chat", chat); err != nil {
				t.Fatal(err)
			}
			for _, detail := range []string{"preview: full", "last application: minimal", "(" + mode + ", state: applied", "2026-11-05T11:00:00Z"} {
				if !strings.Contains(output.String(), detail) {
					t.Fatalf("human status omitted %q: %s", detail, &output)
				}
			}
			after, _ := fsys.ReadFile("/cq/harness.json")
			if !bytes.Equal(before, after) || loads != 2 {
				t.Fatalf("receipt inspection changed policy or performed extra lookups: %d", loads)
			}
		})
	}
}

func TestCodexHarnessStatusMissingReceiptIsReadOnly(t *testing.T) {
	runner, fsys := harnessTestRunner()
	chat := "01a198eb-f8e9-72a1-856c-0e986d10f6f7"
	runner.receipt = func(id string) (harness.Receipt, error) {
		if id != chat {
			t.Fatalf("missing receipt lookup = %q", id)
		}
		return harness.Receipt{}, errors.Join(errors.New("missing receipt"), os.ErrNotExist)
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "status", "--json", "--model", "glm-5.3", "--chat", chat); err != nil {
		t.Fatal(err)
	}
	var status codexHarnessStatus
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || status.LastApplication != nil || status.Selection == nil || status.Selection.Source != "disabled" {
		t.Fatalf("missing receipt status = %s, %v", &output, err)
	}
	if strings.Contains(output.String(), "last_application") {
		t.Fatalf("missing receipt emitted a synthetic last application: %s", &output)
	}
	if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing receipt inspection created config: %v", err)
	}
}

func TestCodexHarnessStatusSurfacesReceiptErrorsAndOnlyReadsSelectedChat(t *testing.T) {
	runner, fsys := harnessTestRunner()
	failure := errors.New("receipt is unreadable")
	loads := 0
	runner.receipt = func(string) (harness.Receipt, error) {
		loads++
		return harness.Receipt{}, failure
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "status", "--model", "glm-5.3"); err != nil || loads != 0 {
		t.Fatalf("status without --chat read a receipt: %v, %d", err, loads)
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "status", "--json", "--chat", "01a198eb-f8e9-72a1-856c-0e986d10f6f7"); !errors.Is(err, failure) || loads != 1 || output.Len() != 0 {
		t.Fatalf("receipt failure = %v, loads=%d, output=%s", err, loads, &output)
	}
	if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed receipt inspection created config: %v", err)
	}
}
