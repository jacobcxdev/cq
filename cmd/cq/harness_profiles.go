package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

type CodexHarnessInventoryCmd struct {
	Project string `help:"Project directory for native inventory (defaults to current directory)"`
	Kind    string `default:"all" enum:"all,plugin,mcp,skill" help:"Filter capability kind"`
	Search  string `help:"Filter by ID, name or description"`
}

type CodexHarnessProfilesCmd struct {
	List struct {
		Project string `help:"Project directory for visible profile definitions"`
	} `cmd:"" default:"1" help:"List visible profiles, including full and minimal"`
	Show struct {
		Name    string `arg:""`
		Project string `help:"Project directory for visible profile definitions"`
	} `cmd:"" help:"Show one profile definition"`
	Save struct {
		Name    string `arg:""`
		File    string `required:"" help:"JSON definition or profile document from profile show --json"`
		Project string `help:"Save in this project's .cq/harness.json instead of user configuration"`
		Replace bool   `help:"Explicitly replace an existing profile"`
	} `cmd:"" help:"Save a profile definition without binding it"`
	Copy struct {
		Source  string `arg:""`
		Target  string `arg:""`
		Project string `help:"Save in this project's .cq/harness.json instead of user configuration"`
		Replace bool   `help:"Explicitly replace an existing profile"`
	} `cmd:"" help:"Copy a visible profile without binding it"`
}

type HarnessBindingSelector struct {
	Scope   string `default:"chat" enum:"chat,project,global" help:"Binding scope"`
	Model   string `help:"Exact model ID or family with one trailing wildcard; omitted means all models"`
	Project string `help:"Project root (defaults to current directory)"`
	Chat    string `help:"Chat UUID, codex://threads/ link or current (CODEX_THREAD_ID only)"`
}

type CodexHarnessBindCmd struct {
	HarnessBindingSelector `embed:""`
	Profiles               []string `arg:"" help:"Profile names in application order"`
}

func (CodexHarnessBindCmd) Help() string {
	return "Binding enables a fresh policy containing only definitions. An existing disabled policy with stored rules stays disabled for chat and project bindings; use cq codex harness enable or an explicit global binding to reactivate its rules."
}

type CodexHarnessUnbindCmd struct {
	HarnessBindingSelector `embed:""`
}

type CodexHarnessExplainCmd struct {
	Chat    string `default:"current" help:"Chat UUID, link or current (CODEX_THREAD_ID only)"`
	Project string `help:"Project directory (defaults to current directory)"`
	Model   string `help:"Exact model ID; omitted reads the selected chat's current model"`
}

type CodexHarnessExportCmd struct {
	Name    string `arg:""`
	Project string `help:"Project directory for visible profile definitions"`
}

type CodexHarnessImportCmd struct {
	File    string `required:"" help:"Portable profile JSON file"`
	Name    string `help:"Override the exported profile name"`
	Project string `help:"Save in this project's .cq/harness.json instead of user configuration"`
	Replace bool   `help:"Explicitly replace an existing profile"`
}

type harnessProfileDocument struct {
	Version    int                `json:"version"`
	Name       string             `json:"name"`
	Definition harness.Definition `json:"definition"`
}

type harnessBindingResult struct {
	Saved             bool     `json:"saved"`
	Scope             string   `json:"scope"`
	Model             string   `json:"model,omitempty"`
	Project           string   `json:"project,omitempty"`
	Chat              string   `json:"chat,omitempty"`
	Profiles          []string `json:"profiles,omitempty"`
	Enabled           bool     `json:"enabled"`
	Application       string   `json:"application"`
	ApplicationReason string   `json:"application_reason,omitempty"`
}

type harnessCapabilityDecision struct {
	harness.Capability
	Selected        bool `json:"selected"`
	BaselineEnabled bool `json:"baseline_enabled"`
}

type harnessApplicationStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

type harnessExplanation struct {
	Chat              string                      `json:"chat"`
	Project           string                      `json:"project"`
	Model             string                      `json:"model"`
	Effective         harness.Effective           `json:"effective"`
	Capabilities      []harnessCapabilityDecision `json:"capabilities"`
	Issues            []harness.InventoryIssue    `json:"issues,omitempty"`
	PlanIssues        []string                    `json:"plan_issues,omitempty"`
	ApplicationStatus harnessApplicationStatus    `json:"application_status"`
	LastApplication   *harness.Receipt            `json:"last_application,omitempty"`
}

func (runner codexHarnessRunner) executeProfiles(operation string, cmd CodexHarnessCmd, jsonOutput bool, output io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch operation {
	case "inventory":
		return runner.harnessInventory(ctx, cmd.Inventory, jsonOutput, output)
	case "profile list", "profile show <name>", "profile save <name>", "profile copy <source> <target>", "export <name>", "import":
		return runner.harnessProfileDefinition(operation, cmd, jsonOutput, output)
	case "bind <profiles>":
		return runner.harnessBinding(ctx, cmd.Bind.HarnessBindingSelector, cmd.Bind.Profiles, false, jsonOutput, output)
	case "unbind":
		return runner.harnessBinding(ctx, cmd.Unbind.HarnessBindingSelector, nil, true, jsonOutput, output)
	case "explain":
		return runner.harnessExplain(ctx, cmd.Explain, jsonOutput, output)
	default:
		return fmt.Errorf("unknown harness profile operation: %s", operation)
	}
}

func (runner codexHarnessRunner) harnessProject(directory string) (string, error) {
	if strings.ContainsRune(directory, '\x00') {
		return "", fmt.Errorf("invalid harness project directory")
	}
	if filepath.IsAbs(directory) {
		return filepath.Clean(directory), nil
	}
	getwd := runner.getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	cwd, err := getwd()
	if err != nil {
		return "", fmt.Errorf("resolve current harness directory: %w", err)
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("current harness directory must be absolute")
	}
	return filepath.Clean(filepath.Join(cwd, directory)), nil
}

func (runner codexHarnessRunner) harnessChat(value string, required bool) (string, error) {
	if value != "" && value != "current" {
		return normaliseHarnessChat(value)
	}
	getenv := runner.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	id := getenv("CODEX_THREAD_ID")
	if id == "" {
		if required || value == "current" {
			return "", fmt.Errorf("current chat is unavailable: provide --chat CHAT_ID or set CODEX_THREAD_ID")
		}
		return "", nil
	}
	if !harnessChatIDPattern.MatchString(id) {
		return "", fmt.Errorf("CODEX_THREAD_ID must be a canonical lower-case chat UUID")
	}
	return id, nil
}

func (runner codexHarnessRunner) harnessContext(project string) (harness.Policy, string, error) {
	path, err := runner.path()
	if err != nil {
		return harness.Policy{}, "", err
	}
	policy, err := harness.LoadForContext(runner.fs, path, project)
	return policy, path, err
}

// Metadata discovery remains read-only even before a user opts into v2 policy.
// Runtime resolution deliberately keeps v1 semantics until an explicit write.
func (runner codexHarnessRunner) harnessDefinitions(policy harness.Policy, project string) (map[string]harness.Definition, error) {
	definitions := policy.Definitions()
	if policy.Version == 2 {
		return definitions, nil
	}
	var ancestors []string
	for path := project; ; path = filepath.Dir(path) {
		ancestors = append(ancestors, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	for index := len(ancestors) - 1; index >= 0; index-- {
		workspace, err := harness.LoadWorkspace(runner.fs, filepath.Join(ancestors[index], ".cq", "harness.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		maps.Copy(definitions, workspace.Profiles)
	}
	return definitions, nil
}

func sortedHarnessCapabilities(capabilities []harness.Capability) []harness.Capability {
	result := slices.Clone(capabilities)
	slices.SortFunc(result, func(left, right harness.Capability) int {
		if kind := strings.Compare(string(left.Kind), string(right.Kind)); kind != 0 {
			return kind
		}
		return strings.Compare(left.ID, right.ID)
	})
	return result
}

func (runner codexHarnessRunner) harnessInventory(ctx context.Context, cmd CodexHarnessInventoryCmd, jsonOutput bool, output io.Writer) error {
	project, err := runner.harnessProject(cmd.Project)
	if err != nil {
		return err
	}
	if cmd.Kind != "" && cmd.Kind != "all" && cmd.Kind != "plugin" && cmd.Kind != "mcp" && cmd.Kind != "skill" {
		return fmt.Errorf("inventory kind must be plugin, mcp, skill or all")
	}
	if runner.snapshot == nil {
		return fmt.Errorf("native harness inventory is unavailable")
	}
	inventory, err := runner.snapshot(ctx, project)
	if err != nil {
		return err
	}
	filtered := harness.Inventory{Capabilities: []harness.Capability{}, Issues: inventory.Issues}
	search := strings.ToLower(cmd.Search)
	for _, capability := range sortedHarnessCapabilities(inventory.Capabilities) {
		if cmd.Kind != "" && cmd.Kind != "all" && string(capability.Kind) != cmd.Kind {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(capability.ID+" "+capability.Name+" "+capability.Description), search) {
			continue
		}
		filtered.Capabilities = append(filtered.Capabilities, capability)
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(filtered)
	}
	var text strings.Builder
	for _, capability := range filtered.Capabilities {
		fmt.Fprintf(&text, "%s  %s  enabled=%t available=%t  %s\n", capability.Kind, capability.ID, capability.Enabled, capability.Available, capability.Name)
	}
	if len(filtered.Capabilities) == 0 {
		text.WriteString("No matching capabilities.\n")
	}
	for _, issue := range filtered.Issues {
		fmt.Fprintf(&text, "Inventory issue: %s (%s %s)\n", issue.Code, issue.Kind, issue.ID)
	}
	_, err = io.WriteString(output, text.String())
	return err
}

func (runner codexHarnessRunner) harnessProfileDefinition(operation string, cmd CodexHarnessCmd, jsonOutput bool, output io.Writer) error {
	var name, target, directory, file string
	var replace bool
	switch operation {
	case "profile list":
		directory = cmd.Profile.List.Project
	case "profile show <name>":
		name, directory = cmd.Profile.Show.Name, cmd.Profile.Show.Project
	case "profile save <name>":
		target, directory, file, replace = cmd.Profile.Save.Name, cmd.Profile.Save.Project, cmd.Profile.Save.File, cmd.Profile.Save.Replace
	case "profile copy <source> <target>":
		name, target, directory, replace = cmd.Profile.Copy.Source, cmd.Profile.Copy.Target, cmd.Profile.Copy.Project, cmd.Profile.Copy.Replace
	case "export <name>":
		name, directory = cmd.Export.Name, cmd.Export.Project
	case "import":
		target, directory, file, replace = cmd.Import.Name, cmd.Import.Project, cmd.Import.File, cmd.Import.Replace
	}
	project, err := runner.harnessProject(directory)
	if err != nil {
		return err
	}
	policy, path, err := runner.harnessContext(project)
	if err != nil {
		return err
	}
	definitions, err := runner.harnessDefinitions(policy, project)
	if err != nil {
		return err
	}
	if operation == "profile list" {
		profiles := make([]harnessProfileDocument, 0, len(definitions))
		for _, name := range slices.Sorted(maps.Keys(definitions)) {
			profiles = append(profiles, harnessProfileDocument{1, name, definitions[name]})
		}
		if jsonOutput {
			return json.NewEncoder(output).Encode(struct {
				Profiles []harnessProfileDocument `json:"profiles"`
			}{profiles})
		}
		var text strings.Builder
		for _, profile := range profiles {
			fmt.Fprintf(&text, "%s  %s\n", profile.Name, profile.Definition.Description)
		}
		_, err = io.WriteString(output, text.String())
		return err
	}
	var definition harness.Definition
	if name != "" {
		var ok bool
		definition, ok = definitions[name]
		if !ok {
			return fmt.Errorf("unknown harness profile %q", name)
		}
	}
	if operation == "profile show <name>" || operation == "export <name>" {
		if operation == "export <name>" {
			if err := portableHarnessDefinition(definition); err != nil {
				return err
			}
		}
		return writeHarnessProfile(output, harnessProfileDocument{1, name, definition}, jsonOutput || operation == "export <name>")
	}
	if file != "" {
		data, err := runner.readHarnessProfileFile(file)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := decodeHarnessJSON(data, &fields); err != nil {
			return err
		}
		_, documentFormat := fields["definition"]
		if operation == "import" || documentFormat {
			var document struct {
				Version    int                 `json:"version"`
				Name       string              `json:"name"`
				Definition *harness.Definition `json:"definition"`
			}
			if err := decodeHarnessJSON(data, &document); err != nil {
				return err
			}
			if document.Version != 1 {
				return fmt.Errorf("unsupported portable harness profile version %d", document.Version)
			}
			if document.Definition == nil {
				return fmt.Errorf("portable harness profile definition is required")
			}
			if target == "" {
				target = document.Name
			}
			definition = *document.Definition
		} else if err := decodeHarnessJSON(data, &definition); err != nil {
			return err
		}
	}
	if err := harness.ValidateDefinition(target, definition); err != nil {
		return err
	}
	if err := portableHarnessDefinition(definition); err != nil {
		return err
	}
	if _, exists := definitions[target]; exists && !replace {
		return fmt.Errorf("harness profile %q already exists; use --replace", target)
	}
	if directory != "" {
		if _, err := harness.Update(runner.fs, path, func(policy *harness.Policy) error { return harness.Promote(policy) }); err != nil {
			return err
		}
		_, err = harness.UpdateWorkspace(runner.fs, filepath.Join(project, ".cq", "harness.json"), func(workspace *harness.WorkspacePolicy) error {
			if _, exists := workspace.Profiles[target]; exists && !replace {
				return fmt.Errorf("harness profile %q already exists; use --replace", target)
			}
			if workspace.Profiles == nil {
				workspace.Profiles = make(map[string]harness.Definition)
			}
			workspace.Profiles[target] = definition
			return nil
		})
	} else {
		_, err = harness.Update(runner.fs, path, func(policy *harness.Policy) error {
			if err := harness.Promote(policy); err != nil {
				return err
			}
			if _, exists := policy.Profiles[target]; exists && !replace {
				return fmt.Errorf("harness profile %q already exists; use --replace", target)
			}
			policy.Profiles[target] = definition
			return nil
		})
	}
	if err != nil {
		if directory != "" {
			return fmt.Errorf("user policy promoted without activation; project profile save failed: %w", err)
		}
		return err
	}
	return writeHarnessProfile(output, harnessProfileDocument{1, target, definition}, jsonOutput)
}

func writeHarnessProfile(output io.Writer, document harnessProfileDocument, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(document)
	}
	changes, err := json.MarshalIndent(document.Definition.Changes, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "%s\n%s\n%s\n", document.Name, document.Definition.Description, changes)
	return err
}

func portableHarnessDefinition(definition harness.Definition) error {
	for id := range definition.Changes.Skills {
		if filepath.IsAbs(id) || strings.HasPrefix(id, `\`) || len(id) >= 3 && id[1] == ':' && (id[2] == '\\' || id[2] == '/') {
			return fmt.Errorf("profile skill IDs must be stable inventory IDs, not absolute paths")
		}
	}
	return nil
}

func decodeHarnessJSON(data []byte, target any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("harness profile must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode harness profile: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("harness profile contains trailing data")
	}
	return nil
}

func (runner codexHarnessRunner) readHarnessProfileFile(file string) ([]byte, error) {
	if file == "" || strings.ContainsRune(file, '\x00') {
		return nil, fmt.Errorf("profile JSON file is required")
	}
	path, err := runner.harnessProject(file)
	if err != nil {
		return nil, err
	}
	// External input may live under a directory alias such as macOS /tmp.
	// Resolve only its parent; the final file still uses the no-follow opener.
	if runner.resolveDirectory != nil {
		directory, err := runner.resolveDirectory(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("resolve harness profile directory: %w", err)
		}
		path = filepath.Join(directory, filepath.Base(path))
	}
	opener, ok := runner.fs.(fsutil.NoFollowFileOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	input, err := opener.OpenNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("open harness profile: %w", err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("profile JSON must be a regular file no larger than 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("profile JSON exceeds 1 MiB")
	}
	return data, nil
}

func changeHarnessBinding(scope *harness.Scope, model string, binding harness.Binding, remove bool) {
	if model == "" {
		scope.All = binding
		return
	}
	for selector := range scope.Models {
		if strings.EqualFold(selector, model) {
			delete(scope.Models, selector)
		}
	}
	if !remove {
		if scope.Models == nil {
			scope.Models = make(map[string]harness.Binding)
		}
		scope.Models[strings.ToLower(model)] = binding
	}
}

func renderHarnessScopes(output io.Writer, policy harness.Policy) {
	write := func(label string, scope harness.Scope) {
		if len(scope.All.Profiles) != 0 || scope.All.Changes.Reset || scope.All.Changes.Default != "" || len(scope.All.Changes.Plugins)+len(scope.All.Changes.MCP)+len(scope.All.Changes.Skills) != 0 {
			fmt.Fprintf(output, "  %s all models: %s\n", label, strings.Join(scope.All.Profiles, ", "))
		}
		for _, model := range slices.Sorted(maps.Keys(scope.Models)) {
			fmt.Fprintf(output, "  %s model %s: %s\n", label, model, strings.Join(scope.Models[model].Profiles, ", "))
		}
	}
	write("global", policy.Global)
	for _, project := range slices.Sorted(maps.Keys(policy.Workspaces)) {
		write("project "+project, policy.Workspaces[project])
	}
	for _, chat := range slices.Sorted(maps.Keys(policy.Tasks)) {
		write("chat "+chat, policy.Tasks[chat])
	}
}

func harnessScopeHasRules(scope harness.Scope) bool {
	hasBinding := func(binding harness.Binding) bool {
		changes := binding.Changes
		return len(binding.Profiles) != 0 || changes.Reset || changes.Default != "" || len(changes.Plugins)+len(changes.MCP)+len(changes.Skills) != 0
	}
	if hasBinding(scope.All) {
		return true
	}
	for _, binding := range scope.Models {
		if hasBinding(binding) {
			return true
		}
	}
	return false
}

func harnessPolicyHasRules(policy harness.Policy) bool {
	if len(policy.Projects)+len(policy.Chats) != 0 || harnessScopeHasRules(policy.Global) {
		return true
	}
	for _, scope := range policy.Workspaces {
		if harnessScopeHasRules(scope) {
			return true
		}
	}
	for _, scope := range policy.Tasks {
		if harnessScopeHasRules(scope) {
			return true
		}
	}
	return false
}

func (runner codexHarnessRunner) harnessProjectHasRules(project string) (bool, error) {
	for directory := project; ; directory = filepath.Dir(directory) {
		workspace, err := harness.LoadWorkspace(runner.fs, filepath.Join(directory, ".cq", "harness.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil && harnessScopeHasRules(workspace.Rules) {
			return true, nil
		}
		if filepath.Dir(directory) == directory {
			return false, nil
		}
	}
}

func (runner codexHarnessRunner) harnessBinding(ctx context.Context, selector HarnessBindingSelector, profiles []string, remove, jsonOutput bool, output io.Writer) error {
	if selector.Scope == "" {
		selector.Scope = "chat"
	}
	if selector.Scope != "chat" && selector.Scope != "project" && selector.Scope != "global" {
		return fmt.Errorf("binding scope must be chat, project or global")
	}
	project, err := runner.harnessProject(selector.Project)
	if err != nil {
		return err
	}
	chat, err := runner.harnessChat(selector.Chat, selector.Scope == "chat")
	if err != nil {
		return err
	}
	binding := harness.Binding{Profiles: slices.Clone(profiles)}
	if !remove && len(profiles) == 0 {
		return fmt.Errorf("at least one harness profile is required")
	}
	validation := harness.Scope{}
	changeHarnessBinding(&validation, selector.Model, binding, false)
	if err := harness.ValidateScope(validation); err != nil {
		return err
	}
	_, path, err := runner.harnessContext(project)
	if err != nil {
		return err
	}
	user, err := harness.Load(runner.fs, path)
	if err != nil {
		return err
	}
	definitions := user.Definitions()
	workspacePath := filepath.Join(project, ".cq", "harness.json")
	if selector.Scope == "project" {
		workspace, err := harness.LoadWorkspace(runner.fs, workspacePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for name, definition := range workspace.Profiles {
			definitions[name] = definition
		}
	}
	for _, name := range profiles {
		if _, ok := definitions[name]; !ok {
			return fmt.Errorf("unknown harness profile %q in the selected scope", name)
		}
	}
	priorProjectRules := false
	if !remove && selector.Scope != "global" {
		priorProjectRules, err = runner.harnessProjectHasRules(project)
		if err != nil {
			return err
		}
	}
	if selector.Scope == "project" {
		if _, err := harness.UpdateWorkspace(runner.fs, workspacePath, func(workspace *harness.WorkspacePolicy) error {
			changeHarnessBinding(&workspace.Rules, selector.Model, binding, remove)
			return nil
		}); err != nil {
			return err
		}
	}
	if selector.Scope != "project" || !remove {
		user, err = harness.Update(runner.fs, path, func(policy *harness.Policy) error {
			// Decide before promotion or inserting the requested binding so an
			// unrelated disabled rule cannot become active as a side effect.
			activate := policy.Enabled || selector.Scope == "global" || !harnessPolicyHasRules(*policy) && !priorProjectRules
			if err := harness.Promote(policy); err != nil {
				return err
			}
			if selector.Scope == "global" {
				changeHarnessBinding(&policy.Global, selector.Model, binding, remove)
			} else if selector.Scope == "chat" {
				scope := policy.Tasks[chat]
				changeHarnessBinding(&scope, selector.Model, binding, remove)
				policy.Tasks[chat] = scope
			}
			if !remove {
				policy.Enabled = activate
			}
			return nil
		})
		if err != nil {
			if selector.Scope == "project" {
				return fmt.Errorf("project binding saved but policy activation failed: %w", err)
			}
			return err
		}
	}
	result := harnessBindingResult{Saved: true, Scope: selector.Scope, Model: selector.Model, Chat: chat, Profiles: profiles, Enabled: user.Enabled, Application: "pending"}
	if !user.Enabled {
		result.ApplicationReason = "policy_disabled"
	}
	if selector.Scope == "project" {
		result.Project = project
	}
	var applyErr error
	if user.Enabled && chat != "" && runner.apply != nil {
		applyErr = runner.apply(ctx, chat)
		if applyErr == nil {
			result.Application = "requested"
		}
	}
	var writeErr error
	if jsonOutput {
		writeErr = json.NewEncoder(output).Encode(result)
	} else {
		application := result.Application
		if result.ApplicationReason != "" {
			application += " (" + result.ApplicationReason + ")"
		}
		_, writeErr = fmt.Fprintf(output, "Harness binding saved (%s). Policy enabled: %t. Application: %s.\n", result.Scope, result.Enabled, application)
	}
	if applyErr != nil {
		return errors.Join(writeErr, fmt.Errorf("binding saved; application pending: %w", applyErr))
	}
	return writeErr
}

func (runner codexHarnessRunner) harnessExplain(ctx context.Context, cmd CodexHarnessExplainCmd, jsonOutput bool, output io.Writer) error {
	project, err := runner.harnessProject(cmd.Project)
	if err != nil {
		return err
	}
	chat, err := runner.harnessChat(cmd.Chat, true)
	if err != nil {
		return err
	}
	model := cmd.Model
	if model == "" {
		if runner.readModel == nil {
			return fmt.Errorf("current model unavailable; pass --model")
		}
		snapshot, err := runner.readModel(ctx, chat)
		if err != nil {
			return err
		}
		if snapshot.ChatID != chat || snapshot.Model == "" {
			return fmt.Errorf("current model response does not identify the selected chat")
		}
		model = snapshot.Model
	}
	policy, _, err := runner.harnessContext(project)
	if err != nil {
		return err
	}
	result := harnessExplanation{Chat: chat, Project: project, Model: model, Effective: policy.ResolveEffective(model, project, chat), Capabilities: []harnessCapabilityDecision{}}
	if runner.receipt != nil {
		receipt, err := runner.receipt(chat)
		if err == nil {
			result.LastApplication = &receipt
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	result.ApplicationStatus = harnessApplicationStatus{State: "pending", Reason: "not_confirmed"}
	if receipt := result.LastApplication; receipt != nil {
		if receipt.Fingerprint != result.Effective.Selection.Fingerprint || receipt.Profile != result.Effective.Selection.Profile {
			result.ApplicationStatus.Reason = "outdated"
		} else {
			result.ApplicationStatus = harnessApplicationStatus{State: receipt.State, Reason: "receipt_matches"}
		}
	} else if !result.Effective.Selection.Managed {
		result.ApplicationStatus = harnessApplicationStatus{State: "unmanaged", Reason: "not_managed"}
		if !policy.Enabled {
			result.ApplicationStatus.Reason = "policy_disabled"
		}
	} else if result.Effective.Selection.Profile == harness.Full {
		result.ApplicationStatus = harnessApplicationStatus{State: "baseline", Reason: "nothing_to_apply"}
	}
	if runner.snapshot != nil {
		inventory, err := runner.snapshot(ctx, project)
		if err != nil {
			return err
		}
		nativeID := func(capability harness.Capability) string {
			if capability.NativeID != "" {
				return capability.NativeID
			}
			return capability.ID
		}
		baseline := func(capability harness.Capability) bool {
			if result.LastApplication != nil {
				original := result.LastApplication.BaselinePlugins
				if capability.Kind == harness.PluginKind {
					return capability.Enabled && !slices.Contains(original, nativeID(capability))
				}
				if capability.Kind == harness.SkillKind && capability.PluginID != "" {
					for _, owner := range inventory.Capabilities {
						if owner.Kind == harness.PluginKind && owner.ID == capability.PluginID {
							return capability.Enabled && owner.Enabled && !slices.Contains(original, nativeID(owner))
						}
					}
				}
			}
			return capability.Enabled
		}
		selection := harness.SelectCapabilities(result.Effective, inventory, baseline)
		for _, capability := range sortedHarnessCapabilities(inventory.Capabilities) {
			result.Capabilities = append(result.Capabilities, harnessCapabilityDecision{Capability: capability, Selected: selection.Wants(capability.Kind, capability.ID), BaselineEnabled: baseline(capability)})
		}
		result.Issues = inventory.Issues
		result.PlanIssues = selection.Issues
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(result)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Chat: %s\nProject: %s\nModel: %s\nSelection: %s (managed: %t, source: %s)\n", chat, project, model, result.Effective.Selection.Profile, result.Effective.Selection.Managed, result.Effective.Selection.Source)
	fmt.Fprintf(&text, "Application: %s (%s)\n", result.ApplicationStatus.State, result.ApplicationStatus.Reason)
	for _, layer := range result.Effective.Layers {
		fmt.Fprintf(&text, "Layer: %s %s %s profiles=%s\n", layer.Scope, layer.Path, layer.Model, strings.Join(layer.Profiles, ","))
	}
	for _, capability := range result.Capabilities {
		fmt.Fprintf(&text, "%s %s selected=%t baseline=%t available=%t\n", capability.Kind, capability.ID, capability.Selected, capability.BaselineEnabled, capability.Available)
	}
	for _, issue := range result.PlanIssues {
		fmt.Fprintf(&text, "Plan issue: %s\n", issue)
	}
	if receipt := result.LastApplication; receipt != nil {
		fmt.Fprintf(&text, "Last application: %s (%s, state: %s)\n", receipt.Profile, receipt.Mode, receipt.State)
	}
	text.WriteString("This inspection reads settings and metadata; it does not read or rewrite chat history.\n")
	_, err = io.WriteString(output, text.String())
	return err
}
