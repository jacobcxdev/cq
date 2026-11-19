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
	var normalisedInput []json.RawMessage
	if input != nil {
		normalisedInput = make([]json.RawMessage, 0, len(input))
	}
	for i, raw := range input {
		var agent struct {
			Type             string          `json:"type"`
			Author           string          `json:"author"`
			Recipient        string          `json:"recipient"`
			Content          json.RawMessage `json:"content"`
			EncryptedContent json.RawMessage `json:"encrypted_content"`
		}
		if err := json.Unmarshal(raw, &agent); err != nil || agent.Type != "agent_message" {
			normalisedInput = append(normalisedInput, raw)
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
		} else if isLegacyEncryptedAgentContent(content, agent.Author, agent.Recipient) {
			// Old chats can retain unreadable worker tasks before later readable
			// history. Never discard the newest task: it may still need delivery.
			if i == len(input)-1 {
				return nil, errEncryptedSubscriptionAgentMessage
			}
			changed = true
			continue
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
		normalised, err := json.Marshal(map[string]json.RawMessage{
			"type":    json.RawMessage(`"message"`),
			"role":    json.RawMessage(`"user"`),
			"content": agent.Content,
		})
		if err != nil {
			return nil, errors.New("invalid " + name + " agent message content")
		}
		normalisedInput = append(normalisedInput, normalised)
		changed = true
	}
	if tools, edited := plaintextCollaborationSchemas(request["tools"], ""); edited {
		request["tools"] = tools
		changed = true
	}
	if !changed {
		return body, nil
	}
	request["input"], err = json.Marshal(normalisedInput)
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
	payload, ok := legacyAgentContentPayload(content, author, recipient)
	if !ok || openAIAgentCiphertext.MatchString(payload) {
		return content, false
	}
	recovered := append([]json.RawMessage(nil), content...)
	recovered[1], _ = json.Marshal(map[string]string{"type": "input_text", "text": payload})
	return recovered, true
}

func isLegacyEncryptedAgentContent(content []json.RawMessage, author, recipient string) bool {
	payload, ok := legacyAgentContentPayload(content, author, recipient)
	return ok && openAIAgentCiphertext.MatchString(payload)
}

func legacyAgentContentPayload(content []json.RawMessage, author, recipient string) (string, bool) {
	if len(content) != 2 || author == "" || recipient == "" {
		return "", false
	}
	var headerObject, payloadObject map[string]json.RawMessage
	if json.Unmarshal(content[0], &headerObject) != nil || json.Unmarshal(content[1], &payloadObject) != nil ||
		len(headerObject) != 2 || len(payloadObject) != 2 || headerObject["type"] == nil || headerObject["text"] == nil ||
		payloadObject["type"] == nil || payloadObject["encrypted_content"] == nil {
		return "", false
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
		header.Type != "input_text" || header.Text == nil || payload.Type != "encrypted_content" || payload.EncryptedContent == nil {
		return "", false
	}
	newTask := fmt.Sprintf("Message Type: NEW_TASK\nTask name: %s\nSender: %s\nPayload:\n", recipient, author)
	message := fmt.Sprintf("Message Type: MESSAGE\nTask name: %s\nSender: %s\nPayload:\n", recipient, author)
	if *header.Text != newTask && *header.Text != message {
		return "", false
	}
	return *payload.EncryptedContent, true
}
