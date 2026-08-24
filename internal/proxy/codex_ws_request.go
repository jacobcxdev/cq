package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gorilla/websocket"
)

type codexWSPendingFrame struct {
	messageType int
	encoded     []byte
	request     CodexProtocolRequest
	key         LeaseKey
	portable    bool
	prewarm     bool
	diagnostics *routeDiagnostics
	released    bool
}

func newCodexWSPendingFrame(messageType int, payload []byte) (*codexWSPendingFrame, error) {
	if messageType != websocket.TextMessage || len(payload) == 0 || codexLimitExceeded(len(payload), codexWebSocketMessageMaxBytes) {
		return nil, ErrCodexWSInvalidFrame
	}
	request, err := parseCodexProtocolRequest(payload, "", nil, codexWebSocketMessageMaxBytes)
	if err != nil {
		return nil, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	prewarm := request.Metadata.Found && request.Metadata.Metadata.RequestKind == CodexRequestPrewarm
	if prewarm {
		request, err = validateCodexWSPrewarmRequest(payload, request)
	} else {
		request, err = validateCodexWSBrokerRequest(payload, request)
	}
	if err != nil {
		return nil, err
	}
	var key LeaseKey
	if !prewarm {
		key = NewCodexLeaseKey(request.Metadata.Metadata)
	}
	diagnostics := &routeDiagnostics{}
	diagnostics.codex = codexObservationFieldsForRequestShape(classifyCodexRequestShape(request, nil))
	return &codexWSPendingFrame{
		messageType: messageType,
		encoded:     append([]byte(nil), payload...),
		request:     request,
		key:         key,
		portable:    request.PreviousResponseID == "" && !request.HasEncryptedState && !request.HasTurnState,
		prewarm:     prewarm,
		diagnostics: diagnostics,
	}, nil
}

// codexWSFrameWithoutPrewarmAnchor removes the connection-scoped anchor after
// CQ retires a completed prewarm upstream and opens its replacement.
func codexWSFrameWithoutPrewarmAnchor(original *codexWSPendingFrame, anchor string) (*codexWSPendingFrame, error) {
	if original == nil || original.prewarm || anchor == "" || original.request.PreviousResponseID != anchor {
		return nil, ErrCodexWSInvalidFrame
	}
	encoded, removed, err := codexWSObjectWithoutPrewarmAnchor(original.encoded, anchor, true)
	if err != nil {
		return nil, err
	}
	defer clearBytes(encoded)
	if removed == 0 {
		return nil, ErrCodexWSInvalidFrame
	}
	rewritten, err := newCodexWSPendingFrame(original.messageType, encoded)
	if err != nil {
		return nil, err
	}
	if rewritten.request.PreviousResponseID != "" || rewritten.request.HasPreviousResponseID || rewritten.key != original.key || rewritten.request.Model != original.request.Model {
		rewritten.Release()
		return nil, ErrCodexWSInvalidFrame
	}
	return rewritten, nil
}

func codexWSObjectWithoutPrewarmAnchor(source []byte, anchor string, rewriteParams bool) (result []byte, removed int, returnErr error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	var encoded bytes.Buffer
	encoded.Grow(len(source))
	encoded.WriteByte('{')
	defer func() { clearBytes(encoded.Bytes()) }()
	written := 0
	previousFound := false
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok {
			return nil, 0, errors.Join(ErrCodexWSInvalidFrame, tokenErr)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			clearBytes(value)
			return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
		}
		if key == "previous_response_id" {
			if previousFound {
				clearBytes(value)
				return nil, 0, ErrCodexWSInvalidFrame
			}
			previousFound = true
			var previous string
			valueErr := json.Unmarshal(value, &previous)
			clearBytes(value)
			if valueErr != nil || previous != anchor {
				return nil, 0, errors.Join(ErrCodexWSInvalidFrame, valueErr)
			}
			removed++
			continue
		}
		if rewriteParams && key == "params" && len(bytes.TrimSpace(value)) > 0 && bytes.TrimSpace(value)[0] == '{' {
			rewritten, paramsRemoved, rewriteErr := codexWSObjectWithoutPrewarmAnchor(value, anchor, false)
			clearBytes(value)
			if rewriteErr != nil {
				return nil, 0, rewriteErr
			}
			value = rewritten
			removed += paramsRemoved
		}
		encodedKey, marshalErr := json.Marshal(key)
		if marshalErr != nil {
			clearBytes(value)
			return nil, 0, errors.Join(ErrCodexWSInvalidFrame, marshalErr)
		}
		if written > 0 {
			encoded.WriteByte(',')
		}
		encoded.Write(encodedKey)
		encoded.WriteByte(':')
		encoded.Write(value)
		clearBytes(encodedKey)
		clearBytes(value)
		written++
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	encoded.WriteByte('}')
	return append([]byte(nil), encoded.Bytes()...), removed, nil
}

func validateCodexWSPrewarmRequest(payload []byte, request CodexProtocolRequest) (CodexProtocolRequest, error) {
	authority, code, err := extractCodexFrozenAuthority(payload, "", false, nil)
	if err == nil {
		request = authority.protocol
	} else if request.Metadata.Source != CodexTurnMetadataHandshake || code != CodexFrozenRequestMetadataAuthority {
		return CodexProtocolRequest{}, fmt.Errorf("%w: %v", ErrCodexWSInvalidFrame, err)
	}
	if request.Type != "response.create" || request.Model == "" || !request.Metadata.Found || request.Metadata.Metadata.RequestKind != CodexRequestPrewarm {
		return CodexProtocolRequest{}, fmt.Errorf("%w: invalid prewarm authority", ErrCodexWSInvalidFrame)
	}
	generate, found, err := codexWSPrewarmGenerate(payload)
	if err != nil || !found || generate {
		return CodexProtocolRequest{}, fmt.Errorf("%w: invalid prewarm generate authority", ErrCodexWSInvalidFrame)
	}
	return request, nil
}

func codexWSPrewarmGenerate(payload []byte) (bool, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false, false, err
	}
	var generate bool
	found := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return false, false, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false, false, err
		}
		if key != "generate" {
			continue
		}
		if found {
			return false, false, errors.New("duplicate prewarm generate field")
		}
		found = true
		if err := json.Unmarshal(value, &generate); err != nil || string(value) == "null" {
			return false, false, errors.New("invalid prewarm generate field")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return false, false, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false, false, errors.New("invalid trailing prewarm JSON")
	}
	return generate, found, nil
}

func (frame *codexWSPendingFrame) Release() {
	if frame == nil || frame.released {
		return
	}
	clear(frame.encoded)
	frame.encoded = nil
	frame.request = CodexProtocolRequest{}
	frame.key = LeaseKey{}
	frame.portable = false
	frame.prewarm = false
	frame.diagnostics = nil
	frame.released = true
}
