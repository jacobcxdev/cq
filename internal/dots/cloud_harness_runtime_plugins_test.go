package dots

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func cloudHarnessPluginRuntimeFixture(t *testing.T, respond func(cloudMessage) any) *cloudHarness {
	t.Helper()
	b := &cloudBridge{ctx: context.Background(), deps: cloudStdioDependencies{harnessTimeout: 5 * time.Second}}
	h := &cloudHarness{b: b, prefix: "cq-plugin-runtime-test/", calls: make(map[string]chan cloudMessage)}
	b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		var request cloudMessage
		if json.Unmarshal(raw, &request) != nil || request.method() != "experimentalFeature/list" {
			t.Fatal("plugin runtime discovery used an unexpected native operation")
		}
		params := cloudHarnessObject(request["params"])
		if cloudHarnessString(params, "threadId") != "chat" || string(params["limit"]) != "100" {
			t.Fatal("plugin runtime discovery did not select the chat and page limit")
		}
		result := respond(params)
		response := cloudMessage{"id": request["id"], "result": cloudHarnessRaw(result)}
		if result == nil {
			response = cloudMessage{"id": request["id"], "error": cloudHarnessRaw(map[string]string{"message": "private-native-error"})}
		}
		h.observe(cloudHarnessRaw(response))
		return len(raw), nil
	}}}
	return h
}

func TestCloudHarnessPluginRuntimeFoundEffectiveFlagOnThirdPage(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			calls := 0
			h := cloudHarnessPluginRuntimeFixture(t, func(params cloudMessage) any {
				calls++
				if calls > 1 && cloudHarnessString(params, "cursor") != fmt.Sprint(calls-1) {
					t.Fatal("plugin runtime cursor was not preserved")
				}
				if calls == 3 {
					return map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": enabled, "description": "irrelevant metadata"}}, "nextCursor": nil}
				}
				rows := make([]any, 100)
				for index := range rows {
					rows[index] = map[string]any{"name": fmt.Sprintf("feature-%d-%d", calls, index), "enabled": false}
				}
				return map[string]any{"data": rows, "nextCursor": fmt.Sprint(calls)}
			})
			got, err := h.readPluginRuntime("chat")
			if err != nil || got != enabled || calls != 3 {
				t.Fatalf("plugin runtime flag=%t err=%v pages=%d, want %t and three pages", got, err, calls, enabled)
			}
		})
	}
}

func TestCloudHarnessPluginRuntimeRejectedIncompleteOrAmbiguousPages(t *testing.T) {
	for _, test := range []struct {
		name   string
		result any
	}{
		{"rejected native request", nil},
		{"missing data", map[string]any{"nextCursor": nil}},
		{"null data", map[string]any{"data": nil, "nextCursor": nil}},
		{"object data", map[string]any{"data": map[string]any{}, "nextCursor": nil}},
		{"missing plugin flag", map[string]any{"data": []any{map[string]any{"name": "other", "enabled": true}}, "nextCursor": nil}},
		{"missing boolean", map[string]any{"data": []any{map[string]any{"name": "plugins"}}, "nextCursor": nil}},
		{"null boolean", map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": nil}}, "nextCursor": nil}},
		{"string boolean", map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": "true"}}, "nextCursor": nil}},
		{"missing name", map[string]any{"data": []any{map[string]any{"enabled": true}}, "nextCursor": nil}},
		{"invalid name", map[string]any{"data": []any{map[string]any{"name": "private-native-error\n", "enabled": true}}, "nextCursor": nil}},
		{"duplicate plugins", map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": true}, map[string]any{"name": "plugins", "enabled": true}}, "nextCursor": nil}},
		{"missing cursor", map[string]any{"data": []any{map[string]any{"name": "plugins", "enabled": true}}}},
		{"invalid cursor", map[string]any{"data": []any{}, "nextCursor": 1}},
		{"empty cursor", map[string]any{"data": []any{}, "nextCursor": ""}},
		{"control cursor", map[string]any{"data": []any{}, "nextCursor": "private-native-error\n"}},
		{"oversized cursor", map[string]any{"data": []any{}, "nextCursor": strings.Repeat("x", 4097)}},
		{"oversized metadata", map[string]any{"data": []any{}, "nextCursor": nil, "metadata": strings.Repeat("x", 8<<20)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := cloudHarnessPluginRuntimeFixture(t, func(cloudMessage) any { return test.result })
			if _, err := h.readPluginRuntime("chat"); err == nil || strings.Contains(err.Error(), "private-native-error") {
				t.Fatalf("unsafe plugin runtime response was accepted or exposed: %v", err)
			}
		})
	}
	rows := make([]any, 101)
	for index := range rows {
		rows[index] = map[string]any{"name": fmt.Sprint(index), "enabled": false}
	}
	h := cloudHarnessPluginRuntimeFixture(t, func(cloudMessage) any { return map[string]any{"data": rows, "nextCursor": nil} })
	if _, err := h.readPluginRuntime("chat"); err == nil {
		t.Fatal("plugin runtime accepted more than 100 rows in one page")
	}
}

func TestCloudHarnessPluginRuntimeScannedCompleteBoundedPagination(t *testing.T) {
	for _, mode := range []string{"duplicate later", "malformed later", "repeated cursor", "too many pages", "cumulative size"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			h := cloudHarnessPluginRuntimeFixture(t, func(cloudMessage) any {
				calls++
				rows := []any{}
				if calls == 1 || mode == "duplicate later" {
					rows = append(rows, map[string]any{"name": "plugins", "enabled": true})
				}
				if mode == "malformed later" && calls == 2 {
					rows = append(rows, map[string]any{"name": "other", "enabled": nil})
				}
				var cursor any = fmt.Sprint(calls)
				if mode == "repeated cursor" {
					cursor = "again"
				}
				if calls == 2 && (mode == "duplicate later" || mode == "malformed later" || mode == "cumulative size") {
					cursor = nil
				}
				result := map[string]any{"data": rows, "nextCursor": cursor}
				if mode == "cumulative size" {
					result["metadata"] = strings.Repeat("x", (4<<20)+1)
				}
				return result
			})
			if _, err := h.readPluginRuntime("chat"); err == nil || calls > 4 || calls < 2 {
				t.Fatalf("plugin runtime pagination was incomplete or unbounded: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestCloudHarnessPluginRuntimeRejectedEmptyChatAndCancellation(t *testing.T) {
	h := cloudHarnessPluginRuntimeFixture(t, func(cloudMessage) any { t.Fatal("invalid plugin runtime read reached native"); return nil })
	if _, err := h.readPluginRuntime(""); err == nil {
		t.Fatal("empty chat plugin runtime read succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.b.ctx = ctx
	if _, err := h.readPluginRuntime("chat"); err == nil {
		t.Fatal("cancelled plugin runtime read succeeded")
	}
}
