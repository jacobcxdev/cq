package harness

import (
	"slices"
	"testing"
)

func TestCapabilitySelectionUsesFullInventoryAndDependencies(t *testing.T) {
	inventory := Inventory{Capabilities: []Capability{
		{Kind: PluginKind, ID: "research@market", Enabled: true, Available: true},
		{Kind: PluginKind, ID: "design@market", Enabled: true, Available: true},
		{Kind: SkillKind, ID: "plugin:research@market:analyse", Enabled: true, Available: true, Dependencies: []string{"plugin:research@market"}},
		{Kind: MCPKind, ID: "analysis", Enabled: true, Available: true},
	}}
	effective := Effective{Changes: Changes{Default: Disabled, Skills: map[string]Decision{"plugin:research@market:analyse": Enabled}}}
	selected := SelectCapabilities(effective, inventory, nil)
	if !selected.Wants(SkillKind, "plugin:research@market:analyse") || !selected.Wants(PluginKind, "research@market") || selected.Wants(PluginKind, "design@market") || selected.Wants(MCPKind, "analysis") || len(selected.Issues) != 0 {
		t.Fatalf("selection/dependency closure = %#v", selected)
	}
}

func TestCapabilitySelectionOwnershipGapsFollowSelectedBundles(t *testing.T) {
	inventory := Inventory{Capabilities: []Capability{
		{Kind: PluginKind, ID: "owner", Enabled: true, Available: true},
		{Kind: SkillKind, ID: "dependent", Enabled: true, Available: true, Dependencies: []string{"plugin:owner"}},
		{Kind: MCPKind, ID: "standalone", Enabled: true, Available: true},
	}, Issues: []InventoryIssue{{Code: "plugin_mcp_ownership_unresolved", Kind: PluginKind, ID: "owner"}}}
	for _, test := range []struct {
		name      string
		changes   Changes
		wantIssue bool
	}{
		{"unmanaged baseline", Changes{}, false},
		{"plugins excluded", Changes{Default: Disabled, MCP: map[string]Decision{"standalone": Enabled}}, false},
		{"explicit plugin", Changes{Default: Disabled, Plugins: map[string]Decision{"owner": Enabled}}, true},
		{"plugin dependency", Changes{Default: Disabled, Skills: map[string]Decision{"dependent": Enabled}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := SelectCapabilities(Effective{Changes: test.changes}, inventory, nil)
			found := slices.Contains(selected.Issues, "plugin_mcp_ownership_unresolved:plugin:owner")
			if found != test.wantIssue {
				t.Fatalf("bundle ownership gap relevance = %t, want %t; issues=%v", found, test.wantIssue, selected.Issues)
			}
		})
	}
}

func TestCapabilitySelectionPreservesExplicitDenialsAndReportsMissing(t *testing.T) {
	inventory := Inventory{Capabilities: []Capability{
		{Kind: PluginKind, ID: "owner", Available: true},
		{Kind: SkillKind, ID: "selected", Available: true, Dependencies: []string{"plugin:owner", "mcp:absent"}},
		{Kind: MCPKind, ID: "unavailable", Available: false},
	}, Issues: []InventoryIssue{{Code: "partial-inventory", Kind: SkillKind}}}
	effective := Effective{Changes: Changes{Default: Disabled, Plugins: map[string]Decision{"owner": Disabled}, Skills: map[string]Decision{"selected": Enabled, "missing": Enabled}, MCP: map[string]Decision{"unavailable": Enabled}}}
	selected := SelectCapabilities(effective, inventory, nil)
	if selected.Wants(PluginKind, "owner") || selected.Wants(MCPKind, "unavailable") {
		t.Fatal("selection widened an explicit/native denial")
	}
	for _, issue := range []string{"dependency-denied:skill:selected:plugin:owner", "dependency:skill:selected:mcp:absent", "missing:skill:missing", "unavailable:mcp:unavailable", "partial-inventory:skill:"} {
		if !slices.Contains(selected.Issues, issue) {
			t.Fatalf("missing issue %q in %v", issue, selected.Issues)
		}
	}
}

func TestCapabilitySelectionUsesOriginalChatBaseline(t *testing.T) {
	inventory := Inventory{Capabilities: []Capability{{Kind: PluginKind, ID: "manual-off", Enabled: true, Available: true}}}
	selected := SelectCapabilities(Effective{}, inventory, func(Capability) bool { return false })
	if selected.Wants(PluginKind, "manual-off") {
		t.Fatal("global inventory replaced the original per-chat baseline")
	}
}

func TestCapabilitySelectionPreservesUnmanagedOpaqueBaselineAndCuration(t *testing.T) {
	inventory := Inventory{Capabilities: []Capability{
		{Kind: SkillKind, ID: "user:opaque", Enabled: true, Available: false, Dependencies: []string{"plugin:unknown"}},
		{Kind: SkillKind, ID: CurationSkillID, Enabled: true, Available: true},
	}, Issues: []InventoryIssue{{Kind: SkillKind, ID: "user:opaque", Code: "skill_plugin_unresolved"}}}
	effective := Effective{Changes: Changes{Plugins: map[string]Decision{"design": Disabled}}}
	selected := SelectCapabilities(effective, inventory, nil)
	if !selected.Wants(SkillKind, "user:opaque") || len(selected.Issues) != 0 {
		t.Fatalf("unmanaged baseline changed: %#v", selected)
	}
	effective.Changes.Default = Disabled
	selected = SelectCapabilities(effective, inventory, nil)
	if !selected.Wants(SkillKind, CurationSkillID) || selected.Wants(SkillKind, "user:opaque") {
		t.Fatalf("curation entrypoint was reduced away: %#v", selected)
	}
}

func TestCapabilitySelectionCascadesPluginExclusionToInheritedChildren(t *testing.T) {
	inventory := Inventory{Capabilities: []Capability{{Kind: PluginKind, ID: "design", Enabled: true, Available: true}, {Kind: SkillKind, ID: "design-skill", Enabled: true, Available: true, Dependencies: []string{"plugin:design"}}}}
	effective := Effective{Changes: Changes{Plugins: map[string]Decision{"design": Disabled}}}
	selected := SelectCapabilities(effective, inventory, nil)
	if selected.Wants(PluginKind, "design") || selected.Wants(SkillKind, "design-skill") || len(selected.Issues) != 0 {
		t.Fatalf("plugin exclusion did not cascade cleanly: %#v", selected)
	}
}

func TestCapabilitySelectionRespectsNativeCoreDisableAndSupportedSources(t *testing.T) {
	for _, id := range []string{CurationSkillID, CurationSkillID + "@codex/curate-harness", CurationSkillID + "@agents/curate-harness"} {
		inventory := Inventory{Capabilities: []Capability{{Kind: SkillKind, ID: id, Enabled: true, Available: true}}}
		effective := Effective{Changes: Changes{Default: Disabled}}
		selected := SelectCapabilities(effective, inventory, nil)
		if !selected.Wants(SkillKind, id) {
			t.Fatalf("curation entrypoint %s hidden", id)
		}
		selected = SelectCapabilities(effective, inventory, func(Capability) bool { return false })
		if selected.Wants(SkillKind, id) {
			t.Fatalf("global skill instruction disable overridden for%s", id)
		}
	}
}

func TestCapabilitySelectionReportedInheritedPluginAppOwnershipGap(t *testing.T) {
	inventory := Inventory{Capabilities: []Capability{{Kind: PluginKind, ID: "owner", Enabled: true, Available: true}}, Issues: []InventoryIssue{{Code: "plugin_app_ownership_unresolved", Kind: PluginKind, ID: "owner"}}}
	selected := SelectCapabilities(Effective{Changes: Changes{MCP: map[string]Decision{"*": Disabled}}}, inventory, nil)
	if !slices.Contains(selected.Issues, "plugin_app_ownership_unresolved:plugin:owner") {
		t.Fatalf("inherited selected plugin app gap was not reported: %v", selected.Issues)
	}
}
