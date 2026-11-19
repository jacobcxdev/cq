package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

var errEncryptedSubscriptionAgentMessage = errors.New("CQ rejected encrypted agent messages before sending the request upstream. Subscription routes require readable agent messages.")

var openAIAgentCiphertext = regexp.MustCompile(`^gAAAAA[A-Za-z0-9_-]+={0,2}$`)

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
			Author           string          `json:"author"`
			Recipient        string          `json:"recipient"`
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
		if recovered, ok := recoverLegacyPlaintextAgentContent(content, agent.Author, agent.Recipient); ok {
			content = recovered
			agent.Content, _ = json.Marshal(content)
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
	if tools, edited := plaintextCollaborationSchemas(request["tools"], ""); edited {
		request["tools"] = tools
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

// Codex versions before CQ's plaintext response marker stored collaboration
// messages from subscription models in encrypted_content even though the value
// was still plaintext. OpenAI-owned encrypted arguments are Fernet tokens with
// the gAAAAA prefix and must remain unreadable.
func recoverLegacyPlaintextAgentContent(content []json.RawMessage, author, recipient string) ([]json.RawMessage, bool) {
	if len(content) != 2 || author == "" || recipient == "" {
		return content, false
	}
	var headerObject, payloadObject map[string]json.RawMessage
	if json.Unmarshal(content[0], &headerObject) != nil || json.Unmarshal(content[1], &payloadObject) != nil ||
		len(headerObject) != 2 || len(payloadObject) != 2 || headerObject["type"] == nil || headerObject["text"] == nil ||
		payloadObject["type"] == nil || payloadObject["encrypted_content"] == nil {
		return content, false
	}
	var header struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	var payload struct {
		Type             string  `json:"type"`
		EncryptedContent *string `json:"encrypted_content"`
	}
	if json.Unmarshal(content[0], &header) != nil || json.Unmarshal(content[1], &payload) != nil ||
		header.Type != "input_text" || header.Text == nil || payload.Type != "encrypted_content" ||
		payload.EncryptedContent == nil || openAIAgentCiphertext.MatchString(*payload.EncryptedContent) {
		return content, false
	}
	newTask := fmt.Sprintf("Message Type: NEW_TASK\nTask name: %s\nSender: %s\nPayload:\n", recipient, author)
	message := fmt.Sprintf("Message Type: MESSAGE\nTask name: %s\nSender: %s\nPayload:\n", recipient, author)
	if *header.Text != newTask && *header.Text != message {
		return content, false
	}
	recovered := append([]json.RawMessage(nil), content...)
	recovered[1], _ = json.Marshal(map[string]string{"type": "input_text", "text": *payload.EncryptedContent})
	return recovered, true
}
