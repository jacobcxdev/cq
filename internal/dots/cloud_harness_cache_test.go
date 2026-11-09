package dots

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cacheprofile"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode/messages"
)

// The native client owns inference assembly. Project the controller's actual
// selected plan into a synthetic inference fixture, then use CQ's production
// profiler and GLM converter. This measures structural bytes, not provider hits.
func TestCloudHarnessReconciliationRetainedCacheContract(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "glm-5.3"} {
		t.Run(model, func(t *testing.T) {
			h, store, id, methods := cloudHarnessFailureFixture(t)
			thread := h.threads[id]
			thread.model = model
			policy := harness.Policy{Version: 2, Enabled: true, Tasks: map[string]harness.Scope{id: {All: harness.Binding{Changes: harness.Changes{Default: harness.Disabled, MCP: map[string]harness.Decision{"first": harness.Enabled}, Skills: map[string]harness.Decision{"user:keep": harness.Enabled}}}}}}
			policyBefore := cloudHarnessRaw(policy)
			h.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return policy, nil }
			inventory, reads := cloudHarnessSelectiveInventory(), 0
			h.b.deps.readHarnessInventory = func(string) (harness.Inventory, error) { reads++; return inventory, nil }
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			planKey := thread.profilePlan.key
			config := cloudHarnessClone(thread.profilePlan.config)
			plugins := slices.Clone(thread.profilePlan.plugins)
			baseline, lease := cloudHarnessClone(thread.baseline), thread.lease
			fingerprint, selection := thread.profileFingerprint, thread.selection
			unchanged := func() bool {
				return thread.profilePlan != nil && thread.profilePlan.key == planKey && reflect.DeepEqual(thread.profilePlan.config, config) && reflect.DeepEqual(thread.profilePlan.plugins, plugins) && thread.selection == selection && thread.profileFingerprint == fingerprint && reflect.DeepEqual(thread.baseline, baseline) && thread.lease == lease && bytes.Equal(cloudHarnessRaw(policy), policyBefore)
			}
			for name, mutate := range map[string]func() func(){
				"compiled_config": func() func() {
					before := cloudHarnessClone(thread.profilePlan.config)
					thread.profilePlan.config["fixture_new_setting"] = cloudHarnessRaw(true)
					return func() { thread.profilePlan.config = before }
				},
				"plugin_exclusions": func() func() {
					before := slices.Clone(thread.profilePlan.plugins)
					thread.profilePlan.plugins = append(thread.profilePlan.plugins, "new@fixture")
					return func() { thread.profilePlan.plugins = before }
				},
				"full_selection": func() func() {
					before := thread.selection
					thread.selection.Source = "unexpected"
					return func() { thread.selection = before }
				},
			} {
				restore := mutate()
				if unchanged() {
					t.Fatalf("negative control missed %s mutation", name)
				}
				restore()
				if !unchanged() {
					t.Fatalf("negative control did not restore %s", name)
				}
			}
			profiler, err := cacheprofile.New(bytes.Repeat([]byte{7}, 32))
			if err != nil {
				t.Fatal(err)
			}
			control, _ := cloudHarnessCacheSnapshot(t, profiler, thread)
			assertStable := func(stage string) {
				t.Helper()
				current, body := cloudHarnessCacheSnapshot(t, profiler, thread)
				comparison := cacheprofile.Compare(control, current)
				if !comparison.PrefixStable || comparison.SharedComponents != len(control.Components) || comparison.ReusableBytes != comparison.PreviousBytes {
					t.Fatalf("%s lost inference prefix: %+v", stage, comparison)
				}
				if !unchanged() {
					t.Fatalf("%s changed selection, plan, baseline, policy or owner", stage)
				}
				t.Logf("%s: shared_components=%d reusable_bytes=%d previous_bytes=%d wire_bytes=%d model=%s output_limit=4096", stage, comparison.SharedComponents, comparison.ReusableBytes, comparison.PreviousBytes, len(body), thread.model)
			}

			// Discovery is not a curation decision. Neither new inventory nor
			// repeated reconciliation may grow the profile or reload native state.
			inventory.Capabilities = append(inventory.Capabilities, harness.Capability{Kind: harness.MCPKind, ID: "new", NativeID: "new", Enabled: true, Available: true})
			before, inventoryBefore := len(*methods), reads
			for range 3 {
				if err := h.reconcile(id, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			if len(*methods) != before || reads != inventoryBefore {
				t.Fatal("unchanged reconciliation rediscovered or reloaded capabilities")
			}
			assertStable("unchanged_reconciliation")

			settings, reject := 0, true
			base := h.b.local.input
			h.b.local.input = cloudTestWriter{write: func(raw []byte) (int, error) {
				request := cloudHarnessObject(raw)
				if request.method() == "thread/settings/update" {
					settings++
					if reject {
						h.calls[string(request["id"])] <- cloudMessage{"error": cloudHarnessRaw(map[string]any{"code": -32042, "message": "private-fixture"})}
						return len(raw), nil
					}
				}
				return base.Write(raw)
			}}
			thread.profileNeedsSettings = true
			if err := h.reconcile(id, "", ""); err == nil {
				t.Fatal("fixture did not reject native settings")
			}
			failed, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := h.reconcile(id, "", ""); err == nil {
					t.Fatal("failed application was not cached")
				}
			}
			if settings != 1 || len(*methods) != before || reads != inventoryBefore {
				t.Fatalf("cached failure repeated native work: settings=%d other_calls=%d inventory_reads=%d", settings, len(*methods)-before, reads-inventoryBefore)
			}
			assertStable("cached_native_failure")
			if _, err := store.RequestRetry(id, harness.RetryRequest{Fingerprint: failed.Fingerprint, Model: failed.Model, Cwd: failed.Cwd, FailedAt: failed.UpdatedAt}); err != nil {
				t.Fatal(err)
			}
			thread.active = true
			if err := h.reconcile(id, "", ""); err != nil || settings != 1 || len(*methods) != before {
				t.Fatalf("active retry touched native state: %v", err)
			}
			assertStable("retry_waited_for_idle")
			thread.active, reject = false, false
			if err := h.reconcile(id, "", ""); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := h.reconcile(id, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			if settings != 2 || len(*methods) != before+1 || reads != inventoryBefore {
				t.Fatalf("explicit retry was not one hot update: settings=%d other_calls=%d inventory_reads=%d", settings, len(*methods)-before, reads-inventoryBefore)
			}
			assertStable("one_explicit_retry")
			recovered, err := store.Load(id)
			if err != nil || recovered.State == "failed" || recovered.Failure != nil {
				t.Fatalf("recovery retained failure: %#v %v", recovered, err)
			}

			// An ordinary ongoing native request retains exact bytes. Its input,
			// model, reasoning and unknown native fields are not rebuilt by CQ.
			thread.active = true
			raw := []byte(`{"id":101,"method":"turn/start","params":{"threadId":"` + id + `","model":"` + model + `","effort":"high","input":[{"type":"text","text":"Continue the accepted task."}],"futureNativeField":{"retained":true}}}` + "\n")
			forwarded, err := h.prepareFrom("stdio", cloudHarnessObject(raw), raw)
			if err != nil || !bytes.Equal(forwarded, raw) || len(*methods) != before+1 || reads != inventoryBefore {
				t.Fatalf("ongoing turn changed bytes or native state: %v", err)
			}
			assertStable("ordinary_active_turn")
		})
	}
}

func cloudHarnessCacheSnapshot(t *testing.T, profiler *cacheprofile.Profiler, thread *cloudHarnessThread) (cacheprofile.Snapshot, []byte) {
	t.Helper()
	plan := thread.profilePlan
	if plan == nil || !cloudHarnessBool(plan.config, "mcp_servers.first.enabled", false) || cloudHarnessBool(plan.config, "mcp_servers.second.enabled", true) || cloudHarnessBool(plan.config, "mcp_servers.new.enabled", false) {
		t.Fatal("explicit selected tool set changed")
	}
	tools := []any{map[string]any{"type": "function", "name": "fixture_first_read", "parameters": map[string]any{"type": "object"}}}
	request := map[string]any{
		"model": thread.model, "max_output_tokens": 4096, "reasoning": map[string]any{"effort": "high"}, "tools": tools,
		"instructions": "Synthetic selected skill settings: " + string(plan.config["skills.config"]),
		"input":        []any{map[string]any{"role": "user", "content": strings.Repeat("Stable synthetic research context. ", 256)}},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	protocol := cacheprofile.Responses
	if strings.HasPrefix(thread.model, "glm-") {
		body, _, err = messages.Project(body)
		if err != nil {
			t.Fatal(err)
		}
		protocol = cacheprofile.Messages
	}
	snapshot, err := profiler.Capture(protocol, body)
	if err != nil || !snapshot.Complete {
		t.Fatalf("inference fixture was incomplete: %v", err)
	}
	return snapshot, body
}
