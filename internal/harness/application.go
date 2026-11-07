package harness

import (
	"sort"
	"strings"
)

// CapabilitySelection maps stable profile identities to the current native
// inventory. Native paths and configuration values never enter saved profiles.
type CapabilitySelection struct {
	Enabled map[Kind]map[string]bool `json:"enabled"`
	Issues  []string                 `json:"issues,omitempty"`
}

func (selection CapabilitySelection) Wants(kind Kind, id string) bool {
	return selection.Enabled[kind][id]
}

// SelectCapabilities includes required, available dependencies, but never
// overrides an explicit item denial. Baseline permits a native bridge to supply
// the original per-chat state instead of the global inventory's enabled flag.
func SelectCapabilities(effective Effective, inventory Inventory, baseline func(Capability) bool) CapabilitySelection {
	selection := CapabilitySelection{Enabled: make(map[Kind]map[string]bool)}
	known := make(map[Kind]map[string]Capability)
	issueSet := make(map[string]bool)
	issue := func(value string) {
		if !issueSet[value] {
			selection.Issues = append(selection.Issues, value)
			issueSet[value] = true
		}
	}
	for _, capability := range inventory.Capabilities {
		if known[capability.Kind] == nil {
			known[capability.Kind] = make(map[string]Capability)
			selection.Enabled[capability.Kind] = make(map[string]bool)
		}
		known[capability.Kind][capability.ID] = capability
		original := capability.Enabled
		if baseline != nil {
			original = baseline(capability)
		}
		wanted := effective.Choose(capability.Kind, capability.ID, original)
		if IsCurationCapability(capability) && original && capability.Available {
			if effective.Changes.Skills[capability.ID] == Disabled {
				issue("retained-core:skill:" + capability.ID)
			}
			wanted = true
		}
		if wanted && !capability.Available && !original {
			issue("unavailable:" + string(capability.Kind) + ":" + capability.ID)
			wanted = false
		}
		selection.Enabled[capability.Kind][capability.ID] = wanted
	}
	for kind, items := range map[Kind]map[string]Decision{PluginKind: effective.Changes.Plugins, MCPKind: effective.Changes.MCP, SkillKind: effective.Changes.Skills} {
		for id, decision := range items {
			if id != "*" && decision == Enabled {
				if _, ok := known[kind][id]; !ok {
					issue("missing:" + string(kind) + ":" + id)
				}
			}
		}
	}
	// A plugin exclusion removes inherited child capabilities too. Explicit
	// child selections instead retain a dependency conflict for the caller.
	for range len(inventory.Capabilities) {
		changed := false
		for _, capability := range inventory.Capabilities {
			if !selection.Wants(capability.Kind, capability.ID) || requestedDecision(effective, capability.Kind, capability.ID) != Inherit {
				continue
			}
			for _, reference := range capability.Dependencies {
				kindText, id, ok := strings.Cut(reference, ":")
				kind := Kind(kindText)
				if _, knownParent := known[kind][id]; ok && knownParent && !selection.Wants(kind, id) {
					selection.Enabled[capability.Kind][capability.ID] = false
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	queue := make([]Capability, 0, len(inventory.Capabilities))
	for _, capability := range inventory.Capabilities {
		if selection.Wants(capability.Kind, capability.ID) && requestedDecision(effective, capability.Kind, capability.ID) == Enabled {
			queue = append(queue, capability)
		}
	}
	processed := make(map[string]bool)
	for len(queue) > 0 {
		capability := queue[0]
		queue = queue[1:]
		key := string(capability.Kind) + ":" + capability.ID
		if processed[key] {
			continue
		}
		processed[key] = true
		for _, reference := range capability.Dependencies {
			kindText, id, ok := strings.Cut(reference, ":")
			kind := Kind(kindText)
			dependency, exists := known[kind][id]
			if !ok || !exists || !dependency.Available {
				issue("dependency:" + key + ":" + reference)
				continue
			}
			if selection.Wants(kind, id) {
				continue
			}
			items := map[Kind]map[string]Decision{PluginKind: effective.Changes.Plugins, MCPKind: effective.Changes.MCP, SkillKind: effective.Changes.Skills}[kind]
			if items[id] == Disabled {
				issue("dependency-denied:" + key + ":" + reference)
				continue
			}
			selection.Enabled[kind][id] = true
			queue = append(queue, dependency)
		}
	}
	// Dependencies may select a plugin which was not requested directly.
	// Evaluate metadata gaps against that final selection, not its initial seed.
	for _, entry := range inventory.Issues {
		if applicationIssueRelevant(effective, entry, selection) {
			issue(entry.Code + ":" + string(entry.Kind) + ":" + entry.ID)
		}
	}
	sort.Strings(selection.Issues)
	return selection
}

func requestedDecision(effective Effective, kind Kind, id string) Decision {
	items := map[Kind]map[string]Decision{PluginKind: effective.Changes.Plugins, MCPKind: effective.Changes.MCP, SkillKind: effective.Changes.Skills}[kind]
	for _, decision := range []Decision{items[id], items["*"], effective.Changes.Default} {
		if decision == Enabled || decision == Disabled {
			return decision
		}
	}
	return Inherit
}

func applicationIssueRelevant(effective Effective, entry InventoryIssue, selection CapabilitySelection) bool {
	if entry.Code == "plugin_mcp_ownership_unresolved" || entry.Code == "plugin_app_ownership_unresolved" {
		changed := len(effective.Changes.Plugins)+len(effective.Changes.MCP)+len(effective.Changes.Skills) > 0 || (effective.Changes.Default != "" && effective.Changes.Default != Inherit)
		return changed && entry.Kind == PluginKind && entry.ID != "" && selection.Wants(PluginKind, entry.ID)
	}
	items := map[Kind]map[string]Decision{PluginKind: effective.Changes.Plugins, MCPKind: effective.Changes.MCP, SkillKind: effective.Changes.Skills}[entry.Kind]
	touched := len(items) > 0 || (effective.Changes.Default != "" && effective.Changes.Default != Inherit)
	if entry.Code == "inventory_incomplete" {
		return touched
	}
	if entry.ID != "" {
		return requestedDecision(effective, entry.Kind, entry.ID) == Enabled && selection.Wants(entry.Kind, entry.ID)
	}
	return touched
}
