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
