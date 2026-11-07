package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func cachedPluginFixture(t *testing.T) (map[string]json.RawMessage, nativeSkillMetadata) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(nativeCurationFixture), &envelope); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plugins", "cache", "local-tools", "work-kit", "1.2.3", "instructions", "analyse", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unchanged cached skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled, description := true, "Cached plugin skill"
	skill := nativeSkillMetadata{Name: "work-kit:analyse", PluginID: "work-kit@local-tools", Scope: "user", Enabled: &enabled, Description: &description, Path: path}
	var skills map[string]any
	if err := json.Unmarshal(envelope["skills"], &skills); err != nil {
		t.Fatal(err)
	}
	row := skills["data"].([]any)[0].(map[string]any)
	row["skills"] = append(row["skills"].([]any), skill)
	envelope["skills"], _ = json.Marshal(skills)
	return envelope, skill
}

func cachedPluginDiscovery(t *testing.T, envelope map[string]json.RawMessage) *nativeCachedPluginRecovery {
	t.Helper()
	recovery, err := discoverNativeCachedPlugins(envelope["plugins"], envelope["skills"], envelope["config"], "/work")
	if err != nil {
		if runtime.GOOS == "windows" && strings.Contains(err.Error(), "prepare native cached plugin catalogue") {
			t.Skip("Windows fixture requires local symbolic-link permission")
		}
		t.Fatal(err)
	}
	if recovery == nil {
		t.Fatal("explicit cached owner was not discovered")
	}
	t.Cleanup(recovery.close)
	return recovery
}

func cachedPluginRecovered(t *testing.T, recovery *nativeCachedPluginRecovery) map[string]any {
	t.Helper()
	owner := recovery.owners["work-kit@local-tools"]
	return map[string]any{"marketplaceLoadErrors": []any{}, "marketplaces": []any{map[string]any{"name": owner.marketplace, "path": owner.manifest, "plugins": []any{map[string]any{
		"id": "work-kit@local-tools", "name": owner.name, "installed": true, "enabled": true, "availability": "AVAILABLE",
		"source": map[string]any{"type": "local", "path": owner.source}, "private": map[string]string{"TOKEN": "private-secret"},
	}}}}}
}

func TestNativeCachedPluginsRecoveredExplicitOwnersPrivately(t *testing.T) {
	envelope, skill := cachedPluginFixture(t)
	recovery := cachedPluginDiscovery(t, envelope)
	if len(recovery.requests) != 1 || recovery.requests[0].method != "plugin/installed" || recovery.requests[0].name != "cachedPlugins" {
		t.Fatal("cached owner discovery lost its independent native request")
	}
	cwds := recovery.requests[0].params.(map[string]any)["cwds"].([]string)
	if len(cwds) != 1 || cwds[0] == "/work" {
		t.Fatal("cached marketplace discovery must use its sole private project")
	}
	root := cwds[0]
	owner := recovery.owners[skill.PluginID]
	var descriptor struct {
		Name    string `json:"name"`
		Plugins []struct{ Name, Source string }
	}
	data, err := os.ReadFile(owner.manifest)
	if err != nil || json.Unmarshal(data, &descriptor) != nil || descriptor.Name != "local-tools" || len(descriptor.Plugins) != 1 || descriptor.Plugins[0].Name != "work-kit" || descriptor.Plugins[0].Source != "./plugins/work-kit" {
		t.Fatal("private native descriptor changed canonical plugin identity")
	}
	if target, err := os.Readlink(owner.source); err != nil || target != owner.cache {
		t.Fatal("native descriptor did not reference the exact existing cache")
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "HEAD")); err != nil {
		t.Fatal("private repository discovery marker missing")
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{root: 0o700, owner.manifest: 0o600} {
			if stat, err := os.Stat(path); err != nil || stat.Mode().Perm() != mode {
				t.Fatal("private catalogue permissions widened")
			}
		}
	}
	recovered, _ := json.Marshal(cachedPluginRecovered(t, recovery))
	merged, changed := recovery.merge(envelope["plugins"], recovered)
	if !changed {
		t.Fatal("native exact-owner metadata was not merged")
	}
	envelope["plugins"] = merged
	var details map[string]json.RawMessage
	_ = json.Unmarshal(envelope["pluginDetails"], &details)
	details[skill.PluginID] = json.RawMessage(`{"plugin":{"summary":{"id":"work-kit@local-tools","name":"work-kit"},"mcpServers":[],"apps":[]}}`)
	envelope["pluginDetails"], _ = json.Marshal(details)
	encoded, _ := json.Marshal(envelope)
	inventory, err := decodeNativeInventory(encoded, "/work")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := StableSkillID(skill.Scope, skill.Name, skill.PluginID)
	if !slices.ContainsFunc(inventory.Capabilities, func(capability Capability) bool {
		return capability.ID == id && capability.Available && capability.PluginID == skill.PluginID
	}) {
		t.Fatal("cached skill lost its stable ID or remained unavailable")
	}
	if slices.ContainsFunc(inventory.Issues, func(issue InventoryIssue) bool { return issue.ID == id || issue.ID == skill.PluginID }) {
		t.Fatal("complete native cached ownership retained a gap")
	}
	public, _ := json.Marshal(inventory)
	for _, private := range []string{root, "private-secret", owner.manifest} {
		if bytes.Contains(public, []byte(private)) {
			t.Fatal("private recovery metadata leaked into public inventory")
		}
	}
	if _, changed := recovery.merge(merged, recovered); changed {
		t.Fatal("replayed recovery replaced an existing owner")
	}
	recovery.close()
	recovery.close()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("private recovery catalogue survived cleanup")
	}
	if data, err := os.ReadFile(skill.Path); err != nil || string(data) != "unchanged cached skill\n" {
		t.Fatal("recovery cleanup modified the original cached package")
	}
}

func TestNativeCachedPluginsPreservedDisabledOwnerMetadata(t *testing.T) {
	envelope, _ := cachedPluginFixture(t)
	recovery := cachedPluginDiscovery(t, envelope)
	response := cachedPluginRecovered(t, recovery)
	plugin := response["marketplaces"].([]any)[0].(map[string]any)["plugins"].([]any)[0].(map[string]any)
	plugin["enabled"], plugin["availability"] = false, "UNAVAILABLE"
	data, _ := json.Marshal(response)
	merged, changed := recovery.merge(envelope["plugins"], data)
	var parsed nativePluginInventory
	if !changed || json.Unmarshal(merged, &parsed) != nil {
		t.Fatal("disabled owner metadata was discarded")
	}
	for _, marketplace := range *parsed.Marketplaces {
		for _, plugin := range *marketplace.Plugins {
			if plugin.ID == "work-kit@local-tools" && (*plugin.Enabled || *plugin.Availability != "UNAVAILABLE") {
				t.Fatal("recovery enabled or widened a disabled native owner")
			}
		}
	}
}

func TestNativeCachedPluginsKeptOriginalProjectDisabled(t *testing.T) {
	for _, test := range []struct {
		name             string
		original, native bool
	}{
		{"project disabled", false, true},
		{"native disabled", true, false},
		{"both disabled", false, false},
		{"both enabled", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, _ := cachedPluginFixture(t)
			var config map[string]any
			_ = json.Unmarshal(envelope["config"], &config)
			config["config"].(map[string]any)["plugins"] = map[string]any{"work-kit@local-tools": map[string]bool{"enabled": test.original}}
			envelope["config"], _ = json.Marshal(config)
			recovery := cachedPluginDiscovery(t, envelope)
			response := cachedPluginRecovered(t, recovery)
			plugin := response["marketplaces"].([]any)[0].(map[string]any)["plugins"].([]any)[0].(map[string]any)
			plugin["enabled"], plugin["availability"], plugin["disabledReason"] = test.native, "AVAILABLE", "native reason"
			data, _ := json.Marshal(response)
			merged, changed := recovery.merge(envelope["plugins"], data)
			var parsed nativePluginInventory
			if !changed || json.Unmarshal(merged, &parsed) != nil {
				t.Fatal("original plugin context discarded native metadata")
			}
			found := false
			for _, marketplace := range *parsed.Marketplaces {
				for _, plugin := range *marketplace.Plugins {
					if plugin.ID != "work-kit@local-tools" {
						continue
					}
					found = true
					if *plugin.Enabled != (test.original && test.native) || *plugin.Availability != "AVAILABLE" || *plugin.DisabledReason != "native reason" {
						t.Fatal("recovery widened original context or changed native status")
					}
				}
			}
			if !found {
				t.Fatal("originally disabled cached owner disappeared")
			}
		})
	}
}

func TestNativeCachedPluginsRejectedInvalidOriginalEnabledFlags(t *testing.T) {
	for _, flag := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`"false"`), json.RawMessage(`0`), json.RawMessage(`{}`)} {
		t.Run(string(flag), func(t *testing.T) {
			envelope, _ := cachedPluginFixture(t)
			envelope["config"] = json.RawMessage(`{"config":{"plugins":{"work-kit@local-tools":{"enabled":` + string(flag) + `}}}}`)
			recovery, err := discoverNativeCachedPlugins(envelope["plugins"], envelope["skills"], envelope["config"], "/work")
			if recovery != nil {
				defer recovery.close()
			}
			if err != nil || recovery != nil {
				t.Fatal("invalid original enabled flag was trusted")
			}
		})
	}
}

func TestNativeCachedPluginsRejectedUntrustedClaims(t *testing.T) {
	for _, scenario := range []string{"wrong-id", "wrong-name", "wrong-marketplace", "wrong-locator", "wrong-source", "wrong-source-type", "uninstalled", "missing-enabled", "missing-installed", "duplicate", "duplicate-foreign", "extra-private-plugin", "load-error", "null-errors", "null-marketplaces", "null-plugins", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			envelope, _ := cachedPluginFixture(t)
			recovery := cachedPluginDiscovery(t, envelope)
			response := cachedPluginRecovered(t, recovery)
			market := response["marketplaces"].([]any)[0].(map[string]any)
			plugin := market["plugins"].([]any)[0].(map[string]any)
			switch scenario {
			case "wrong-id":
				plugin["id"] = "work-kit@another-market"
			case "wrong-name":
				plugin["name"] = "different"
			case "wrong-marketplace":
				market["name"] = "another-market"
			case "wrong-locator":
				market["path"] = "/unrelated/marketplace.json"
			case "wrong-source":
				plugin["source"].(map[string]any)["path"] = "/unrelated/cache"
			case "wrong-source-type":
				plugin["source"].(map[string]any)["type"] = "remote"
			case "uninstalled":
				plugin["installed"] = false
			case "missing-enabled":
				delete(plugin, "enabled")
			case "missing-installed":
				delete(plugin, "installed")
			case "duplicate":
				market["plugins"] = []any{plugin, plugin}
			case "duplicate-foreign":
				response["marketplaces"] = append(response["marketplaces"].([]any), map[string]any{"name": "foreign", "path": "/foreign/catalogue", "plugins": []any{plugin}})
			case "extra-private-plugin":
				market["plugins"] = append(market["plugins"].([]any), map[string]any{"id": "extra@local-tools", "name": "extra", "installed": true, "enabled": true})
			case "load-error":
				response["marketplaceLoadErrors"] = []any{map[string]any{"message": "private-secret"}}
			case "null-errors":
				response["marketplaceLoadErrors"] = nil
			case "null-marketplaces":
				response["marketplaces"] = nil
			case "null-plugins":
				market["plugins"] = nil
			}
			data, _ := json.Marshal(response)
			if scenario == "malformed" {
				data = []byte("not JSON")
			}
			merged, changed := recovery.merge(envelope["plugins"], data)
			if changed || !bytes.Equal(merged, envelope["plugins"]) {
				t.Fatal("untrusted recovery changed original metadata")
			}
		})
	}
}

func TestNativeCachedPluginsRequiredUnambiguousExplicitCacheOwnership(t *testing.T) {
	for _, scenario := range []string{"known-owner", "known-uninstalled", "no-owner", "invalid-owner", "relative-path", "unclean-path", "wrong-cache-owner", "wrong-cache-parent", "missing-file", "conflicting-root"} {
		t.Run(scenario, func(t *testing.T) {
			envelope, skill := cachedPluginFixture(t)
			var rows map[string]any
			_ = json.Unmarshal(envelope["skills"], &rows)
			entry := rows["data"].([]any)[0].(map[string]any)
			var skills []nativeSkillMetadata
			switch scenario {
			case "known-owner", "known-uninstalled":
				skill.PluginID = "one@market"
				if scenario == "known-uninstalled" {
					var plugins map[string]any
					_ = json.Unmarshal(envelope["plugins"], &plugins)
					plugins["marketplaces"].([]any)[0].(map[string]any)["plugins"].([]any)[0].(map[string]any)["installed"] = false
					envelope["plugins"], _ = json.Marshal(plugins)
				}
			case "no-owner":
				skill.PluginID = ""
			case "invalid-owner":
				skill.PluginID = "../work-kit@local-tools"
			case "relative-path":
				skill.Path = "cache/local-tools/work-kit/1.2.3/skills/analyse/SKILL.md"
			case "unclean-path":
				skill.Path = filepath.Dir(skill.Path) + string(filepath.Separator) + ".." + string(filepath.Separator) + "analyse" + string(filepath.Separator) + "SKILL.md"
			case "wrong-cache-owner":
				skill.PluginID = "other@local-tools"
			case "wrong-cache-parent":
				skill.Path = filepath.Join(t.TempDir(), "cache", "local-tools", "work-kit", "1", "instructions", "analyse", "SKILL.md")
				if err := os.MkdirAll(filepath.Dir(skill.Path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(skill.Path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-file":
				if err := os.Remove(skill.Path); err != nil {
					t.Fatal(err)
				}
			case "conflicting-root":
				_, second := cachedPluginFixture(t)
				skills = append(skills, second)
			}
			skills = append(skills, skill)
			entry["skills"] = skills
			envelope["skills"], _ = json.Marshal(rows)
			recovery, err := discoverNativeCachedPlugins(envelope["plugins"], envelope["skills"], envelope["config"], "/work")
			if recovery != nil {
				defer recovery.close()
			}
			if err != nil || recovery != nil {
				t.Fatalf("ambiguous or already-known owner was recovered: %v", err)
			}
		})
	}
}

func TestNativeCachedPluginsKeptMarketplaceDiscoverySeparate(t *testing.T) {
	envelope, skill := cachedPluginFixture(t)
	second := skill
	second.PluginID, second.Name = "other-kit@second-market", "other-kit:analyse"
	second.Path = filepath.Join(t.TempDir(), "plugins", "cache", "second-market", "other-kit", "2", "instructions", "analyse", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(second.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second.Path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	envelope["skills"], _ = json.Marshal(map[string]any{"data": []any{map[string]any{"cwd": "/work", "errors": []any{}, "skills": []nativeSkillMetadata{skill, second}}}})
	recovery := cachedPluginDiscovery(t, envelope)
	if len(recovery.requests) != 2 || len(recovery.roots) != 2 {
		t.Fatal("independent cached marketplaces shared discovery context")
	}
	first, _ := json.Marshal(cachedPluginRecovered(t, recovery))
	merged, changed := recovery.merge(envelope["plugins"], first)
	if !changed {
		t.Fatal("first recovered marketplace was lost")
	}
	owner := recovery.owners[second.PluginID]
	data, _ := json.Marshal(map[string]any{"marketplaceLoadErrors": []any{}, "marketplaces": []any{map[string]any{"name": owner.marketplace, "path": owner.manifest, "plugins": []any{map[string]any{"id": second.PluginID, "name": owner.name, "installed": true, "enabled": true, "source": map[string]string{"type": "local", "path": owner.source}}}}}})
	merged, changed = recovery.merge(merged, data)
	var decoded nativePluginInventory
	if !changed || json.Unmarshal(merged, &decoded) != nil || len(*decoded.Marketplaces) != 3 {
		t.Fatal("later recovery replaced an earlier marketplace")
	}
}

func TestNativeCachedPluginsBoundedOwnerDiscovery(t *testing.T) {
	root := t.TempDir()
	var skills []nativeSkillMetadata
	for i := range 125 {
		name := fmt.Sprintf("kit-%03d", i)
		path := filepath.Join(root, "plugins", "cache", "market", name, "1", "skills", "work", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		skills = append(skills, nativeSkillMetadata{Name: name + ":work", PluginID: name + "@market", Path: path})
	}
	data, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"cwd": "/work", "errors": []any{}, "skills": skills}}})
	recovery, err := discoverNativeCachedPlugins(json.RawMessage(`{"marketplaces":[],"marketplaceLoadErrors":[]}`), data, json.RawMessage(`{"config":{}}`), "/work")
	if recovery != nil {
		defer recovery.close()
	}
	if err != nil || recovery != nil {
		t.Fatal("native cached owner discovery exceeded fixed bound")
	}
}
