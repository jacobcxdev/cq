package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

const defaultZAIUpstream = "https://api.z.ai/api/v1"

func (s *Server) hasZAICatalog() bool {
	if s.Catalog == nil {
		return false
	}
	for _, entry := range s.Catalog.Snapshot().Entries {
		if entry.Provider == modelregistry.ProviderZAI {
			return true
		}
	}
	return false
}

func (s *Server) zaiKey() (string, error) {
	if s.ZAIAPIKey == nil {
		return "", nil
	}
	key, err := s.ZAIAPIKey()
	return strings.TrimSpace(key), err
}

func (s *Server) zaiResponsesURL() (string, error) {
	base := s.ZAIUpstream
	if base == "" {
		base = defaultZAIUpstream
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + "/responses")
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("invalid Z.ai upstream")
	}
	return u.String(), nil
}

// tryHandleZAI inspects native Responses before any Codex account admission.
// Non-Z.ai requests retain their exact encoded request bytes for Codex.
func (s *Server) tryHandleZAI(w http.ResponseWriter, r *http.Request) (bool, string) {
	if s.ZAIAPIKey == nil && !s.hasZAICatalog() {
		return false, ""
	}
	if r.Body == nil {
		return false, ""
	}
	body, err := readCodexNativeHTTPRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return true, ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	encoding, err := parseCodexContentEncoding(r.Header)
	if err != nil {
		return false, ""
	}
	decodedBody := body
	if encoding == "zstd" || (encoding == "" && codexZstdFrame(body)) {
		decoded, err := DecodeCodexRequest(body, encoding, codexHTTPZstdLimits())
		if err != nil {
			return false, ""
		}
		decodedBody = decoded.Decoded()
	}
	model := extractModel(decodedBody)
	if RouteModelWithCatalog(model, s.Catalog) != ProviderZAI {
		return false, ""
	}
	if strings.HasSuffix(r.URL.Path, "/responses/compact") {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Z.ai does not support Responses compaction")
		return true, model
	}
	if len(body) > maxRequestBody || len(decodedBody) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Z.ai request body exceeds 10 MiB")
		return true, model
	}
	key, err := s.zaiKey()
	if err != nil || key == "" {
		writeError(w, http.StatusServiceUnavailable, "api_error", "Z.ai API key unavailable")
		return true, model
	}
	upstream, err := s.zaiResponsesURL()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "invalid Z.ai upstream")
		return true, model
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, bytes.NewReader(decodedBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to create Z.ai request")
		return true, model
	}
	fmt.Fprintf(os.Stderr, "cq: route POST %s model=%q provider=zai (native)\n", r.URL.Path, model)
	setZAIHeaders(req.Header, r.Header, key)
	transport := s.ZAITransport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "Z.ai upstream request failed")
		return true, model
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "X-Request-Id"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(zaiFlushWriter{w}, response.Body)
	return true, model
}

// Build headers afresh: local bearer, cookies and ChatGPT account identities
// must never cross into a different provider's trust boundary.
func setZAIHeaders(dst, src http.Header, key string) {
	dst.Set("Authorization", "Bearer "+key)
	dst.Set("Content-Type", "application/json")
	for _, name := range []string{"Accept", "User-Agent"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

type zaiFlushWriter struct{ http.ResponseWriter }

func (w zaiFlushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

// Z.ai accepts a WebSocket handshake but rejects native response.create
// frames. Execute each Codex frame over HTTP SSE and retain connection history
// because Coding Plan responses with store:false cannot be resumed upstream.
func (s *Server) serveZAIWebSocket(ctx context.Context, downstream *websocket.Conn, header http.Header, kind int, first []byte) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCancelClose := context.AfterFunc(serveCtx, func() { _ = downstream.Close() })
	defer stopCancelClose()
	bound := &providerBoundWebSocketConn{websocketRelayConn: downstream, acceptsModel: func(model string) bool { return RouteModelWithCatalog(model, s.Catalog) == ProviderZAI }}
	reader := startCodexWSDownstreamReader(serveCtx, cancel, bound)
	defer reader.close()
	var previousID string
	var history []json.RawMessage
	for {
		if kind != websocket.TextMessage {
			return errors.New("Z.ai requires text response.create frames")
		}
		var request map[string]json.RawMessage
		if err := json.Unmarshal(first, &request); err != nil || request == nil {
			return errors.New("invalid Z.ai WebSocket request")
		}
		var frameType string
		_ = json.Unmarshal(request["type"], &frameType)
		if frameType != "response.create" {
			return errors.New("Z.ai requires response.create frames")
		}
		delete(request, "type")
		prewarm := false
		if raw, ok := request["generate"]; ok {
			var generate bool
			if err := json.Unmarshal(raw, &generate); err != nil {
				return errors.New("invalid Z.ai generate flag")
			}
			prewarm = !generate
		}
		delete(request, "generate")
		input, err := zaiResponseInput(request["input"])
		if err != nil {
			return err
		}
		var previous string
		_ = json.Unmarshal(request["previous_response_id"], &previous)
		if previous != "" {
			if previous != previousID {
				return errors.New("Z.ai previous response unavailable; reconnect with full input")
			}
			input = append(append([]json.RawMessage(nil), history...), input...)
		}
		delete(request, "previous_response_id")
		request["input"], _ = json.Marshal(input)
		if prewarm {
			previousID = fmt.Sprintf("cq_zai_prewarm_%x", time.Now().UnixNano())
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
				return errors.New("invalid Z.ai WebSocket request")
			}
			if len(body) > maxRequestBody {
				return errors.New("Z.ai request body exceeds 10 MiB")
			}
			id, output, err := s.streamZAIWebSocketResponse(serveCtx, downstream, header, body)
			if err != nil {
				return err
			}
			previousID = id
			history = append(input, output...)
		}
		kind, first, err = reader.read(ctx, serveCtx, nil, nil)
		if err != nil {
			return err
		}
	}
}

func zaiResponseInput(raw json.RawMessage) ([]json.RawMessage, error) {
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
	return nil, errors.New("invalid Z.ai response input")
}

func (s *Server) streamZAIWebSocketResponse(ctx context.Context, downstream *websocket.Conn, header http.Header, body []byte) (string, []json.RawMessage, error) {
	key, err := s.zaiKey()
	if err != nil || key == "" {
		return "", nil, errors.New("Z.ai API key unavailable")
	}
	target, err := s.zaiResponsesURL()
	if err != nil {
		return "", nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", nil, errors.New("invalid Z.ai request")
	}
	setZAIHeaders(request.Header, header, key)
	request.Header.Set("Accept", "text/event-stream")
	transport := s.ZAITransport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", nil, errors.New("Z.ai HTTP stream request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := json.Marshal(map[string]any{"type": "error", "status": response.StatusCode, "error": map[string]string{"type": "api_error", "code": "zai_upstream_error", "message": "Z.ai request failed"}})
		_ = downstream.WriteMessage(websocket.TextMessage, data)
		return "", nil, fmt.Errorf("Z.ai HTTP stream returned status %d", response.StatusCode)
	}
	parser := NewCodexSSEParser(1 << 20)
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			events, err := parser.Feed(buffer[:n])
			if err != nil {
				return "", nil, errors.New("invalid Z.ai SSE stream")
			}
			for _, event := range events {
				if event.Kind == CodexSSEDone {
					continue
				}
				if !json.Valid(event.Data) {
					return "", nil, errors.New("invalid Z.ai SSE event")
				}
				if err := downstream.WriteMessage(websocket.TextMessage, event.Data); err != nil {
					return "", nil, err
				}
				if event.Type == "response.completed" {
					var completed struct {
						Response struct {
							ID     string            `json:"id"`
							Output []json.RawMessage `json:"output"`
						} `json:"response"`
					}
					if err := json.Unmarshal(event.Data, &completed); err != nil || completed.Response.ID == "" {
						return "", nil, errors.New("invalid Z.ai completed response")
					}
					return completed.Response.ID, completed.Response.Output, nil
				}
				switch event.Type {
				case "response.failed", "response.incomplete", "error":
					return "", nil, errors.New("Z.ai response failed")
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return "", nil, errors.New("Z.ai stream ended before response completion")
			}
			return "", nil, errors.New("Z.ai HTTP stream read failed")
		}
	}
}

// A consumed frame is replayed exactly once before reading the connection.
type prefetchedWebSocketConn struct {
	websocketRelayConn
	kind    int
	payload []byte
	pending bool
}

func (c *prefetchedWebSocketConn) ReadMessage() (int, []byte, error) {
	if c.pending {
		c.pending = false
		return c.kind, c.payload, nil
	}
	return c.websocketRelayConn.ReadMessage()
}

type initialCodexWebSocketRoutingHandler interface {
	ServeInitial(context.Context, *websocket.Conn, http.Header, int, []byte, func(string) bool) error
}

// Model switches across providers require a fresh connection. Reject the frame
// before either account planner or upstream transport can consume it.
type providerBoundWebSocketConn struct {
	websocketRelayConn
	acceptsModel func(string) bool
}

func (c *providerBoundWebSocketConn) ReadMessage() (int, []byte, error) {
	kind, payload, err := c.websocketRelayConn.ReadMessage()
	if err != nil {
		return kind, payload, err
	}
	if kind == websocket.TextMessage {
		model := extractCodexWebSocketFrameModel(payload)
		if model != "" && !c.acceptsModel(model) {
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "model provider changed; reconnect"), time.Now().Add(time.Second))
			_ = c.Close()
			return 0, nil, errors.New("model provider changed; reconnect WebSocket")
		}
	}
	return kind, payload, nil
}
