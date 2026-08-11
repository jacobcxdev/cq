package proxy

import (
	"errors"

	"github.com/gorilla/websocket"
)

type codexWSPendingFrame struct {
	messageType int
	encoded     []byte
	request     CodexProtocolRequest
	key         LeaseKey
	portable    bool
	released    bool
}

func newCodexWSPendingFrame(messageType int, payload []byte) (*codexWSPendingFrame, error) {
	if messageType != websocket.TextMessage || len(payload) == 0 || len(payload) > maxRequestBody {
		return nil, ErrCodexWSInvalidFrame
	}
	request, err := ParseCodexProtocolRequest(payload, "", nil)
	if err != nil {
		return nil, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	request, err = validateCodexWSBrokerRequest(payload, request)
	if err != nil {
		return nil, err
	}
	key := NewCodexLeaseKey(request.Metadata.Metadata)
	return &codexWSPendingFrame{
		messageType: messageType,
		encoded:     append([]byte(nil), payload...),
		request:     request,
		key:         key,
		portable:    request.PreviousResponseID == "" && !request.HasEncryptedState && !request.HasTurnState,
	}, nil
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
	frame.released = true
}
