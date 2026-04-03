package proxy

import (
	"encoding/json"
	"testing"
)

func TestTranslateRequest_SimpleText(t *testing.T) {
	input := `{
		"model": "gpt-5.4",
		"max_tokens": 100,
		"messages": [{"role": "user", "content": "hello"}]
	}`

	out, err := translateRequest([]byte(input), "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiResponsesRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}

	if req.Model != "gpt-5.4" {
		t.Errorf("model = %q, want gpt-5.4", req.Model)
	}
	if req.MaxOutputTokens != 0 {
		t.Errorf("max_output_tokens = %d, want 0 (omitted for ChatGPT backend)", req.MaxOutputTokens)
	}

	var items []openaiInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Type != "message" || items[0].Role != "user" {
		t.Errorf("item = %+v, want message/user", items[0])
	}
}

func TestTranslateRequest_SystemString(t *testing.T) {
	input := `{
		"model": "gpt-5.4",
		"max_tokens": 10,
		"system": "You are helpful.",
		"messages": [{"role": "user", "content": "hi"}]
	}`

	out, err := translateRequest([]byte(input), "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiResponsesRequest
	json.Unmarshal(out, &req)

	if req.Instructions != "You are helpful." {
		t.Errorf("instructions = %q, want 'You are helpful.'", req.Instructions)
	}
}

func TestTranslateRequest_SystemBlocks(t *testing.T) {
	input := `{
		"model": "gpt-5.4",
		"max_tokens": 10,
		"system": [{"type":"text","text":"Part 1"},{"type":"text","text":"Part 2"}],
		"messages": [{"role": "user", "content": "hi"}]
	}`

	out, err := translateRequest([]byte(input), "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiResponsesRequest
	json.Unmarshal(out, &req)

	if req.Instructions != "Part 1\n\nPart 2" {
		t.Errorf("instructions = %q, want 'Part 1\\n\\nPart 2'", req.Instructions)
	}
}

func TestTranslateRequest_ToolDefinitions(t *testing.T) {
	input := `{
		"model": "gpt-5.4",
		"max_tokens": 10,
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{
			"name": "get_weather",
			"description": "Get weather",
			"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}
		}]
	}`

	out, err := translateRequest([]byte(input), "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiResponsesRequest
	json.Unmarshal(out, &req)

	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.Type != "function" {
		t.Errorf("tool type = %q, want function", tool.Type)
	}
	if tool.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", tool.Name)
	}
}

func TestTranslateRequest_ToolUseAndResult(t *testing.T) {
	input := `{
		"model": "gpt-5.4",
		"max_tokens": 10,
		"messages": [
			{"role": "user", "content": "What's the weather?"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "London"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": "Sunny, 22C"}
			]}
		]
	}`

	out, err := translateRequest([]byte(input), "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiResponsesRequest
	json.Unmarshal(out, &req)

	var items []openaiInputItem
	json.Unmarshal(req.Input, &items)

	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}

	// First: user message
	if items[0].Type != "message" {
		t.Errorf("items[0].type = %q, want message", items[0].Type)
	}

	// Second: function_call
	if items[1].Type != "function_call" {
		t.Errorf("items[1].type = %q, want function_call", items[1].Type)
	}
	if items[1].Name != "get_weather" {
		t.Errorf("items[1].name = %q, want get_weather", items[1].Name)
	}
	if items[1].CallID != "call_1" {
		t.Errorf("items[1].call_id = %q, want call_1", items[1].CallID)
	}

	// Third: function_call_output
	if items[2].Type != "function_call_output" {
		t.Errorf("items[2].type = %q, want function_call_output", items[2].Type)
	}
	if items[2].CallID != "call_1" {
		t.Errorf("items[2].call_id = %q, want call_1", items[2].CallID)
	}
	if items[2].Output != "Sunny, 22C" {
		t.Errorf("items[2].output = %q, want 'Sunny, 22C'", items[2].Output)
	}
}

func TestTranslateRequest_ToolChoice(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"auto", `{"type":"auto"}`, `"auto"`},
		{"any", `{"type":"any"}`, `"required"`},
		{"specific", `{"type":"tool","name":"foo"}`, `{"name":"foo","type":"function"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateToolChoice(json.RawMessage(tt.input))
			if string(got) != tt.want {
				t.Errorf("translateToolChoice(%s) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestTranslateRequest_MultiTurn(t *testing.T) {
	input := `{
		"model": "gpt-5.4",
		"max_tokens": 10,
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi there!"},
			{"role": "user", "content": "How are you?"}
		]
	}`

	out, err := translateRequest([]byte(input), "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiResponsesRequest
	json.Unmarshal(out, &req)

	var items []openaiInputItem
	json.Unmarshal(req.Input, &items)

	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if items[0].Role != "user" {
		t.Errorf("items[0].role = %q, want user", items[0].Role)
	}
	if items[1].Role != "assistant" {
		t.Errorf("items[1].role = %q, want assistant", items[1].Role)
	}
	if items[2].Role != "user" {
		t.Errorf("items[2].role = %q, want user", items[2].Role)
	}
}

func TestTranslateResponse_TextOnly(t *testing.T) {
	oResp := `{
		"id": "resp_123",
		"status": "completed",
		"model": "gpt-5.4",
		"output": [{
			"type": "message",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "Hello!"}]
		}],
		"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
	}`

	out, err := translateResponse([]byte(oResp), "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}

	var resp anthropicResponse
	json.Unmarshal(out, &resp)

	if resp.ID != "msg_resp_123" {
		t.Errorf("id = %q, want msg_resp_123", resp.ID)
	}
	if resp.Role != "assistant" {
		t.Errorf("role = %q, want assistant", resp.Role)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content = %d, want 1", len(resp.Content))
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text != "Hello!" {
		t.Errorf("content[0] = %+v, want text/Hello!", resp.Content[0])
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 15 {
		t.Fatalf("total_tokens = %v, want 15", resp.Usage.TotalTokens)
	}
}

func TestTranslateResponse_RichUsageFields(t *testing.T) {
	oResp := `{
		"id": "resp_rich",
		"status": "completed",
		"model": "gpt-5.4",
		"output": [{
			"type": "message",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "Hello!"}]
		}],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"total_tokens": 15,
			"input_tokens_details": {"cached_tokens": 4},
			"output_tokens_details": {"reasoning_tokens": 2}
		}
	}`

	out, err := translateResponse([]byte(oResp), "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}

	var resp anthropicResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Usage.CacheReadInputTokens == nil || *resp.Usage.CacheReadInputTokens != 4 {
		t.Fatalf("cache_read_input_tokens = %v, want 4", resp.Usage.CacheReadInputTokens)
	}
	if resp.Usage.ReasoningOutputTokens == nil || *resp.Usage.ReasoningOutputTokens != 2 {
		t.Fatalf("reasoning_output_tokens = %v, want 2", resp.Usage.ReasoningOutputTokens)
	}
	if resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 15 {
		t.Fatalf("total_tokens = %v, want 15", resp.Usage.TotalTokens)
	}
}

func TestTranslateResponse_FunctionCall(t *testing.T) {
	oResp := `{
		"id": "resp_456",
		"status": "completed",
		"model": "gpt-5.4",
		"output": [{
			"type": "function_call",
			"name": "get_weather",
			"arguments": "{\"city\":\"London\"}",
			"call_id": "call_abc"
		}]
	}`

	out, err := translateResponse([]byte(oResp), "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}

	var resp anthropicResponse
	json.Unmarshal(out, &resp)

	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content = %d, want 1", len(resp.Content))
	}
	block := resp.Content[0]
	if block.Type != "tool_use" {
		t.Errorf("type = %q, want tool_use", block.Type)
	}
	if block.Name != "get_weather" {
		t.Errorf("name = %q, want get_weather", block.Name)
	}
	if block.ID != "call_abc" {
		t.Errorf("id = %q, want call_abc", block.ID)
	}

	var input map[string]string
	json.Unmarshal(block.Input, &input)
	if input["city"] != "London" {
		t.Errorf("input.city = %q, want London", input["city"])
	}
}

func TestTranslateResponse_Incomplete(t *testing.T) {
	oResp := `{
		"id": "resp_789",
		"status": "incomplete",
		"model": "gpt-5.4",
		"output": [{
			"type": "message",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "partial"}]
		}]
	}`

	out, err := translateResponse([]byte(oResp), "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}

	var resp anthropicResponse
	json.Unmarshal(out, &resp)

	if resp.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %q, want max_tokens", resp.StopReason)
	}
}
