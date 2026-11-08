package zcode_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cacheprofile"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode/messages"
)

// These are semantic cache contracts, not snapshots of implementation output.
// Every append-only conversion must retain its complete prior content prefix.
// A deliberate loss needs a case-specific reason, reviewed with the change.
func TestCacheContractZCodeContinuations(t *testing.T) {
	base := map[string]any{
		"model":        "glm-5.3",
		"instructions": "Stable harness policy.",
		"tools":        []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
		"input": []any{
			map[string]any{"role": "developer", "content": "Stable developer instructions."},
			map[string]any{"role": "user", "content": strings.Repeat("Existing conversation content. ", 200)},
			map[string]any{"type": "function_call", "name": "read_file", "call_id": "call-1", "arguments": `{"path":"example.txt"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "Existing tool result."},
		},
	}
	for _, test := range []struct {
		name          string
		append        []any
		reset         bool
		justification string
	}{
		{name: "user follow-up", append: []any{map[string]any{"role": "user", "content": "Continue."}}},
		{name: "runtime developer reminder", append: []any{map[string]any{"role": "developer", "content": "<current_time_reminder>2026-11-08T12:00Z</current_time_reminder>"}}},
		{name: "runtime system reminder", append: []any{map[string]any{"role": "system", "content": "New runtime constraint."}}},
		{name: "tool continuation", append: []any{map[string]any{"type": "function_call", "name": "read_file", "call_id": "call-2", "arguments": `{"path":"next.txt"}`}, map[string]any{"type": "function_call_output", "call_id": "call-2", "output": "Next tool result."}}},
		{name: "compacted history", reset: true, justification: "Compaction intentionally replaces old history with a summary; warming this new prefix is expected."},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			var next map[string]any
			if err = json.Unmarshal(before, &next); err != nil {
				t.Fatal(err)
			}
			if test.reset {
				next["input"] = []any{map[string]any{"role": "user", "content": "Compacted summary."}}
			} else {
				next["input"] = append(next["input"].([]any), test.append...)
			}
			after, err := json.Marshal(next)
			if err != nil {
				t.Fatal(err)
			}
			first, _, err := messages.Project(before)
			if err != nil {
				t.Fatal(err)
			}
			second, _, err := messages.Project(after)
			if err != nil {
				t.Fatal(err)
			}
			profiler, err := cacheprofile.New([]byte(strings.Repeat("x", 32)))
			if err != nil {
				t.Fatal(err)
			}
			a, err := profiler.Capture(cacheprofile.Messages, first)
			if err != nil {
				t.Fatal(err)
			}
			b, err := profiler.Capture(cacheprofile.Messages, second)
			if err != nil {
				t.Fatal(err)
			}
			comparison := cacheprofile.Compare(a, b)
			if !comparison.PrefixStable && strings.TrimSpace(test.justification) == "" {
				t.Fatalf("unjustified cache prefix regression: %+v", comparison)
			}
			if test.reset && comparison.PrefixStable {
				t.Fatal("compaction control did not exercise a prefix reset")
			}
			if !test.reset && comparison.ReusableBytes != comparison.PreviousBytes {
				t.Fatalf("append-only conversion lost prefix: %+v", comparison)
			}
		})
	}
}
