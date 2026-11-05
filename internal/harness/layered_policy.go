package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

type Kind string

const (
	PluginKind Kind = "plugin"
	MCPKind    Kind = "mcp"
	SkillKind  Kind = "skill"
)

type Decision string

const (
	Enabled  Decision = "enabled"
	Disabled Decision = "disabled"
	Inherit  Decision = "inherit"
)

type Changes struct {
	Reset   bool                `json:"reset,omitempty"`
	Default Decision            `json:"default,omitempty"`
	Plugins map[string]Decision `json:"plugins,omitempty"`
	MCP     map[string]Decision `json:"mcp,omitempty"`
	Skills  map[string]Decision `json:"skills,omitempty"`
}

type Definition struct {
	Description string  `json:"description,omitempty"`
	Changes     Changes `json:"changes"`
}

type Binding struct {
	Profiles []string `json:"profiles,omitempty"`
	Changes  Changes  `json:"changes,omitempty"`
}

type Scope struct {
	All    Binding            `json:"all,omitempty"`
	Models map[string]Binding `json:"models,omitempty"`
}

type Layer struct {
	Scope    string   `json:"scope"`
	Path     string   `json:"path,omitempty"`
	Model    string   `json:"model,omitempty"`
	Profiles []string `json:"profiles,omitempty"`
	Changes  Changes  `json:"changes"`
}

type Effective struct {
	Selection Selection `json:"selection"`
	Changes   Changes   `json:"changes"`
	Layers    []Layer   `json:"layers,omitempty"`
}

type WorkspacePolicy struct {
	Version  int                   `json:"version"`
	Profiles map[string]Definition `json:"profiles,omitempty"`
	Rules    Scope                 `json:"rules"`
}

type workspaceLayer struct {
	path   string
	policy WorkspacePolicy
}

// ResolveEffective folds matching bindings from broad scopes to narrow scopes.
func (policy Policy) ResolveEffective(model, cwd, chatID string) Effective {
	if policy.Version != 2 {
		selection := policy.resolveLegacy(model, cwd, chatID)
		effective := Effective{Selection: selection}
		if selection.Managed && selection.Profile == Minimal {
			effective.Changes.Default = Disabled
		}
		return effective
	}
	effective := Effective{Selection: Selection{Profile: Full, Source: "model"}}
	if !policy.Enabled {
		effective.Selection.Source = "disabled"
		return effective
	}
	valid := applyScope(&effective, policy.Global, model, "global", "", policy.Profiles)
	if filepath.IsAbs(cwd) {
		cwd = filepath.Clean(cwd)
		directories := make(map[string]bool)
		for path := range policy.Workspaces {
			if ancestorPath(path, cwd) {
				directories[path] = true
			}
		}
		for _, file := range policy.workspaceFiles {
			path := filepath.Dir(filepath.Dir(file.path))
			if ancestorPath(path, cwd) {
				directories[path] = true
			}
		}
		paths := make([]string, 0, len(directories))
		for path := range directories {
			paths = append(paths, path)
		}
		sort.Slice(paths, func(i, j int) bool { return len(paths[i]) < len(paths[j]) })
		for _, path := range paths {
			if scope, ok := policy.Workspaces[path]; ok {
				valid = applyScope(&effective, scope, model, "workspace", path, policy.Profiles) && valid
			}
			for _, file := range policy.workspaceFiles {
				if filepath.Dir(filepath.Dir(file.path)) == path {
					valid = applyScope(&effective, file.policy.Rules, model, "workspace", file.path, mergedDefinitions(policy.Profiles, file.policy.Profiles)) && valid
				}
			}
		}
	}
	if scope, ok := policy.Tasks[chatID]; ok {
		valid = applyScope(&effective, scope, model, "task", chatID, policy.Profiles) && valid
	}
	if !valid {
		return Effective{Selection: Selection{Profile: Full, Source: "invalid"}}
	}
	if !effective.Selection.Managed {
		return effective
	}
	effective.Changes.Reset = false
	effective.Selection.Profile = Custom
	if len(effective.Changes.Plugins) == 0 && len(effective.Changes.MCP) == 0 && len(effective.Changes.Skills) == 0 {
		switch effective.Changes.Default {
		case "", Inherit:
			effective.Selection.Profile = Full
		case Disabled:
			effective.Selection.Profile = Minimal
		}
	}
	data, _ := json.Marshal(effective.Changes)
	digest := sha256.Sum256(data)
	effective.Selection.Fingerprint = hex.EncodeToString(digest[:])
	return effective
}

func applyScope(effective *Effective, scope Scope, model, source, path string, definitions map[string]Definition) bool {
	selectors := make([]string, 0, len(scope.Models))
	lowerModel := strings.ToLower(model)
	for selector := range scope.Models {
		lower := strings.ToLower(selector)
		if lower == lowerModel || strings.HasSuffix(lower, "*") && strings.HasPrefix(lowerModel, strings.TrimSuffix(lower, "*")) {
			selectors = append(selectors, selector)
		}
	}
	sort.Slice(selectors, func(i, j int) bool {
		a, b := selectors[i], selectors[j]
		if strings.HasSuffix(a, "*") != strings.HasSuffix(b, "*") {
			return strings.HasSuffix(a, "*")
		}
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	selectors = append([]string{""}, selectors...)
	for _, selector := range selectors {
		binding := scope.All
		if selector != "" {
			binding = scope.Models[selector]
		}
		if emptyBinding(binding) {
			continue
		}
		var expanded Changes
		for _, name := range binding.Profiles {
			switch name {
			case "full":
				mergeChanges(&expanded, Changes{Reset: true})
			case "minimal":
				mergeChanges(&expanded, Changes{Default: Disabled})
			default:
				definition, ok := definitions[name]
				if !ok {
					return false
				}
				mergeChanges(&expanded, definition.Changes)
			}
		}
		mergeChanges(&expanded, binding.Changes)
		mergeChanges(&effective.Changes, expanded)
		effective.Layers = append(effective.Layers, Layer{Scope: source, Path: path, Model: selector, Profiles: append([]string(nil), binding.Profiles...), Changes: expanded})
		effective.Selection = Selection{Managed: true, Source: source, Scope: path}
	}
	return true
}

// Choose preserves the native baseline when no CQ decision applies.
func (effective Effective) Choose(kind Kind, id string, baseline bool) bool {
	var decisions map[string]Decision
	switch kind {
	case PluginKind:
		decisions = effective.Changes.Plugins
	case MCPKind:
		decisions = effective.Changes.MCP
	case SkillKind:
		decisions = effective.Changes.Skills
	default:
		return baseline
	}
	for _, decision := range []Decision{decisions[id], decisions["*"], effective.Changes.Default} {
		switch decision {
		case Enabled:
			return true
		case Disabled:
			return false
		}
	}
	return baseline
}

func mergeChanges(target *Changes, source Changes) {
	if source.Reset {
		*target = Changes{Reset: true}
	}
	if source.Default != "" && source.Default != Inherit {
		target.Default = source.Default
	}
	merge := func(target *map[string]Decision, source map[string]Decision) {
		for id, decision := range source {
			if decision == Inherit {
				continue
			}
			if *target == nil {
				*target = make(map[string]Decision)
			}
			(*target)[id] = decision
		}
	}
	merge(&target.Plugins, source.Plugins)
	merge(&target.MCP, source.MCP)
	merge(&target.Skills, source.Skills)
}

func ancestorPath(parent, path string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func emptyChanges(changes Changes) bool {
	return !changes.Reset && changes.Default == "" && len(changes.Plugins) == 0 && len(changes.MCP) == 0 && len(changes.Skills) == 0
}
func emptyBinding(binding Binding) bool {
	return len(binding.Profiles) == 0 && emptyChanges(binding.Changes)
}
func emptyScope(scope Scope) bool { return emptyBinding(scope.All) && len(scope.Models) == 0 }
func mergedDefinitions(user, local map[string]Definition) map[string]Definition {
	result := make(map[string]Definition, len(user)+len(local))
	for name, definition := range user {
		result[name] = definition
	}
	for name, definition := range local {
		result[name] = definition
	}
	return result
}

// Definitions returns visible definitions for discovery and export. Runtime
// resolution retains each portable file's namespace for that file's rules.
func (policy Policy) Definitions() map[string]Definition {
	result := mergedDefinitions(nil, policy.Profiles)
	for _, file := range policy.workspaceFiles {
		result = mergedDefinitions(result, file.policy.Profiles)
	}
	result["full"] = Definition{Description: "Preserve native capability choices", Changes: Changes{Reset: true}}
	result["minimal"] = Definition{Description: "Disable capabilities by default", Changes: Changes{Default: Disabled}}
	return result
}

// Promote preserves active v1 GLM defaults and explicit scoped overrides in v2.
func Promote(policy *Policy) error {
	if policy == nil {
		return fmt.Errorf("harness policy is required")
	}
	if err := validatePolicy(*policy); err != nil {
		return err
	}
	if policy.Version == 2 {
		return nil
	}
	converted := Policy{Version: 2, Enabled: policy.Enabled, Profiles: make(map[string]Definition), Workspaces: make(map[string]Scope), Tasks: make(map[string]Scope)}
	if policy.Enabled {
		converted.Global = Scope{Models: map[string]Binding{"glm-*": {Profiles: []string{"minimal"}}}}
	}
	for path, profile := range policy.Projects {
		converted.Workspaces[path] = Scope{Models: map[string]Binding{"glm-*": {Profiles: []string{string(profile)}}}}
	}
	for chat, profile := range policy.Chats {
		converted.Tasks[chat] = Scope{Models: map[string]Binding{"glm-*": {Profiles: []string{string(profile)}}}}
	}
	*policy = converted
	return nil
}

func validateDefinitions(definitions map[string]Definition) error {
	for name, definition := range definitions {
		if !validProfileName(name) || name == "full" || name == "minimal" {
			return fmt.Errorf("invalid or reserved harness profile name %q", name)
		}
		if err := validateChanges(definition.Changes); err != nil {
			return fmt.Errorf("profile %s: %w", name, err)
		}
	}
	return nil
}

// ValidateDefinition checks a user-defined profile before a storage operation.
func ValidateDefinition(name string, definition Definition) error {
	return validateDefinitions(map[string]Definition{name: definition})
}

// ValidateBinding validates structure; reference resolution needs a profile namespace.
func ValidateBinding(binding Binding) error { return validateScope(Scope{All: binding}, nil, false) }

// ValidateScope checks selectors and binding structure before storage.
func ValidateScope(scope Scope) error { return validateScope(scope, nil, false) }
func validProfileName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if c > 127 || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '-' || c == '_' || c == '.')) {
			return false
		}
	}
	return true
}
func validIdentifier(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && !strings.ContainsFunc(id, unicode.IsControl)
}
func validateChanges(changes Changes) error {
	valid := func(decision Decision) bool {
		return decision == Enabled || decision == Disabled || decision == Inherit
	}
	if changes.Default != "" && !valid(changes.Default) {
		return fmt.Errorf("invalid harness default decision %q", changes.Default)
	}
	for _, items := range []map[string]Decision{changes.Plugins, changes.MCP, changes.Skills} {
		for id, decision := range items {
			if !validIdentifier(id) || !valid(decision) {
				return fmt.Errorf("invalid harness capability decision for %q", id)
			}
		}
	}
	return nil
}
func validateScope(scope Scope, definitions map[string]Definition, requireReferences bool) error {
	validateBinding := func(binding Binding) error {
		if err := validateChanges(binding.Changes); err != nil {
			return err
		}
		for _, name := range binding.Profiles {
			if !validProfileName(name) {
				return fmt.Errorf("invalid harness profile reference %q", name)
			}
			if name == "full" || name == "minimal" {
				continue
			}
			if _, ok := definitions[name]; requireReferences && !ok {
				return fmt.Errorf("unknown harness profile %q", name)
			}
		}
		return nil
	}
	if err := validateBinding(scope.All); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for selector, binding := range scope.Models {
		lower := strings.ToLower(selector)
		if !validIdentifier(selector) || strings.ContainsFunc(selector, unicode.IsSpace) || strings.Count(selector, "*") > 1 || strings.Contains(strings.TrimSuffix(selector, "*"), "*") || seen[lower] {
			return fmt.Errorf("invalid or ambiguous harness model selector %q", selector)
		}
		seen[lower] = true
		if err := validateBinding(binding); err != nil {
			return fmt.Errorf("model %s: %w", selector, err)
		}
	}
	return nil
}
