package dots

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

type cloudHarnessProfilePlan struct {
	config              cloudMessage
	plugins             []string
	unsupported, issues []string
	key                 string
}

func cloudRuntimeHarnessPolicy(fsys fsutil.FileSystem, path, cwd string) (harness.Policy, error) {
	policy, err := harness.Load(fsys, path)
	if err != nil || !policy.Enabled {
		return policy, err
	}
	return harness.LoadForContext(fsys, path, cwd)
}

func (h *cloudHarness) readPolicy(cwd string) (harness.Policy, error) {
	if h.b.deps.readHarnessContextPolicy != nil {
		return h.b.deps.readHarnessContextPolicy(cwd)
	}
	if h.b.deps.readHarnessPolicy == nil {
		return harness.Policy{}, errors.New("harness policy unavailable")
	}
	return h.b.deps.readHarnessPolicy()
}

func cloudHarnessProfileState(plan *cloudHarnessProfilePlan, runtimeIssues ...string) string {
	if len(runtimeIssues) > 0 || (plan != nil && (len(plan.unsupported) > 0 || len(plan.issues) > 0)) {
		return "partial"
	}
	return "applied"
}

func cloudHarnessManualPlugins(thread *cloudHarnessThread, raw json.RawMessage) {
	var actual []string
	if json.Unmarshal(raw, &actual) != nil {
		return
	}
	if thread.profileManaged && thread.profilePlan != nil {
		// Safe plugin settings may have preceded the full configuration reload.
		owned := thread.profilePlan.plugins
		if thread.appliedPlugins != nil {
			owned = thread.appliedPlugins
		}
		if cloudHarnessPluginEqual(actual, owned) || (thread.pendingProfile != nil && cloudHarnessPluginEqual(actual, thread.pendingProfile.plugins)) {
			thread.plugins = slices.Clone(actual)
			thread.pluginsKnown = true
			return
		}
		var original []string
		_ = json.Unmarshal(thread.baseline["disabledPluginIds"], &original)
		baseline, applied, current := make(map[string]bool), make(map[string]bool), make(map[string]bool)
		for _, id := range original {
			baseline[id] = true
		}
		for _, id := range owned {
			applied[id] = true
		}
		for _, id := range actual {
			current[id] = true
		}
		for id := range applied {
			if !current[id] {
				delete(baseline, id)
			}
		}
		for id := range current {
			if !applied[id] {
				baseline[id] = true
			}
		}
		original = nil
		for id := range baseline {
			original = append(original, id)
		}
		slices.Sort(original)
		thread.baseline["disabledPluginIds"] = cloudHarnessRaw(original)
	} else {
		thread.baseline["disabledPluginIds"] = bytes.Clone(raw)
	}
	thread.plugins = slices.Clone(actual)
	thread.pluginsKnown = true
	thread.profileRefresh = true
}

func cloudHarnessBool(config cloudMessage, key string, fallback bool) bool {
	raw, ok := config[key]
	if !ok {
		return fallback
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return fallback
	}
	return value
}

func cloudHarnessSkillSettings(raw json.RawMessage) map[string]cloudMessage {
	result := make(map[string]cloudMessage)
	var entries []cloudMessage
	_ = json.Unmarshal(raw, &entries)
	for _, entry := range entries {
		if path := cloudHarnessString(entry, "path"); path != "" {
			result[path] = cloudHarnessClone(entry)
		}
	}
	return result
}

func cloudHarnessCompileProfile(effective harness.Effective, inventory harness.Inventory, baseline cloudMessage, excluded []string) cloudHarnessProfilePlan {
	plan := cloudHarnessProfilePlan{config: cloudHarnessClone(baseline), plugins: make([]string, 0)}
	pluginBaseline := make(map[string]bool)
	for _, id := range excluded {
		pluginBaseline[id] = true
	}
	skillBaseline := cloudHarnessSkillSettings(baseline["skills.config"])
	pluginsEnabled := cloudHarnessBool(baseline, "features.plugins", true)
	skillsIncluded := cloudHarnessBool(baseline, "skills.include_instructions", true)
	selected := harness.SelectCapabilities(effective, inventory, func(capability harness.Capability) bool {
		enabled := capability.Enabled
		switch capability.Kind {
		case harness.PluginKind:
			enabled = enabled && pluginsEnabled && !pluginBaseline[capability.NativeID]
		case harness.MCPKind:
			enabled = cloudHarnessBool(baseline, cloudHarnessMCPKey(capability.NativeID, "enabled"), enabled)
		case harness.SkillKind:
			enabled = enabled && skillsIncluded
			if setting, ok := skillBaseline[capability.Path]; ok {
				enabled = cloudHarnessBool(setting, "enabled", capability.Enabled) && skillsIncluded
			}
		}
		if capability.PluginID != "" {
			enabled = enabled && pluginsEnabled && !pluginBaseline[capability.PluginID]
		}
		return enabled
	})
	plan.issues = slices.Clone(selected.Issues)
	exclusions := make(map[string]bool)
	for _, id := range excluded {
		exclusions[id] = true
	}
	skills := cloudHarnessSkillSettings(baseline["skills.config"])
	anyPlugin, anySkill := false, false
	for _, capability := range inventory.Capabilities {
		wanted := selected.Wants(capability.Kind, capability.ID)
		switch capability.Kind {
		case harness.PluginKind:
			if wanted && !capability.Enabled {
				plan.unsupported = append(plan.unsupported, "plugin-enable:"+capability.ID)
				wanted = false
			}
			if capability.NativeID == "" {
				if wanted {
					plan.unsupported = append(plan.unsupported, "plugin:"+capability.ID)
				}
				continue
			}
			if wanted {
				delete(exclusions, capability.NativeID)
				anyPlugin = true
			} else {
				exclusions[capability.NativeID] = true
			}
		case harness.MCPKind:
			if capability.NativeID == "" || capability.PluginID != "" {
				plan.unsupported = append(plan.unsupported, "mcp:"+capability.ID)
				continue
			}
			plan.config[cloudHarnessMCPKey(capability.NativeID, "enabled")] = cloudHarnessRaw(wanted)
		case harness.SkillKind:
			if !filepath.IsAbs(capability.Path) || filepath.Base(capability.Path) != "SKILL.md" {
				plan.unsupported = append(plan.unsupported, "skill:"+capability.ID)
				continue
			}
			entry := skills[capability.Path]
			if entry == nil {
				entry = make(cloudMessage)
			}
			entry["path"] = cloudHarnessRaw(capability.Path)
			entry["enabled"] = cloudHarnessRaw(wanted)
			skills[capability.Path] = entry
			anySkill = anySkill || wanted
		}
	}
	cloudHarnessCompileApps(effective, inventory, selected, baseline, plan.config)
	plan.config["features.plugins"] = cloudHarnessRaw(anyPlugin || effective.Choose(harness.PluginKind, "*", pluginsEnabled))
	plan.config["skills.include_instructions"] = cloudHarnessRaw(anySkill || effective.Choose(harness.SkillKind, "*", skillsIncluded))
	paths := make([]string, 0, len(skills))
	for path := range skills {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	entries := make([]cloudMessage, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, skills[path])
	}
	plan.config["skills.config"] = cloudHarnessRaw(entries)
	for id := range exclusions {
		plan.plugins = append(plan.plugins, id)
	}
	slices.Sort(plan.plugins)
	slices.Sort(plan.unsupported)
	hash := sha256.Sum256(cloudHarnessRaw(map[string]any{"fingerprint": effective.Selection.Fingerprint, "config": plan.config, "plugins": plan.plugins, "unsupported": plan.unsupported, "issues": plan.issues}))
	plan.key = hex.EncodeToString(hash[:])
	return plan
}

// Explicit arrays clear native same-model overrides on Full restoration. Native
// default absence is an empty skills.config list; all unknown entries survive.
func cloudHarnessBaselineSkillConfig(native cloudMessage, original json.RawMessage) (json.RawMessage, error) {
	path := []string{"skills", "config"}
	if raw := cloudHarnessConfigValue(cloudHarnessObject(original), path); len(raw) > 0 {
		return raw, nil
	}
	if raw := cloudHarnessConfigValue(cloudHarnessObject(native["config"]), path); len(raw) > 0 {
		return raw, nil
	}
	var layers []cloudMessage
	_ = json.Unmarshal(native["layers"], &layers)
	origin := cloudHarnessObject(cloudHarnessObject(native["origins"])["skills.config"])
	version := cloudHarnessString(origin, "version")
	var found json.RawMessage
	for _, layer := range layers {
		if cloudHarnessString(layer, "disabledReason") != "" {
			continue
		}
		raw := cloudHarnessConfigValue(cloudHarnessObject(layer["config"]), path)
		if len(raw) == 0 {
			continue
		}
		if version != "" && cloudHarnessString(layer, "version") == version {
			return raw, nil
		}
		if found != nil && !bytes.Equal(bytes.TrimSpace(found), bytes.TrimSpace(raw)) {
			return nil, errors.New("native skill baseline layer precedence unavailable")
		}
		found = raw
	}
	if found == nil {
		return json.RawMessage("[]"), nil
	}
	return found, nil
}

func (h *cloudHarness) profilePlan(effective harness.Effective, native cloudMessage, snapshot cloudHarnessThread) (cloudHarnessProfilePlan, cloudMessage, error) {
	if h.b.deps.readHarnessInventory == nil && h.b.deps.nativeHarnessInventory == nil {
		return cloudHarnessProfilePlan{}, nil, errors.New("native harness inventory unavailable")
	}
	var inventory harness.Inventory
	var err error
	if h.b.deps.readHarnessInventory != nil {
		inventory, err = h.b.deps.readHarnessInventory(snapshot.cwd)
	} else {
		inventory, err = h.b.deps.nativeHarnessInventory(h.b.ctx, snapshot.cwd)
	}
	if err != nil {
		return cloudHarnessProfilePlan{}, nil, errors.New("native harness inventory unavailable")
	}
	baseline := snapshot.fullConfig
	baselineThreadID := snapshot.id
	if baseline != nil && snapshot.fullConfigCwd != snapshot.cwd {
		baseline = nil
		baselineThreadID = ""
	}
	if baseline == nil {
		baseline, err = h.baselineConfig(native, snapshot.baseline["config"], baselineThreadID)
		if err != nil {
			return cloudHarnessProfilePlan{}, nil, err
		}
	}
	baseline = cloudHarnessClone(baseline)
	if _, ok := baseline["skills.config"]; !ok {
		baseline["skills.config"], err = cloudHarnessBaselineSkillConfig(native, snapshot.baseline["config"])
		if err != nil {
			return cloudHarnessProfilePlan{}, nil, err
		}
	}
	var excluded []string
	_ = json.Unmarshal(snapshot.baseline["disabledPluginIds"], &excluded)
	return cloudHarnessCompileProfile(effective, inventory, baseline, excluded), baseline, nil
}

type cloudHarnessMCPRuntime struct {
	Exposed         int
	Status          string
	DiscoveryFailed bool
}

// Read selected servers through the chat's existing connections. Broad native
// discovery can start unrelated servers. Disabled rows may have empty catalogues.
func (h *cloudHarness) readMCPRuntime(threadID string, wanted map[string]bool) (map[string]cloudHarnessMCPRuntime, error) {
	unavailable := errors.New("native MCP runtime unavailable")
	if threadID == "" || len(wanted) > 400 {
		return nil, unavailable
	}
	counts := make(map[string]cloudHarnessMCPRuntime)
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		if name == "" || len(name) > 4096 || strings.TrimSpace(name) != name || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, unavailable
		}
		names = append(names, name)
	}
	slices.Sort(names)
	totalBytes := 0
	for _, name := range names {
		seenCursors := make(map[string]bool)
		cursor, complete := "", false
		for page := 0; page < 4; page++ {
			params := map[string]any{"threadId": threadID, "serverName": name, "detail": "full", "limit": 100}
			if cursor != "" {
				params["cursor"] = cursor
			}
			result, err := h.call("mcpServerStatus/list", params)
			if err != nil {
				return nil, unavailable
			}
			totalBytes += 2
			for key, raw := range result {
				totalBytes += len(cloudHarnessRaw(key)) + len(raw) + 2
			}
			if totalBytes > 8<<20 {
				return nil, unavailable
			}
			var rows *[]struct {
				Name              string                      `json:"name"`
				PluginID          *string                     `json:"pluginId"`
				Tools             *map[string]json.RawMessage `json:"tools"`
				Resources         *[]json.RawMessage          `json:"resources"`
				ResourceTemplates *[]json.RawMessage          `json:"resourceTemplates"`
				RuntimeStatus     string                      `json:"runtimeStatus"`
				ToolsError        *string                     `json:"toolsError"`
			}
			if json.Unmarshal(result["data"], &rows) != nil || rows == nil || len(*rows) > 100 {
				return nil, unavailable
			}
			for _, row := range *rows {
				if row.Name != name || row.Tools == nil || row.Resources == nil || row.ResourceTemplates == nil {
					return nil, unavailable
				}
				if row.PluginID != nil && *row.PluginID != "" {
					continue
				}
				if _, duplicate := counts[row.Name]; duplicate {
					return nil, unavailable
				}
				counts[row.Name] = cloudHarnessMCPRuntime{Exposed: len(*row.Tools) + len(*row.Resources) + len(*row.ResourceTemplates), Status: row.RuntimeStatus, DiscoveryFailed: row.ToolsError != nil && *row.ToolsError != ""}
			}
			var next *string
			if raw, present := result["nextCursor"]; !present || json.Unmarshal(raw, &next) != nil {
				return nil, unavailable
			}
			if next == nil {
				complete = true
				break
			}
			if *next == "" || len(*next) > 4096 || seenCursors[*next] {
				return nil, unavailable
			}
			cursor, seenCursors[*next] = *next, true
		}
		if !complete {
			return nil, unavailable
		}
	}
	return counts, nil
}

// Only exclusions and explicit disabled-to-enabled restorations need readback.
func cloudHarnessMCPVerificationTargets(desired, previous cloudMessage) (map[string]bool, bool) {
	wanted := make(map[string]bool)
	for key, raw := range desired {
		name, property, ok := cloudHarnessMCPPath(key)
		if !ok || property != "enabled" {
			continue
		}
		raw = bytes.TrimSpace(raw)
		if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
			return nil, false
		}
		enabled := bytes.Equal(raw, []byte("true"))
		if enabled {
			if !bytes.Equal(bytes.TrimSpace(cloudHarnessConfigValue(previous, []string{"mcp_servers", name, "enabled"})), []byte("false")) {
				continue
			}
		}
		if prior, duplicate := wanted[name]; duplicate && prior != enabled {
			return nil, false
		}
		wanted[name] = enabled
	}
	return wanted, true
}

// A successful resume can retain an existing assembly owned by another native
// subscriber. The transition requires a valid pre-reload snapshot; already empty
// catalogues cannot prove that a skill configuration was reassembled.
func (h *cloudHarness) verifyMCPRuntime(threadID string, desired, previous cloudMessage, before map[string]cloudHarnessMCPRuntime) ([]string, bool) {
	wanted, valid := cloudHarnessMCPVerificationTargets(desired, previous)
	if !valid {
		return []string{"native_mcp_runtime_unverified"}, false
	}
	if len(wanted) == 0 {
		return nil, false
	}
	after, err := h.readMCPRuntime(threadID, wanted)
	if err != nil {
		return []string{"native_mcp_runtime_unverified"}, false
	}
	var issues []string
	transition := false
	for name, enabled := range wanted {
		actual, exists := after[name]
		prior, existed := before[name]
		if exists && (actual.DiscoveryFailed || (actual.Status != "connected" && actual.Status != "disabled")) {
			issues = append(issues, "native_mcp_runtime_unverified:"+name)
		} else if !enabled && actual.Exposed > 0 {
			issues = append(issues, "native_mcp_runtime_retained:"+name)
		} else if enabled && (actual.Exposed == 0 || actual.Status != "connected") || !enabled && exists && actual.Status != "disabled" {
			issues = append(issues, "native_mcp_runtime_unverified:"+name)
		} else if before != nil && !prior.DiscoveryFailed && ((!enabled && prior.Status == "connected" && prior.Exposed > 0) || (enabled && (!existed || prior.Status == "disabled") && prior.Exposed == 0)) {
			transition = true
		}
	}
	slices.Sort(issues)
	return issues, transition
}
