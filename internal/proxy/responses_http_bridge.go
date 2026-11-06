package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"time"
)

// responsesHTTPBridge retains downstream Responses history while provider
// adapters own account admission, request mapping and each upstream stream.
type responsesHTTPBridge struct {
	Name          string
	PrewarmPrefix string
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
	var previousID string
	var history []json.RawMessage
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
		err := func() error {
			defer releaseTurn()
			if kind != websocket.TextMessage {
				return errors.New(bridge.Name + " requires text response.create frames")
			}
			var request map[string]json.RawMessage
			if err := json.Unmarshal(first, &request); err != nil || request == nil {
				return errors.New("invalid " + bridge.Name + " WebSocket request")
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
			var previous string
			_ = json.Unmarshal(request["previous_response_id"], &previous)
			if previous != "" {
				if previous != previousID {
					return errors.New(bridge.Name + " previous response unavailable; reconnect with full input")
				}
				input = append(append([]json.RawMessage(nil), history...), input...)
			}
			delete(request, "previous_response_id")
			request["input"], _ = json.Marshal(input)
			if prewarm {
				previousID = fmt.Sprintf(bridge.PrewarmPrefix+"%x", time.Now().UnixNano())
				history = input
				model := extractModel(first)
				for _, event := range []string{"response.created", "response.completed"} {
					data, _ := json.Marshal(map[string]any{"type": event, "response": map[string]any{"id": previousID, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": []any{}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
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
				id, output, err := bridge.Stream(serveCtx, downstream, header, body)
				if err != nil {
					return err
				}
				previousID = id
				compact, _ := bridge.Compaction(body)
				if compact {
					history = output
				} else {
					history = append(input, output...)
				}
			}
			return nil
		}()
		if err != nil {
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
