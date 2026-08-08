package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const codexProtocolMaxBytes = 8 << 20

type CodexProtocolRequest struct {
	Type               string
	Model              string
	PreviousResponseID string
	Metadata           CodexTurnMetadataResult
	TurnState          string
	HasTurnState       bool
	HasEncryptedState  bool
}

func ParseCodexProtocolRequest(body []byte, directMetadata string, handshake *CodexTurnMetadata) (CodexProtocolRequest, error) {
	if len(body) > codexProtocolMaxBytes {
		return CodexProtocolRequest{}, errors.New("Codex protocol request exceeds limit")
	}
	metadata, err := ParseCodexTurnMetadata(body, directMetadata, handshake)
	if err != nil {
		return CodexProtocolRequest{}, err
	}
	var envelope struct {
		Type               string          `json:"type"`
		Model              string          `json:"model"`
		PreviousResponseID string          `json:"previous_response_id"`
		Params             json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return CodexProtocolRequest{}, fmt.Errorf("decode Codex protocol request: %w", err)
	}
	if len(envelope.Params) != 0 && !bytes.Equal(envelope.Params, []byte("null")) {
		var params struct {
			Model              string `json:"model"`
			PreviousResponseID string `json:"previous_response_id"`
		}
		if err := json.Unmarshal(envelope.Params, &params); err != nil {
			return CodexProtocolRequest{}, fmt.Errorf("decode Codex protocol params: %w", err)
		}
		if envelope.Model == "" {
			envelope.Model = params.Model
		}
		if envelope.PreviousResponseID == "" {
			envelope.PreviousResponseID = params.PreviousResponseID
		}
	}
	return CodexProtocolRequest{
		Type:               envelope.Type,
		Model:              envelope.Model,
		PreviousResponseID: envelope.PreviousResponseID,
		Metadata:           metadata,
		HasEncryptedState:  jsonContainsKey(body, "encrypted_content"),
	}, nil
}

type CodexWrappedError struct {
	Found          bool
	Status         int
	ErrorType      string
	Code           string
	Message        string
	AuthFailure    bool
	HardUsageLimit bool
}

func ParseCodexWrappedError(payload []byte) (CodexWrappedError, error) {
	if len(payload) > codexProtocolMaxBytes {
		return CodexWrappedError{}, errors.New("Codex error event exceeds limit")
	}
	var envelope struct {
		Type       string `json:"type"`
		Status     int    `json:"status"`
		StatusCode int    `json:"status_code"`
		Error      struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	if envelope.Type != "error" {
		return CodexWrappedError{}, nil
	}
	status := envelope.Status
	if status == 0 {
		status = envelope.StatusCode
	}
	return CodexWrappedError{
		Found:          true,
		Status:         status,
		ErrorType:      envelope.Error.Type,
		Code:           envelope.Error.Code,
		Message:        envelope.Error.Message,
		AuthFailure:    status == http.StatusUnauthorized || status == http.StatusForbidden,
		HardUsageLimit: status == http.StatusTooManyRequests && envelope.Error.Type == "usage_limit_reached",
	}, nil
}

type CodexCompactObservation struct {
	ResponseID        string
	HasEncryptedState bool
}

func ParseCodexCompactResponse(body []byte) (CodexCompactObservation, error) {
	if len(body) > codexProtocolMaxBytes {
		return CodexCompactObservation{}, errors.New("Codex compact response exceeds limit")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		return CodexCompactObservation{}, fmt.Errorf("decode Codex compact response: %w", err)
	}
	if value == nil {
		return CodexCompactObservation{}, errors.New("Codex compact response must be an object")
	}
	var responseID string
	if raw, ok := value["id"]; ok {
		if err := json.Unmarshal(raw, &responseID); err != nil {
			return CodexCompactObservation{}, errors.New("Codex compact response id must be a string")
		}
	}
	return CodexCompactObservation{ResponseID: responseID, HasEncryptedState: jsonContainsKey(body, "encrypted_content")}, nil
}

func ParseCodexTurnStateHeader(header http.Header) (string, bool, error) {
	values := header.Values("x-codex-turn-state")
	if len(values) == 0 {
		return "", false, nil
	}
	value := values[0]
	if len(value) > codexTurnMetadataMaxBytes {
		return "", false, errors.New("Codex turn state header exceeds limit")
	}
	for _, other := range values[1:] {
		if other != value {
			return "", false, errors.New("conflicting Codex turn state headers")
		}
	}
	return value, value != "", nil
}

func parseCodexTurnStateObject(raw json.RawMessage) (string, bool, error) {
	var envelope struct {
		Headers map[string]json.RawMessage `json:"headers"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", false, err
	}
	for name, value := range envelope.Headers {
		if !strings.EqualFold(name, "x-codex-turn-state") {
			continue
		}
		var state string
		if err := json.Unmarshal(value, &state); err != nil {
			return "", false, errors.New("Codex turn state metadata must be a string")
		}
		if len(state) > codexTurnMetadataMaxBytes {
			return "", false, errors.New("Codex turn state metadata exceeds limit")
		}
		return state, state != "", nil
	}
	return "", false, nil
}

func jsonContainsKey(body []byte, key string) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			for name, child := range typed {
				if name == key {
					return true
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}
