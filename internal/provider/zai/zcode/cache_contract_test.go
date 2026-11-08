package zcode_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cacheprofile"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode/messages"
)

type cacheContractPath struct {
	name, model, otherModel string
	protocol                cacheprofile.Protocol
	project                 func([]byte) ([]byte, error)
}

// Native Responses already owns its protocol; ZCode runs the real converter.
// All content is synthetic. No account, model request or user transcript is used.
func cacheContractPaths() []cacheContractPath {
	identity := func(body []byte) ([]byte, error) { return body, nil }
	project := func(body []byte) ([]byte, error) { out, _, err := messages.Project(body); return out, err }
	return []cacheContractPath{
		{"native_responses_luna", "gpt-6-luna", "gpt-6.1-sol", cacheprofile.Responses, identity},
		{"native_responses_sol", "gpt-6.1-sol", "gpt-6-luna", cacheprofile.Responses, identity},
		{"zcode_messages_flagship", "glm-5.3", "glm-5.3-flash", cacheprofile.Messages, project},
		{"zcode_messages_flash", "glm-5.3-flash", "glm-5.3", cacheprofile.Messages, project},
	}
}

// Append-only conversions must retain prior content. Intentional loss names its
// cause and comparison boundary; structural reuse never predicts provider hits.
func TestCacheContractTransitionMatrix(t *testing.T) {
	for _, path := range cacheContractPaths() {
		t.Run(path.name, func(t *testing.T) {
			for _, test := range []struct {
				name               string
				append             []any
				change             func(map[string]any)
				changed, rationale string
				beforeEdit         bool
			}{
				{name: "warm_user_append", append: []any{map[string]any{"role": "user", "content": "Continue."}}},
				{name: "same_history_warm_resume"},
				{name: "pause_resume_without_history_rewrite"},
				{name: "runtime_developer_reminder", append: []any{map[string]any{"role": "developer", "content": "New runtime constraint."}}},
				{name: "runtime_system_reminder", append: []any{map[string]any{"role": "system", "content": "New runtime constraint."}}},
				{name: "runtime_time_reminder", append: []any{map[string]any{"role": "developer", "content": "<current_time_reminder>2026-11-08T12:00Z</current_time_reminder>"}}},
				{name: "automatic_goal_continuation", append: []any{
					map[string]any{"role": "developer", "content": "<goal_context>Goal remains active. Continue the accepted task.</goal_context>"},
					map[string]any{"role": "user", "content": "Continue pursuing the current goal."},
				}},
				{name: "tool_continuation", append: []any{
					map[string]any{"type": "function_call", "name": "read_file", "call_id": "call-2", "arguments": `{"path":"next.txt"}`},
					map[string]any{"type": "function_call_output", "call_id": "call-2", "output": "Next tool result."},
				}},
				{
					name: "edited_earlier_user_truncated_suffix", changed: "content", beforeEdit: true,
					rationale: "Editing an earlier user request discards its obsolete suffix; messages before the edit retain their prefix.",
					change: func(next map[string]any) {
						input := next["input"].([]any)
						next["input"] = append(input[:3:3], map[string]any{"role": "user", "content": "Revised earlier request."})
					},
				},
				{
					name: "compacted_history", changed: "content",
					rationale: "Compaction replaces conversation with a summary while retaining configured harness and tools.",
					change: func(next map[string]any) {
						next["input"] = []any{next["input"].([]any)[0], map[string]any{"role": "user", "content": "Compacted summary of completed work."}}
					},
				},
				{
					name: "changed_tool_catalogue", changed: "tool",
					rationale: "Enabling another tool changes the catalogue before history; existing earlier tools still match.",
					change: func(next map[string]any) {
						next["tools"] = append(next["tools"].([]any), map[string]any{"type": "function", "name": "new_tool", "parameters": map[string]any{"type": "object"}})
					},
				},
				{
					name: "changed_skill_catalogue", changed: "system",
					rationale: "Explicit skill selection changes leading developer instructions; earlier configuration and tools still match.",
					change: func(next map[string]any) {
						next["input"].([]any)[0].(map[string]any)["content"] = "Stable developer instructions. Available skills: fixture-skill and new-skill."
					},
				},
				{
					name: "changed_model", changed: "model",
					rationale: "Models have distinct comparison identities; equal content cannot establish cross-model cache reuse.",
					change:    func(next map[string]any) { next["model"] = path.otherModel },
				},
				{
					name: "changed_top_level_reasoning_effort", changed: "config",
					rationale: "Top-level inference configuration changed. This structural boundary does not assert provider cache invalidation.",
					change:    func(next map[string]any) { next["reasoning"] = map[string]any{"effort": "max"} },
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					before, next := cacheContractRequest(path.model), cacheContractRequest(path.model)
					if test.change != nil {
						test.change(next)
					} else {
						next["input"] = append(next["input"].([]any), test.append...)
					}
					profiler := cacheContractProfiler(t)
					a, b := captureCacheContract(t, profiler, path, before), captureCacheContract(t, profiler, path, next)
					comparison := cacheprofile.Compare(a, b)
					if test.changed == "" {
						if !comparison.PrefixStable || comparison.ReusableBytes != comparison.PreviousBytes || comparison.SharedComponents != len(a.Components) {
							t.Fatalf("append or unchanged history lost earlier prefix: %+v", comparison)
						}
						return
					}
					if test.rationale == "" || comparison.PrefixStable || comparison.ChangedComponent != test.changed || comparison.ReusableBytes >= comparison.PreviousBytes {
						t.Fatalf("intentional change did not stop at documented boundary: reason=%q comparison=%+v", test.rationale, comparison)
					}
					if test.changed == "model" || test.changed == "config" {
						if comparison.ReusableBytes != 0 {
							t.Fatalf("stale model/configuration comparison: %+v", comparison)
						}
					} else if comparison.ReusableBytes <= 0 {
						t.Fatalf("intentional change lost unchanged leading content: %+v", comparison)
					}
					if test.beforeEdit {
						prefix := cacheContractRequest(path.model)
						prefix["input"] = prefix["input"].([]any)[:3]
						prior := captureCacheContract(t, profiler, path, prefix)
						shared := cacheprofile.Compare(prior, prior).PreviousBytes
						if comparison.ReusableBytes < shared {
							t.Fatalf("edit lost content before edited request: reusable=%d earlier=%d", comparison.ReusableBytes, shared)
						}
					}
				})
			}
		})
	}
}

func TestCacheContractStructuralReuseDoesNotInventProviderWarmth(t *testing.T) {
	for _, path := range cacheContractPaths() {
		t.Run(path.name, func(t *testing.T) {
			profiler := cacheContractProfiler(t)
			request := cacheContractRequest(path.model)
			first := captureCacheContract(t, profiler, path, request)
			initial := cacheprofile.Compare(cacheprofile.Snapshot{}, first)
			if initial.PrefixStable || initial.ReusableBytes != 0 || initial.ChangedComponent != "unknown" {
				t.Fatalf("initial request invented prior baseline: %+v", initial)
			}
			second := captureCacheContract(t, profiler, path, request)
			comparison := cacheprofile.Compare(first, second)
			if !comparison.PrefixStable || comparison.ReusableBytes != comparison.PreviousBytes {
				t.Fatalf("same-history observation lost structural prefix: %+v", comparison)
			}
			for _, test := range []struct {
				name, native, projected string
				known                   bool
				cached                  int64
			}{
				{"observed_warm", `{"usage":{"input_tokens":1000,"output_tokens":20,"input_tokens_details":{"cached_tokens":900}}}`, `{"usage":{"input_tokens":100,"cache_read_input_tokens":900,"cache_creation_input_tokens":0,"output_tokens":20}}`, true, 900},
				{"same_history_without_cache_counters", `{"usage":{"input_tokens":1000,"output_tokens":20}}`, `{"usage":{"input_tokens":1000,"output_tokens":20}}`, false, 0},
				{"observed_cold", `{"usage":{"input_tokens":1000,"output_tokens":20,"input_tokens_details":{"cached_tokens":0}}}`, `{"usage":{"input_tokens":1000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":20}}`, true, 0},
			} {
				t.Run(test.name, func(t *testing.T) {
					body := test.native
					if path.protocol == cacheprofile.Messages {
						body = test.projected
					}
					usage, ok := cacheprofile.UsageFromJSON([]byte(body))
					if !ok || usage.Known != test.known || usage.CachedInputTokens != test.cached || usage.InputTokens != 1000 {
						t.Fatalf("structural sameness replaced provider cache evidence: usage=%+v valid=%t", usage, ok)
					}
				})
			}
			// No retention timeout is inferred, including after goal pause/resume.
		})
	}
}

func TestCacheContractEditedTailRetainedLongUserPrefix(t *testing.T) {
	for _, path := range cacheContractPaths() {
		t.Run(path.name, func(t *testing.T) {
			sharedText := strings.Repeat("unchanged fixture text ", 1024)
			before, after := cacheContractRequest(path.model), cacheContractRequest(path.model)
			before["input"] = []any{map[string]any{"role": "user", "content": sharedText + "Old tail."}}
			after["input"] = []any{map[string]any{"role": "user", "content": sharedText + "Edited tail."}}
			profiler := cacheContractProfiler(t)
			a, b := captureCacheContract(t, profiler, path, before), captureCacheContract(t, profiler, path, after)
			comparison := cacheprofile.Compare(a, b)
			// Editing the tail truncates its obsolete suffix, not the large
			// unchanged text before it. Permit one conservative 4 KiB chunk;
			// this remains canonical bytes rather than tokenizer-exact reuse.
			minimum := len(sharedText) - 4096
			if comparison.PrefixStable || !strings.HasPrefix(comparison.ChangedComponent, "content") || comparison.ReusableBytes < minimum || comparison.ReusableBytes >= comparison.PreviousBytes {
				t.Fatalf("tail edit undercounted unchanged content: shared_text=%d minimum=%d comparison=%+v", len(sharedText), minimum, comparison)
			}
		})
	}
}

// Native Responses supports an append-only configuration_update. Rewriting
// top-level reasoning would test a different operation. Messages has no mapped
// equivalent and must reject this item rather than silently dropping it.
func TestCacheContractEffortUpdateRespectedProtocolSupport(t *testing.T) {
	for _, path := range cacheContractPaths() {
		t.Run(path.name, func(t *testing.T) {
			before, after := cacheContractRequest(path.model), cacheContractRequest(path.model)
			before["reasoning"] = map[string]any{"effort": "low"}
			after["reasoning"] = map[string]any{"effort": "low"}
			after["input"] = append(after["input"].([]any), map[string]any{
				"type": "configuration_update", "reasoning": map[string]any{"effort": "high"},
			})
			if path.protocol == cacheprofile.Messages {
				body, err := json.Marshal(after)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := path.project(body); err == nil {
					t.Fatal("Messages conversion silently accepted an unsupported effort-update item")
				}
				return
			}
			profiler := cacheContractProfiler(t)
			a := captureCacheContract(t, profiler, path, before)
			b := captureCacheContract(t, profiler, path, after)
			comparison := cacheprofile.Compare(a, b)
			if !comparison.PrefixStable || comparison.ReusableBytes != comparison.PreviousBytes {
				t.Fatalf("append-only native effort update rewrote earlier configuration/history: %+v", comparison)
			}
		})
	}
}

func cacheContractRequest(model string) map[string]any {
	return map[string]any{
		"model": model, "reasoning": map[string]any{"effort": "high"}, "instructions": "Stable harness policy.",
		"tools": []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
		"input": []any{
			map[string]any{"role": "developer", "content": "Stable developer instructions. Available skills: fixture-skill."},
			map[string]any{"role": "user", "content": "Initial task context."},
			map[string]any{"role": "assistant", "content": "Initial result."},
			map[string]any{"role": "user", "content": strings.Repeat("Earlier request content. ", 200)},
			map[string]any{"role": "assistant", "content": "Subsequent work."},
			map[string]any{"type": "function_call", "name": "read_file", "call_id": "call-1", "arguments": `{"path":"example.txt"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "Existing tool result."},
		},
	}
}

func cacheContractProfiler(t *testing.T) *cacheprofile.Profiler {
	t.Helper()
	profiler, err := cacheprofile.New([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return profiler
}

func captureCacheContract(t *testing.T, profiler *cacheprofile.Profiler, path cacheContractPath, request map[string]any) cacheprofile.Snapshot {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := path.project(body)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := profiler.Capture(path.protocol, wire)
	if err != nil || !snapshot.Complete {
		t.Fatalf("incomplete protocol capture: err=%v snapshot=%+v", err, snapshot)
	}
	return snapshot
}
