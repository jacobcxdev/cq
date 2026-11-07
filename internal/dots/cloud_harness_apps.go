package dots

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jacobcxdev/cq/internal/harness"
)

// Apps use an independent native feature gate. Plugin exclusion alone does not
// remove connected app tools. Keep ownership in the existing plugin selectors.
func cloudHarnessCompileApps(effective harness.Effective, inventory harness.Inventory, selected harness.CapabilitySelection, baseline, config cloudMessage) {
	if len(effective.Changes.Plugins) == 0 && (effective.Changes.Default == "" || effective.Changes.Default == harness.Inherit) {
		return
	}
	apps := cloudHarnessAppSettings(baseline)
	defaults := cloudHarnessObject(apps["_default"])
	defaultEnabled := cloudHarnessBool(defaults, "enabled", true)
	gate := cloudHarnessBool(baseline, "features.apps", true)
	unowned := effective.Choose(harness.PluginKind, "*", true)
	owners := make(map[string]bool)
	for _, capability := range inventory.Capabilities {
		if capability.Kind != harness.PluginKind {
			continue
		}
		eligible := selected.Wants(harness.PluginKind, capability.ID) && capability.Enabled && capability.Available
		for _, id := range capability.AppIDs {
			owners[id] = owners[id] || eligible
		}
	}
	for id := range owners {
		if _, present := apps[id]; !present {
			apps[id] = cloudHarnessRaw(cloudMessage{})
		}
	}
	anyApp := false
	for id, raw := range apps {
		if id == "_default" {
			continue
		}
		entry := cloudHarnessObject(raw)
		allowed, owned := owners[id]
		if !owned {
			allowed = unowned
		}
		wanted := gate && allowed && cloudHarnessBool(entry, "enabled", defaultEnabled)
		entry["enabled"] = cloudHarnessRaw(wanted)
		apps[id] = cloudHarnessRaw(entry)
		anyApp = anyApp || wanted
	}
	defaults["enabled"] = cloudHarnessRaw(gate && defaultEnabled && unowned)
	apps["_default"] = cloudHarnessRaw(defaults)
	// Canonicalise enabled overrides so a quoted or literal request override
	// cannot win over the curated table. Other approval/tool overrides survive.
	for key := range config {
		if _, property, ok := cloudHarnessAppPath(key); ok && (property == "" || property == "enabled") {
			delete(config, key)
		}
	}
	config["apps"] = cloudHarnessRaw(apps)
	config["features.apps"] = cloudHarnessRaw(gate && (anyApp || defaultEnabled && unowned))
}

func cloudHarnessAppPath(key string) (string, string, bool) {
	if !strings.HasPrefix(key, "apps.") {
		return "", "", false
	}
	return cloudHarnessMCPPath("mcp_servers." + strings.TrimPrefix(key, "apps."))
}

func cloudHarnessMergeAppSettings(target, override cloudMessage) {
	for key, raw := range override {
		var object cloudMessage
		if json.Unmarshal(raw, &object) == nil && object != nil {
			entry := cloudHarnessObject(target[key])
			cloudHarnessMergeAppSettings(entry, object)
			target[key] = cloudHarnessRaw(entry)
		} else {
			target[key] = bytes.Clone(raw)
		}
	}
}

func cloudHarnessAppSettings(config cloudMessage) cloudMessage {
	apps := cloudHarnessClone(cloudHarnessObject(config["apps"]))
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		raw := config[key]
		id, property, ok := cloudHarnessAppPath(key)
		if !ok || property != "" && property != "enabled" {
			continue
		}
		entry := cloudHarnessObject(apps[id])
		if property == "enabled" {
			entry["enabled"] = bytes.Clone(raw)
		} else {
			cloudHarnessMergeAppSettings(entry, cloudHarnessObject(raw))
		}
		apps[id] = cloudHarnessRaw(entry)
	}
	return apps
}

// Capture the full table, including approvals and tool/account settings. An
// explicit empty table restores native defaults on a same-model reload.
func cloudHarnessBaselineApps(native, original, restore cloudMessage) error {
	if err := cloudHarnessValidateAppOverrides(original); err != nil {
		return err
	}
	gate, found, err := cloudHarnessBaselineBool(native, original, []string{"features", "apps"})
	if err != nil {
		return err
	}
	if !found {
		gate = true // The qualified native Feature::Apps default.
	}
	config := cloudHarnessObject(native["config"])
	raw := config["apps"]
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = nil
		var layers []cloudMessage
		_ = json.Unmarshal(native["layers"], &layers)
		for _, layer := range layers {
			if cloudHarnessString(layer, "disabledReason") != "" {
				continue
			}
			next := cloudHarnessObject(layer["config"])["apps"]
			if len(next) == 0 {
				continue
			}
			if len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(next)) {
				return errors.New("native app baseline layer precedence unavailable")
			}
			raw = next
		}
	}
	apps := make(cloudMessage)
	if len(raw) > 0 && (json.Unmarshal(raw, &apps) != nil || apps == nil) {
		return errors.New("native app baseline is not a table")
	}
	if raw, present := original["apps"]; present {
		var override cloudMessage
		if json.Unmarshal(raw, &override) != nil || override == nil {
			return errors.New("native app baseline is not a table")
		}
		cloudHarnessMergeAppSettings(apps, override)
	}
	merged := cloudHarnessClone(original)
	merged["apps"] = cloudHarnessRaw(apps)
	apps = cloudHarnessAppSettings(merged)
	for _, raw := range apps {
		var entry cloudMessage
		if json.Unmarshal(raw, &entry) != nil || entry == nil {
			return errors.New("native app baseline entry is not a table")
		}
		if setting, present := entry["enabled"]; present {
			setting = bytes.TrimSpace(setting)
			if !bytes.Equal(setting, []byte("true")) && !bytes.Equal(setting, []byte("false")) {
				return errors.New("native app baseline setting is not boolean")
			}
		}
	}
	restore["apps"] = cloudHarnessRaw(apps)
	restore["features.apps"] = cloudHarnessRaw(gate)
	return nil
}

// Reject contradictory table, dotted, literal and quoted enabled aliases rather
// than guessing native precedence. Global defaults and per-app IDs stay distinct.
func cloudHarnessValidateAppOverrides(config cloudMessage) error {
	seen, values := make(map[string]bool), make(map[string]bool)
	add := func(id string, raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		raw = bytes.TrimSpace(raw)
		if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
			return errors.New("native app baseline setting is not boolean")
		}
		value := bytes.Equal(raw, []byte("true"))
		if seen[id] && values[id] != value {
			return errors.New("native app baseline enabled overrides conflict")
		}
		seen[id], values[id] = true, value
		return nil
	}
	for id, raw := range cloudHarnessObject(config["apps"]) {
		if err := add(id, cloudHarnessObject(raw)["enabled"]); err != nil {
			return err
		}
	}
	for key, raw := range config {
		id, property, ok := cloudHarnessAppPath(key)
		if !ok {
			continue
		}
		if property == "" {
			raw = cloudHarnessObject(raw)["enabled"]
		} else if property != "enabled" {
			continue
		}
		if err := add(id, raw); err != nil {
			return err
		}
	}
	return nil
}

func (h *cloudHarness) readAppsRuntime(threadID string) (bool, error) {
	return h.readRuntimeFeature(threadID, "apps")
}

func cloudHarnessAppsChanged(previous, desired cloudMessage) bool {
	if cloudHarnessBool(previous, "features.apps", true) != cloudHarnessBool(desired, "features.apps", true) {
		return true
	}
	before, after := cloudHarnessAppSettings(previous), cloudHarnessAppSettings(desired)
	beforeDefault := cloudHarnessBool(cloudHarnessObject(before["_default"]), "enabled", true)
	afterDefault := cloudHarnessBool(cloudHarnessObject(after["_default"]), "enabled", true)
	if beforeDefault != afterDefault {
		return true
	}
	for id := range before {
		if _, present := after[id]; !present {
			after[id] = cloudHarnessRaw(cloudMessage{})
		}
	}
	for id, entry := range after {
		if id != "_default" && cloudHarnessBool(cloudHarnessObject(before[id]), "enabled", beforeDefault) != cloudHarnessBool(cloudHarnessObject(entry), "enabled", afterDefault) {
			return true
		}
	}
	return false
}
