package dots

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessSelectivePlanKeptOnlySelectedCapabilities(t *testing.T) {
	baseline := cloudMessage{"features.plugins": cloudHarnessRaw(true), "skills.include_instructions": cloudHarnessRaw(true), "mcp_servers.one.enabled": cloudHarnessRaw(true), "mcp_servers.two.enabled": cloudHarnessRaw(false), "skills.config": cloudHarnessRaw([]any{})}
	inventory := harness.Inventory{Capabilities: []harness.Capability{
		{Kind: harness.PluginKind, ID: "keep@market", NativeID: "keep@market", Enabled: true, Available: true},
		{Kind: harness.PluginKind, ID: "drop@market", NativeID: "drop@market", Enabled: true, Available: true},
		{Kind: harness.MCPKind, ID: "one", NativeID: "one", Enabled: true, Available: true},
		{Kind: harness.MCPKind, ID: "two", NativeID: "two", Enabled: false, Available: true},
		{Kind: harness.SkillKind, ID: "user:keep", Path: "/skills/keep/SKILL.md", Enabled: true, Available: true},
		{Kind: harness.SkillKind, ID: "user:drop", Path: "/skills/drop/SKILL.md", Enabled: true, Available: true},
	}}
	effective := harness.Effective{Selection: harness.Selection{Managed: true, Profile: harness.Custom, Fingerprint: "one"}, Changes: harness.Changes{Default: harness.Disabled, Plugins: map[string]harness.Decision{"keep@market": harness.Enabled}, MCP: map[string]harness.Decision{"two": harness.Enabled}, Skills: map[string]harness.Decision{"user:keep": harness.Enabled}}}
	plan := cloudHarnessCompileProfile(effective, inventory, baseline, []string{"manual@market"})
	if !cloudHarnessPluginEqual(plan.plugins, []string{"manual@market", "drop@market"}) {
		t.Fatalf("wrong selective plugin exclusions: %v", plan.plugins)
	}
	if !bytes.Equal(plan.config["mcp_servers.one.enabled"], []byte("false")) || !bytes.Equal(plan.config["mcp_servers.two.enabled"], []byte("true")) || !bytes.Equal(plan.config["skills.include_instructions"], []byte("true")) {
		t.Fatalf("wrong selective native flags: %v", plan.config)
	}
	var skills []struct {
		Path    string
		Enabled bool
	}
	_ = json.Unmarshal(plan.config["skills.config"], &skills)
	states := map[string]bool{}
	for _, skill := range skills {
		states[skill.Path] = skill.Enabled
	}
	if !states["/skills/keep/SKILL.md"] || states["/skills/drop/SKILL.md"] {
		t.Fatal("skill catalogue entries did not follow exact path selections")
	}
	if len(plan.unsupported) != 0 || len(plan.issues) != 0 {
		t.Fatalf("valid selective profile reported unsupported: %v %v", plan.unsupported, plan.issues)
	}
}

func TestCloudHarnessSelectivePlanSkippedInactiveMCP(t *testing.T) {
	inventory := harness.Inventory{Capabilities: []harness.Capability{
		{Kind: harness.MCPKind, ID: "live", NativeID: "live", Enabled: true, Available: true},
		{Kind: harness.MCPKind, ID: "disabled", NativeID: "disabled", Available: true},
		{Kind: harness.MCPKind, ID: "inactive", NativeID: "inactive"},
	}}
	for _, decision := range []harness.Decision{harness.Disabled, harness.Inherit, harness.Enabled} {
		t.Run(string(decision), func(t *testing.T) {
			effective := harness.Effective{Changes: harness.Changes{Default: decision}}
			plan := cloudHarnessCompileProfile(effective, inventory, nil, nil)
			if _, exists := plan.config["mcp_servers.inactive.enabled"]; exists {
				// An inactive layer supplies no transport to the live config.
				// Even enabled=false would create an invalid native definition.
				t.Fatal("inactive MCP was materialised without a transport")
			}
			for _, name := range []string{"live", "disabled"} {
				if _, exists := plan.config[cloudHarnessMCPKey(name, "enabled")]; !exists {
					t.Fatalf("active MCP definition %q lost its per-chat control", name)
				}
			}
			if decision == harness.Enabled && !slices.Contains(plan.issues, "unavailable:mcp:inactive") {
				t.Fatal("request to enable an inactive MCP lost its availability issue")
			}
		})
	}
}

func TestCloudHarnessMCPInlineOverrideKeptSelection(t *testing.T) {
	for _, key := range []string{"mcp_servers.node_repl", `mcp_servers."node_repl"`, "mcp_servers"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", key, enabled), func(t *testing.T) {
				server := cloudMessage{"command": cloudHarnessRaw("/native/node"), "enabled": cloudHarnessRaw(!enabled)}
				raw := cloudHarnessRaw(server)
				if key == "mcp_servers" {
					raw = cloudHarnessRaw(cloudMessage{"node_repl": raw})
				}
				baseline := cloudMessage{key: raw}
				decision := harness.Disabled
				if enabled {
					decision = harness.Enabled
				}
				plan := cloudHarnessCompileProfile(harness.Effective{Changes: harness.Changes{MCP: map[string]harness.Decision{"node_repl": decision}}}, harness.Inventory{Capabilities: []harness.Capability{{Kind: harness.MCPKind, ID: "node_repl", NativeID: "node_repl", Available: true}}}, baseline, nil)
				actual := cloudHarnessObject(plan.config[key])
				if key == "mcp_servers" {
					actual = cloudHarnessObject(actual["node_repl"])
				}
				if cloudHarnessBool(actual, "enabled", !enabled) != enabled || cloudHarnessBool(plan.config, "mcp_servers.node_repl.enabled", !enabled) != enabled {
					t.Fatal("whole-server and enabled overrides disagreed")
				}
				if !bytes.Equal(actual["command"], server["command"]) || !bytes.Equal(baseline[key], raw) {
					t.Fatal("MCP selection changed transport or caller baseline")
				}
			})
		}
	}
}

func cloudHarnessSelectiveInventory() harness.Inventory {
	return harness.Inventory{Capabilities: []harness.Capability{
		{Kind: harness.PluginKind, ID: "keep@market", NativeID: "keep@market", Enabled: true, Available: true},
		{Kind: harness.PluginKind, ID: "drop@market", NativeID: "drop@market", Enabled: true, Available: true},
		{Kind: harness.MCPKind, ID: "first", NativeID: "first", Enabled: true, Available: true},
		{Kind: harness.MCPKind, ID: "second", NativeID: "second", Enabled: true, Available: true},
		{Kind: harness.SkillKind, ID: "user:keep", Path: "/skills/keep/SKILL.md", Enabled: true, Available: true},
		{Kind: harness.SkillKind, ID: "user:drop", Path: "/skills/drop/SKILL.md", Enabled: true, Available: true},
	}}
}

func newCloudHarnessSelectiveFixture(t *testing.T) *cloudHarnessFixture {
	f := newCloudHarnessFixture(t)
	f.policy = harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled, Plugins: map[string]harness.Decision{"keep@market": harness.Enabled}, MCP: map[string]harness.Decision{"first": harness.Enabled}, Skills: map[string]harness.Decision{"user:keep": harness.Enabled}}}}}
	f.deps.readHarnessPolicy = func() (harness.Policy, error) { return harness.Policy{}, errors.New("legacy reader must not be used") }
	f.deps.readHarnessContextPolicy = func(string) (harness.Policy, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.policy, nil }
	f.deps.readHarnessInventory = func(string) (harness.Inventory, error) { return cloudHarnessSelectiveInventory(), nil }
	return f
}

func TestCloudHarnessSelectiveGPTNewChatAppliedBeforeFirstTurn(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	f.deps.harnessReceipts = store
	f.start()
	f.initialize()
	response := f.request(2, "thread/start", map[string]any{"model": "gpt-5.5", "cwd": "/work", "unknown": "retained", "disabledPluginIds": []string{"manual@market"}})
	id := cloudThreadID(response["result"])
	if id == "" {
		t.Fatalf("selective OpenAI start failed: %s", response["error"])
	}
	f.request(3, "turn/start", map[string]any{"threadId": id})
	f.waitEvent(id, "applied", "custom")
	starts := f.messages("thread/start")
	var params struct {
		Config  cloudMessage
		Unknown string
	}
	_ = json.Unmarshal(starts[0]["params"], &params)
	if params.Unknown != "retained" || !bytes.Equal(params.Config["mcp_servers.first.enabled"], []byte("true")) || !bytes.Equal(params.Config["mcp_servers.second.enabled"], []byte("false")) || !bytes.Equal(params.Config["skills.include_instructions"], []byte("true")) {
		t.Fatalf("selective GPT config incorrect: %s", starts[0]["params"])
	}
	if len(f.messages("thread/resume")) != 0 {
		t.Fatal("new chat unnecessarily released its initial subscription")
	}
	updates := f.messages("thread/settings/update")
	if len(updates) != 1 || !strings.Contains(string(updates[0]["params"]), "drop@market") || !strings.Contains(string(updates[0]["params"]), "manual@market") || strings.Contains(string(updates[0]["params"]), "keep@market") {
		t.Fatal("plugin subset was not set before first turn")
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.Profile != harness.Custom || receipt.Fingerprint == "" || receipt.Model != "gpt-5.5" || receipt.Cwd != "/work" {
		t.Fatalf("selective receipt incomplete: %#v %v", receipt, err)
	}
}

func TestCloudHarnessSelectiveTaskChangeWaitedForIdleAndChangedSameCustomProfile(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.start()
	f.initialize()
	id := f.startThread("gpt-5.5", "/project")
	f.request(3, "turn/start", map[string]any{"threadId": id})
	f.waitEvent(id, "applied", "custom")
	f.mu.Lock()
	f.policy.Tasks = map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{MCP: map[string]harness.Decision{"first": harness.Disabled, "second": harness.Enabled}}}}}
	f.mu.Unlock()
	_, _ = f.localOutput.Write([]byte(`{"method":"thread/status/changed","params":{"threadId":"` + id + `","status":{"type":"active"}}}` + "\n"))
	f.waitEvent(id, "pending", "custom")
	if len(f.messages("thread/resume")) != 0 {
		t.Fatal("active custom chat was reloaded")
	}
	f.completeStoredTurn(id)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for len(f.messages("thread/resume")) == 0 {
		select {
		case <-deadline.C:
			t.Fatal("changed custom profile was never reloaded")
		case <-time.After(time.Millisecond):
		}
	}
	f.request(4, "turn/start", map[string]any{"threadId": id})
	resumes := f.messages("thread/resume")
	var params struct {
		ThreadID string `json:"threadId"`
		Config   cloudMessage
	}
	_ = json.Unmarshal(resumes[0]["params"], &params)
	if params.ThreadID != id || !bytes.Equal(params.Config["mcp_servers.first.enabled"], []byte("false")) || !bytes.Equal(params.Config["mcp_servers.second.enabled"], []byte("true")) {
		t.Fatalf("same-labelled custom profile failed to change capabilities: %s", resumes[0]["params"])
	}
}

func TestCloudHarnessSelectivePortableWorkspaceRulesWereLoaded(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	fsys := fsutil.NewMemFS()
	policy := harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}
	if err := harness.Save(fsys, "/config/harness.json", policy); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll("/work/.cq", 0o755); err != nil {
		t.Fatal(err)
	}
	portable := harness.WorkspacePolicy{Version: 2, Rules: harness.Scope{All: harness.Binding{Changes: harness.Changes{MCP: map[string]harness.Decision{"second": harness.Enabled}}}}}
	raw, _ := json.Marshal(portable)
	if err := fsys.WriteFile("/work/.cq/harness.json", raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f.deps.readHarnessContextPolicy = func(cwd string) (harness.Policy, error) {
		return harness.LoadForContext(fsys, "/config/harness.json", cwd)
	}
	f.start()
	f.initialize()
	f.startThread("gpt-5.5", filepath.Join("/work", "child"))
	starts := f.messages("thread/start")
	var params struct{ Config cloudMessage }
	_ = json.Unmarshal(starts[0]["params"], &params)
	if !bytes.Equal(params.Config["mcp_servers.second.enabled"], []byte("true")) || !bytes.Equal(params.Config["mcp_servers.first.enabled"], []byte("false")) {
		t.Fatal("portable workspace selection was not applied")
	}
}

func TestCloudHarnessSelectiveModelRulesChangedNativeCapabilities(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy = harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}, Models: map[string]harness.Binding{"glm-*": {Changes: harness.Changes{MCP: map[string]harness.Decision{"first": harness.Enabled}}}, "gpt-*": {Changes: harness.Changes{MCP: map[string]harness.Decision{"second": harness.Enabled}}}}}}
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.completeStoredTurn(id)
	f.request(3, "turn/start", map[string]any{"threadId": id, "model": "gpt-5.5"})
	resumes := f.messages("thread/resume")
	if len(resumes) != 1 {
		t.Fatalf("model-specific profile did not reload: %d", len(resumes))
	}
	var params struct {
		Model  string
		Config cloudMessage
	}
	_ = json.Unmarshal(resumes[0]["params"], &params)
	if params.Model != "gpt-5.5" || !bytes.Equal(params.Config["mcp_servers.first.enabled"], []byte("false")) || !bytes.Equal(params.Config["mcp_servers.second.enabled"], []byte("true")) {
		t.Fatalf("model-specific native controls incorrect: %s", resumes[0]["params"])
	}
}

func TestCloudHarnessSelectiveEmptyChatWaitedForDurableHistory(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		t.Run(fmt.Sprint("ephemeral=", ephemeral), func(t *testing.T) {
			f := newCloudHarnessSelectiveFixture(t)
			store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
			f.deps.harnessReceipts = store
			originalPolicy := f.policy
			f.start()
			f.initialize()
			response := f.request(2, "thread/start", map[string]any{"model": "glm-5.3", "cwd": "/project", "ephemeral": ephemeral})
			id := cloudThreadID(response["result"])
			f.waitEvent(id, "applied", "custom")
			f.mu.Lock()
			f.policy.Tasks = map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Plugins: map[string]harness.Decision{"keep@market": harness.Disabled, "drop@market": harness.Enabled}, MCP: map[string]harness.Decision{"first": harness.Disabled, "second": harness.Enabled}}}}}
			f.mu.Unlock()
			if reply := f.request(3, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) != 0 {
				t.Fatal("pending first-turn change blocked the original chat")
			}
			f.waitEvent(id, "pending", "custom")
			f.mu.Lock()
			plugins := slices.Clone(f.plugins[id])
			f.mu.Unlock()
			if !cloudHarnessPluginEqual(plugins, []string{"keep@market"}) {
				t.Fatal("pending configuration change did not apply safe plugin exclusions")
			}
			if len(f.messages("thread/unsubscribe")) != 0 || len(f.messages("thread/resume")) != 0 {
				t.Fatal("empty chat was released before it had durable history")
			}
			f.completeStoredTurn(id)
			if ephemeral {
				f.request(4, "thread/settings/update", map[string]any{"threadId": id, "disabledPluginIds": []string{"keep@market", "manual@market"}})
				f.setPolicy(originalPolicy)
				f.request(5, "turn/start", map[string]any{"threadId": id})
				f.mu.Lock()
				plugins = slices.Clone(f.plugins[id])
				f.mu.Unlock()
				if !cloudHarnessPluginEqual(plugins, []string{"drop@market", "manual@market"}) {
					t.Fatal("policy revert confused pending exclusions with manual choices")
				}
				revertedReceipt, err := store.Load(id)
				if err != nil || !cloudHarnessPluginEqual(revertedReceipt.BaselinePlugins, []string{"manual@market"}) {
					t.Fatal("pending plugin exclusions were absorbed into the manual baseline")
				}
				f.completeStoredTurn(id)
				f.request(6, "thread/settings/update", map[string]any{"threadId": id, "disabledPluginIds": []string{"manual@market"}})
				f.request(7, "turn/start", map[string]any{"threadId": id})
				receipt, err := store.Load(id)
				if err != nil || receipt.State != "pending" {
					t.Fatalf("same-key plugin reapplication left receipt %q: %v", receipt.State, err)
				}
				if len(f.messages("thread/unsubscribe")) != 0 {
					t.Fatal("ephemeral chat was released after a completed turn")
				}
				return
			}
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			for len(f.messages("thread/resume")) == 0 {
				select {
				case <-deadline.C:
					t.Fatal("pending change was not applied after the first turn")
				case <-time.After(time.Millisecond):
				}
			}
			if len(f.messages("turn/start")) != 1 || len(f.messages("turn/interrupt")) != 0 {
				t.Fatal("idle application replayed or interrupted the original turn")
			}
		})
	}
}

func TestCloudHarnessSelectiveEmptyChatRejectedPluginSettingsBlockedTurn(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.waitEvent(id, "applied", "custom")
	f.mu.Lock()
	f.policy.Tasks = map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Plugins: map[string]harness.Decision{"keep@market": harness.Disabled}, MCP: map[string]harness.Decision{"first": harness.Disabled}}}}}
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "thread/settings/update" && strings.Contains(string(request["params"]), "keep@market") {
			return nil, errors.New("reject plugin exclusions"), true
		}
		return nil, nil, false
	}
	f.mu.Unlock()
	if reply := f.request(3, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) == 0 {
		t.Fatal("rejected plugin exclusions admitted the first turn")
	}
	f.waitEvent(id, "failed", "custom")
	if len(f.messages("turn/start")) != 0 || len(f.messages("thread/unsubscribe")) != 0 {
		t.Fatal("failed first-turn settings forwarded work or released the chat")
	}
}

func TestCloudHarnessPendingPluginsKeptManualOldPlanChoices(t *testing.T) {
	thread := cloudHarnessThread{
		baseline:       cloudMessage{"disabledPluginIds": cloudHarnessRaw([]string{})},
		profileManaged: true,
		profilePlan:    &cloudHarnessProfilePlan{plugins: []string{"drop@market"}},
		pendingProfile: &cloudHarnessProfilePlan{plugins: []string{"keep@market"}},
		appliedPlugins: []string{"keep@market"},
	}
	cloudHarnessManualPlugins(&thread, cloudHarnessRaw([]string{"drop@market"}))
	var baseline []string
	if json.Unmarshal(thread.baseline["disabledPluginIds"], &baseline) != nil || !cloudHarnessPluginEqual(baseline, []string{"drop@market"}) {
		t.Fatal("manual old-plan choice was mistaken for a CQ acknowledgement")
	}
}

func TestCloudHarnessSelectiveUnsupportedAndDependencyFailureWerePartial(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	f.deps.harnessReceipts = store
	f.policy.Global.All.Changes = harness.Changes{Default: harness.Disabled, Plugins: map[string]harness.Decision{"keep@market": harness.Disabled}, Skills: map[string]harness.Decision{"user:missing-path": harness.Enabled, "plugin:dependent": harness.Enabled}}
	f.deps.readHarnessInventory = func(string) (harness.Inventory, error) {
		inventory := cloudHarnessSelectiveInventory()
		inventory.Capabilities = append(inventory.Capabilities, harness.Capability{Kind: harness.SkillKind, ID: "user:missing-path", Available: true}, harness.Capability{Kind: harness.SkillKind, ID: "plugin:dependent", Path: "/skills/dependent/SKILL.md", PluginID: "keep@market", Available: true, Dependencies: []string{"plugin:keep@market"}})
		return inventory, nil
	}
	f.start()
	f.initialize()
	id := f.startThread("gpt-5.5", "/project")
	f.request(3, "turn/start", map[string]any{"threadId": id})
	f.waitEvent(id, "partial", "custom")
	receipt, err := store.Load(id)
	if err != nil || receipt.State != "partial" || len(receipt.Unsupported) == 0 || len(receipt.Issues) == 0 {
		t.Fatalf("unsupported/dependency selection falsely complete: %#v %v", receipt, err)
	}
	f.eventsMu.Lock()
	defer f.eventsMu.Unlock()
	for _, event := range f.events {
		if event["thread_id"] == id && event["state"] == "applied" {
			t.Fatal("partially supported selection emitted applied")
		}
	}
}

func TestCloudHarnessSelectiveFullRestoredManualChoicesWithoutPolicyEchoes(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy.Global.All.Changes = harness.Changes{Plugins: map[string]harness.Decision{"drop@market": harness.Disabled}}
	f.start()
	f.initialize()
	response := f.request(2, "thread/start", map[string]any{"model": "gpt-5.5", "cwd": "/project", "disabledPluginIds": []string{"manual@market"}})
	id := cloudThreadID(response["result"])
	f.request(3, "turn/start", map[string]any{"threadId": id, "disabledPluginIds": []string{"manual@market", "drop@market"}})
	f.request(4, "thread/settings/update", map[string]any{"threadId": id, "disabledPluginIds": []string{"manual@market", "drop@market", "keep@market"}})
	f.setPolicy(harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Profiles: []string{"full"}}}})
	f.completeStoredTurn(id)
	f.waitEvent(id, "applied", "full")
	updates := f.messages("thread/settings/update")
	var params struct {
		Disabled []string `json:"disabledPluginIds"`
	}
	_ = json.Unmarshal(updates[len(updates)-1]["params"], &params)
	if !cloudHarnessPluginEqual(params.Disabled, []string{"manual@market", "keep@market"}) {
		t.Fatalf("Full lost manual exclusions or retained policy echo: %v", params.Disabled)
	}
}

func TestCloudHarnessSelectiveFullClearedPluginExclusionsWithAnArray(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy.Global.All.Changes = harness.Changes{Plugins: map[string]harness.Decision{"drop@market": harness.Disabled}}
	f.start()
	f.initialize()
	id := f.startThread("gpt-5.5", "/project")
	f.request(3, "turn/start", map[string]any{"threadId": id})
	// This fixture helper supplies a manual exclusion. Remove it as an explicit
	// user choice while retaining the exclusion applied by the policy.
	f.request(4, "thread/settings/update", map[string]any{"threadId": id, "disabledPluginIds": []string{"drop@market"}})
	f.setPolicy(harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Profiles: []string{"full"}}}})
	f.completeStoredTurn(id)
	f.waitEvent(id, "applied", "full")
	updates := f.messages("thread/settings/update")
	params := cloudHarnessObject(updates[len(updates)-1]["params"])
	if !bytes.Equal(params["disabledPluginIds"], []byte("[]")) {
		t.Fatalf("clearing exclusions must send an explicit native array: %s", params["disabledPluginIds"])
	}
}

func TestCloudHarnessSelectiveCurationKeptOnlyEnabledNativeBaseline(t *testing.T) {
	for _, included := range []bool{false, true} {
		t.Run(map[bool]string{false: "globally-off", true: "enabled"}[included], func(t *testing.T) {
			baseline := cloudMessage{"features.plugins": cloudHarnessRaw(true), "skills.include_instructions": cloudHarnessRaw(included), "skills.config": cloudHarnessRaw([]any{})}
			inventory := harness.Inventory{Capabilities: []harness.Capability{{Kind: harness.SkillKind, ID: harness.CurationSkillID, NativeID: "curate-harness", Path: "/skills/curate-harness/SKILL.md", Enabled: true, Available: true}}}
			effective := harness.Effective{Selection: harness.Selection{Managed: true, Profile: harness.Minimal}, Changes: harness.Changes{Default: harness.Disabled}}
			plan := cloudHarnessCompileProfile(effective, inventory, baseline, nil)
			if cloudHarnessBool(plan.config, "skills.include_instructions", !included) != included {
				t.Fatal("curation pin changed the native global-off baseline or removed enabled escape hatch")
			}
			var entries []struct{ Enabled bool }
			_ = json.Unmarshal(plan.config["skills.config"], &entries)
			if len(entries) != 1 || entries[0].Enabled != included {
				t.Fatal("curation exact path did not follow shared capability decision")
			}
		})
	}
}

func TestCloudHarnessSelectiveNativeDisabledPluginEnableRemainedUnsupported(t *testing.T) {
	baseline := cloudMessage{"features.plugins": cloudHarnessRaw(true), "skills.include_instructions": cloudHarnessRaw(true), "skills.config": cloudHarnessRaw([]any{})}
	inventory := harness.Inventory{Capabilities: []harness.Capability{{Kind: harness.PluginKind, ID: "native-off@market", NativeID: "native-off@market", Enabled: false, Available: true}}}
	effective := harness.Effective{Selection: harness.Selection{Managed: true, Profile: harness.Custom}, Changes: harness.Changes{Plugins: map[string]harness.Decision{"native-off@market": harness.Enabled}}}
	plan := cloudHarnessCompileProfile(effective, inventory, baseline, nil)
	if cloudHarnessProfileState(&plan) != "partial" || len(plan.unsupported) != 1 || plan.unsupported[0] != "plugin-enable:native-off@market" {
		t.Fatal("unsupported per-thread native plugin enable was reported applied")
	}
}

func TestCloudHarnessSelectivePlanReportedUnavailableAndMissingSkillPath(t *testing.T) {
	baseline := cloudMessage{"features.plugins": cloudHarnessRaw(true), "skills.include_instructions": cloudHarnessRaw(true), "skills.config": cloudHarnessRaw([]any{})}
	inventory := harness.Inventory{Capabilities: []harness.Capability{{Kind: harness.SkillKind, ID: "user:missing-path", Enabled: true, Available: true}, {Kind: harness.PluginKind, ID: "missing@market", NativeID: "missing@market", Available: false}}}
	effective := harness.Effective{Selection: harness.Selection{Managed: true, Profile: harness.Custom}, Changes: harness.Changes{Default: harness.Enabled}}
	plan := cloudHarnessCompileProfile(effective, inventory, baseline, nil)
	if len(plan.unsupported) == 0 || len(plan.issues) == 0 {
		t.Fatalf("incomplete native profile was reported complete: %v %v", plan.unsupported, plan.issues)
	}
}

func TestCloudHarnessSelectiveUnmatchedModelWasByteExactAndSkippedNativeInventory(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy = harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{Models: map[string]harness.Binding{"glm-*": {Changes: harness.Changes{Default: harness.Disabled}}}}}
	var reads atomic.Int64
	f.deps.readHarnessInventory = func(string) (harness.Inventory, error) {
		reads.Add(1)
		return harness.Inventory{}, errors.New("unmanaged inventory must not be called")
	}
	fsys := fsutil.NewMemFS()
	f.deps.harnessReceipts = harness.Receipts{FS: fsys, Dir: "/state/harness"}
	f.start()
	f.initialize()
	start := []byte(`{ "id": 2, "method": "thread/start", "params": {"model":"gpt-5.5", "cwd":"/project", "unknown": {"retained":true}} }` + "\n")
	if _, err := f.input.Write(start); err != nil {
		t.Fatal(err)
	}
	response := f.await(2)
	id := cloudThreadID(response["result"])
	if id == "" {
		t.Fatal("unmatched GPT start failed")
	}
	turn := []byte(`{ "id": 3, "method": "turn/start", "params": {"threadId":"` + id + `", "input":[], "unknown": 17} }` + "\n")
	if _, err := f.input.Write(turn); err != nil {
		t.Fatal(err)
	}
	if reply := f.await(3); len(reply["error"]) != 0 {
		t.Fatalf("unmanaged chat was blocked: %s", reply["error"])
	}
	if reads.Load() != 0 {
		t.Fatalf("unmatched model made %d inventory calls", reads.Load())
	}
	for _, method := range []string{"config/read", "thread/read", "thread/unsubscribe", "thread/resume", "thread/settings/update"} {
		if len(f.messages(method)) != 0 {
			t.Fatalf("unmatched model made private %s calls", method)
		}
	}
	for _, raw := range f.localMessages() {
		var message cloudMessage
		_ = json.Unmarshal(raw, &message)
		if message.method() == "thread/start" && !bytes.Equal(raw, start) || message.method() == "turn/start" && !bytes.Equal(raw, turn) {
			t.Fatal("unmatched native request bytes changed")
		}
	}
	if _, err := fsys.Stat("/state/harness"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmanaged chat created ownership/receipt state: %v", err)
	}
}

func TestCloudHarnessSelectiveManagedToUnmatchedModelRestoredThenSkippedInventory(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy = harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{Models: map[string]harness.Binding{"glm-*": {Changes: harness.Changes{Default: harness.Disabled}}}}}
	var reads atomic.Int64
	var fail atomic.Bool
	f.deps.readHarnessInventory = func(string) (harness.Inventory, error) {
		reads.Add(1)
		if fail.Load() {
			return harness.Inventory{}, errors.New("unmanaged inventory unavailable")
		}
		return cloudHarnessSelectiveInventory(), nil
	}
	f.start()
	f.initialize()
	id := f.startThread("glm-5.3", "/project")
	f.completeStoredTurn(id)
	f.waitEvent(id, "applied", "minimal")
	if response := f.request(3, "turn/start", map[string]any{"threadId": id, "model": "gpt-5.5"}); len(response["error"]) != 0 {
		t.Fatal("model transition did not restore baseline")
	}
	resumes := f.messages("thread/resume")
	if len(resumes) != 1 {
		t.Fatalf("managed-to-unmatched transition made %d reloads", len(resumes))
	}
	var params struct {
		Model  string
		Config cloudMessage
	}
	_ = json.Unmarshal(resumes[0]["params"], &params)
	if params.Model != "gpt-5.5" || !cloudHarnessBool(params.Config, "mcp_servers.first.enabled", false) || !cloudHarnessBool(params.Config, "skills.include_instructions", false) || !cloudHarnessBool(params.Config, "features.plugins", false) {
		t.Fatalf("unmatched model inherited managed restrictions: %s", resumes[0]["params"])
	}
	before := reads.Load()
	fail.Store(true)
	if response := f.request(4, "turn/start", map[string]any{"threadId": id}); len(response["error"]) != 0 {
		t.Fatalf("restored unmanaged chat was blocked by inventory: %s", response["error"])
	}
	if reads.Load() != before || len(f.messages("thread/resume")) != 1 {
		t.Fatal("restored unmanaged chat was managed again")
	}
}

func TestCloudHarnessSelectiveWorkspaceMoveRebasedProjectDefaults(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy = harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	f.deps.harnessReceipts = store
	nativeCwd := "/A"
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		params := cloudHarnessObject(request["params"])
		switch request.method() {
		case "config/read":
			cwd := cloudHarnessString(params, "cwd")
			name := "server_a"
			if cwd == "/B" {
				name = "server_b"
			}
			return map[string]any{"config": map[string]any{"model": "gpt-5.5", "features": map[string]any{"plugins": true}, "mcp_servers": map[string]any{name: map[string]any{"enabled": true}}}, "layers": []any{}}, nil, true
		case "thread/settings/update":
			if cwd := cloudHarnessString(params, "cwd"); cwd != "" {
				nativeCwd = cwd
			}
			id := cloudThreadID(request["params"])
			if raw, ok := params["disabledPluginIds"]; ok {
				var plugins []string
				_ = json.Unmarshal(raw, &plugins)
				f.plugins[id] = plugins
			}
			return map[string]any{}, nil, true
		case "thread/read":
			id := cloudThreadID(request["params"])
			return map[string]any{"thread": map[string]any{"id": id, "model": f.models[id], "cwd": nativeCwd, "status": map[string]any{"type": "idle"}}}, nil, true
		}
		return nil, nil, false
	}
	f.deps.readHarnessInventory = func(cwd string) (harness.Inventory, error) {
		name, skill := "server_a", "a"
		if cwd == "/B" {
			name, skill = "server_b", "b"
		}
		return harness.Inventory{Capabilities: []harness.Capability{{Kind: harness.MCPKind, ID: name, NativeID: name, Enabled: true, Available: true}, {Kind: harness.SkillKind, ID: "repo:" + skill, Path: cwd + "/.agents/skills/" + skill + "/SKILL.md", Enabled: true, Available: true}}}, nil
	}
	f.start()
	f.initialize()
	id := f.startThread("gpt-5.5", "/A")
	f.completeStoredTurn(id)
	f.waitEvent(id, "applied", "minimal")
	f.request(3, "thread/settings/update", map[string]any{"threadId": id, "cwd": "/B"})
	waitB := func(profile harness.Profile, state string) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		for {
			receipt, err := store.Load(id)
			if err == nil && receipt.Cwd == "/B" && receipt.Profile == profile && receipt.State == state {
				return
			}
			select {
			case <-deadline.C:
				t.Fatalf("workspace B profile %s did not reach %s", profile, state)
			case <-time.After(time.Millisecond):
			}
		}
	}
	// Both workspaces already expose no selected MCPs or plugins. No runtime
	// transition can confirm that the changed skill paths were reassembled.
	waitB(harness.Minimal, "partial")
	f.setPolicy(harness.Policy{Version: 2, Enabled: true, Global: harness.Scope{All: harness.Binding{Profiles: []string{"full"}}}})
	_, _ = f.localOutput.Write([]byte(`{"method":"thread/status/changed","params":{"threadId":"` + id + `","status":{"type":"idle"}}}` + "\n"))
	waitB(harness.Full, "applied")
	resumes := f.messages("thread/resume")
	if len(resumes) != 2 {
		t.Fatalf("workspace move and Full restore made %d reloads", len(resumes))
	}
	for index, resume := range resumes {
		params := cloudHarnessObject(resume["params"])
		config := cloudHarnessObject(params["config"])
		if cloudThreadID(resume["params"]) != id || cloudHarnessString(params, "cwd") != "/B" {
			t.Fatal("workspace move changed chat identity or working directory")
		}
		for key := range config {
			if strings.Contains(key, "server_a") {
				t.Fatalf("workspace A MCP override leaked into B: %s", resume["params"])
			}
		}
		if bytes.Contains(config["skills.config"], []byte("/A/")) || !bytes.Contains(config["skills.config"], []byte("/B/.agents/skills/b/SKILL.md")) {
			t.Fatalf("workspace skill overrides were not rebased: %s", config["skills.config"])
		}
		if cloudHarnessBool(config, "mcp_servers.server_b.enabled", index == 0) != (index == 1) || cloudHarnessString(config, "user.option") != "retained" {
			t.Fatalf("workspace B native defaults or original per-chat override lost: %s", resume["params"])
		}
	}
	if len(f.messages("thread/start")) != 1 {
		t.Fatal("workspace move created another chat")
	}
}

func TestCloudHarnessRuntimeDisabledIgnoredMalformedPortablePolicy(t *testing.T) {
	fsys := fsutil.NewMemFS()
	policy := harness.Policy{Version: 2, Enabled: false}
	if err := harness.Save(fsys, "/config/harness.json", policy); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll("/work/.cq", 0o755); err != nil {
		t.Fatal(err)
	}
	path := "/work/.cq/harness.json"
	malformed := []byte(`{"version":2,`)
	if err := fsys.WriteFile(path, malformed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.LoadForContext(fsys, "/config/harness.json", "/work"); err == nil {
		t.Fatal("fixture did not contain an invalid portable policy")
	}
	got, err := cloudRuntimeHarnessPolicy(fsys, "/config/harness.json", "/work")
	if err != nil || got.Enabled || got.Version != 2 {
		t.Fatalf("runtime disable escape was blocked by portable metadata: %#v %v", got, err)
	}
	after, err := fsys.ReadFile(path)
	if err != nil || !bytes.Equal(after, malformed) {
		t.Fatal("runtime policy lookup changed portable policy")
	}
	policy.Enabled = true
	if err := harness.Save(fsys, "/config/harness.json", policy); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudRuntimeHarnessPolicy(fsys, "/config/harness.json", "/work"); err == nil {
		t.Fatal("enabled runtime policy ignored malformed portable rules")
	}
	after, err = fsys.ReadFile(path)
	if err != nil || !bytes.Equal(after, malformed) {
		t.Fatal("enabled lookup changed portable policy")
	}
}
