package messages

import (
	"encoding/json"
	"errors"
	"fmt"
)

type usage struct {
	Input   int64 `json:"input_tokens"`
	Output  int64 `json:"output_tokens"`
	Cached  int64 `json:"cache_read_input_tokens"`
	Created int64 `json:"cache_creation_input_tokens"`
}

func (u *usage) UnmarshalJSON(raw []byte) error {
	obj, err := readObject(raw)
	if err != nil {
		return protocolError("usage")
	}
	for key, dst := range map[string]*int64{"input_tokens": &u.Input, "output_tokens": &u.Output, "cache_read_input_tokens": &u.Cached, "cache_creation_input_tokens": &u.Created} {
		value, exists := obj[key]
		if !exists && (key == "input_tokens" || key == "output_tokens") {
			return protocolError("usage")
		}
		if exists && (absent(value) || json.Unmarshal(value, dst) != nil || *dst < 0) {
			return protocolError("usage")
		}
	}
	return nil
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

func (b *contentBlock) UnmarshalJSON(raw []byte) error {
	obj, err := readObject(raw)
	if err != nil {
		return protocolError("content block")
	}
	kind, ok := stringField(obj, "type")
	if !ok {
		return protocolError("content block type")
	}
	b.Type = kind
	switch kind {
	case "text":
		b.Text, ok = stringField(obj, "text")
		if !ok {
			return protocolError("text block")
		}
	case "thinking":
		b.Thinking, ok = stringField(obj, "thinking")
		if !ok {
			return protocolError("thinking block")
		}
		if !absent(obj["signature"]) {
			b.Signature, ok = stringField(obj, "signature")
			if !ok {
				return protocolError("thinking signature")
			}
		}
	case "tool_use":
		b.ID, ok = stringField(obj, "id")
		if !ok || b.ID == "" {
			return protocolError("tool identity")
		}
		b.Name, ok = stringField(obj, "name")
		if !ok || b.Name == "" {
			return protocolError("tool identity")
		}
		var input object
		if json.Unmarshal(obj["input"], &input) != nil || input == nil {
			return protocolError("tool arguments")
		}
		b.Input = obj["input"]
	default:
		return protocolError("content block type")
	}
	return nil
}

type reply struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *usage         `json:"usage"`
}

// ConvertJSON converts a complete Messages reply. A token-limited reply remains
// incomplete; an unknown termination reason cannot be represented as success.
func (c *Codec) ConvertJSON(raw []byte) ([]byte, error) {
	var response reply
	if json.Unmarshal(raw, &response) != nil || response.ID == "" || response.Type != "message" || response.Role != "assistant" || response.Model == "" || response.Content == nil || response.Usage == nil {
		return nil, protocolError("response")
	}
	output := make([]any, 0, len(response.Content))
	for i, block := range response.Content {
		item, err := c.outputItem(response.ID, i, block)
		if err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	result, err := nativeResponse(response.ID, response.Model, output, response.StopReason, *response.Usage)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (c *Codec) outputItem(responseID string, index int, block contentBlock) (map[string]any, error) {
	id := fmt.Sprintf("%s_%d", responseID, index)
	switch block.Type {
	case "text":
		return map[string]any{"type": "message", "id": id, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": block.Text, "annotations": []any{}}}}, nil
	case "thinking":
		item := map[string]any{"type": "reasoning", "id": id, "summary": []any{map[string]any{"type": "summary_text", "text": block.Thinking}}}
		if block.Signature != "" {
			item["signature"] = block.Signature
		}
		return item, nil
	case "tool_use":
		tool, ok := c.tools[block.Name]
		if !ok || block.ID == "" {
			return nil, protocolError("tool identity")
		}
		var input object
		if json.Unmarshal(block.Input, &input) != nil || input == nil {
			return nil, protocolError("tool arguments")
		}
		item := map[string]any{"id": id, "call_id": block.ID, "name": tool.Name, "status": "completed"}
		if tool.Namespace != "" {
			item["namespace"] = tool.Namespace
		}
		if tool.Custom {
			value, ok := stringField(input, "input")
			if !ok || len(input) != 1 {
				return nil, protocolError("custom tool input")
			}
			item["type"] = "custom_tool_call"
			item["input"] = value
		} else {
			item["type"] = "function_call"
			item["arguments"] = string(block.Input)
		}
		return item, nil
	default:
		return nil, protocolError("content block")
	}
}

func nativeResponse(id, model string, output []any, reason string, used usage) (map[string]any, error) {
	status := "completed"
	var details any
	switch reason {
	case "end_turn", "stop_sequence", "tool_use":
	case "max_tokens":
		status = "incomplete"
		details = map[string]string{"reason": "max_output_tokens"}
	case "refusal":
		status = "incomplete"
		details = map[string]string{"reason": "content_filter"}
	default:
		return nil, protocolError("stop reason")
	}
	if used.Input < 0 || used.Output < 0 || used.Cached < 0 || used.Created < 0 {
		return nil, protocolError("usage")
	}
	input := used.Input + used.Cached + used.Created
	if input < used.Input || input < used.Cached || input < used.Created || input+used.Output < input {
		return nil, protocolError("usage")
	}
	return map[string]any{"id": id, "object": "response", "status": status, "model": model, "output": output, "error": nil, "incomplete_details": details,
		"usage": map[string]any{"input_tokens": input, "output_tokens": used.Output, "total_tokens": input + used.Output, "input_tokens_details": map[string]int64{"cached_tokens": used.Cached}}}, nil
}

func readObject(raw []byte) (object, error) {
	var obj object
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, errors.New("invalid ZCode Messages JSON")
	}
	return obj, nil
}
