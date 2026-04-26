package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamTranslator_TextOnly(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
		`data: {"type":"response.content_part.added","part":{"type":"output_text"}}`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		`data: {"type":"response.output_text.delta","delta":" world"}`,
		`data: {"type":"response.content_part.done","part":{"type":"output_text"}}`,
		`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4")
	if err := st.Translate(w, strings.NewReader(events)); err != nil {
		t.Fatal(err)
	}

	body := w.Body.String()

	// Check key events are present.
	if !strings.Contains(body, "event: message_start") {
		t.Error("missing message_start")
	}
	if !strings.Contains(body, "event: content_block_start") {
		t.Error("missing content_block_start")
	}
	if !strings.Contains(body, "event: content_block_delta") {
		t.Error("missing content_block_delta")
	}
	if !strings.Contains(body, `"text_delta"`) {
		t.Error("missing text_delta type")
	}
	if !strings.Contains(body, "Hello") {
		t.Error("missing Hello text")
	}
	if !strings.Contains(body, " world") {
		t.Error("missing world text")
	}
	if !strings.Contains(body, "event: content_block_stop") {
		t.Error("missing content_block_stop")
	}
	if !strings.Contains(body, "event: message_delta") {
		t.Error("missing message_delta")
	}
	if !strings.Contains(body, `"end_turn"`) {
		t.Error("missing end_turn stop reason")
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Error("missing message_stop")
	}
}

func TestStreamTranslator_FunctionCall(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"call_1","name":"get_weather"}}`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"ci"}`,
		`data: {"type":"response.function_call_arguments.delta","delta":"ty\":\"London\"}"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":8,"total_tokens":18}}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4")
	if err := st.Translate(w, strings.NewReader(events)); err != nil {
		t.Fatal(err)
	}

	body := w.Body.String()

	if !strings.Contains(body, "event: content_block_start") {
		t.Error("missing content_block_start")
	}
	if !strings.Contains(body, `"tool_use"`) {
		t.Error("missing tool_use type in content_block_start")
	}
	if !strings.Contains(body, `"get_weather"`) {
		t.Error("missing tool name")
	}
	if !strings.Contains(body, `"input_json_delta"`) {
		t.Error("missing input_json_delta type")
	}
	if !strings.Contains(body, `"partial_json"`) {
		t.Error("missing partial_json field")
	}
}

func TestStreamTranslator_AssembleResponseUntranslatableOutputReturnsClearError(t *testing.T) {
	st := NewStreamTranslator("gpt-5.5")

	_, err := st.AssembleResponse("gpt-5.5")
	if err == nil {
		t.Fatal("AssembleResponse succeeded, want clear untranslatable output error")
	}
	if !strings.Contains(err.Error(), "no translatable content") {
		t.Fatalf("error = %q, want no translatable content", err)
	}
}

func TestStreamTranslator_UsageInMessageDelta(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_3"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":42,"output_tokens":17,"total_tokens":59,"input_tokens_details":{"cached_tokens":11},"output_tokens_details":{"reasoning_tokens":7}}}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4")
	st.Translate(w, strings.NewReader(events))

	assertMessageDeltaUsage(t, w.Body.String(), map[string]float64{
		"input_tokens":            42,
		"output_tokens":           17,
		"total_tokens":            59,
		"cache_read_input_tokens": 11,
		"reasoning_output_tokens": 7,
	})
}

func TestStreamTranslator_UsageFromThreadTokenUsageUpdated(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_usage"}}`,
		`data: {"type":"thread.token_usage.updated","usage":{"input_tokens":4200,"output_tokens":1700,"total_tokens":5900,"input_tokens_details":{"cached_tokens":1100},"output_tokens_details":{"reasoning_tokens":700}}}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4")
	if err := st.Translate(w, strings.NewReader(events)); err != nil {
		t.Fatal(err)
	}

	assertMessageDeltaUsage(t, w.Body.String(), map[string]float64{
		"input_tokens":            4200,
		"output_tokens":           1700,
		"total_tokens":            5900,
		"cache_read_input_tokens": 1100,
		"reasoning_output_tokens": 700,
	})
}

func TestStreamTranslator_UsageFromResponseCompletedOverridesEarlierTokenUpdate(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_usage_override"}}`,
		`data: {"type":"thread.token_usage.updated","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":4200,"output_tokens":1700,"total_tokens":5900,"input_tokens_details":{"cached_tokens":1100},"output_tokens_details":{"reasoning_tokens":700}}}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4")
	if err := st.Translate(w, strings.NewReader(events)); err != nil {
		t.Fatal(err)
	}

	assertMessageDeltaUsage(t, w.Body.String(), map[string]float64{
		"input_tokens":            4200,
		"output_tokens":           1700,
		"total_tokens":            5900,
		"cache_read_input_tokens": 1100,
		"reasoning_output_tokens": 700,
	})
}

func TestStreamTranslator_MissingUsageEmitsZeroMessageDelta(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_zero_usage"}}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4")
	if err := st.Translate(w, strings.NewReader(events)); err != nil {
		t.Fatal(err)
	}

	assertMessageDeltaUsage(t, w.Body.String(), map[string]float64{
		"input_tokens":  0,
		"output_tokens": 0,
	})
}

func assertMessageDeltaUsage(t *testing.T, body string, want map[string]float64) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if ev["type"] != "message_delta" {
			continue
		}
		usage, ok := ev["usage"].(map[string]any)
		if !ok {
			t.Fatal("missing usage in message_delta")
		}
		for key, wantValue := range want {
			got, ok := usage[key].(float64)
			if !ok {
				t.Fatalf("usage[%q] missing", key)
			}
			if got != wantValue {
				t.Fatalf("usage[%q] = %v, want %v", key, got, wantValue)
			}
		}
		return
	}
	t.Fatal("no message_delta event found")
}

func TestStreamTranslator_MaxTokens(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_4"}}`,
		`data: {"type":"response.completed","response":{"status":"incomplete","usage":{"input_tokens":5,"output_tokens":100,"total_tokens":105}}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4")
	st.Translate(w, strings.NewReader(events))

	body := w.Body.String()
	if !strings.Contains(body, `"max_tokens"`) {
		t.Error("missing max_tokens stop reason for incomplete status")
	}
}

func TestStreamTranslator_ModelInMessageStart(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_5"}}`,
		`data: [DONE]`,
	}, "\n")

	w := httptest.NewRecorder()
	st := NewStreamTranslator("gpt-5.4-mini")
	st.Translate(w, strings.NewReader(events))

	body := w.Body.String()
	if !strings.Contains(body, `"gpt-5.4-mini"`) {
		t.Error("model not included in message_start")
	}
}
