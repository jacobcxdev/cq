package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCodexHarnessProfileCommandParsing(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"inventory", "--kind", "skill", "--search", "review", "--project", ".", "--json"}, "inventory"},
		{[]string{"profile"}, "profile list"},
		{[]string{"profile", "list", "--project", "."}, "profile list"},
		{[]string{"profile", "show", "review"}, "profile show <name>"},
		{[]string{"profile", "save", "review", "--file", "profile.json", "--replace"}, "profile save <name>"},
		{[]string{"profile", "copy", "minimal", "review", "--project", "."}, "profile copy <source> <target>"},
		{[]string{"bind", "minimal", "review", "--scope", "project", "--project", ".", "--model", "glm-*"}, "bind <profiles>"},
		{[]string{"unbind", "--scope", "chat", "--chat", "current"}, "unbind"},
		{[]string{"explain", "--chat", "current", "--project", "."}, "explain"},
		{[]string{"export", "review", "--project", "."}, "export <name>"},
		{[]string{"import", "--file", "profile.json", "--name", "review", "--replace"}, "import"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			_, parsed, err := parseCodexResetCLI(t, append([]string{"codex", "harness"}, test.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Command() != "codex harness "+test.want {
				t.Fatalf("command = %q, want %q", parsed.Command(), test.want)
			}
		})
	}
}

const harnessProfilesChat = "01a198eb-f8e9-72a1-856c-0e986d10f6f7"

func harnessProfilesFixture(t *testing.T) (codexHarnessRunner, *fsutil.MemFS) {
	t.Helper()
	runner, fsys := harnessTestRunner()
	runner.getwd = func() (string, error) { return "/work/project", nil }
	runner.getenv = func(name string) string {
		if name != "CODEX_THREAD_ID" {
			t.Fatalf("current chat consulted unrelated environment %q", name)
		}
		return harnessProfilesChat
	}
	if err := fsys.MkdirAll("/work/project", 0o755); err != nil {
		t.Fatal(err)
	}
	return runner, fsys
}

func harnessProfileInput(t *testing.T, fsys *fsutil.MemFS, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func harnessReviewDefinition() harness.Definition {
	return harness.Definition{Description: "Review with selected integrations", Changes: harness.Changes{
		Plugins: map[string]harness.Decision{"jira@market": harness.Disabled},
		MCP:     map[string]harness.Decision{"calendar": harness.Enabled},
		Skills:  map[string]harness.Decision{"repo:review": harness.Enabled},
	}}
}

func TestCodexHarnessProfileSavesCopiesAndRequiresExplicitReplacement(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	definition := harnessReviewDefinition()
	harnessProfileInput(t, fsys, "/work/profile.json", definition)
	runner.apply = func(context.Context, string) error { t.Fatal("definition write activated a chat"); return nil }
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", "../profile.json"); err != nil {
		t.Fatal(err)
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || policy.Version != 2 || policy.Enabled || !reflect.DeepEqual(policy.Profiles["review"], definition) || len(policy.Tasks) != 0 {
		t.Fatalf("definition save changed activation or data: %#v, %v", policy, err)
	}
	before, _ := fsys.ReadFile("/cq/harness.json")
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", "/work/profile.json"); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("existing profile replacement = %v", err)
	}
	after, _ := fsys.ReadFile("/cq/harness.json")
	if !bytes.Equal(before, after) {
		t.Fatal("rejected replacement changed policy")
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "copy", "review", "review-copy"); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "copy", "minimal", "review-copy"); err == nil {
		t.Fatal("copy silently replaced an existing profile")
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "copy", "minimal", "review-copy", "--replace"); err != nil {
		t.Fatal(err)
	}
	policy, _ = harness.Load(fsys, "/cq/harness.json")
	if policy.Enabled || policy.Profiles["review-copy"].Changes.Default != harness.Disabled || !reflect.DeepEqual(policy.Profiles["review"], definition) {
		t.Fatalf("copy changed source or activation: %#v", policy)
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "profile", "list", "--json"); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Profiles []harnessProfileDocument `json:"profiles"`
	}
	if err := json.Unmarshal(output.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, profile := range list.Profiles {
		names = append(names, profile.Name)
	}
	if !slices.Equal(names, []string{"full", "minimal", "review", "review-copy"}) {
		t.Fatalf("visible profile order = %v", names)
	}
}

func TestCodexHarnessProjectDefinitionsStayPortableAndInactive(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	definition := harnessReviewDefinition()
	harnessProfileInput(t, fsys, "/work/profile.json", definition)
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", "/work/profile.json", "--project", "."); err != nil {
		t.Fatal(err)
	}
	workspace, err := harness.LoadWorkspace(fsys, "/work/project/.cq/harness.json")
	if err != nil || workspace.Version != 2 || !reflect.DeepEqual(workspace.Profiles["review"], definition) || len(workspace.Rules.All.Profiles) != 0 || len(workspace.Rules.Models) != 0 {
		t.Fatalf("portable project definition = %#v, %v", workspace, err)
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || policy.Version != 2 || policy.Enabled || len(policy.Profiles) != 0 {
		t.Fatalf("project save activated or copied definitions into user state: %#v, %v", policy, err)
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "profile", "show", "review", "--json"); err != nil {
		t.Fatal(err)
	}
	var shown harnessProfileDocument
	if err := json.Unmarshal(output.Bytes(), &shown); err != nil || !reflect.DeepEqual(shown.Definition, definition) {
		t.Fatalf("saved project profile is not visible: %s, %v", &output, err)
	}
	info, err := fsys.Stat("/work/project")
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("project save changed root permissions: %v, %v", info, err)
	}
}

func TestCodexHarnessProjectProfileDiscoveryDoesNotPromoteFreshUserPolicy(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	definition := harnessReviewDefinition()
	if _, err := harness.UpdateWorkspace(fsys, "/work/.cq/harness.json", func(workspace *harness.WorkspacePolicy) error {
		workspace.Profiles["project-review"] = definition
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := fsys.ReadFile("/work/.cq/harness.json")
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "profile", "show", "project-review", "--json"); err != nil {
		t.Fatal(err)
	}
	var shown harnessProfileDocument
	if err := json.Unmarshal(output.Bytes(), &shown); err != nil || !reflect.DeepEqual(shown.Definition, definition) {
		t.Fatalf("fresh project discovery = %s, %v", &output, err)
	}
	if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata discovery promoted or activated user policy: %v", err)
	}
	after, _ := fsys.ReadFile("/work/.cq/harness.json")
	if !bytes.Equal(before, after) {
		t.Fatal("metadata discovery changed portable policy")
	}
}

func TestCodexHarnessProfileExportImportRoundTripWithoutBinding(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	definition := harnessReviewDefinition()
	harnessProfileInput(t, fsys, "/work/source.json", definition)
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", "/work/source.json"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "export", "review"); err != nil {
		t.Fatal(err)
	}
	var exported harnessProfileDocument
	if err := json.Unmarshal(output.Bytes(), &exported); err != nil || exported.Version != 1 || exported.Name != "review" || !reflect.DeepEqual(exported.Definition, definition) {
		t.Fatalf("portable export = %s, %v", &output, err)
	}
	if err := fsys.WriteFile("/work/export.json", output.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "import", "--file", "/work/export.json"); err == nil {
		t.Fatal("import replaced an existing name without --replace")
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "import", "--file", "/work/export.json", "--name", "imported"); err != nil {
		t.Fatal(err)
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || policy.Enabled || len(policy.Tasks) != 0 || !reflect.DeepEqual(policy.Profiles["imported"], definition) {
		t.Fatalf("import activated a profile or lost stable IDs: %#v, %v", policy, err)
	}
}

func TestCodexHarnessProfileInvalidFilesDoNotCreateUserState(t *testing.T) {
	for _, test := range []struct {
		name, json string
		importFile bool
	}{
		{"bad-name", `{"changes":{"default":"disabled"}}`, false},
		{"reserved", `{"changes":{"default":"disabled"}}`, false},
		{"unknown decision", `{"changes":{"default":"sometimes"}}`, false},
		{"unknown field", `{"changes":{},"secret":"hidden"}`, false},
		{"trailing", `{"changes":{}} {}`, false},
		{"null", `null`, false},
		{"absolute skill", `{"changes":{"skills":{"/absolute/skill":"disabled"}}}`, false},
		{"Windows skill", `{"changes":{"skills":{"C:\\skills\\review":"disabled"}}}`, false},
		{"unknown portable version", `{"version":99,"name":"imported","definition":{"changes":{}}}`, true},
		{"missing portable definition", `{"version":1,"name":"imported"}`, true},
		{"null portable definition", `{"version":1,"name":"imported","definition":null}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, fsys := harnessProfilesFixture(t)
			if err := fsys.WriteFile("/work/input.json", []byte(test.json), 0o644); err != nil {
				t.Fatal(err)
			}
			name := "review"
			if test.name == "bad-name" {
				name = "../invalid"
			}
			if test.name == "reserved" {
				name = "full"
			}
			args := []string{"profile", "save", name, "--file", "/work/input.json", "--replace"}
			if test.importFile {
				args = []string{"import", "--file", "/work/input.json"}
			}
			if err := runHarnessTestCLI(t, runner, io.Discard, args...); err == nil {
				t.Fatal("invalid profile input was accepted")
			}
			if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid profile created user state: %v", err)
			}
		})
	}
}

func TestCodexHarnessBindSavesBeforeApplyingCurrentChat(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	harnessProfileInput(t, fsys, "/work/profile.json", harnessReviewDefinition())
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", "/work/profile.json"); err != nil {
		t.Fatal(err)
	}
	applied := 0
	runner.apply = func(_ context.Context, id string) error {
		applied++
		policy, err := harness.Load(fsys, "/cq/harness.json")
		if err != nil || !policy.Enabled || id != harnessProfilesChat || !slices.Equal(policy.Tasks[id].All.Profiles, []string{"minimal", "review"}) {
			t.Fatalf("apply ran before binding was saved: %#v, %v, %s", policy, err, id)
		}
		return nil
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "bind", "minimal", "review", "--json"); err != nil {
		t.Fatal(err)
	}
	var result harnessBindingResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Saved || !result.Enabled || result.Application != "requested" || result.Chat != harnessProfilesChat || applied != 1 {
		t.Fatalf("binding result = %s, %v; applied=%d", &output, err, applied)
	}
	policy, _ := harness.Load(fsys, "/cq/harness.json")
	effective := policy.ResolveEffective("gpt-6.1", "/work/project", harnessProfilesChat)
	if !effective.Selection.Managed || effective.Changes.Plugins["jira@market"] != harness.Disabled || effective.Changes.Skills["repo:review"] != harness.Enabled {
		t.Fatalf("binding failed for non-GLM model: %#v", effective)
	}
}

func TestCodexHarnessProjectModelBindingAndUnbindPreserveDefinitions(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	harnessProfileInput(t, fsys, "/work/profile.json", harnessReviewDefinition())
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", "/work/profile.json", "--project", "."); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "review", "--scope", "project", "--model", "gpt-*"); err != nil {
		t.Fatal(err)
	}
	workspace, err := harness.LoadWorkspace(fsys, "/work/project/.cq/harness.json")
	if err != nil || !slices.Equal(workspace.Rules.Models["gpt-*"].Profiles, []string{"review"}) || len(workspace.Rules.All.Profiles) != 0 {
		t.Fatalf("project model binding = %#v, %v", workspace, err)
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || !policy.Enabled || len(policy.Profiles) != 0 {
		t.Fatalf("project binding activation = %#v, %v", policy, err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "unbind", "--scope", "project", "--model", "GPT-*"); err != nil {
		t.Fatal(err)
	}
	workspace, err = harness.LoadWorkspace(fsys, "/work/project/.cq/harness.json")
	if err != nil || len(workspace.Rules.Models) != 0 || workspace.Profiles["review"].Description == "" {
		t.Fatalf("unbind discarded definition or retained rule: %#v, %v", workspace, err)
	}
}

func TestCodexHarnessBindingValidationPrecedesWrites(t *testing.T) {
	for _, args := range [][]string{
		{"bind", "minimal", "--chat", "not-a-chat"},
		{"bind", "minimal", "--chat", "current"},
		{"bind", "unknown", "--scope", "global"},
		{"bind", "minimal", "--scope", "global", "--model", "g*p*t"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			runner, fsys := harnessProfilesFixture(t)
			runner.getenv = func(string) string { return "" }
			if err := runHarnessTestCLI(t, runner, io.Discard, args...); err == nil {
				t.Fatal("invalid binding was accepted")
			}
			if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid binding wrote policy: %v", err)
			}
		})
	}
}

func TestCodexHarnessApplyFailureReportsSavedPendingBinding(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	failure := errors.New("chat owned by full bridge")
	runner.apply = func(context.Context, string) error { return failure }
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "bind", "minimal", "--json"); !errors.Is(err, failure) {
		t.Fatalf("apply failure = %v", err)
	}
	var result harnessBindingResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Saved || result.Application != "pending" {
		t.Fatalf("pending result = %s, %v", &output, err)
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || !policy.Enabled || !slices.Equal(policy.Tasks[harnessProfilesChat].All.Profiles, []string{"minimal"}) {
		t.Fatalf("application error lost saved binding: %#v, %v", policy, err)
	}
}

func TestCodexHarnessInventoryUsesStableIDsAndFiltersMetadata(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	runner.snapshot = func(_ context.Context, cwd string) (harness.Inventory, error) {
		if cwd != "/work/project" {
			t.Fatalf("inventory cwd = %q", cwd)
		}
		return harness.Inventory{Capabilities: []harness.Capability{
			{ID: "z@market", Kind: harness.PluginKind, Name: "Review plugin", Enabled: true, Available: true},
			{ID: "repo:review", Kind: harness.SkillKind, Name: "Review", Description: "Review changes", Path: "/work/project/.agents/skills/review/SKILL.md", Enabled: false, Available: true},
			{ID: "calendar", Kind: harness.MCPKind, Name: "Calendar", Enabled: true, Available: true},
		}, Issues: []harness.InventoryIssue{{Code: "partial", Kind: harness.PluginKind}}}, nil
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "inventory", "--json", "--kind", "skill", "--search", "REVIEW"); err != nil {
		t.Fatal(err)
	}
	var inventory harness.Inventory
	if err := json.Unmarshal(output.Bytes(), &inventory); err != nil || len(inventory.Capabilities) != 1 || inventory.Capabilities[0].ID != "repo:review" || inventory.Capabilities[0].Enabled || len(inventory.Issues) != 1 {
		t.Fatalf("inventory = %s, %v", &output, err)
	}
	if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inventory wrote policy: %v", err)
	}
}

func TestCodexHarnessExplainUsesCurrentModelAndKeepsReceiptSeparate(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	harnessProfileInput(t, fsys, "/work/profile.json", harnessReviewDefinition())
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", "/work/profile.json"); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "review", "--scope", "global"); err != nil {
		t.Fatal(err)
	}
	runner.readModel = func(_ context.Context, chat string) (harness.Snapshot, error) {
		if chat != harnessProfilesChat {
			t.Fatalf("model lookup chat = %q", chat)
		}
		return harness.Snapshot{ChatID: chat, Model: "gpt-6.1"}, nil
	}
	runner.snapshot = func(context.Context, string) (harness.Inventory, error) {
		return harness.Inventory{Capabilities: []harness.Capability{{Kind: harness.PluginKind, ID: "jira@market", Enabled: true, Available: true}}}, nil
	}
	runner.receipt = func(chat string) (harness.Receipt, error) {
		return harness.Receipt{ChatID: chat, Mode: "plugins", State: "applied", Profile: harness.Minimal}, nil
	}
	before, _ := fsys.ReadFile("/cq/harness.json")
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "explain", "--json"); err != nil {
		t.Fatal(err)
	}
	var result harnessExplanation
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Chat != harnessProfilesChat || result.Model != "gpt-6.1" || result.Project != "/work/project" || result.Effective.Selection.Profile != harness.Custom || len(result.Effective.Layers) == 0 || len(result.Capabilities) != 1 || result.Capabilities[0].Selected || result.LastApplication == nil || result.LastApplication.Profile != harness.Minimal {
		t.Fatalf("explanation = %s, %v", &output, err)
	}
	after, _ := fsys.ReadFile("/cq/harness.json")
	if !bytes.Equal(before, after) {
		t.Fatal("explain rewrote saved policy")
	}
}

func TestCodexHarnessUnbindRemovesOnlySelectedUserRule(t *testing.T) {
	for _, scope := range []string{"global", "chat"} {
		t.Run(scope, func(t *testing.T) {
			runner, fsys := harnessProfilesFixture(t)
			for _, args := range [][]string{
				{"bind", "full", "--scope", scope},
				{"bind", "minimal", "--scope", scope, "--model", "gpt-*"},
				{"unbind", "--scope", scope, "--model", "GPT-*"},
			} {
				if err := runHarnessTestCLI(t, runner, io.Discard, args...); err != nil {
					t.Fatal(err)
				}
			}
			policy, err := harness.Load(fsys, "/cq/harness.json")
			if err != nil {
				t.Fatal(err)
			}
			selected := policy.Global
			if scope == "chat" {
				selected = policy.Tasks[harnessProfilesChat]
			}
			if !policy.Enabled || !slices.Equal(selected.All.Profiles, []string{"full"}) {
				t.Fatalf("unbind removed all-model binding or disabled policy: %#v", policy)
			}
			if _, exists := selected.Models["gpt-*"]; exists {
				t.Fatal("unbind retained the selected model rule")
			}
			if err := runHarnessTestCLI(t, runner, io.Discard, "unbind", "--scope", scope); err != nil {
				t.Fatal(err)
			}
			policy, _ = harness.Load(fsys, "/cq/harness.json")
			selected = policy.Global
			if scope == "chat" {
				selected = policy.Tasks[harnessProfilesChat]
			}
			if len(selected.All.Profiles) != 0 || !policy.Enabled {
				t.Fatalf("all-model unbind did not preserve enabled state: %#v", policy)
			}
		})
	}
}

func TestCodexHarnessGlobalBindingDoesNotRequireCurrentChat(t *testing.T) {
	runner, _ := harnessProfilesFixture(t)
	runner.getenv = func(string) string { return "" }
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "bind", "full", "--scope", "global", "--json"); err != nil {
		t.Fatal(err)
	}
	var result harnessBindingResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Chat != "" || !result.Enabled || result.Application != "pending" {
		t.Fatalf("global binding required a current chat: %s, %v", &output, err)
	}
}

func TestCodexHarnessCurrentChatRejectsMissingAndInvalidContext(t *testing.T) {
	for _, id := range []string{"", "not-a-chat", strings.ToUpper(harnessProfilesChat), "codex://threads/" + harnessProfilesChat} {
		for _, operation := range []string{"bind", "explain"} {
			t.Run(operation+id, func(t *testing.T) {
				runner, fsys := harnessProfilesFixture(t)
				runner.getenv = func(name string) string {
					if name != "CODEX_THREAD_ID" {
						t.Fatalf("current chat used session fallback %q", name)
					}
					return id
				}
				args := []string{"bind", "minimal", "--chat", "current"}
				if operation == "explain" {
					args = []string{"explain", "--chat", "current", "--model", "gpt-6.1"}
				}
				if err := runHarnessTestCLI(t, runner, io.Discard, args...); err == nil {
					t.Fatal("invalid current-chat context was accepted")
				}
				if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid current context wrote user state: %v", err)
				}
			})
		}
	}
}

func TestCodexHarnessFailedBindingWriteDoesNotApply(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "full", "--scope", "global"); err != nil {
		t.Fatal(err)
	}
	before, _ := fsys.ReadFile("/cq/harness.json")
	runner.fs = harnessRenameFailureFS{fsys}
	runner.apply = func(context.Context, string) error { t.Fatal("failed write applied runtime policy"); return nil }
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "bind", "minimal", "--json"); err == nil || output.Len() != 0 {
		t.Fatalf("failed binding write emitted success: %v, %s", err, &output)
	}
	after, _ := fsys.ReadFile("/cq/harness.json")
	if !bytes.Equal(before, after) {
		t.Fatal("failed binding write changed prior policy")
	}
}

func TestCodexHarnessProfileFileBoundaries(t *testing.T) {
	for _, test := range []struct{ name, path, data string }{
		{"missing", "/work/missing.json", ""},
		{"directory", "/work/project", ""},
		{"oversized", "/work/oversized.json", strings.Repeat(" ", (1<<20)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, fsys := harnessProfilesFixture(t)
			if test.data != "" {
				if err := fsys.WriteFile(test.path, []byte(test.data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "review", "--file", test.path); err == nil {
				t.Fatal("invalid definition source was accepted")
			}
			if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid definition source wrote state: %v", err)
			}
		})
	}
}

func TestCodexHarnessLegacyOverridesRemainUsableAfterPromotion(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "full", "--scope", "global"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"project", "/work/project", "minimal"}, {"chat", harnessProfilesChat, "full"}} {
		if err := runHarnessTestCLI(t, runner, io.Discard, args...); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || policy.Resolve("glm-5.3", "/work/project", harnessProfilesChat).Profile != harness.Full || policy.Resolve("glm-5.3", "/work/project", "").Profile != harness.Minimal || policy.Resolve("gpt-6.1", "/work/project", harnessProfilesChat).Profile != harness.Full {
		t.Fatalf("legacy v2 scope changed: %#v, %v", policy, err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "chat", harnessProfilesChat, "auto"); err != nil {
		t.Fatal(err)
	}
	policy, _ = harness.Load(fsys, "/cq/harness.json")
	if policy.Resolve("glm-5.3", "/work/project", harnessProfilesChat).Profile != harness.Minimal {
		t.Fatal("legacy auto no longer inherited project choice")
	}
}

func harnessExplainPlan(t *testing.T, definition harness.Definition, inventory harness.Inventory, receipt *harness.Receipt) harnessExplanation {
	t.Helper()
	runner, fsys := harnessProfilesFixture(t)
	harnessProfileInput(t, fsys, "/work/plan.json", definition)
	if err := runHarnessTestCLI(t, runner, io.Discard, "profile", "save", "plan", "--file", "/work/plan.json"); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "plan", "--scope", "global", "--model", "gpt-6.1"); err != nil {
		t.Fatal(err)
	}
	runner.snapshot = func(context.Context, string) (harness.Inventory, error) { return inventory, nil }
	runner.receipt = func(string) (harness.Receipt, error) {
		if receipt == nil {
			return harness.Receipt{}, os.ErrNotExist
		}
		return *receipt, nil
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "explain", "--model", "gpt-6.1", "--json"); err != nil {
		t.Fatal(err)
	}
	var result harnessExplanation
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCodexHarnessExplainIncludesRequiredPluginDependencies(t *testing.T) {
	definition := harness.Definition{Changes: harness.Changes{Default: harness.Disabled, Skills: map[string]harness.Decision{"plugin:research@market:analyse": harness.Enabled}}}
	inventory := harness.Inventory{Capabilities: []harness.Capability{
		{ID: "research@market", NativeID: "research-native", Kind: harness.PluginKind, Enabled: true, Available: true},
		{ID: "plugin:research@market:analyse", Kind: harness.SkillKind, PluginID: "research@market", Dependencies: []string{"plugin:research@market"}, Enabled: true, Available: true},
	}}
	result := harnessExplainPlan(t, definition, inventory, nil)
	if len(result.Capabilities) != 2 || !result.Capabilities[0].Selected || !result.Capabilities[1].Selected {
		t.Fatalf("explain omitted the selected skill's required plugin: %#v", result.Capabilities)
	}
}

func TestCodexHarnessExplainUsesOriginalPerChatPluginBaseline(t *testing.T) {
	inventory := harness.Inventory{Capabilities: []harness.Capability{
		{ID: "research@market", NativeID: "research-native", Kind: harness.PluginKind, Enabled: true, Available: true},
		{ID: "plugin:research@market:analyse", Kind: harness.SkillKind, PluginID: "research@market", Dependencies: []string{"plugin:research@market"}, Enabled: true, Available: true},
	}}
	receipt := &harness.Receipt{ChatID: harnessProfilesChat, Mode: "plugins", Profile: harness.Minimal, State: "applied", BaselinePlugins: []string{"research-native"}}
	result := harnessExplainPlan(t, harness.Definition{Changes: harness.Changes{Reset: true}}, inventory, receipt)
	if len(result.Capabilities) != 2 {
		t.Fatalf("missing inventory rows: %#v", result.Capabilities)
	}
	for _, capability := range result.Capabilities {
		if capability.Selected || capability.BaselineEnabled || !capability.Enabled {
			t.Fatalf("explain replaced the original per-chat baseline with global inventory: %#v", capability)
		}
	}
}

func TestCodexHarnessExplainPinsOnlyAvailableBaselineCurationSkill(t *testing.T) {
	for _, test := range []struct {
		name                         string
		enabled, available, selected bool
	}{
		{"baseline enabled", true, true, true},
		{"baseline disabled", false, true, false},
		{"unavailable", true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := harness.Definition{Changes: harness.Changes{Default: harness.Disabled, Skills: map[string]harness.Decision{harness.CurationSkillID: harness.Disabled}}}
			inventory := harness.Inventory{Capabilities: []harness.Capability{{ID: harness.CurationSkillID, Kind: harness.SkillKind, Enabled: test.enabled, Available: test.available}}}
			result := harnessExplainPlan(t, definition, inventory, nil)
			if len(result.Capabilities) != 1 || result.Capabilities[0].Selected != test.selected {
				t.Fatalf("curation pin differs from native plan: %#v", result.Capabilities)
			}
			if test.selected && !slices.Contains(result.PlanIssues, "retained-core:skill:"+harness.CurationSkillID) {
				t.Fatalf("explain omitted core retention issue: %v", result.PlanIssues)
			}
		})
	}
}

func TestCodexHarnessExplainReportsUnavailableAndDependencyDeniedPlans(t *testing.T) {
	definition := harness.Definition{Changes: harness.Changes{
		Default: harness.Disabled,
		Plugins: map[string]harness.Decision{"research@market": harness.Disabled},
		Skills:  map[string]harness.Decision{"repo:unavailable": harness.Enabled, "plugin:research@market:analyse": harness.Enabled},
	}}
	inventory := harness.Inventory{Capabilities: []harness.Capability{
		{ID: "repo:unavailable", Kind: harness.SkillKind, Enabled: false, Available: false},
		{ID: "research@market", Kind: harness.PluginKind, Enabled: true, Available: true},
		{ID: "plugin:research@market:analyse", Kind: harness.SkillKind, PluginID: "research@market", Dependencies: []string{"plugin:research@market"}, Enabled: true, Available: true},
	}, Issues: []harness.InventoryIssue{{Code: "inventory_incomplete", Kind: harness.SkillKind}}}
	result := harnessExplainPlan(t, definition, inventory, nil)
	for _, capability := range result.Capabilities {
		if (capability.ID == "repo:unavailable" || capability.ID == "research@market") && capability.Selected {
			t.Fatalf("unavailable or explicitly denied capability selected: %#v", capability)
		}
	}
	for _, issue := range []string{"unavailable:skill:repo:unavailable", "dependency-denied:skill:plugin:research@market:analyse:plugin:research@market", "inventory_incomplete:skill:"} {
		if !slices.Contains(result.PlanIssues, issue) {
			t.Fatalf("explain omitted runtime planning issue %q: %v", issue, result.PlanIssues)
		}
	}
	if len(result.Issues) != 1 || result.Issues[0].Code != "inventory_incomplete" {
		t.Fatalf("explain dropped inventory diagnostics: %#v", result.Issues)
	}
}

func TestCodexHarnessExplainReportsSavedBindingAwaitingConfirmation(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	applicationUnavailable := errors.New("Desktop IPC unavailable")
	runner.apply = func(context.Context, string) error { return applicationUnavailable }
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "minimal"); !errors.Is(err, applicationUnavailable) {
		t.Fatalf("bind did not retain its saved/pending outcome: %v", err)
	}
	runner.receipt = func(string) (harness.Receipt, error) { return harness.Receipt{}, os.ErrNotExist }
	runner.snapshot = func(context.Context, string) (harness.Inventory, error) {
		return harness.Inventory{Capabilities: []harness.Capability{}}, nil
	}
	before, _ := fsys.ReadFile("/cq/harness.json")
	entriesBefore, _ := fsys.ReadDir("/cq")
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "explain", "--model", "gpt-6.1", "--json"); err != nil {
		t.Fatal(err)
	}
	var result struct {
		ApplicationStatus struct{ State, Reason string } `json:"application_status"`
		LastApplication   *harness.Receipt               `json:"last_application"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ApplicationStatus.State != "pending" || result.ApplicationStatus.Reason != "not_confirmed" || result.LastApplication != nil {
		t.Fatalf("saved binding with no receipt appeared applied or unclassified: %s", &output)
	}
	output.Reset()
	if err := runHarnessTestCLI(t, runner, &output, "explain", "--model", "gpt-6.1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Application: pending (not_confirmed)") {
		t.Fatalf("human explanation hid pending state: %s", &output)
	}
	after, _ := fsys.ReadFile("/cq/harness.json")
	entriesAfter, _ := fsys.ReadDir("/cq")
	if !bytes.Equal(before, after) || len(entriesBefore) != len(entriesAfter) {
		t.Fatal("application-status inspection changed policy or wrote a synthetic receipt")
	}
}

func TestCodexHarnessLocalBindDoesNotReactivateStoredDisabledRules(t *testing.T) {
	other := "019c7714-3b77-74d1-9866-e1f484aae2ab"
	for _, existing := range []string{"global", "workspace", "task", "portable"} {
		for _, scope := range []string{"chat", "project"} {
			t.Run(existing+"/"+scope, func(t *testing.T) {
				runner, fsys := harnessProfilesFixture(t)
				old := harness.Scope{All: harness.Binding{Profiles: []string{"saved"}}}
				policy := harness.Policy{Version: 2, Profiles: map[string]harness.Definition{"saved": {Description: "retained definition", Changes: harness.Changes{Default: harness.Disabled}}}, Workspaces: map[string]harness.Scope{}, Tasks: map[string]harness.Scope{}}
				switch existing {
				case "global":
					policy.Global = old
				case "workspace":
					policy.Workspaces["/other-project"] = old
				case "task":
					policy.Tasks[other] = old
				case "portable":
					if _, err := harness.UpdateWorkspace(fsys, "/work/.cq/harness.json", func(workspace *harness.WorkspacePolicy) error {
						workspace.Rules.All.Profiles = []string{"minimal"}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := harness.Save(fsys, "/cq/harness.json", policy); err != nil {
					t.Fatal(err)
				}
				runner.apply = func(context.Context, string) error { t.Fatal("local bind reactivated runtime application"); return nil }
				var output bytes.Buffer
				if err := runHarnessTestCLI(t, runner, &output, "bind", "minimal", "--scope", scope, "--json"); err != nil {
					t.Fatal(err)
				}
				var result struct {
					Enabled           bool
					Application       string
					ApplicationReason string `json:"application_reason"`
				}
				if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Enabled || result.Application != "pending" || result.ApplicationReason != "policy_disabled" {
					t.Fatalf("local bind silently reactivated stored rules: %s, %v", &output, err)
				}
				saved, err := harness.Load(fsys, "/cq/harness.json")
				if err != nil || saved.Enabled || !reflect.DeepEqual(saved.Profiles, policy.Profiles) || saved.Resolve("glm-5.3", "/other-project", other).Managed {
					t.Fatalf("local bind altered disabled policy scope: %#v, %v", saved, err)
				}
				switch existing {
				case "global":
					if !reflect.DeepEqual(saved.Global, old) {
						t.Fatal("global rules changed")
					}
				case "workspace":
					if !reflect.DeepEqual(saved.Workspaces["/other-project"], old) {
						t.Fatal("other workspace rules changed")
					}
				case "task":
					if !reflect.DeepEqual(saved.Tasks[other], old) {
						t.Fatal("other chat rules changed")
					}
				}
				if scope == "chat" && !slices.Equal(saved.Tasks[harnessProfilesChat].All.Profiles, []string{"minimal"}) {
					t.Fatal("new chat binding was not saved")
				}
				if scope == "project" {
					workspace, err := harness.LoadWorkspace(fsys, "/work/project/.cq/harness.json")
					if err != nil || !slices.Equal(workspace.Rules.All.Profiles, []string{"minimal"}) {
						t.Fatalf("new project binding not saved: %#v, %v", workspace, err)
					}
				}
			})
		}
	}
}

func TestCodexHarnessExplicitGlobalBindMayEnableStoredDisabledRules(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	other := "019c7714-3b77-74d1-9866-e1f484aae2ab"
	old := harness.Scope{All: harness.Binding{Profiles: []string{"minimal"}}}
	if err := harness.Save(fsys, "/cq/harness.json", harness.Policy{Version: 2, Tasks: map[string]harness.Scope{other: old}}); err != nil {
		t.Fatal(err)
	}
	applied := 0
	runner.apply = func(context.Context, string) error { applied++; return nil }
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "full", "--scope", "global"); err != nil {
		t.Fatal(err)
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || !policy.Enabled || !reflect.DeepEqual(policy.Tasks[other], old) || applied != 1 {
		t.Fatalf("explicit global bind did not retain and enable stored rules: %#v, %v, applied=%d", policy, err, applied)
	}
}

func TestCodexHarnessExplainDistinguishesMatchingAndOutdatedReceipts(t *testing.T) {
	definition := harness.Definition{Changes: harness.Changes{Default: harness.Disabled}}
	inventory := harness.Inventory{Capabilities: []harness.Capability{}}
	initial := harnessExplainPlan(t, definition, inventory, nil)
	fingerprint := initial.Effective.Selection.Fingerprint
	if fingerprint == "" {
		t.Fatal("managed V2 plan has no fingerprint")
	}
	for _, state := range []string{"prepared", "applied", "pending", "failed", "diverged", "restored"} {
		t.Run(state, func(t *testing.T) {
			receipt := &harness.Receipt{Version: 1, ChatID: harnessProfilesChat, Mode: "plugins", Profile: harness.Minimal, State: state, Source: "global", Model: "gpt-6.1", Cwd: "/work/project", Fingerprint: fingerprint, UpdatedAt: time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC)}
			matched := harnessExplainPlan(t, definition, inventory, receipt)
			if matched.ApplicationStatus.State != state || matched.ApplicationStatus.Reason != "receipt_matches" || matched.LastApplication == nil || !reflect.DeepEqual(*matched.LastApplication, *receipt) {
				t.Fatalf("matching receipt was not reported as its actual state: %#v", matched)
			}
			receipt.Profile = harness.Full
			profileMismatch := harnessExplainPlan(t, definition, inventory, receipt)
			if profileMismatch.ApplicationStatus.State != "pending" || profileMismatch.ApplicationStatus.Reason != "outdated" || profileMismatch.LastApplication == nil || !reflect.DeepEqual(*profileMismatch.LastApplication, *receipt) {
				t.Fatalf("matching fingerprint confirmed a different recorded profile: %#v", profileMismatch)
			}
			receipt.Profile = harness.Minimal
			receipt.Fingerprint = "older-policy-fingerprint"
			outdated := harnessExplainPlan(t, definition, inventory, receipt)
			if outdated.ApplicationStatus.State != "pending" || outdated.ApplicationStatus.Reason != "outdated" || outdated.LastApplication == nil || !reflect.DeepEqual(*outdated.LastApplication, *receipt) {
				t.Fatalf("outdated receipt appeared current or was rewritten: %#v", outdated)
			}
		})
	}
}

func TestCodexHarnessExplainClassifiesUnmanagedAndFullWithoutReceipt(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "unbound"}[enabled], func(t *testing.T) {
			runner, fsys := harnessProfilesFixture(t)
			if enabled {
				if err := harness.Save(fsys, "/cq/harness.json", harness.Policy{Version: 2, Enabled: true}); err != nil {
					t.Fatal(err)
				}
			}
			runner.snapshot = func(context.Context, string) (harness.Inventory, error) {
				return harness.Inventory{Capabilities: []harness.Capability{}}, nil
			}
			runner.receipt = func(string) (harness.Receipt, error) { return harness.Receipt{}, os.ErrNotExist }
			var output bytes.Buffer
			if err := runHarnessTestCLI(t, runner, &output, "explain", "--model", "gpt-6.1", "--json"); err != nil {
				t.Fatal(err)
			}
			var result harnessExplanation
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			wantReason := "not_managed"
			if !enabled {
				wantReason = "policy_disabled"
			}
			if result.ApplicationStatus.State != "unmanaged" || result.ApplicationStatus.Reason != wantReason || result.LastApplication != nil {
				t.Fatalf("unmanaged plan was reported pending or applied: %#v", result)
			}
			if !enabled {
				if _, err := fsys.Stat("/cq"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unmanaged inspection created state: %v", err)
				}
			}
		})
	}
	baseline := harnessExplainPlan(t, harness.Definition{Changes: harness.Changes{Reset: true}}, harness.Inventory{Capabilities: []harness.Capability{}}, nil)
	if !baseline.Effective.Selection.Managed || baseline.Effective.Selection.Profile != harness.Full || baseline.ApplicationStatus.State != "baseline" || baseline.ApplicationStatus.Reason != "nothing_to_apply" || baseline.LastApplication != nil {
		t.Fatalf("managed full without receipt fabricated application: %#v", baseline)
	}
}

func TestCodexHarnessLocalBindPreservesDisabledLegacyRules(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	other := "019c7714-3b77-74d1-9866-e1f484aae2ab"
	legacy := harness.Policy{Version: 1, Chats: map[string]harness.Profile{other: harness.Minimal}, Projects: map[string]harness.Profile{"/other-project": harness.Minimal}}
	if err := harness.Save(fsys, "/cq/harness.json", legacy); err != nil {
		t.Fatal(err)
	}
	runner.apply = func(context.Context, string) error { t.Fatal("legacy disabled rules reactivated"); return nil }
	if err := runHarnessTestCLI(t, runner, io.Discard, "bind", "minimal"); err != nil {
		t.Fatal(err)
	}
	policy, err := harness.Load(fsys, "/cq/harness.json")
	if err != nil || policy.Version != 2 || policy.Enabled || !slices.Equal(policy.Tasks[other].Models["glm-*"].Profiles, []string{"minimal"}) || !slices.Equal(policy.Workspaces["/other-project"].Models["glm-*"].Profiles, []string{"minimal"}) {
		t.Fatalf("binding changed legacy disabled scope during promotion: %#v, %v", policy, err)
	}
}

func TestCodexHarnessExplainDoesNotMatchDifferentLegacyProfilesWithEmptyFingerprints(t *testing.T) {
	runner, fsys := harnessProfilesFixture(t)
	if err := harness.Save(fsys, "/cq/harness.json", harness.Policy{Version: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runner.receipt = func(string) (harness.Receipt, error) {
		return harness.Receipt{ChatID: harnessProfilesChat, Profile: harness.Minimal, Mode: "plugins", State: "applied"}, nil
	}
	runner.snapshot = func(context.Context, string) (harness.Inventory, error) {
		return harness.Inventory{Capabilities: []harness.Capability{}}, nil
	}
	var output bytes.Buffer
	if err := runHarnessTestCLI(t, runner, &output, "explain", "--model", "gpt-6.1", "--json"); err != nil {
		t.Fatal(err)
	}
	var result harnessExplanation
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Effective.Selection.Profile != harness.Full || result.ApplicationStatus.State != "pending" || result.ApplicationStatus.Reason != "outdated" || result.LastApplication == nil || result.LastApplication.State != "applied" {
		t.Fatalf("different legacy profile was considered confirmed by empty fingerprints: %s", &output)
	}
}
