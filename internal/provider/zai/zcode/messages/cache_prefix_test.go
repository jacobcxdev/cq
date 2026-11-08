package messages

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestProjectKeptRuntimeInstructionsAfterCachedHistory(t *testing.T) {
	base := []byte(`{"model":"glm-5.3","instructions":"Stable harness.","input":[
	 {"role":"system","content":"Stable system policy."},
	 {"role":"developer","content":"Stable developer policy."},
	 {"role":"user","content":"Inspect fixture."},
	 {"type":"function_call","name":"inspect","call_id":"call-1","arguments":"{}"},
	 {"type":"function_call_output","call_id":"call-1","output":"Fixture result."}
	],"tools":[{"type":"function","name":"inspect","description":"Inspect fixture."}]}`)
	before := projectCacheFixture(t, base)
	var request map[string]json.RawMessage
	if err := json.Unmarshal(base, &request); err != nil {
		t.Fatal(err)
	}
	var input []json.RawMessage
	if err := json.Unmarshal(request["input"], &input); err != nil {
		t.Fatal(err)
	}
	input = append(input,
		json.RawMessage(`{"role":"developer","content":[{"type":"input_text","text":"Current time: 2026-11-08 11:30 UTC."}]}`),
		json.RawMessage(`{"role":"assistant","content":"Continue inspecting fixture."}`),
	)
	request["input"], _ = json.Marshal(input)
	raw, _ := json.Marshal(request)
	after := projectCacheFixture(t, raw)
	if !bytes.Equal(before["system"], after["system"]) {
		t.Fatalf("runtime instruction changed cached system prefix: before=%s after=%s", before["system"], after["system"])
	}
	if !bytes.Equal(before["tools"], after["tools"]) {
		t.Fatal("runtime instruction changed cached tool definitions")
	}
	var system []struct{ Type, Text string }
	if err := json.Unmarshal(after["system"], &system); err != nil {
		t.Fatal(err)
	}
	wantSystem := []struct{ Type, Text string }{
		{"text", "Stable harness."},
		{"text", "Stable system policy."},
		{"text", "Stable developer policy."},
	}
	if !reflect.DeepEqual(system, wantSystem) {
		t.Fatalf("leading instructions changed: got=%v want=%v", system, wantSystem)
	}
	var earlier, later []json.RawMessage
	if err := json.Unmarshal(before["messages"], &earlier); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after["messages"], &later); err != nil {
		t.Fatal(err)
	}
	if len(earlier) != 3 || len(later) != 5 {
		t.Fatalf("conversation boundaries changed: before=%s after=%s", before["messages"], after["messages"])
	}
	for i := range earlier {
		if !bytes.Equal(earlier[i], later[i]) {
			t.Fatalf("cached history item %d changed: before=%s after=%s", i, earlier[i], later[i])
		}
	}
	var reminder struct {
		Role    string
		Content string
	}
	if err := json.Unmarshal(later[3], &reminder); err != nil {
		t.Fatal(err)
	}
	if reminder.Role != "system" || reminder.Content != "Current time: 2026-11-08 11:30 UTC." {
		t.Fatalf("runtime instruction lost authority, position or text: %s", later[3])
	}
	var continuation struct{ Role string }
	if err := json.Unmarshal(later[4], &continuation); err != nil || continuation.Role != "assistant" {
		t.Fatalf("assistant continuation moved: %s", later[4])
	}
}

func TestProjectPreservedRuntimeSystemTextBlocks(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		t.Run(role, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"model": "glm-5.3", "input": []any{
				map[string]any{"role": "user", "content": "Inspect fixture."},
				map[string]any{"role": role, "content": []any{
					map[string]string{"type": "input_text", "text": "First block."},
					map[string]string{"type": "text", "text": "\nSecond block."},
				}},
			}})
			projected := projectCacheFixture(t, raw)
			if _, exists := projected["system"]; exists {
				t.Fatalf("runtime instruction moved ahead of user: %s", projected["system"])
			}
			var history []struct {
				Role    string
				Content []struct{ Type, Text string }
			}
			if err := json.Unmarshal(projected["messages"], &history); err != nil {
				t.Fatal(err)
			}
			want := []struct{ Type, Text string }{{"text", "First block."}, {"text", "\nSecond block."}}
			if len(history) != 2 || history[1].Role != "system" || !reflect.DeepEqual(history[1].Content, want) {
				t.Fatalf("runtime text blocks changed: %s", projected["messages"])
			}
		})
	}
}

func TestProjectRejectedNonTextSystemContent(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		for _, runtime := range []bool{false, true} {
			name := role + "/leading"
			if runtime {
				name = role + "/runtime"
			}
			t.Run(name, func(t *testing.T) {
				input := []any{}
				if runtime {
					input = append(input, map[string]string{"role": "user", "content": "Inspect fixture."})
				}
				input = append(input, map[string]any{"role": role, "content": []any{
					map[string]string{"type": "input_image", "image_url": "https://fixture.test/image.png"},
				}})
				if !runtime {
					input = append(input, map[string]string{"role": "user", "content": "Inspect fixture."})
				}
				body, _ := json.Marshal(map[string]any{"model": "glm-5.3", "input": input})
				if _, _, err := Project(body); err == nil {
					t.Fatal("accepted image as system instruction")
				}
			})
		}
	}
}

func projectCacheFixture(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	raw, _, err := Project(body)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	return projected
}
