package harness

import (
	"context"
	"slices"
	"testing"
)

type profileInventory struct {
	inventory Inventory
	err       error
}

func (source profileInventory) Snapshot(context.Context, string) (Inventory, error) {
	return source.inventory, source.err
}

func TestControllerV2SelectivePluginsAndPartialStandaloneControls(t *testing.T) {
	controller, desktop, _, receipts := controllerFixture()
	controller.Inventory = profileInventory{inventory: Inventory{Capabilities: []Capability{
		{Kind: PluginKind, ID: "a-plugin", Enabled: true, Available: true},
		{Kind: PluginKind, ID: "z-plugin", Enabled: true, Available: true},
		{Kind: PluginKind, ID: "user-disabled", Enabled: true, Available: true},
		{Kind: MCPKind, ID: "analysis", Enabled: true, Available: true},
		{Kind: SkillKind, ID: "user:research", Enabled: true, Available: true},
	}}}
	policy := Policy{Version: 2, Enabled: true, Global: Scope{All: Binding{Changes: Changes{Default: Disabled, Plugins: map[string]Decision{"a-plugin": Enabled}, MCP: map[string]Decision{"analysis": Enabled}}}}}
	if _, err := controller.Reconcile(context.Background(), testChat, policy); err != nil {
		t.Fatal(err)
	}
	if got := desktop.snapshots[testChat].DisabledPluginIDs; !slices.Equal(got, []string{"user-disabled", "z-plugin"}) {
		t.Fatalf("selective plugin exclusions = %v", got)
	}
	receipt, err := receipts.Load(testChat)
	if err != nil || receipt.State != "partial" || receipt.Profile != Custom || !slices.Equal(receipt.Unsupported, []string{"skill:user:research"}) || receipt.Model != "glm-5.3" || receipt.Fingerprint == "" {
		t.Fatalf("partial receipt = %#v, %v", receipt, err)
	}
	policy.Global.All = Binding{Profiles: []string{"full"}}
	if _, err := controller.Reconcile(context.Background(), testChat, policy); err != nil {
		t.Fatal(err)
	}
	if got := desktop.snapshots[testChat].DisabledPluginIDs; !slices.Equal(got, []string{"user-disabled"}) {
		t.Fatalf("full restore = %v", got)
	}
	receipt, _ = receipts.Load(testChat)
	if receipt.State != "restored" || len(receipt.Unsupported) != 0 {
		t.Fatalf("restore retained stale limitations: %#v", receipt)
	}
}

func TestControllerV2UsesWorkspacePolicyForAllModels(t *testing.T) {
	controller, desktop, _, _ := controllerFixture()
	state := desktop.snapshots[testChat]
	state.Model = "gpt-6.1-sol"
	desktop.snapshots[testChat] = state
	controller.Inventory = profileInventory{inventory: Inventory{Capabilities: []Capability{{Kind: PluginKind, ID: "a-plugin", Enabled: true, Available: true}}}}
	policy := Policy{Version: 2, Enabled: true, Global: Scope{All: Binding{Changes: Changes{Plugins: map[string]Decision{"a-plugin": Disabled}}}}}
	controller.Policy = func() (Policy, error) { return Policy{Version: 2, Enabled: true}, nil }
	controller.ContextPolicy = func(cwd string) (Policy, error) {
		if cwd != "/work/project" {
			t.Fatalf("policy cwd=%q", cwd)
		}
		return policy, nil
	}
	controller.Observe(testChat, state.Model)
	if len(controller.queue) != 1 {
		t.Fatal("v2 ignored non-GLM request")
	}
	if err := controller.Apply(context.Background(), testChat); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(desktop.snapshots[testChat].DisabledPluginIDs, "a-plugin") {
		t.Fatal("workspace rule did not apply to GPT")
	}
}

func TestControllerV2MissingCapabilityIsNotClaimedApplied(t *testing.T) {
	controller, _, _, receipts := controllerFixture()
	controller.Inventory = profileInventory{}
	policy := Policy{Version: 2, Enabled: true, Tasks: map[string]Scope{testChat: {All: Binding{Changes: Changes{Skills: map[string]Decision{"user:missing": Enabled}}}}}}
	if _, err := controller.Reconcile(context.Background(), testChat, policy); err != nil {
		t.Fatal(err)
	}
	receipt, err := receipts.Load(testChat)
	if err != nil || receipt.State != "partial" || !slices.Contains(receipt.Issues, "missing:skill:user:missing") {
		t.Fatalf("missing capability receipt %#v, %v", receipt, err)
	}
}
