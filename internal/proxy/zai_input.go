package proxy

import (
	"encoding/json"
	"errors"
)

var errEncryptedSubscriptionAgentMessage = errors.New("CQ rejected encrypted agent messages before sending the request upstream. Subscription routes require readable agent messages.")

// normaliseZAIRequest translates plaintext Codex agent messages into the
// ordinary user messages understood by Z.ai's Responses endpoint.
func normaliseZAIRequest(body []byte) ([]byte, error) {
	return normaliseSubscriptionAgentMessages(body, "Z.ai")
}

func normaliseSubscriptionAgentMessages(body []byte, name string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, errors.New("invalid " + name + " request body")
	}
	input, err := responsesBridgeInput(request["input"], name)
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
			return nil, errEncryptedSubscriptionAgentMessage
		}
		var content []json.RawMessage
		if err := json.Unmarshal(agent.Content, &content); err != nil || len(content) == 0 {
			return nil, errors.New("invalid " + name + " agent message content")
		}
		for _, rawPart := range content {
			var part struct {
				Type             string          `json:"type"`
				Text             *string         `json:"text"`
				EncryptedContent json.RawMessage `json:"encrypted_content"`
			}
			if err := json.Unmarshal(rawPart, &part); err != nil {
				return nil, errors.New("invalid " + name + " agent message content")
			}
			if part.Type == "encrypted_content" || len(part.EncryptedContent) != 0 {
				return nil, errEncryptedSubscriptionAgentMessage
			}
			if part.Type != "input_text" {
				return nil, errors.New("unsupported " + name + " agent message content")
			}
			if part.Text == nil {
				return nil, errors.New("invalid " + name + " agent message content")
			}
		}
		input[i], err = json.Marshal(map[string]json.RawMessage{
			"type":    json.RawMessage(`"message"`),
			"role":    json.RawMessage(`"user"`),
			"content": agent.Content,
		})
		if err != nil {
			return nil, errors.New("invalid " + name + " agent message content")
		}
		changed = true
	}
	if !changed {
		return body, nil
	}
	request["input"], err = json.Marshal(input)
	if err != nil {
		return nil, errors.New("invalid " + name + " response input")
	}
	normalised, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("invalid " + name + " request body")
	}
	return normalised, nil
}
