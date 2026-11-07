package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const nativeCurationFixture = `{
 "plugins":{"marketplaceLoadErrors":[],"marketplaces":[{"name":"market","path":"/private/marketplace.json","plugins":[
  {"id":"one@market","name":"one","installed":true,"enabled":true,"availability":"AVAILABLE","interface":{"displayName":"One","shortDescription":"Native plugin description"}},
  {"id":"disabled@market","name":"disabled","installed":true,"enabled":false,"availability":"AVAILABLE"}
 ]}]},
 "pluginDetails":{
  "one@market":{"plugin":{"summary":{"id":"one@market","name":"one"},"mcpServers":[],"apps":[]}},
  "disabled@market":{"plugin":{"summary":{"id":"disabled@market","name":"disabled"},"mcpServers":[],"apps":[]}}
 },
 "skills":{"data":[{"cwd":"/work","errors":[],"skills":[
  {"name":"plain","description":"Plain skill","enabled":false,"path":"/home/skills/plain/SKILL.md","scope":"user"},
  {"name":"local","description":"Repo skill","enabled":true,"path":"/work/.agents/skills/local/SKILL.md","scope":"repo"},
  {"name":"owned","description":"Plugin skill","enabled":true,"path":"/cache/42/owned/SKILL.md","scope":"user","pluginId":"one@market","dependencies":{"tools":[{"type":"mcp","value":"active"},{"type":"mcp","value":"https://private-dependency"}]}},
  {"name":"system","description":"System skill","enabled":true,"path":"/system/system/SKILL.md","scope":"system"}
 ]}]},
 "config":{"config":{"mcp_servers":{
  "active":{"enabled":true,"command":"private-command","env":{"TOKEN":"private-secret"}},
  "disabled":{"enabled":false,"url":"https://private-url"}
 }},"layers":[{"name":{"type":"project","dotCodexFolder":"/work/.codex"},"version":"1","disabledReason":"private diagnostic","config":{"mcp_servers":{"inactive":{"command":"private-command"}}}}],"origins":{"mcp_servers.originOnly.command":{"name":{"type":"user","file":"/private/config"},"version":"1"}}}
}`

func nativeCurationClient(data string) NativeInventory {
	return NativeInventory{Executable: "/native/codex", Run: func(_ context.Context, _, _ string, output io.Writer) error {
		_, err := io.WriteString(output, data)
		return err
	}}
}

func nativeOwnershipFixture(t *testing.T, change func(map[string]any)) string {
	t.Helper()
	var fixture map[string]any
	if json.Unmarshal([]byte(nativeCurationFixture), &fixture) != nil {
		t.Fatal("decode native ownership fixture")
	}
	change(fixture)
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNativeInventoryKeptPluginAppIDsIncludingDisabled(t *testing.T) {
	data := nativeOwnershipFixture(t, func(fixture map[string]any) {
		details := fixture["pluginDetails"].(map[string]any)
		details["one@market"].(map[string]any)["plugin"].(map[string]any)["apps"] = []any{map[string]any{"id": "asdk_app_second", "name": "Second", "description": "private-description"}, map[string]any{"id": "asdk_app_first"}}
		details["disabled@market"].(map[string]any)["plugin"].(map[string]any)["apps"] = []any{map[string]any{"id": "asdk_app_dormant"}}
	})
	inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(inventory)
	var decoded struct {
		Capabilities []struct {
			ID     string
			AppIDs []string `json:"app_ids"`
		}
	}
	_ = json.Unmarshal(encoded, &decoded)
	found := make(map[string][]string)
	for _, capability := range decoded.Capabilities {
		if capability.AppIDs != nil {
			found[capability.ID] = capability.AppIDs
		}
	}
	if !reflect.DeepEqual(found, map[string][]string{"one@market": {"asdk_app_first", "asdk_app_second"}, "disabled@market": {"asdk_app_dormant"}}) {
		t.Fatalf("plugin app ownership lost or independently controllable: %v", found)
	}
	if strings.Contains(string(encoded), "private-description") {
		t.Fatal("unneeded app metadata leaked into inventory")
	}
}

func TestNativeInventoryAppOwnershipFailuresWereLocal(t *testing.T) {
	for _, scenario := range []string{"missing", "null", "wrong-type", "empty-id", "duplicate-id", "reserved-id"} {
		t.Run(scenario, func(t *testing.T) {
			data := nativeOwnershipFixture(t, func(fixture map[string]any) {
				plugin := fixture["pluginDetails"].(map[string]any)["one@market"].(map[string]any)["plugin"].(map[string]any)
				switch scenario {
				case "missing":
					delete(plugin, "apps")
				case "null":
					plugin["apps"] = nil
				case "wrong-type":
					plugin["apps"] = "private-secret"
				case "empty-id":
					plugin["apps"] = []any{map[string]any{"id": ""}}
				case "duplicate-id":
					plugin["apps"] = []any{map[string]any{"id": "same"}, map[string]any{"id": "same"}}
				case "reserved-id":
					plugin["apps"] = []any{map[string]any{"id": "_default"}}
				}
			})
			inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
			if err != nil || !slices.Contains(inventory.Issues, InventoryIssue{Code: "plugin_app_ownership_unresolved", Kind: PluginKind, ID: "one@market"}) {
				t.Fatalf("app metadata gap not localised: %v %v", inventory.Issues, err)
			}
		})
	}
}

func TestNativeInventoryKeptPluginMCPNamesAtomicIncludingDisabled(t *testing.T) {
	data := nativeOwnershipFixture(t, func(fixture map[string]any) {
		details := fixture["pluginDetails"].(map[string]any)
		details["one@market"].(map[string]any)["plugin"].(map[string]any)["mcpServers"] = []string{"bundle-two", "bundle-one"}
		details["disabled@market"].(map[string]any)["plugin"].(map[string]any)["mcpServers"] = []string{"dormant"}
	})
	inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(inventory)
	var decoded struct {
		Capabilities []struct {
			ID         string
			Kind       Kind
			MCPServers []string `json:"mcp_servers"`
		}
	}
	_ = json.Unmarshal(encoded, &decoded)
	for _, capability := range decoded.Capabilities {
		if capability.ID == "one@market" && !slices.Equal(capability.MCPServers, []string{"bundle-one", "bundle-two"}) {
			t.Fatal("enabled plugin lost its declared MCP names")
		}
		if capability.ID == "disabled@market" && !slices.Equal(capability.MCPServers, []string{"dormant"}) {
			t.Fatal("disabled plugin lost its declared MCP names")
		}
		if capability.Kind == MCPKind && slices.Contains([]string{"bundle-one", "bundle-two", "dormant"}, capability.ID) {
			t.Fatal("bundled MCP became independently controllable")
		}
	}
	for _, issue := range inventory.Issues {
		if issue.Kind == PluginKind {
			t.Fatalf("complete atomic plugin metadata reported a gap: %s", issue.Code)
		}
	}
}

func TestNativeInventoryPluginMCPFailuresWereLocalAndPrivate(t *testing.T) {
	for _, scenario := range []string{"missing", "rejected", "missing-servers", "null-servers", "wrong-type", "duplicate-name", "wrong-id", "wrong-name"} {
		t.Run(scenario, func(t *testing.T) {
			data := nativeOwnershipFixture(t, func(fixture map[string]any) {
				details := fixture["pluginDetails"].(map[string]any)
				plugin := details["one@market"].(map[string]any)["plugin"].(map[string]any)
				switch scenario {
				case "missing":
					delete(details, "one@market")
				case "rejected":
					details["one@market"] = map[string]any{"error": map[string]any{"message": "private-secret", "url": "https://private-endpoint"}}
				case "missing-servers":
					delete(plugin, "mcpServers")
				case "null-servers":
					plugin["mcpServers"] = nil
				case "wrong-type":
					plugin["mcpServers"] = []any{map[string]any{"env": "private-secret"}}
				case "duplicate-name":
					plugin["mcpServers"] = []string{"same", "same"}
				case "wrong-id":
					plugin["summary"].(map[string]any)["id"] = "different@market"
				case "wrong-name":
					plugin["summary"].(map[string]any)["name"] = "different"
				}
			})
			inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
			if err != nil || len(inventory.Capabilities) != 10 {
				t.Fatalf("plugin detail failure discarded the usable inventory: %v", err)
			}
			var pluginIssues []InventoryIssue
			for _, issue := range inventory.Issues {
				if issue.Kind == PluginKind {
					pluginIssues = append(pluginIssues, issue)
				}
			}
			want := []InventoryIssue{{Code: "plugin_mcp_ownership_unresolved", Kind: PluginKind, ID: "one@market"}}
			if scenario == "missing" || scenario == "rejected" || scenario == "wrong-id" || scenario == "wrong-name" {
				want = append([]InventoryIssue{{Code: "plugin_app_ownership_unresolved", Kind: PluginKind, ID: "one@market"}}, want...)
			}
			if !reflect.DeepEqual(pluginIssues, want) {
				t.Fatalf("plugin detail gap was not localised: %+v", pluginIssues)
			}
			encoded, _ := json.Marshal(inventory)
			if strings.Contains(string(encoded), "private-secret") || strings.Contains(string(encoded), "private-endpoint") || strings.Contains(string(encoded), "/private/marketplace") {
				t.Fatal("raw plugin detail or locator leaked into inventory")
			}
		})
	}
}

func TestNativeInventoryResolvedSkillMCPToUniqueAtomicOwner(t *testing.T) {
	for _, scenario := range []string{"unique-owner", "disabled-owner", "standalone", "two-owners", "standalone-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			data := nativeOwnershipFixture(t, func(fixture map[string]any) {
				details := fixture["pluginDetails"].(map[string]any)
				one := details["one@market"].(map[string]any)["plugin"].(map[string]any)
				disabled := details["disabled@market"].(map[string]any)["plugin"].(map[string]any)
				dependency := "bundled"
				switch scenario {
				case "unique-owner":
					one["mcpServers"] = []string{dependency}
				case "disabled-owner":
					disabled["mcpServers"] = []string{dependency}
				case "standalone":
					dependency = "active"
				case "two-owners":
					one["mcpServers"], disabled["mcpServers"] = []string{dependency}, []string{dependency}
				case "standalone-conflict":
					dependency = "active"
					one["mcpServers"] = []string{dependency}
				}
				skills := fixture["skills"].(map[string]any)["data"].([]any)[0].(map[string]any)["skills"].([]any)
				skills[0].(map[string]any)["dependencies"] = map[string]any{"tools": []any{map[string]any{"type": "mcp", "value": dependency}}}
			})
			inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
			if err != nil {
				t.Fatal(err)
			}
			var plain Capability
			for _, capability := range inventory.Capabilities {
				if capability.Kind == SkillKind && capability.NativeID == "plain" {
					plain = capability
				}
			}
			want := map[string][]string{"unique-owner": {"plugin:one@market"}, "disabled-owner": {"plugin:disabled@market"}, "standalone": {"mcp:active"}}
			if !slices.Equal(plain.Dependencies, want[scenario]) {
				t.Fatalf("wrong atomic dependency: %v", plain.Dependencies)
			}
			if scenario == "two-owners" || scenario == "standalone-conflict" {
				if !slices.ContainsFunc(inventory.Issues, func(issue InventoryIssue) bool {
					return issue.Kind == SkillKind && issue.ID == plain.ID && issue.Code == "skill_dependency_ambiguous"
				}) {
					t.Fatal("ambiguous bundled MCP dependency was guessed")
				}
			}
		})
	}
}

func TestNativeInventoryDetailReadQueueWasBoundedAndSkippedUninstalled(t *testing.T) {
	var plugins []map[string]any
	for index := 0; index < 130; index++ {
		plugins = append(plugins, map[string]any{"id": "plugin-" + strconv.Itoa(index) + "@market", "name": "plugin-" + strconv.Itoa(index), "installed": true, "enabled": index%2 == 0})
	}
	plugins = append(plugins, map[string]any{"id": "uninstalled@market", "name": "uninstalled", "installed": false, "enabled": false})
	raw, _ := json.Marshal(map[string]any{"marketplaces": []any{map[string]any{"name": "market", "path": "/private/market.json", "plugins": plugins}}})
	requests := nativePluginDetailRequests(raw)
	if len(requests) != 124 {
		t.Fatalf("native detail read bound changed: %d", len(requests))
	}
	var previous string
	for _, request := range requests {
		if request.method != "plugin/read" || request.pluginID == "uninstalled@market" || previous != "" && request.pluginID <= previous {
			t.Fatal("native detail queue was not deterministic metadata-only discovery")
		}
		previous = request.pluginID
	}
}

func TestNativeInventoryIncludedDisabledMetadataAndPortableIDs(t *testing.T) {
	inventory, err := nativeCurationClient(nativeCurationFixture).Snapshot(context.Background(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Capability)
	for _, capability := range inventory.Capabilities {
		byID[string(capability.Kind)+":"+capability.ID] = capability
		if strings.Contains(capability.ID, "/cache/") {
			t.Fatal("cache path became persistent identity")
		}
	}
	if len(byID) != 10 {
		t.Fatalf("capability count = %d", len(byID))
	}
	for _, key := range []string{"plugin:disabled@market", "skill:user:plain", "mcp:disabled", "mcp:inactive", "mcp:originOnly"} {
		capability, ok := byID[key]
		if !ok || capability.Enabled {
			t.Fatalf("disabled metadata missing or enabled: %q, %+v", key, capability)
		}
	}
	if !byID["mcp:active"].Available || byID["mcp:inactive"].Available || !byID["skill:user:plain"].Available {
		t.Fatal("native availability was misrepresented")
	}
	owned := byID["skill:plugin:one@market:owned"]
	if owned.Path != "/cache/42/owned/SKILL.md" || owned.NativeID != "owned" || owned.PluginID != "one@market" || !slices.Equal(owned.Dependencies, []string{"mcp:active", "plugin:one@market"}) {
		t.Fatalf("owned skill = %+v", owned)
	}
	if byID["plugin:one@market"].Description != "Native plugin description" {
		t.Fatal("native description lost")
	}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-secret", "private-command", "private-url", "private diagnostic", "private-dependency", "/private/config"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("inventory leaked %s", secret)
		}
	}
	if !slices.ContainsFunc(inventory.Issues, func(issue InventoryIssue) bool { return issue.Code == "skill_dependency_unresolved" }) {
		t.Fatal("unresolved dependency was silently dropped")
	}
}

func TestNativeInventoryRejectedCollisionsAndPartialSchemas(t *testing.T) {
	for name, data := range map[string]string{
		"missing response":    `{"plugins":{},"skills":{}}`,
		"missing plugin flag": strings.Replace(nativeCurationFixture, `"installed":true,"enabled":true`, `"installed":true`, 1),
		"null plugin flag":    strings.Replace(nativeCurationFixture, `"installed":true,"enabled":true`, `"installed":true,"enabled":null`, 1),
		"missing skill flag":  strings.Replace(nativeCurationFixture, `"enabled":false,"path"`, `"path"`, 1),
		"null skill flag":     strings.Replace(nativeCurationFixture, `"enabled":false,"path"`, `"enabled":null,"path"`, 1),
		"duplicate plugin":    strings.Replace(nativeCurationFixture, `"id":"disabled@market"`, `"id":"one@market"`, 1),
		"duplicate skill":     strings.Replace(nativeCurationFixture, `"name":"local","description":"Repo skill"`, `"name":"plain","description":"Repo skill"`, 1),
		"missing raw layers":  strings.Replace(nativeCurationFixture, `"layers":[`, `"unrequested_layers":[`, 1),
		"invalid mcp flag":    strings.Replace(nativeCurationFixture, `"active":{"enabled":true`, `"active":{"enabled":null`, 1),
		"trailing data":       nativeCurationFixture + ` {}`,
	} {
		if name == "duplicate skill" {
			data = strings.Replace(data, `"scope":"repo"`, `"scope":"user"`, 1)
		}
		t.Run(name, func(t *testing.T) {
			if _, err := nativeCurationClient(data).Snapshot(context.Background(), "/work"); err == nil {
				t.Fatal("invalid inventory accepted")
			}
		})
	}
}

func TestNativeInventoryKeptNativeLoadErrorsAsSafeIssues(t *testing.T) {
	data := strings.Replace(nativeCurationFixture, `"marketplaceLoadErrors":[]`, `"marketplaceLoadErrors":[{"message":"private-secret","marketplacePath":"/private/config"}]`, 1)
	inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
	if err != nil || len(inventory.Capabilities) == 0 || !slices.ContainsFunc(inventory.Issues, func(issue InventoryIssue) bool { return issue.Code == "plugin_load_error" }) || !slices.ContainsFunc(inventory.Issues, func(issue InventoryIssue) bool {
		return issue.Code == "inventory_incomplete" && issue.Kind == PluginKind
	}) {
		t.Fatalf("partial native load = %+v, %v", inventory, err)
	}
}

func TestNativeInventoryPreservedDistinctCollidingSkillsWithPortableQualifiers(t *testing.T) {
	data := strings.Replace(nativeCurationFixture, `"name":"local","description":"Repo skill"`, `"name":"plain","description":"Repo skill"`, 1)
	data = strings.Replace(data, `"scope":"repo"`, `"scope":"user"`, 1)
	data = strings.Replace(data, `/home/skills/plain/SKILL.md`, `/home/one/.agents/skills/plain/SKILL.md`, 1)
	data = strings.Replace(data, `/work/.agents/skills/local/SKILL.md`, `/home/one/.agents/skills/plain/skills/plain/SKILL.md`, 1)
	inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[string]Capability)
	for _, capability := range inventory.Capabilities {
		if capability.Kind == SkillKind && capability.NativeID == "plain" {
			rows[capability.ID] = capability
		}
	}
	if len(rows) != 2 || rows["user:plain@agents/plain"].Enabled || !rows["user:plain@agents/plain/skills/plain"].Enabled {
		t.Fatalf("qualified rows = %+v", rows)
	}
}

func TestNativeInventoryRejectedStillAmbiguousVersionedSkillOrigins(t *testing.T) {
	data := strings.Replace(nativeCurationFixture, `"name":"local","description":"Repo skill"`, `"name":"plain","description":"Repo skill"`, 1)
	data = strings.Replace(data, `"scope":"repo"`, `"scope":"user"`, 1)
	data = strings.Replace(data, `/home/skills/plain/SKILL.md`, `/home/one/.codex/plugins/cache/market/one/1.0.0/skills/plain/SKILL.md`, 1)
	data = strings.Replace(data, `/work/.agents/skills/local/SKILL.md`, `/home/one/.codex/plugins/cache/market/one/2.0.0/skills/plain/SKILL.md`, 1)
	if _, err := nativeCurationClient(data).Snapshot(context.Background(), "/work"); err == nil {
		t.Fatal("version-only qualifier silently resolved a real collision")
	}
}

func TestNativeInventorySkillIDsSurvivedAddingAndRemovingDuplicateNames(t *testing.T) {
	base := strings.Replace(nativeCurationFixture, `/home/skills/plain/SKILL.md`, `/home/one/.agents/skills/plain/SKILL.md`, 1)
	duplicate := strings.Replace(base, `"name":"local","description":"Repo skill"`, `"name":"plain","description":"Repo skill"`, 1)
	duplicate = strings.Replace(duplicate, `"scope":"repo"`, `"scope":"user"`, 1)
	duplicate = strings.Replace(duplicate, `/work/.agents/skills/local/SKILL.md`, `/home/one/.codex/skills/plain/SKILL.md`, 1)
	for _, data := range []string{base, duplicate, base} {
		inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, capability := range inventory.Capabilities {
			if capability.Path == "/home/one/.agents/skills/plain/SKILL.md" {
				found = true
				if capability.ID != "user:plain@agents/plain" {
					t.Fatalf("saved identity retargeted: %q", capability.ID)
				}
			}
			if capability.PluginID == "one@market" && capability.NativeID == "owned" && capability.ID != "plugin:one@market:owned" {
				t.Fatal("plugin-owned identity acquired an unrelated source qualifier")
			}
		}
		if !found {
			t.Fatal("original skill disappeared")
		}
	}
}

func TestNativeInventoryAvailabilityRespectedNativeEnableBoundaries(t *testing.T) {
	data := strings.Replace(nativeCurationFixture, `"pluginId":"one@market"`, `"pluginId":"disabled@market"`, 1)
	inventory, err := nativeCurationClient(data).Snapshot(context.Background(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range inventory.Capabilities {
		if capability.Kind == SkillKind && capability.NativeID == "plain" && (capability.Enabled || !capability.Available) {
			t.Fatal("globally disabled standalone user skill lost per-thread availability")
		}
		if capability.Kind == PluginKind && capability.NativeID == "disabled@market" && (capability.Enabled || capability.Available) {
			t.Fatal("globally disabled plugin falsely offered per-thread enabling")
		}
		if capability.Kind == SkillKind && capability.NativeID == "owned" && (!capability.Enabled || capability.Available) {
			t.Fatal("plugin-owned skill ignored its disabled owner")
		}
	}
	data = strings.Replace(nativeCurationFixture, `"scope":"user"`, `"scope":"admin"`, 1)
	inventory, err = nativeCurationClient(data).Snapshot(context.Background(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range inventory.Capabilities {
		if capability.Kind == SkillKind && capability.NativeID == "plain" && capability.Available {
			t.Fatal("disabled managed skill falsely offered per-thread enabling")
		}
	}
}

func TestNativeInventoryRecoveredEffectiveMCPFromActiveLayersAndOrigins(t *testing.T) {
	for _, scenario := range []string{"missing-merged", "origin-precedence", "inherit-flag", "disabled-layer", "merged-authority"} {
		t.Run(scenario, func(t *testing.T) {
			var fixture map[string]any
			_ = json.Unmarshal([]byte(nativeCurationFixture), &fixture)
			config := fixture["config"].(map[string]any)
			config["config"] = map[string]any{}
			lowSource := map[string]any{"type": "user", "file": "/private/low-config"}
			highSource := map[string]any{"type": "project", "dotCodexFolder": "/work/.codex"}
			low := map[string]any{"name": lowSource, "version": "low", "config": map[string]any{"mcp_servers": map[string]any{"layered": map[string]any{"enabled": true, "command": "private-command"}}}}
			high := map[string]any{"name": highSource, "version": "high", "config": map[string]any{"mcp_servers": map[string]any{"blocked": map[string]any{"enabled": true}}}, "disabledReason": "private-disabled-reason"}
			config["origins"] = map[string]any{}
			switch scenario {
			case "origin-precedence":
				low["config"].(map[string]any)["mcp_servers"].(map[string]any)["layered"].(map[string]any)["enabled"] = false
				high["config"] = map[string]any{"mcp_servers": map[string]any{"layered": map[string]any{"enabled": true}}}
				delete(high, "disabledReason")
				config["origins"] = map[string]any{`mcp_servers."layered".enabled`: map[string]any{"name": highSource, "version": "high"}}
			case "inherit-flag":
				low["config"].(map[string]any)["mcp_servers"].(map[string]any)["layered"].(map[string]any)["enabled"] = false
				high["config"] = map[string]any{"mcp_servers": map[string]any{"layered": map[string]any{"args": []string{"private-arg"}}}}
				delete(high, "disabledReason")
			case "disabled-layer":
				high["config"] = map[string]any{"mcp_servers": map[string]any{"layered": map[string]any{"enabled": false}, "blocked": map[string]any{"enabled": true}}}
				config["origins"] = map[string]any{"mcp_servers.layered.enabled": map[string]any{"name": highSource, "version": "high"}}
			case "merged-authority":
				config["config"] = map[string]any{"mcp_servers": map[string]any{"layered": map[string]any{"enabled": false}}}
			}
			config["layers"] = []any{high, low} // origins, not array position, choose conflicting flags.
			encoded, _ := json.Marshal(fixture)
			inventory, err := nativeCurationClient(string(encoded)).Snapshot(context.Background(), "/work")
			if err != nil {
				t.Fatal(err)
			}
			var layered Capability
			for _, capability := range inventory.Capabilities {
				if capability.Kind == MCPKind && capability.ID == "layered" {
					layered = capability
				}
				if capability.Kind == MCPKind && capability.ID == "blocked" && (capability.Enabled || capability.Available) {
					t.Fatal("disabled layer became active")
				}
			}
			wantEnabled := scenario != "inherit-flag" && scenario != "merged-authority"
			if layered.ID == "" || layered.Enabled != wantEnabled || !layered.Available {
				t.Fatalf("effective layered MCP = %+v", layered)
			}
		})
	}
}

func TestNativeInventoryBoundedRunAndRedactedFailure(t *testing.T) {
	client := NativeInventory{Executable: "/native/codex", Run: func(ctx context.Context, executable, cwd string, output io.Writer) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 20*time.Second || executable != "/native/codex" || cwd != "/work" {
			t.Fatal("unbounded or incorrect native invocation")
		}
		return errors.New("private-secret")
	}}
	_, err := client.Snapshot(context.Background(), "/work")
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("native failure = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.Run = func(ctx context.Context, _, _ string, _ io.Writer) error { return ctx.Err() }
	if _, err := client.Snapshot(ctx, "/work"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestNativeInventoryRunCouldNotBypassOutputBound(t *testing.T) {
	for _, copyOutput := range []bool{false, true} {
		client := NativeInventory{Run: func(_ context.Context, _, _ string, output io.Writer) error {
			data := nativeCurationFixture + strings.Repeat(" ", nativeCurationLimit)
			if copyOutput {
				_, _ = io.Copy(output, struct{ io.Reader }{strings.NewReader(data)})
			} else {
				_, _ = io.WriteString(output, data)
			}
			return nil
		}}
		if _, err := client.Snapshot(context.Background(), "/work"); err == nil {
			t.Fatal("oversized injected output accepted")
		}
	}
}

func TestNativeInventoryPeerUsedOnlyReadMethodsAndClosedPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper shell executable")
	}
	for _, scenario := range []string{"success", "remote-details", "detail-window", "detail-rejected", "detail-mismatch", "detail-not-object", "ambiguous-locator", "detail-hang", "detail-oversized", "partial", "malformed", "request-error", "wrong-id", "oversized", "inherited-stdout"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "native codex")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexec \"$CQ_TEST_NATIVE_INVENTORY_BINARY\" -test.run=^TestNativeInventoryPeerHelper$ -- \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			testBinary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CQ_TEST_NATIVE_INVENTORY_BINARY", testBinary)
			t.Setenv("CQ_TEST_NATIVE_INVENTORY_SCENARIO", scenario)
			t.Setenv("CQ_TEST_NATIVE_INVENTORY_DIR", dir)
			t.Cleanup(func() {
				if data, err := os.ReadFile(filepath.Join(dir, "holder-pid")); err == nil {
					pid, _ := strconv.Atoi(string(data))
					if process, err := os.FindProcess(pid); err == nil {
						_ = process.Kill()
						_ = process.Release()
					}
				}
			})
			timeout := 3 * time.Second
			if scenario == "inherited-stdout" || scenario == "detail-hang" {
				timeout = 500 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			started := time.Now()
			inventory, err := (NativeInventory{Executable: executable}).Snapshot(ctx, dir)
			if slices.Contains([]string{"success", "remote-details", "detail-window", "detail-rejected", "detail-mismatch", "detail-not-object", "ambiguous-locator"}, scenario) {
				wantCount := 10
				if scenario == "detail-window" {
					wantCount = 18
				}
				if err != nil || len(inventory.Capabilities) != wantCount {
					t.Fatalf("native snapshot = %d capabilities, %v", len(inventory.Capabilities), err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "eof"))
				if err != nil || string(data) != "closed" {
					t.Fatal("native child did not observe stdin EOF before completion")
				}
				if slices.Contains([]string{"detail-rejected", "detail-mismatch", "detail-not-object", "ambiguous-locator"}, scenario) {
					if !slices.ContainsFunc(inventory.Issues, func(issue InventoryIssue) bool {
						return issue.Code == "plugin_mcp_ownership_unresolved" && issue.ID == "one@market"
					}) {
						t.Fatal("native detail failure did not produce a local ownership gap")
					}
				} else if slices.ContainsFunc(inventory.Issues, func(issue InventoryIssue) bool { return issue.Kind == PluginKind }) {
					t.Fatal("read-only detail protocol left complete plugin metadata unresolved")
				}
			} else if err == nil {
				t.Fatal("invalid native child accepted")
			}
			if scenario == "inherited-stdout" {
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
					t.Fatalf("inherited pipe escaped cancellation bound: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "holder-pid")); err != nil {
					t.Fatal("inherited stdout fixture did not start")
				}
			}
			if scenario == "detail-hang" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second) {
				t.Fatal("plugin detail read escaped the caller's cancellation bound")
			}
		})
	}
}

func TestNativeInventoryPeerHelper(t *testing.T) {
	scenario := os.Getenv("CQ_TEST_NATIVE_INVENTORY_SCENARIO")
	if scenario == "" {
		return
	}
	if !slices.Equal(os.Args[len(os.Args)-3:], []string{"app-server", "--stdio", "--strict-config"}) {
		os.Exit(80)
	}
	dir := os.Getenv("CQ_TEST_NATIVE_INVENTORY_DIR")
	reader := bufio.NewReader(os.Stdin)
	read := func() map[string]any {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			os.Exit(81)
		}
		var message map[string]any
		if json.Unmarshal(line, &message) != nil {
			os.Exit(82)
		}
		return message
	}
	write := func(value any) {
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			os.Exit(83)
		}
	}
	initialize := read()
	if initialize["method"] != "initialize" {
		os.Exit(84)
	}
	if scenario == "inherited-stdout" {
		binary, _ := os.Executable()
		holder := exec.Command(binary, "-test.run=^TestNativeInventoryPipeHolder$")
		holder.Stdout, holder.Stderr = os.Stdout, io.Discard
		if holder.Start() != nil {
			os.Exit(85)
		}
		_ = os.WriteFile(filepath.Join(dir, "holder-pid"), []byte(strconv.Itoa(holder.Process.Pid)), 0o600)
		os.Exit(0)
	}
	if scenario == "malformed" {
		_, _ = io.WriteString(os.Stdout, "invalid JSON\n")
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(0)
	}
	if scenario == "oversized" {
		_, _ = io.WriteString(os.Stdout, strings.Repeat(" ", 4<<20)+"\n")
		_, _ = io.Copy(io.Discard, reader)
		os.Exit(0)
	}
	write(map[string]any{"id": 99, "result": map[string]any{}})
	write(map[string]any{"id": initialize["id"], "result": map[string]any{}})
	if read()["method"] != "initialized" {
		os.Exit(86)
	}
	fixture := strings.ReplaceAll(nativeCurationFixture, `"cwd":"/work"`, `"cwd":`+strconv.Quote(dir))
	var response map[string]json.RawMessage
	_ = json.Unmarshal([]byte(fixture), &response)
	if scenario == "remote-details" {
		response["plugins"] = json.RawMessage(strings.Replace(string(response["plugins"]), `"path":"/private/marketplace.json"`, `"path":null`, 1))
		for _, name := range []string{"one", "disabled"} {
			response["plugins"] = json.RawMessage(strings.Replace(string(response["plugins"]), `"name":"`+name+`"`, `"name":"`+name+`","remotePluginId":"remote-`+name+`"`, 1))
		}
	}
	if scenario == "ambiguous-locator" {
		response["plugins"] = json.RawMessage(strings.Replace(string(response["plugins"]), `"name":"disabled"`, `"name":"one"`, 1))
	}
	if scenario == "detail-window" {
		var plugins map[string]any
		var details map[string]any
		_ = json.Unmarshal(response["plugins"], &plugins)
		_ = json.Unmarshal(response["pluginDetails"], &details)
		marketplace := plugins["marketplaces"].([]any)[0].(map[string]any)
		for index := 0; index < 8; index++ {
			name := "extra-" + strconv.Itoa(index)
			marketplace["plugins"] = append(marketplace["plugins"].([]any), map[string]any{"id": name + "@market", "name": name, "installed": true, "enabled": true})
			details[name+"@market"] = map[string]any{"plugin": map[string]any{"summary": map[string]string{"id": name + "@market", "name": name}, "mcpServers": []string{}, "apps": []any{}}}
		}
		response["plugins"], _ = json.Marshal(plugins)
		response["pluginDetails"], _ = json.Marshal(details)
	}
	for index, method := range []string{"plugin/installed", "skills/list", "config/read"} {
		request := read()
		if request["method"] != method {
			os.Exit(87)
		}
		params, ok := request["params"].(map[string]any)
		if !ok {
			os.Exit(88)
		}
		if index < 2 && !reflect.DeepEqual(params["cwds"], []any{dir}) {
			os.Exit(89)
		}
		if index == 1 && params["forceReload"] != true {
			os.Exit(90)
		}
		if index == 2 && (params["cwd"] != dir || params["includeLayers"] != true) {
			os.Exit(91)
		}
		if scenario == "partial" && index == 1 {
			_, _ = io.WriteString(os.Stdout, `{"id":3,"result":`)
			os.Exit(0)
		}
		if scenario == "request-error" && index == 2 {
			write(map[string]any{"id": request["id"], "error": map[string]any{"code": -1, "message": "private-secret"}})
			_, _ = io.Copy(io.Discard, reader)
			os.Exit(0)
		}
		id := request["id"]
		if scenario == "wrong-id" && index == 2 {
			id = 999
		}
		write(map[string]any{"id": id, "result": response[[]string{"plugins", "skills", "config"}[index]]})
		if scenario == "wrong-id" && index == 2 {
			os.Exit(0)
		}
	}
	if scenario == "detail-window" {
		var details map[string]json.RawMessage
		_ = json.Unmarshal(response["pluginDetails"], &details)
		// Refuse to answer until the bounded window arrives, then reply out of
		// order. A serial reader or response-order assumption cannot pass.
		for _, count := range []int{8, 2} {
			var batch []map[string]any
			for index := 0; index < count; index++ {
				request := read()
				params, ok := request["params"].(map[string]any)
				if request["method"] != "plugin/read" || !ok || params["marketplacePath"] != "/private/marketplace.json" {
					os.Exit(96)
				}
				batch = append(batch, request)
			}
			for index := len(batch) - 1; index >= 0; index-- {
				request := batch[index]
				name, _ := request["params"].(map[string]any)["pluginName"].(string)
				write(map[string]any{"id": request["id"], "result": details[name+"@market"]})
			}
		}
	} else if scenario != "ambiguous-locator" {
		var details map[string]json.RawMessage
		_ = json.Unmarshal(response["pluginDetails"], &details)
		for _, name := range []string{"disabled", "one"} {
			request := read()
			params, ok := request["params"].(map[string]any)
			requestedName := name
			if scenario == "remote-details" {
				requestedName = "remote-" + name
			}
			if request["method"] != "plugin/read" || !ok || params["pluginName"] != requestedName {
				os.Exit(93)
			}
			if scenario == "remote-details" {
				if params["remoteMarketplaceName"] != "market" || params["marketplacePath"] != nil {
					os.Exit(94)
				}
			} else if params["marketplacePath"] != "/private/marketplace.json" || params["remoteMarketplaceName"] != nil {
				os.Exit(95)
			}
			if name == "one" && scenario == "detail-hang" {
				_, _ = io.Copy(io.Discard, reader)
				os.Exit(0)
			}
			if name == "one" && scenario == "detail-oversized" {
				_, _ = io.WriteString(os.Stdout, strings.Repeat(" ", 4<<20)+"\n")
				_, _ = io.Copy(io.Discard, reader)
				os.Exit(0)
			}
			if name == "one" && scenario == "detail-rejected" {
				write(map[string]any{"id": request["id"], "error": map[string]any{"message": "private-secret"}})
				continue
			}
			var result any = details[name+"@market"]
			if name == "one" && scenario == "detail-mismatch" {
				result = json.RawMessage(strings.Replace(string(details[name+"@market"]), `"id":"one@market"`, `"id":"wrong@market"`, 1))
			}
			if name == "one" && scenario == "detail-not-object" {
				result = []any{}
			}
			write(map[string]any{"id": request["id"], "result": result})
		}
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		os.Exit(92)
	}
	_ = os.WriteFile(filepath.Join(dir, "eof"), []byte("closed"), 0o600)
	os.Exit(0)
}

func TestNativeInventoryPipeHolder(t *testing.T) {
	if os.Getenv("CQ_TEST_NATIVE_INVENTORY_SCENARIO") != "inherited-stdout" {
		return
	}
	time.Sleep(5 * time.Second)
	os.Exit(0)
}

func TestNativeInventoryInstalledReadOnly(t *testing.T) {
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI for native metadata-only inspection")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var captured nativeCurationOutput
	client := NativeInventory{Executable: executable, Run: func(ctx context.Context, executable, cwd string, output io.Writer) error {
		return runNativeCurationInventory(ctx, executable, cwd, io.MultiWriter(output, &captured))
	}}
	inventory, err := client.Snapshot(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[Kind]int)
	enabledCounts := make(map[Kind]int)
	availableCounts := make(map[Kind]int)
	issueCodes := make(map[string]int)
	disabled := 0
	for _, capability := range inventory.Capabilities {
		counts[capability.Kind]++
		if capability.Enabled {
			enabledCounts[capability.Kind]++
		}
		if capability.Available {
			availableCounts[capability.Kind]++
		}
		if !capability.Enabled {
			disabled++
		}
	}
	for _, issue := range inventory.Issues {
		issueCodes[issue.Code]++
	}
	t.Logf("metadata counts: plugins=%d mcp=%d skills=%d disabled=%d issues=%v", counts[PluginKind], counts[MCPKind], counts[SkillKind], disabled, issueCodes)
	t.Logf("native enabled=%v available=%v", enabledCounts, availableCounts)
	// Exercise the native-alpha omission using the same live raw layers/origins,
	// without another child or any configuration write.
	var envelope map[string]json.RawMessage
	var config map[string]json.RawMessage
	var merged map[string]json.RawMessage
	if json.Unmarshal(captured.Bytes(), &envelope) != nil || json.Unmarshal(envelope["config"], &config) != nil || json.Unmarshal(config["config"], &merged) != nil {
		t.Fatal("live config metadata could not be decoded")
	}
	delete(merged, "mcp_servers")
	config["config"], _ = json.Marshal(merged)
	envelope["config"], _ = json.Marshal(config)
	masked, _ := json.Marshal(envelope)
	fallback, err := decodeNativeInventory(masked, cwd)
	if err != nil {
		t.Fatal(err)
	}
	eligibility := func(inventory Inventory) map[string][2]bool {
		values := make(map[string][2]bool)
		for _, capability := range inventory.Capabilities {
			if capability.Kind == MCPKind {
				values[capability.ID] = [2]bool{capability.Enabled, capability.Available}
			}
		}
		return values
	}
	if !reflect.DeepEqual(eligibility(inventory), eligibility(fallback)) {
		t.Fatal("active raw-layer fallback changed live MCP eligibility")
	}
	t.Logf("same live MCP eligibility recovered after omitting merged mcp_servers: %d entries", len(eligibility(fallback)))
}
