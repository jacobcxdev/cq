package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func event(kind, data string) string { return "event: " + kind + "\ndata: " + data + "\n\n" }

const start = `{"type":"message_start","message":{"id":"message-1","type":"message","role":"assistant","model":"glm","content":[],"usage":{"input_tokens":8,"cache_read_input_tokens":4,"output_tokens":0}}}`
const delta = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`
const stop = `{"type":"message_stop"}`

func codecForTest(t *testing.T) *Codec {
	t.Helper()
	_, c, err := Project([]byte(`{"model":"glm","input":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStreamFragmentedTextAndThinking(t *testing.T) {
	input := event("message_start", start) + event("ping", `{"type":"ping"}`) +
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Plan λ"}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"real-signature"}}`) +
		event("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Hello"}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" world"}}`) +
		event("content_block_stop", `{"type":"content_block_stop","index":1}`) + event("message_delta", delta) + event("message_stop", stop)
	var output bytes.Buffer
	if err := codecForTest(t).ConvertSSE(&output, iotest.OneByteReader(strings.NewReader(input))); err != nil {
		t.Fatal(err)
	}
	events := readNativeEvents(t, output.String())
	if events[0]["type"] != "response.created" || events[1]["type"] != "response.in_progress" || events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("lifecycle: %s", output.String())
	}
	terminal := events[len(events)-1]["response"].(map[string]any)
	items := terminal["output"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["signature"] != "real-signature" || items[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "Hello world" {
		t.Fatalf("lost blocks: %s", output.String())
	}
	usage := terminal["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(5) || usage["total_tokens"] != float64(17) {
		t.Fatalf("usage: %v", usage)
	}
	var text string
	for _, e := range events {
		if e["type"] == "response.output_text.delta" {
			text += e["delta"].(string)
		}
	}
	if text != "Hello world" {
		t.Fatalf("delta text %q", text)
	}
}

func TestStreamToolArgumentsAndCustomInput(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "function", true: "custom"}[custom], func(t *testing.T) {
			kind := "function"
			args := `{"cmd":"pwd"}`
			if custom {
				kind = "custom"
				args = `{"input":"*** Begin Patch\nλ"}`
			}
			raw, codec, err := Project([]byte(`{"model":"glm","input":"test","tools":[{"type":"` + kind + `","namespace":"functions","name":"exec"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatal(err)
			}
			block, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call-1", "name": request.Tools[0].Name, "input": map[string]any{}}})
			input := event("message_start", start) + event("content_block_start", string(block))
			for _, part := range []string{args[:5], args[5:]} {
				raw, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
				input += event("content_block_delta", string(raw))
			}
			input += event("content_block_stop", `{"type":"content_block_stop","index":0}`) + event("message_delta", strings.Replace(delta, "end_turn", "tool_use", 1)) + event("message_stop", stop)
			var output bytes.Buffer
			if err := codec.ConvertSSE(&output, strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			events := readNativeEvents(t, output.String())
			terminal := events[len(events)-1]["response"].(map[string]any)
			item := terminal["output"].([]any)[0].(map[string]any)
			if item["name"] != "exec" || item["namespace"] != "functions" || item["call_id"] != "call-1" {
				t.Fatalf("identity: %v", item)
			}
			var joined string
			for _, e := range events {
				if e["type"] == "response.function_call_arguments.delta" || e["type"] == "response.custom_tool_call_input.delta" {
					joined += e["delta"].(string)
				}
			}
			if custom {
				if item["type"] != "custom_tool_call" || item["input"] != "*** Begin Patch\nλ" || joined != item["input"] {
					t.Fatalf("custom arguments: %v", item)
				}
			} else if item["arguments"] != args || joined != args {
				t.Fatalf("function arguments: %v", item)
			}
		})
	}
}

func TestStreamNeverCompletedAfterFailureOrTruncation(t *testing.T) {
	textStart := event("message_start", start) + event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for name, input := range map[string]string{
		"empty":            "",
		"missing stop":     event("message_start", start) + event("message_delta", delta),
		"unfinished event": event("message_start", start) + `data: {"type":"message_stop"}`,
		"premature stop":   event("message_start", start) + event("message_stop", stop),
		"unclosed block":   textStart + event("message_delta", delta) + event("message_stop", stop),
		"unknown delta":    textStart + event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"future_delta"}}`),
		"duplicate start":  event("message_start", start) + event("message_start", start),
		"wrong index":      textStart + event("content_block_stop", `{"type":"content_block_stop","index":2}`),
		"null index":       textStart + event("content_block_stop", `{"type":"content_block_stop","index":null}`),
		"null text":        textStart + event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":null}}`),
		"unknown block":    event("message_start", start) + event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque"}}`),
		"upstream error":   event("error", `{"type":"error","error":{"type":"rate_limit_error","message":"secret upstream value"}}`),
		"event mismatch":   event("message_delta", start),
		"unknown stop":     event("message_start", start) + event("message_delta", strings.Replace(delta, "end_turn", "unknown", 1)) + event("message_stop", stop),
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := codecForTest(t).ConvertSSE(&output, strings.NewReader(input)); err == nil {
				t.Fatal("expected failure")
			}
			if strings.Contains(output.String(), "response.completed") || strings.Contains(output.String(), "secret upstream value") {
				t.Fatalf("false completion or leaked upstream: %s", output.String())
			}
			if name == "upstream error" && !strings.Contains(output.String(), `"status_code":429`) {
				t.Fatalf("rejection classification missing: %s", output.String())
			}
		})
	}
}

func TestStreamIncompleteTerminalAndIOErrors(t *testing.T) {
	input := event("message_start", start) + event("message_delta", strings.Replace(delta, "end_turn", "max_tokens", 1)) + event("message_stop", stop)
	var output bytes.Buffer
	if err := codecForTest(t).ConvertSSE(&output, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "response.incomplete") || strings.Contains(output.String(), "response.completed") {
		t.Fatalf("truncation completed: %s", output.String())
	}
	sentinel := errors.New("source cancelled")
	output.Reset()
	if err := codecForTest(t).ConvertSSE(&output, io.MultiReader(strings.NewReader(event("message_start", start)), iotest.ErrReader(sentinel))); !errors.Is(err, sentinel) {
		t.Fatalf("read error %v", err)
	}
	if err := codecForTest(t).ConvertSSE(shortWriter{}, strings.NewReader(event("message_start", start))); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("write error %v", err)
	}
}

func TestStreamInterleavedContentBlocks(t *testing.T) {
	input := event("message_start", start) +
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`) +
		event("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		event("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		event("message_delta", delta) + event("message_stop", stop)
	var output bytes.Buffer
	if err := codecForTest(t).ConvertSSE(&output, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	events := readNativeEvents(t, output.String())
	items := events[len(events)-1]["response"].(map[string]any)["output"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["type"] != "reasoning" || items[1].(map[string]any)["type"] != "message" {
		t.Fatalf("lost output order: %v", items)
	}
}

func TestStreamEventSizeAndMalformedToolArguments(t *testing.T) {
	var output bytes.Buffer
	if err := codecForTest(t).ConvertSSE(&output, strings.NewReader("data: "+strings.Repeat("x", maxEventBytes+1)+"\n\n")); err == nil {
		t.Fatal("accepted oversized event")
	}
	raw, codec, err := Project([]byte(`{"model":"glm","input":"test","tools":[{"type":"custom","name":"edit"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(raw, &request)
	input := event("message_start", start) + event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"`+request.Tools[0].Name+`","input":{}}}`) + event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"wrong\":42}"}}`) + event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	output.Reset()
	if err := codec.ConvertSSE(&output, strings.NewReader(input)); err == nil {
		t.Fatal("accepted malformed custom input")
	}
	if strings.Contains(output.String(), "response.completed") {
		t.Fatal("completed malformed tool")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func readNativeEvents(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
