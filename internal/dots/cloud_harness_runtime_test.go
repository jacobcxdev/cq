package dots

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func cloudHarnessRuntimeFixture(t *testing.T, respond func(cloudMessage) any) *cloudHarness {
	t.Helper()
	b := &cloudBridge{ctx: context.Background(), deps: cloudStdioDependencies{harnessTimeout: 5 * time.Second}}
	h := &cloudHarness{b: b, prefix: "cq-runtime-test/", calls: make(map[string]chan cloudMessage)}
	b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		var request cloudMessage
		if json.Unmarshal(raw, &request) != nil || request.method() != "mcpServerStatus/list" {
			t.Fatal("runtime verification used an unexpected native operation")
		}
		params := cloudHarnessObject(request["params"])
		if cloudHarnessString(params, "threadId") != "chat" || cloudHarnessString(params, "detail") != "full" || string(params["limit"]) != "100" {
			t.Fatal("runtime verification was not bounded to the selected chat")
		}
		result := respond(params)
		response := cloudMessage{"id": request["id"], "result": cloudHarnessRaw(result)}
		if result == nil {
			response = cloudMessage{"id": request["id"], "error": cloudHarnessRaw(map[string]string{"message": "private-secret"})}
		}
		h.observe(cloudHarnessRaw(response))
		return len(raw), nil
	}}}
	return h
}

func TestCloudHarnessRuntimeReadCountedStandaloneToolsAcrossPages(t *testing.T) {
	calls := 0
	h := cloudHarnessRuntimeFixture(t, func(params cloudMessage) any {
		calls++
		if calls == 1 {
			return map[string]any{"data": json.RawMessage(`[{"name":"disabled","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"},{"name":"probe","pluginId":"bundle@market","tools":{"bundled":{}},"resources":[],"resourceTemplates":[],"runtimeStatus":"connected"}]`), "nextCursor": "next"}
		}
		if cloudHarnessString(params, "cursor") != "next" {
			t.Fatal("runtime cursor was not preserved")
		}
		return map[string]any{"data": json.RawMessage(`[{"name":"probe.with.dot","tools":{"one":{},"two":{}},"resources":[{}],"resourceTemplates":[{}],"runtimeStatus":"connected"}]`), "nextCursor": nil}
	})
	got, err := h.readMCPRuntime("chat")
	if err != nil || !reflect.DeepEqual(got, map[string]cloudHarnessMCPRuntime{"disabled": {Status: "disabled"}, "probe.with.dot": {Exposed: 4, Status: "connected"}}) || calls != 2 {
		t.Fatalf("runtime counts = %v, err=%v, calls=%d", got, err, calls)
	}
}

func TestCloudHarnessRuntimeRejectedIncompleteOrUnboundedMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		result any
	}{
		{"rejected", nil}, {"missing data", map[string]any{}}, {"null data", map[string]any{"data": nil}},
		{"missing cursor", map[string]any{"data": []any{}}},
		{"missing tools", map[string]any{"data": json.RawMessage(`[{"name":"probe","resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"}]`)}},
		{"null tools", map[string]any{"data": json.RawMessage(`[{"name":"probe","tools":null,"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"}]`)}},
		{"array tools", map[string]any{"data": json.RawMessage(`[{"name":"probe","tools":[],"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"}]`)}},
		{"missing resources", map[string]any{"data": json.RawMessage(`[{"name":"probe","tools":{},"resourceTemplates":[],"runtimeStatus":"disabled"}]`)}},
		{"null templates", map[string]any{"data": json.RawMessage(`[{"name":"probe","tools":{},"resources":[],"resourceTemplates":null,"runtimeStatus":"disabled"}]`)}},
		{"invalid server name", map[string]any{"data": json.RawMessage(`[{"name":"probe\nprivate-secret","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"}]`)}},
		{"duplicate server", map[string]any{"data": json.RawMessage(`[{"name":"probe","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"},{"name":"probe","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"}]`)}},
		{"invalid cursor", map[string]any{"data": []any{}, "nextCursor": 42}},
		{"oversized cursor", map[string]any{"data": []any{}, "nextCursor": strings.Repeat("private-secret", 400)}},
		{"oversized page", map[string]any{"data": []any{}, "unknown": strings.Repeat("x", 8<<20)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if result, ok := test.result.(map[string]any); ok && test.name != "missing cursor" {
				if _, exists := result["nextCursor"]; !exists {
					result["nextCursor"] = nil
				}
			}
			h := cloudHarnessRuntimeFixture(t, func(cloudMessage) any { return test.result })
			got, err := h.readMCPRuntime("chat")
			if err == nil || got != nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("unsafe runtime result: %v %v", got, err)
			}
		})
	}
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprintf("bounded-pages-repeat-%t", repeat), func(t *testing.T) {
			calls := 0
			h := cloudHarnessRuntimeFixture(t, func(cloudMessage) any {
				calls++
				cursor := fmt.Sprint(calls)
				if repeat {
					cursor = "next"
				}
				return map[string]any{"data": []any{}, "nextCursor": cursor}
			})
			if _, err := h.readMCPRuntime("chat"); err == nil || calls > 4 {
				t.Fatalf("unbounded pages: calls=%d err=%v", calls, err)
			}
		})
	}
	h := cloudHarnessRuntimeFixture(t, func(cloudMessage) any { t.Fatal("cancelled read reached native"); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.b.ctx = ctx
	if _, err := h.readMCPRuntime("chat"); err == nil {
		t.Fatal("cancelled runtime read succeeded")
	}
}

func TestCloudHarnessRuntimeVerifiedExclusionsRestorationAndAssemblyTransition(t *testing.T) {
	for _, test := range []struct {
		name             string
		wanted, previous bool
		rows             string
		before           map[string]cloudHarnessMCPRuntime
		issues           []string
		changed          bool
	}{
		{name: "disabled row", wanted: false, previous: true, rows: `[{"name":"probe","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, changed: true},
		{name: "retained tools", wanted: false, previous: true, rows: `[{"name":"probe","tools":{"retained":{}},"resources":[],"resourceTemplates":[],"runtimeStatus":"connected"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, issues: []string{"native_mcp_runtime_retained:probe"}},
		{name: "retained resources", wanted: false, previous: true, rows: `[{"name":"probe","tools":{},"resources":[{}],"resourceTemplates":[],"runtimeStatus":"connected"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, issues: []string{"native_mcp_runtime_retained:probe"}},
		{name: "retained templates", wanted: false, previous: true, rows: `[{"name":"probe","tools":{},"resources":[],"resourceTemplates":[{}],"runtimeStatus":"connected"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, issues: []string{"native_mcp_runtime_retained:probe"}},
		{name: "connected empty", wanted: false, previous: true, rows: `[{"name":"probe","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"connected"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, issues: []string{"native_mcp_runtime_unverified:probe"}},
		{name: "startup failed", wanted: false, previous: true, rows: `[{"name":"probe","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"failed"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, issues: []string{"native_mcp_runtime_unverified:probe"}},
		{name: "discovery failed", wanted: false, previous: true, rows: `[{"name":"probe","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled","toolsError":"private-secret"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, issues: []string{"native_mcp_runtime_unverified:probe"}},
		{name: "absent server", wanted: false, previous: true, rows: `[]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Exposed: 1, Status: "connected"}}, changed: true},
		{name: "already empty", wanted: false, previous: true, rows: `[]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Status: "disabled"}}},
		{name: "no before snapshot", wanted: false, previous: true, rows: `[]`},
		{name: "restored tools", wanted: true, previous: false, rows: `[{"name":"probe","tools":{"restored":{}},"resources":[],"resourceTemplates":[],"runtimeStatus":"connected"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Status: "disabled"}}, changed: true},
		{name: "recovered startup is not assembly proof", wanted: true, previous: false, rows: `[{"name":"probe","tools":{"restored":{}},"resources":[],"resourceTemplates":[],"runtimeStatus":"connected"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Status: "failed", DiscoveryFailed: true}}},
		{name: "restoration absent", wanted: true, previous: false, rows: `[]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Status: "disabled"}}, issues: []string{"native_mcp_runtime_unverified:probe"}},
		{name: "restoration empty", wanted: true, previous: false, rows: `[{"name":"probe","tools":{},"resources":[],"resourceTemplates":[],"runtimeStatus":"disabled"}]`, before: map[string]cloudHarnessMCPRuntime{"probe": {Status: "disabled"}}, issues: []string{"native_mcp_runtime_unverified:probe"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := cloudHarnessRuntimeFixture(t, func(cloudMessage) any {
				return map[string]any{"data": json.RawMessage(test.rows), "nextCursor": nil}
			})
			key := cloudHarnessMCPKey("probe", "enabled")
			got, changed := h.verifyMCPRuntime("chat", cloudMessage{key: cloudHarnessRaw(test.wanted)}, cloudMessage{key: cloudHarnessRaw(test.previous)}, test.before)
			if !reflect.DeepEqual(got, test.issues) || changed != test.changed {
				t.Fatalf("issues=%v transition=%t, want %v %t", got, changed, test.issues, test.changed)
			}
		})
	}
	h := cloudHarnessRuntimeFixture(t, func(cloudMessage) any {
		t.Fatal("unchanged enabled baseline or skill-only config triggered discovery")
		return nil
	})
	if issues, changed := h.verifyMCPRuntime("chat", cloudMessage{"mcp_servers.probe.enabled": cloudHarnessRaw(true), "skills.config": cloudHarnessRaw([]any{})}, nil, nil); len(issues) > 0 || changed {
		t.Fatalf("unchanged baseline: %v %t", issues, changed)
	}
	h = cloudHarnessRuntimeFixture(t, func(cloudMessage) any { return nil })
	if issues, changed := h.verifyMCPRuntime("chat", cloudMessage{"mcp_servers.probe.enabled": cloudHarnessRaw(false)}, nil, nil); !reflect.DeepEqual(issues, []string{"native_mcp_runtime_unverified"}) || changed {
		t.Fatalf("failed read: %v %t", issues, changed)
	}
	for _, raw := range []json.RawMessage{json.RawMessage("null"), cloudHarnessRaw("false")} {
		issues, changed := h.verifyMCPRuntime("chat", cloudMessage{"mcp_servers.probe.enabled": raw}, nil, nil)
		if !reflect.DeepEqual(issues, []string{"native_mcp_runtime_unverified"}) || changed {
			t.Fatalf("invalid desired flag: %v %t", issues, changed)
		}
	}
}
