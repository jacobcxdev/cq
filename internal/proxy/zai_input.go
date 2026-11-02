package proxy

import (
	"encoding/json"
	"errors"
)

// normaliseZAIRequest translates plaintext Codex agent messages into the
// ordinary user messages understood by Z.ai's Responses endpoint.
func normaliseZAIRequest(body []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, errors.New("invalid Z.ai request body")
	}
	input, err := zaiResponseInput(request["input"])
	if err != nil {
		return nil, err
	}
	changed := false
	for i, raw := range input {
		var agent struct {
			Type             string          `json:"type"`
			Content          json.RawMessage `json:"content"`
			EncryptedContent json.RawMessage `json:"encrypted_content"`
		}
		if err := json.Unmarshal(raw, &agent); err != nil || agent.Type != "agent_message" {
			continue
		}
		if len(agent.EncryptedContent) != 0 {
			return nil, errors.New("Z.ai does not support encrypted agent messages")
		}
		var content []json.RawMessage
		if err := json.Unmarshal(agent.Content, &content); err != nil || len(content) == 0 {
			return nil, errors.New("invalid Z.ai agent message content")
		}
		for _, rawPart := range content {
			var part struct {
				Type             string          `json:"type"`
				Text             *string         `json:"text"`
				EncryptedContent json.RawMessage `json:"encrypted_content"`
			}
			if err := json.Unmarshal(rawPart, &part); err != nil {
				return nil, errors.New("invalid Z.ai agent message content")
			}
			if part.Type == "encrypted_content" || len(part.EncryptedContent) != 0 {
				return nil, errors.New("Z.ai does not support encrypted agent messages")
			}
			if part.Type != "input_text" {
				return nil, errors.New("unsupported Z.ai agent message content")
			}
			if part.Text == nil {
				return nil, errors.New("invalid Z.ai agent message content")
			}
		}
		input[i], err = json.Marshal(map[string]json.RawMessage{
			"type":    json.RawMessage(`"message"`),
			"role":    json.RawMessage(`"user"`),
			"content": agent.Content,
		})
		if err != nil {
			return nil, errors.New("invalid Z.ai agent message content")
		}
		changed = true
	}
	if !changed {
		return body, nil
	}
	request["input"], err = json.Marshal(input)
	if err != nil {
		return nil, errors.New("invalid Z.ai response input")
	}
	normalised, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("invalid Z.ai request body")
	}
	return normalised, nil
}
