package dots

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

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

func cloudHarnessProfileState(plan *cloudHarnessProfilePlan) string {
	if plan != nil && (len(plan.unsupported) > 0 || len(plan.issues) > 0) {
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
