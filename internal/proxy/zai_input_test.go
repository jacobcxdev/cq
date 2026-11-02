package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormaliseZAIRequestPreservesPlaintextAgentTasksAndFollowups(t *testing.T) {
	const body = `{
		"model":"glm-test", "stream":true, "store":false,
		"instructions":"Keep these instructions.",
		"tools":[{"type":"function","name":"test_tool","parameters":{"type":"object"}}],
		"future_option":{"large_integer":9007199254740993,"nested":[true,null,"<&>"]},
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Keep context."}],"future_message_option":true},
			{"type":"agent_message","id":"amsg-test","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/worker\nSender: /root\nPayload:\n"},{"type":"input_text","text":"Reply TASK_ONE.\nKeep <tags> & symbols.","future_text_option":{"enabled":true}}]},
			{"type":"function_call_output","call_id":"call-test","output":"Keep tool output.","future_tool_option":9007199254740993},
			{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/worker\nSender: /root\nPayload:\n"},{"type":"input_text","text":"Now reply TASK_TWO."},{"type":"input_text","text":""}]},
			{"type":"agent_message","author":"/root/worker","recipient":"/root","content":[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root\nSender: /root/worker\nPayload:\n"},{"type":"input_text","text":"TASK_TWO completed."}]}
		]
	}`
	original := []byte(body)
	got, err := normaliseZAIRequest(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != body {
		t.Fatal("normalisation mutated the caller's request")
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("request fields changed: got %d, want %d", len(after), len(before))
	}
	for key, value := range before {
		if key != "input" && !reflect.DeepEqual(zaiInputJSONValue(t, value), zaiInputJSONValue(t, after[key])) {
			t.Errorf("top-level field %q changed", key)
		}
	}
	var beforeItems, afterItems []json.RawMessage
	if err := json.Unmarshal(before["input"], &beforeItems); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after["input"], &afterItems); err != nil {
		t.Fatal(err)
	}
	if len(afterItems) != len(beforeItems) {
		t.Fatalf("input length = %d, want %d", len(afterItems), len(beforeItems))
	}
	for i, raw := range afterItems {
		if i == 0 || i == 2 {
			if !reflect.DeepEqual(zaiInputJSONValue(t, raw), zaiInputJSONValue(t, beforeItems[i])) {
				t.Errorf("ordinary input item %d changed", i)
			}
			continue
		}
		var oldAgent, message map[string]json.RawMessage
		if err := json.Unmarshal(beforeItems[i], &oldAgent); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		if string(message["type"]) != `"message"` || string(message["role"]) != `"user"` || len(message) != 3 {
			t.Errorf("agent input %d did not become a standard user message: %s", i, raw)
		}
		if !reflect.DeepEqual(zaiInputJSONValue(t, message["content"]), zaiInputJSONValue(t, oldAgent["content"])) {
			t.Errorf("agent input %d lost header, task, follow-up or content metadata", i)
		}
	}
}

func TestNormaliseZAIRequestLeavesOrdinaryRequestsByteExact(t *testing.T) {
	for _, body := range []string{
		" \n{ \"model\": \"glm-test\", \"input\": \"Text mentioning agent_message\" }\n",
		`{"model":"glm-test","input":[ {"type":"message","role":"user","content":[{"type":"input_text","text":"Keep whitespace & text."}]} ],"future_option":9007199254740993}`,
		`{"model":"glm-test","input":[{"type":"function_call_output","call_id":"call-test","output":"unchanged"}]}`,
		`{"model":"glm-test","input":[]}`,
		`{"model":"glm-test","input":null}`,
		`{"model":"glm-test"}`,
	} {
		got, err := normaliseZAIRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("ordinary request bytes changed: got %q, want %q", got, body)
		}
	}
}

func TestNormaliseZAIRequestRejectsUnreadableAgentContent(t *testing.T) {
	for _, tc := range []struct {
		name, agent, errorKind string
	}{
		{"encrypted part", `{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"TASK_CONTENT_MUST_NOT_APPEAR"}]}`, "encrypted"},
		{"mixed plaintext and encryption", `{"type":"agent_message","content":[{"type":"input_text","text":"TASK_CONTENT_MUST_NOT_APPEAR"},{"type":"encrypted_content","encrypted_content":"opaque-task"}]}`, "encrypted"},
		{"encrypted envelope", `{"type":"agent_message","encrypted_content":"TASK_CONTENT_MUST_NOT_APPEAR","content":[{"type":"input_text","text":"header"}]}`, "encrypted"},
		{"encryption on text part", `{"type":"agent_message","content":[{"type":"input_text","text":"header","encrypted_content":"TASK_CONTENT_MUST_NOT_APPEAR"}]}`, "encrypted"},
		{"unsupported part", `{"type":"agent_message","content":[{"type":"input_image","image_url":"TASK_CONTENT_MUST_NOT_APPEAR"}]}`, "unsupported"},
		{"missing content", `{"type":"agent_message"}`, "invalid"},
		{"null content", `{"type":"agent_message","content":null}`, "invalid"},
		{"empty content", `{"type":"agent_message","content":[]}`, "invalid"},
		{"string content", `{"type":"agent_message","content":"TASK_CONTENT_MUST_NOT_APPEAR"}`, "invalid"},
		{"object content", `{"type":"agent_message","content":{"type":"input_text","text":"TASK_CONTENT_MUST_NOT_APPEAR"}}`, "invalid"},
		{"string part", `{"type":"agent_message","content":["TASK_CONTENT_MUST_NOT_APPEAR"]}`, "invalid"},
		{"null part", `{"type":"agent_message","content":[null]}`, "unsupported"},
		{"missing part type", `{"type":"agent_message","content":[{"text":"TASK_CONTENT_MUST_NOT_APPEAR"}]}`, "unsupported"},
		{"missing text", `{"type":"agent_message","content":[{"type":"input_text"}]}`, "invalid"},
		{"null text", `{"type":"agent_message","content":[{"type":"input_text","text":null}]}`, "invalid"},
		{"numeric text", `{"type":"agent_message","content":[{"type":"input_text","text":42}]}`, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"glm-test","input":[` + tc.agent + `],"instructions":"TASK_CONTENT_MUST_NOT_APPEAR"}`
			got, err := normaliseZAIRequest([]byte(body))
			if err == nil || !strings.Contains(err.Error(), tc.errorKind) {
				t.Fatalf("expected %s error, got body %q, error %v", tc.errorKind, got, err)
			}
			if got != nil || strings.Contains(err.Error(), "TASK_CONTENT_MUST_NOT_APPEAR") || strings.Contains(err.Error(), "opaque-task") {
				t.Fatalf("rejected content leaked into output or diagnostic: %v", err)
			}
		})
	}
}

func TestNormaliseZAIRequestRejectsMalformedRequests(t *testing.T) {
	for _, body := range []string{
		`{"input":"TASK_CONTENT_MUST_NOT_APPEAR"`,
		`null`,
		`[]`,
		`{"input":42,"instructions":"TASK_CONTENT_MUST_NOT_APPEAR"}`,
		`{"input":{},"instructions":"TASK_CONTENT_MUST_NOT_APPEAR"}`,
	} {
		got, err := normaliseZAIRequest([]byte(body))
		if err == nil || got != nil || strings.Contains(err.Error(), "TASK_CONTENT_MUST_NOT_APPEAR") {
			t.Fatalf("malformed request not safely rejected: %v", err)
		}
	}
}

func zaiInputJSONValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
