package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// responsesHTTPBridge retains downstream Responses history while provider
// adapters own account admission, request mapping and each upstream stream.
type responsesHTTPBridge struct {
	Name          string
	PrewarmPrefix string
	MaxInputBytes int
	AcceptsModel  func(string) bool
	Stream        func(context.Context, *websocket.Conn, http.Header, []byte) (string, []json.RawMessage, error)
	Compaction    func([]byte) (bool, error)
}

func (s *Server) serveResponsesHTTPWebSocket(ctx context.Context, downstream *websocket.Conn, header http.Header, kind int, first []byte, bridge responsesHTTPBridge) error {
	var upgrade <-chan struct{}
	if gate := s.runtimeUpgrade; gate != nil {
		release, err := gate.beginSession()
		if err != nil {
			_ = downstream.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "proxy upgrading"), time.Now().Add(time.Second))
			return nil
		}
		defer release()
		upgrade = gate.PauseSignal()
		if releaseRequest, ok := ctx.Value(runtimeUpgradeRequestReleaseKey{}).(func()); ok {
			releaseRequest()
		}
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCancelClose := context.AfterFunc(serveCtx, func() { _ = downstream.Close() })
	defer stopCancelClose()
	bound := &providerBoundWebSocketConn{websocketRelayConn: downstream, acceptsModel: bridge.AcceptsModel}
	reader := startCodexWSDownstreamReader(serveCtx, cancel, bound)
	defer reader.close()
	var state responsesHTTPBridgeState
	for {
		select {
		case <-upgrade:
			_ = downstream.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "proxy upgrading"), time.Now().Add(time.Second))
			return nil
		default:
		}
		releaseTurn := func() {}
		if gate := s.runtimeUpgrade; gate != nil {
			release, err := gate.BeginTurn()
			if err != nil {
				_ = downstream.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "proxy upgrading"), time.Now().Add(time.Second))
				return nil
			}
			releaseTurn = release
		}
		err := state.serveFrame(serveCtx, downstream, header, kind, first, bridge)
		releaseTurn()
		if err != nil {
			_ = downstream.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "subscription stream failed"), time.Now().Add(time.Second))
			return err
		}
		kind, first, err = reader.read(ctx, serveCtx, nil, upgrade)
		if errors.Is(err, errCodexWSUpgrading) {
			_ = downstream.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "proxy upgrading"), time.Now().Add(time.Second))
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type responsesHTTPBridgeState struct {
	previousID string
	history    []json.RawMessage
}

func (state *responsesHTTPBridgeState) serveFrame(ctx context.Context, downstream *websocket.Conn, header http.Header, kind int, first []byte, bridge responsesHTTPBridge) error {
	if kind != websocket.TextMessage {
		return errors.New(bridge.Name + " requires text response.create frames")
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(first, &request); err != nil || request == nil {
		return errors.New("invalid " + bridge.Name + " WebSocket request")
	}
	if (request["access_programs"] != nil || request["params"] != nil) && !standardResponsesAccessPrograms(first) {
		return errors.New(bridge.Name + " cannot enforce the requested access programme")
	}
	var frameType string
	_ = json.Unmarshal(request["type"], &frameType)
	if frameType != "response.create" {
		return errors.New(bridge.Name + " requires response.create frames")
	}
	delete(request, "type")
	prewarm := false
	if raw, ok := request["generate"]; ok {
		var generate bool
		if err := json.Unmarshal(raw, &generate); err != nil {
			return errors.New("invalid " + bridge.Name + " generate flag")
		}
		prewarm = !generate
	}
	delete(request, "generate")
	input, err := responsesBridgeInput(request["input"], bridge.Name)
	if err != nil {
		return err
	}
	currentItems := len(input)
	var previous string
	_ = json.Unmarshal(request["previous_response_id"], &previous)
	if previous != "" {
		if previous != state.previousID {
			return errors.New(bridge.Name + " previous response unavailable; reconnect with full input")
		}
		input = append(append([]json.RawMessage(nil), state.history...), input...)
	} else if bridge.MaxInputBytes > 0 {
		// A reconnect can supply the whole conversation instead of a delta.
		// Keep its latest user turn when bounding older replay history.
		for index := len(input) - 1; index >= 0; index-- {
			if responsesBridgeMessageRole(input[index]) == "user" {
				currentItems = len(input) - index
				break
			}
		}
	}
	delete(request, "previous_response_id")
	if bridge.MaxInputBytes > 0 {
		input, _ = boundResponsesBridgeInput(input, currentItems, bridge.MaxInputBytes)
	}
	request["input"], _ = json.Marshal(input)
	if prewarm {
		state.previousID = fmt.Sprintf(bridge.PrewarmPrefix+"%x", time.Now().UnixNano())
		state.history = input
		model := extractModel(first)
		for _, event := range []string{"response.created", "response.completed"} {
			data, _ := json.Marshal(map[string]any{"type": event, "response": map[string]any{"id": state.previousID, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": []any{}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
			if err := downstream.WriteMessage(websocket.TextMessage, data); err != nil {
				return err
			}
		}
	} else {
		request["stream"] = json.RawMessage("true")
		body, err := json.Marshal(request)
		if err != nil {
			return errors.New("invalid " + bridge.Name + " WebSocket request")
		}
		if len(body) > maxRequestBody {
			return errors.New(bridge.Name + " request body exceeds 10 MiB")
		}
		id, output, err := bridge.Stream(ctx, downstream, header, body)
		if err != nil {
			if errors.Is(err, errEncryptedSubscriptionAgentMessage) {
				_ = downstream.WriteJSON(map[string]any{
					"type":   "error",
					"status": http.StatusBadRequest,
					"error": map[string]string{
						"type":    "invalid_request_error",
						"code":    "encrypted_agent_message_unsupported",
						"message": err.Error(),
					},
				})
			}
			return err
		}
		state.previousID = id
		compact, _ := bridge.Compaction(body)
		if compact {
			state.history = output
		} else {
			state.history = append(input, output...)
		}
	}
	return nil
}

var responsesHistoryOmittedMessage = json.RawMessage(`{"type":"message","role":"developer","content":"Earlier conversation omitted to fit provider context. Continue using available messages; ask user for missing details."}`)

// boundResponsesBridgeInput keeps the latest complete turn when a stateless
// Responses provider would otherwise receive an ever-growing replay.
func boundResponsesBridgeInput(input []json.RawMessage, currentItems, maxBytes int) ([]json.RawMessage, bool) {
	if maxBytes <= 0 || len(input) == 0 {
		return input, false
	}
	size := 2
	for _, item := range input {
		size += len(item) + 1
	}
	if size <= maxBytes {
		return input, false
	}
	if currentItems < 0 || currentItems > len(input) {
		return input, false
	}
	var preamble []json.RawMessage
	if responsesBridgeMessageRole(input[0]) == "developer" {
		preamble = input[:1]
	}
	reserved := 2 + len(responsesHistoryOmittedMessage) + 1
	for _, item := range preamble {
		reserved += len(item) + 1
	}
	currentStart := len(input) - currentItems
	if currentItems == 0 {
		currentStart = len(input) - 1
	}
	suffixSize := 0
	start := -1
	for index := len(input) - 1; index >= 1; index-- {
		suffixSize += len(input[index]) + 1
		if reserved+suffixSize > maxBytes {
			break
		}
		if index <= currentStart {
			role := responsesBridgeMessageRole(input[index])
			if role == "user" || role == "developer" {
				start = index
			}
		}
	}
	if start < 0 {
		// A single large current turn may be valid upstream. Only discard
		// complete older history, never part of the current turn.
		role := ""
		if currentStart < len(input) {
			role = responsesBridgeMessageRole(input[currentStart])
		}
		if currentStart == 0 || (role != "user" && role != "developer") {
			return input, false
		}
		start = currentStart
	}
	bounded := make([]json.RawMessage, 0, len(preamble)+1+len(input)-start)
	bounded = append(bounded, preamble...)
	bounded = append(bounded, responsesHistoryOmittedMessage)
	bounded = append(bounded, input[start:]...)
	return bounded, true
}

func responsesBridgeMessageRole(item json.RawMessage) string {
	var message struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(item, &message) != nil || (message.Type != "message" && (message.Type != "" || message.Content == nil)) {
		return ""
	}
	return message.Role
}

func responsesBridgeInput(raw json.RawMessage, name string) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return []json.RawMessage{}, nil
	}
	var input []json.RawMessage
	if err := json.Unmarshal(raw, &input); err == nil {
		return input, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		message, _ := json.Marshal(map[string]string{"role": "user", "content": text})
		return []json.RawMessage{message}, nil
	}
	return nil, errors.New("invalid " + name + " response input")
}
