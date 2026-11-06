package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// prepareCQCompactionRequest replaces a terminal trigger, or appends the
// continuation prompt for the standalone compact endpoint.
func prepareCQCompactionRequest(body []byte, replaceTrigger bool) ([]byte, error) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return nil, errors.New("invalid CQ compaction request")
	}
	input, err := responsesBridgeInput(request["input"], "CQ")
	if err != nil || len(input) == 0 {
		return nil, errors.New("invalid CQ compaction input")
	}
	instruction, _ := json.Marshal(map[string]any{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": zaiCompactionPrompt}}})
	if replaceTrigger {
		input[len(input)-1] = instruction
	} else {
		input = append(input, instruction)
	}
	request["input"], _ = json.Marshal(input)
	request["stream"] = json.RawMessage("true")
	request["max_output_tokens"] = json.RawMessage("8192")
	request["reasoning"] = json.RawMessage(`{"effort":"low"}`)
	request["tool_choice"] = json.RawMessage(`"none"`)
	delete(request, "tools")
	delete(request, "parallel_tool_calls")
	delete(request, "previous_response_id")
	delete(request, "context_management")
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("encode CQ compaction request")
	}
	return encoded, nil
}

// sealCQCompactionSummary shares the existing CQ state format across providers.
func sealCQCompactionSummary(localToken string, model json.RawMessage, completed zaiCompactResponse) (zaiCompactResponse, error) {
	var empty zaiCompactResponse
	var summary strings.Builder
	for _, raw := range completed.Output {
		var item struct {
			Type, Role string
			Content    []struct{ Type, Text string }
		}
		if json.Unmarshal(raw, &item) != nil {
			return empty, errors.New("invalid CQ compaction summary")
		}
		if item.Type == "reasoning" {
			continue
		}
		if item.Type != "message" || item.Role != "assistant" {
			return empty, errors.New("CQ compaction returned non-summary output")
		}
		for _, part := range item.Content {
			if part.Type != "output_text" {
				return empty, errors.New("CQ compaction returned non-text summary")
			}
			summary.WriteString(part.Text)
		}
	}
	item, err := sealZAICompaction(localToken, summary.String())
	if err != nil {
		return empty, err
	}
	var identity struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(item, &identity)
	return zaiCompactResponse{ID: identity.ID + "_response", Object: "response", CreatedAt: time.Now().Unix(), Status: "completed", Model: model, Output: []json.RawMessage{item}, Usage: completed.Usage}, nil
}
