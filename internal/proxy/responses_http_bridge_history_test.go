package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestResponsesHTTPBridgeBoundsRetainedHistory(t *testing.T) {
	item := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	state := responsesHTTPBridgeState{
		previousID: "previous",
		history: []json.RawMessage{
			item(map[string]any{"type": "message", "role": "developer", "content": "Keep instructions."}),
			item(map[string]any{"type": "message", "role": "user", "content": strings.Repeat("old", 1000)}),
			item(map[string]any{"type": "message", "role": "assistant", "content": "old reply"}),
			item(map[string]any{"type": "message", "role": "user", "content": "recent"}),
			item(map[string]any{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": "{}"}),
			item(map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "result"}),
		},
	}
	frame := item(map[string]any{
		"type": "response.create", "model": "glm-5.3", "previous_response_id": "previous",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "now"}},
	})
	var sent []json.RawMessage
	bridge := responsesHTTPBridge{
		Name: "OpenCode Go", MaxInputBytes: 1000,
		Compaction: func([]byte) (bool, error) { return false, nil },
		Stream: func(_ context.Context, _ *websocket.Conn, _ http.Header, body []byte) (string, []json.RawMessage, error) {
			var request struct {
				Input []json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			sent = request.Input
			return "next", []json.RawMessage{item(map[string]any{"type": "message", "role": "assistant", "content": "answer"})}, nil
		},
	}
	if err := state.serveFrame(context.Background(), nil, nil, websocket.TextMessage, frame, bridge); err != nil {
		t.Fatal(err)
	}
	if len(sent) < 6 || len(state.history) != len(sent)+1 {
		t.Fatalf("sent %d items, retained %d", len(sent), len(state.history))
	}
	var first, marker, recent, call, output, current struct {
		Type, Role, Content string
		CallID              string `json:"call_id"`
	}
	for index, target := range []any{&first, &marker, &recent, &call, &output, &current} {
		if err := json.Unmarshal(sent[index], target); err != nil {
			t.Fatal(err)
		}
	}
	if first.Role != "developer" || first.Content != "Keep instructions." ||
		marker.Role != "developer" || !strings.Contains(marker.Content, "omitted") ||
		recent.Content != "recent" || call.CallID != "call-1" || output.CallID != "call-1" || current.Content != "now" {
		t.Fatalf("bounded history lost required context: %s", sent)
	}
	if strings.Contains(string(sent[0]), "oldold") || strings.Contains(string(sent[2]), "old reply") {
		t.Fatal("old history remained in bounded request")
	}
}

func TestResponsesHTTPBridgePreservesOversizedCurrentInput(t *testing.T) {
	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"` + strings.Repeat("x", 200) + `"}`)}
	bounded, changed := boundResponsesBridgeInput(input, len(input), 100)
	if changed || len(bounded) != 1 || string(bounded[0]) != string(input[0]) {
		t.Fatal("oversized current input changed")
	}
}

func TestResponsesHTTPBridgeBoundsFullReconnectInput(t *testing.T) {
	frame, err := json.Marshal(map[string]any{
		"type": "response.create", "model": "glm-5.3",
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": "Keep instructions."},
			map[string]any{"type": "message", "role": "user", "content": strings.Repeat("old", 1000)},
			map[string]any{"type": "message", "role": "assistant", "content": "old answer"},
			map[string]any{"type": "message", "role": "user", "content": "current"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sent []json.RawMessage
	bridge := responsesHTTPBridge{
		Name: "OpenCode Go", MaxInputBytes: 500,
		Compaction: func([]byte) (bool, error) { return false, nil },
		Stream: func(_ context.Context, _ *websocket.Conn, _ http.Header, body []byte) (string, []json.RawMessage, error) {
			var request struct{ Input []json.RawMessage }
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			sent = request.Input
			return "next", nil, nil
		},
	}
	var state responsesHTTPBridgeState
	if err := state.serveFrame(context.Background(), nil, nil, websocket.TextMessage, frame, bridge); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || !strings.Contains(string(sent[2]), "current") || strings.Contains(string(sent[2]), "old") {
		t.Fatalf("reconnect input not bounded: %s", sent)
	}
}

func TestResponsesHTTPBridgeBoundsRoleOnlyMessages(t *testing.T) {
	input := []json.RawMessage{
		json.RawMessage(`{"role":"developer","content":"Keep instructions."}`),
		json.RawMessage(`{"role":"user","content":"` + strings.Repeat("old", 1000) + `"}`),
		json.RawMessage(`{"role":"assistant","content":"old answer"}`),
		json.RawMessage(`{"role":"user","content":"current"}`),
	}
	bounded, changed := boundResponsesBridgeInput(input, 1, 500)
	if !changed || len(bounded) != 3 ||
		!strings.Contains(string(bounded[0]), "Keep instructions.") ||
		!strings.Contains(string(bounded[1]), "omitted") ||
		!strings.Contains(string(bounded[2]), "current") {
		t.Fatalf("role-only replay not bounded: %s", bounded)
	}
}

func TestResponsesHTTPBridgeCountsEncodedInputBytes(t *testing.T) {
	input := []json.RawMessage{
		json.RawMessage(`{"role":"developer","content":"Keep instructions."}`),
		json.RawMessage(`{"role":"user","content":"` + strings.Repeat("<", 100) + `"}`),
		json.RawMessage(`{"role":"assistant","content":"old answer"}`),
		json.RawMessage(`{"role":"user","content":"current"}`),
	}
	const limit = 350
	bounded, changed := boundResponsesBridgeInput(input, 1, limit)
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(encoded) > limit || len(bounded) != 3 ||
		!strings.Contains(string(encoded), "Keep instructions.") ||
		!strings.Contains(string(encoded), "omitted") ||
		!strings.Contains(string(encoded), "current") {
		t.Fatalf("encoded replay not bounded: changed=%t bytes=%d items=%d", changed, len(encoded), len(bounded))
	}
}

func TestResponsesHTTPBridgeDoesNotSplitParallelToolCalls(t *testing.T) {
	input := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"developer","content":"Keep instructions."}`),
		json.RawMessage(`{"type":"message","role":"user","content":"` + strings.Repeat("old", 1000) + `"}`),
		json.RawMessage(`{"type":"message","role":"user","content":"` + strings.Repeat("latest", 200) + `"}`),
		json.RawMessage(`{"type":"function_call","call_id":"a","name":"lookup","arguments":"{}"}`),
		json.RawMessage(`{"type":"custom_tool_call","call_id":"b","name":"lookup","input":"query"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"a","output":"done"}`),
		json.RawMessage(`{"type":"message","role":"user","content":"steer"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"b","output":"done"}`),
	}
	if bounded, changed := boundResponsesBridgeInput(input, 1, 400); changed || len(bounded) != len(input) {
		t.Fatalf("split an indivisible tool sequence: changed=%t items=%d", changed, len(bounded))
	}
	bounded, changed := boundResponsesBridgeInput(input, 1, 2300)
	if !changed || len(bounded) != 8 || !strings.Contains(string(bounded[2]), "latest") ||
		!strings.Contains(string(bounded[7]), `"call_id":"b"`) {
		t.Fatalf("lost complete tool sequence: changed=%t items=%d", changed, len(bounded))
	}
}

func TestResponsesHTTPBridgeBoundsContinuationBeforeBodyLimit(t *testing.T) {
	state := responsesHTTPBridgeState{
		previousID: "previous",
		history: []json.RawMessage{
			json.RawMessage(`{"role":"developer","content":"Keep instructions."}`),
			json.RawMessage(`{"role":"user","content":"` + strings.Repeat("old", 3_200_000) + `"}`),
			json.RawMessage(`{"role":"assistant","content":"old answer"}`),
		},
	}
	frame, err := json.Marshal(map[string]any{
		"type": "response.create", "model": "glm-5.3", "previous_response_id": "previous",
		"input": []any{map[string]any{"role": "user", "content": strings.Repeat("current", 135_000)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge := responsesHTTPBridge{
		Name: "Z.ai", MaxInputBytes: maxRequestBody - (1 << 20),
		Stream: func(_ context.Context, _ *websocket.Conn, _ http.Header, body []byte) (string, []json.RawMessage, error) {
			if len(body) > maxRequestBody || !strings.Contains(string(body), "Keep instructions.") ||
				!strings.Contains(string(body), "Earlier conversation omitted") ||
				!strings.Contains(string(body), "current") || strings.Contains(string(body), "oldold") {
				t.Errorf("continuation was not bounded before body limit: bytes=%d", len(body))
			}
			return "next", nil, nil
		},
		Compaction: func([]byte) (bool, error) { return false, nil },
	}
	if err := state.serveFrame(context.Background(), nil, nil, websocket.TextMessage, frame, bridge); err != nil {
		t.Fatal(err)
	}
}
