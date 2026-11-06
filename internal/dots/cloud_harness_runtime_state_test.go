package dots

import (
	"slices"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

func TestCloudHarnessRetainedMCPAssemblyWasNotFullyApplied(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy.Global = harness.Scope{}
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	f.deps.harnessReceipts = store
	// Another subscriber keeps the original assembly alive. Native accepts
	// unsubscribe/resume but still exposes the MCP tool that CQ deselected.
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "mcpServerStatus/list" {
			return map[string]any{"data": []any{map[string]any{"name": "first", "tools": map[string]any{"probe": map[string]any{}}, "resources": []any{}, "resourceTemplates": []any{}, "runtimeStatus": "connected"}}, "nextCursor": nil}, nil, true
		}
		return nil, nil, false
	}
	f.start()
	f.initialize()
	id := f.startThread("gpt-5.5", "/project")
	f.completeStoredTurn(id)
	f.setPolicy(harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled}}}}})
	if reply := f.request(3, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) != 0 {
		t.Fatalf("partially applied profile blocked the chat: %s", reply["error"])
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.State != "partial" || len(receipt.Issues) == 0 {
		t.Fatalf("retained callable MCP was reported as fully applied: %#v, %v", receipt, err)
	}
	resumes := len(f.messages("thread/resume"))
	f.completeStoredTurn(id)
	if reply := f.request(4, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) != 0 {
		t.Fatalf("same profile refresh blocked the chat: %s", reply["error"])
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.State != "partial" || len(receipt.Issues) == 0 || len(f.messages("thread/resume")) != resumes {
		t.Fatalf("same profile erased verification failure or reloaded repeatedly: %#v, %v", receipt, err)
	}
}

func TestCloudHarnessSkillUncertaintySurvivedRestoreAndRebind(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy.Global = harness.Scope{}
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	f.deps.harnessReceipts = store
	f.start()
	f.initialize()
	id := f.startThread("gpt-5.5", "/project")
	f.completeStoredTurn(id)
	changes := harness.Changes{Skills: map[string]harness.Decision{"user:drop": harness.Disabled}}
	f.setPolicy(harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: changes}}}})
	checkPartial := func(requestID int) {
		t.Helper()
		if reply := f.request(requestID, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) != 0 {
			t.Fatalf("unverified skill edit blocked the chat: %s", reply["error"])
		}
		receipt, err := store.Load(id)
		if err != nil || receipt.State != "partial" || !slices.Contains(receipt.Issues, "native_skill_runtime_unverified") {
			t.Fatalf("skill uncertainty was discarded: %#v, %v", receipt, err)
		}
		f.completeStoredTurn(id)
	}
	checkPartial(3)
	f.mu.Lock()
	stillEnabled := cloudHarnessBool(cloudHarnessSkillSettings(f.configs[id]["skills.config"])["/skills/drop/SKILL.md"], "enabled", true)
	// The next reload is accepted but ignored by a retained native subscriber.
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "thread/resume" {
			return map[string]any{"thread": map[string]any{"id": id, "cwd": "/project"}, "model": "gpt-5.5"}, nil, true
		}
		return nil, nil, false
	}
	f.mu.Unlock()
	if stillEnabled {
		t.Fatal("fixture did not physically apply the unverified skill reduction")
	}
	f.setPolicy(harness.Policy{Version: 2, Enabled: true})
	checkPartial(4)
	// A different selection fingerprint cannot promote that uncertainty to
	// confirmed Full or treat the previously requested configuration as actual.
	f.setPolicy(harness.Policy{Version: 2, Enabled: true, Profiles: map[string]harness.Definition{"again": {Changes: changes}}, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Profiles: []string{"again"}}}}})
	checkPartial(5)
}

func TestCloudHarnessRetainedPluginFeaturePreventedFullRestoration(t *testing.T) {
	f := newCloudHarnessSelectiveFixture(t)
	f.policy.Global = harness.Scope{}
	store := harness.Receipts{FS: fsutil.NewMemFS(), Dir: "/state/harness"}
	f.deps.harnessReceipts = store
	f.start()
	f.initialize()
	id := f.startThread("gpt-5.5", "/project")
	f.completeStoredTurn(id)
	changes := harness.Changes{Default: harness.Disabled, MCP: map[string]harness.Decision{"first": harness.Enabled, "second": harness.Enabled}, Skills: map[string]harness.Decision{"user:keep": harness.Enabled, "user:drop": harness.Enabled}}
	f.setPolicy(harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: changes}}}})
	if reply := f.request(3, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) != 0 {
		t.Fatalf("plugin reduction failed: %s", reply["error"])
	}
	receipt, err := store.Load(id)
	if err != nil || receipt.State != "applied" {
		t.Fatalf("observed plugin feature change was not confirmed: %#v, %v", receipt, err)
	}
	f.completeStoredTurn(id)
	f.mu.Lock()
	f.onRequest = func(request cloudMessage) (any, error, bool) {
		if request.method() == "thread/resume" {
			return map[string]any{"thread": map[string]any{"id": id, "cwd": "/project"}, "model": "gpt-5.5"}, nil, true
		}
		return nil, nil, false
	}
	f.mu.Unlock()
	f.setPolicy(harness.Policy{Version: 2, Enabled: true})
	if reply := f.request(4, "turn/start", map[string]any{"threadId": id}); len(reply["error"]) != 0 {
		t.Fatalf("unverified plugin restoration blocked the chat: %s", reply["error"])
	}
	receipt, err = store.Load(id)
	if err != nil || receipt.State != "partial" || !slices.Contains(receipt.Issues, "native_plugin_runtime_retained") {
		t.Fatalf("retained disabled plugin feature was reported as restored: %#v, %v", receipt, err)
	}
}
