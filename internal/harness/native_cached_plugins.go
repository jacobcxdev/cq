package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Cached plugin skills can outlive their marketplace registration. Let the
// native reader inspect those exact packages through private local catalogues.
type nativeCachedPluginRecovery struct {
	requests []nativeCurationRequest
	roots    []string
	owners   map[string]nativeCachedPluginOwner
}

type nativeCachedPluginOwner struct {
	name, marketplace, cache, source, manifest string
	enabled                                    *bool
}

func discoverNativeCachedPlugins(plugins, skills, config json.RawMessage, cwd string) (*nativeCachedPluginRecovery, error) {
	var installed nativePluginInventory
	var skillRows struct {
		Data *[]struct {
			Cwd    string                 `json:"cwd"`
			Errors json.RawMessage        `json:"errors"`
			Skills *[]nativeSkillMetadata `json:"skills"`
		} `json:"data"`
	}
	if json.Unmarshal(plugins, &installed) != nil || installed.Marketplaces == nil || json.Unmarshal(skills, &skillRows) != nil || skillRows.Data == nil {
		return nil, fmt.Errorf("invalid native cached plugin metadata")
	}
	var context struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if json.Unmarshal(config, &context) != nil || context.Config == nil {
		return nil, fmt.Errorf("invalid native cached plugin context")
	}
	var flags map[string]json.RawMessage
	if raw, ok := context.Config["plugins"]; ok {
		if json.Unmarshal(raw, &flags) != nil || flags == nil {
			return nil, nil
		}
	}
	known := make(map[string]bool)
	for _, marketplace := range *installed.Marketplaces {
		if marketplace.Plugins == nil {
			return nil, fmt.Errorf("invalid native cached plugin metadata")
		}
		for _, plugin := range *marketplace.Plugins {
			known[plugin.ID] = true
		}
	}
	owners := make(map[string]nativeCachedPluginOwner)
	ambiguous := make(map[string]bool)
	for _, row := range *skillRows.Data {
		if row.Cwd != cwd || row.Skills == nil {
			return nil, fmt.Errorf("invalid native cached skill project")
		}
		if count, err := nativeMetadataErrorCount(row.Errors, false); err != nil || count > 0 {
			continue
		}
		for _, skill := range *row.Skills {
			if skill.PluginID == "" || known[skill.PluginID] {
				continue
			}
			owner, ok := nativeCachedSkillOwner(skill)
			if !ok {
				ambiguous[skill.PluginID] = true
				continue
			}
			if raw, present := flags[skill.PluginID]; present {
				var entry map[string]json.RawMessage
				if json.Unmarshal(raw, &entry) != nil || entry == nil {
					ambiguous[skill.PluginID] = true
					continue
				}
				if raw, present := entry["enabled"]; present {
					if json.Unmarshal(raw, &owner.enabled) != nil || owner.enabled == nil {
						ambiguous[skill.PluginID] = true
						continue
					}
				}
			}
			if previous, exists := owners[skill.PluginID]; exists && previous.cache != owner.cache {
				ambiguous[skill.PluginID] = true
			}
			owners[skill.PluginID] = owner
		}
	}
	for id := range ambiguous {
		delete(owners, id)
	}
	if len(owners) == 0 || len(owners) > 124 {
		return nil, nil
	}
	groups := make(map[string][]string)
	for id, owner := range owners {
		groups[owner.marketplace] = append(groups[owner.marketplace], id)
	}
	marketplaces := make([]string, 0, len(groups))
	for marketplace := range groups {
		marketplaces = append(marketplaces, marketplace)
	}
	sort.Strings(marketplaces)
	recovery := &nativeCachedPluginRecovery{owners: owners}
	for _, marketplace := range marketplaces {
		root, err := os.MkdirTemp("", "cq-native-plugin-cache-")
		if err != nil {
			recovery.close()
			return nil, fmt.Errorf("prepare native cached plugin catalogue")
		}
		recovery.roots = append(recovery.roots, root)
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			recovery.close()
			return nil, fmt.Errorf("prepare native cached plugin catalogue")
		}
		for _, directory := range []string{".claude-plugin", "plugins", ".git/objects", ".git/refs/heads"} {
			if err = os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
				break
			}
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\nrepositoryformatversion = 0\nbare = false\n"), 0o600)
		}
		manifest := filepath.Join(root, ".claude-plugin", "marketplace.json")
		entries := make([]map[string]string, 0, len(groups[marketplace]))
		sort.Strings(groups[marketplace])
		for _, id := range groups[marketplace] {
			if err != nil {
				break
			}
			owner := owners[id]
			owner.source, owner.manifest = filepath.Join(root, "plugins", owner.name), manifest
			owners[id] = owner
			err = os.Symlink(owner.cache, owner.source)
			entries = append(entries, map[string]string{"name": owner.name, "source": "./plugins/" + owner.name})
		}
		if err == nil {
			data, encodeErr := json.Marshal(map[string]any{"name": marketplace, "owner": map[string]string{"name": "Local cache"}, "plugins": entries})
			if encodeErr != nil {
				err = encodeErr
			} else {
				err = os.WriteFile(manifest, data, 0o600)
			}
		}
		if err != nil {
			recovery.close()
			return nil, fmt.Errorf("prepare native cached plugin catalogue")
		}
		recovery.requests = append(recovery.requests, nativeCurationRequest{method: "plugin/installed", name: "cachedPlugins", params: map[string]any{"cwds": []string{root}}})
	}
	return recovery, nil
}

func nativeCachedSkillOwner(skill nativeSkillMetadata) (nativeCachedPluginOwner, bool) {
	parts := strings.Split(skill.PluginID, "@")
	if len(parts) != 2 || !nativeCachedPluginSegment(parts[0]) || !nativeCachedPluginSegment(parts[1]) || !filepath.IsAbs(skill.Path) || filepath.Clean(skill.Path) != skill.Path || strings.ContainsRune(skill.Path, '\x00') || filepath.Base(skill.Path) != "SKILL.md" {
		return nativeCachedPluginOwner{}, false
	}
	var root string
	for path := filepath.Dir(skill.Path); path != filepath.Dir(path); path = filepath.Dir(path) {
		cache := filepath.Dir(filepath.Dir(filepath.Dir(path)))
		if filepath.Base(filepath.Dir(path)) == parts[0] && filepath.Base(filepath.Dir(filepath.Dir(path))) == parts[1] && filepath.Base(cache) == "cache" && filepath.Base(filepath.Dir(cache)) == "plugins" && nativeCachedPluginSegment(filepath.Base(path)) {
			if root != "" {
				return nativeCachedPluginOwner{}, false
			}
			root = path
		}
	}
	if root == "" {
		return nativeCachedPluginOwner{}, false
	}
	info, err := os.Stat(skill.Path)
	if err != nil || !info.Mode().IsRegular() {
		return nativeCachedPluginOwner{}, false
	}
	return nativeCachedPluginOwner{name: parts[0], marketplace: parts[1], cache: root}, true
}

func nativeCachedPluginSegment(value string) bool {
	return inventoryIdentifier(value) && value != "." && value != ".." && !strings.ContainsAny(value, "/\\:@")
}

func (recovery *nativeCachedPluginRecovery) close() {
	if recovery == nil {
		return
	}
	for _, root := range recovery.roots {
		_ = os.RemoveAll(root)
	}
	recovery.roots = nil
}

func (recovery *nativeCachedPluginRecovery) merge(original, recovered json.RawMessage) (json.RawMessage, bool) {
	var before, after map[string]json.RawMessage
	var existing, candidates []json.RawMessage
	if recovery == nil || len(recovery.roots) == 0 || json.Unmarshal(original, &before) != nil || json.Unmarshal(recovered, &after) != nil || json.Unmarshal(before["marketplaces"], &existing) != nil || existing == nil || json.Unmarshal(after["marketplaces"], &candidates) != nil || candidates == nil {
		return original, false
	}
	if count, err := nativeMetadataErrorCount(after["marketplaceLoadErrors"], false); err != nil || count != 0 {
		return original, false
	}
	known, claims := make(map[string]bool), make(map[string]int)
	for _, raw := range existing {
		var marketplace struct{ Plugins *[]nativePluginMetadata }
		if json.Unmarshal(raw, &marketplace) != nil || marketplace.Plugins == nil {
			return original, false
		}
		for _, plugin := range *marketplace.Plugins {
			if !inventoryIdentifier(plugin.ID) || !inventoryIdentifier(plugin.Name) || plugin.Installed == nil || plugin.Enabled == nil || known[plugin.ID] {
				return original, false
			}
			known[plugin.ID] = true
		}
	}
	for _, raw := range candidates {
		var marketplace struct{ Plugins *[]nativePluginMetadata }
		if json.Unmarshal(raw, &marketplace) != nil || marketplace.Plugins == nil {
			return original, false
		}
		for _, plugin := range *marketplace.Plugins {
			if !inventoryIdentifier(plugin.ID) || !inventoryIdentifier(plugin.Name) || plugin.Installed == nil || plugin.Enabled == nil {
				return original, false
			}
			claims[plugin.ID]++
		}
	}
	var additions []json.RawMessage
	for _, raw := range candidates {
		var marketplace struct {
			Name    string             `json:"name"`
			Path    string             `json:"path"`
			Plugins *[]json.RawMessage `json:"plugins"`
		}
		if json.Unmarshal(raw, &marketplace) != nil || marketplace.Plugins == nil {
			return original, false
		}
		var accepted []json.RawMessage
		private := false
		for _, owner := range recovery.owners {
			private = private || owner.manifest == marketplace.Path
		}
		for _, data := range *marketplace.Plugins {
			var plugin struct {
				nativePluginMetadata
				Source *struct{ Type, Path string } `json:"source"`
			}
			if json.Unmarshal(data, &plugin) != nil {
				return original, false
			}
			owner, requested := recovery.owners[plugin.ID]
			if private && !requested {
				return original, false
			}
			if !requested || known[plugin.ID] {
				continue
			}
			if claims[plugin.ID] != 1 || marketplace.Name != owner.marketplace || marketplace.Path != owner.manifest || plugin.Name != owner.name || plugin.Installed == nil || !*plugin.Installed || plugin.Enabled == nil || plugin.Source == nil || plugin.Source.Type != "local" || plugin.Source.Path != owner.source {
				return original, false
			}
			// Recovery's private project must not override a disabled owner in
			// the original project's effective configuration.
			if owner.enabled != nil && !*owner.enabled && *plugin.Enabled {
				var value map[string]json.RawMessage
				if json.Unmarshal(data, &value) != nil {
					return original, false
				}
				value["enabled"] = json.RawMessage("false")
				var err error
				data, err = json.Marshal(value)
				if err != nil {
					return original, false
				}
			}
			accepted = append(accepted, data)
		}
		if len(accepted) > 0 {
			var value map[string]json.RawMessage
			if json.Unmarshal(raw, &value) != nil {
				return original, false
			}
			value["plugins"], _ = json.Marshal(accepted)
			data, err := json.Marshal(value)
			if err != nil {
				return original, false
			}
			additions = append(additions, data)
		}
	}
	if len(additions) == 0 {
		return original, false
	}
	before["marketplaces"], _ = json.Marshal(append(existing, additions...))
	data, err := json.Marshal(before)
	if err != nil {
		return original, false
	}
	return data, true
}
