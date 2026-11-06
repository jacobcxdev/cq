package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
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
	accounts, err := s.zaiAccounts()
	if err != nil || len(accounts) == 0 {
		return "", err
	}
	return accounts[0].APIKey, nil
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
	zaiEnabled := s.ZAIAPIKey != nil || s.ZAIAccounts != nil || s.hasZAICatalog()
	if !zaiEnabled && s.HarnessObserver == nil {
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
	s.observeHarness(r.Header, model, decodedBody)
	if !zaiEnabled {
		return false, ""
	}
	if RouteModelWithCatalog(model, s.Catalog) != ProviderZAI {
		return false, ""
	}
	compact, err := zaiCompactionRequested(decodedBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true, model
	}
	if compact {
		fmt.Fprintf(os.Stderr, "cq: route POST %s model=%q provider=zai (native compaction)\n", r.URL.Path, model)
		s.handleZAICompaction(w, r, decodedBody)
		return true, model
	}
	if strings.HasSuffix(r.URL.Path, "/responses/compact") {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Z.ai does not support Responses compaction")
		return true, model
	}
	if len(body) > maxRequestBody || len(decodedBody) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Z.ai request body exceeds 10 MiB")
		return true, model
	}
	decodedBody, err = normaliseZAIRequest(decodedBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true, model
	}
	if len(decodedBody) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Z.ai request body exceeds 10 MiB")
		return true, model
	}
	accounts, err := s.zaiAccounts()
	if err != nil || len(accounts) == 0 {
		writeError(w, http.StatusServiceUnavailable, "api_error", "Z.ai API key unavailable")
		return true, model
	}
	upstream, err := s.zaiResponsesURL()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "invalid Z.ai upstream")
		return true, model
	}
	fmt.Fprintf(os.Stderr, "cq: route POST %s model=%q provider=zai (native)\n", r.URL.Path, model)
	response, err := s.doZAIRequest(r.Context(), upstream, r.Header, decodedBody, accounts)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "Z.ai upstream request failed")
		return true, model
	}
	defer response.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	success := response.StatusCode >= 200 && response.StatusCode < 300
	var responseBody io.Reader = response.Body
	if success && mediaType == "application/json" {
		data, readErr := httputil.ReadBody(response.Body)
		if readErr != nil {
			writeError(w, http.StatusBadGateway, "api_error", "failed to read Z.ai response")
			return true, model
		}
		if json.Valid(data) {
			data, err = normaliseZAIResponse(data)
			if err != nil {
				writeError(w, http.StatusBadGateway, "api_error", "invalid Z.ai response")
				return true, model
			}
		}
		responseBody = bytes.NewReader(data)
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "X-Request-Id"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if success && mediaType == "text/event-stream" {
		if err := copyZAIResponseSSE(zaiFlushWriter{w}, responseBody); err != nil {
			fmt.Fprintln(os.Stderr, "cq: Z.ai HTTP response stream failed")
		}
	} else {
		_, _ = io.Copy(zaiFlushWriter{w}, responseBody)
	}
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
	return s.serveResponsesHTTPWebSocket(ctx, downstream, header, kind, first, responsesHTTPBridge{
		Name: "Z.ai", PrewarmPrefix: "cq_zai_prewarm_",
		AcceptsModel: func(model string) bool { return RouteModelWithCatalog(model, s.Catalog) == ProviderZAI },
		Stream:       s.streamZAIWebSocketResponse, Compaction: zaiCompactionRequested,
	})
}

func zaiResponseInput(raw json.RawMessage) ([]json.RawMessage, error) {
	return responsesBridgeInput(raw, "Z.ai")
}

func (s *Server) streamZAIWebSocketResponse(ctx context.Context, downstream *websocket.Conn, header http.Header, body []byte) (string, []json.RawMessage, error) {
	s.observeHarness(header, extractModel(body), body)
	body, _, err := normaliseCQCompactionInput(body, s.cqCompactionToken())
	if err != nil {
		return "", nil, err
	}
	compact, err := zaiCompactionRequested(body)
	if err != nil {
		return "", nil, err
	}
	if compact {
		fmt.Fprintln(os.Stderr, "cq: Z.ai native WebSocket compaction")
		response, err := s.compactZAI(ctx, header, body)
		if err != nil {
			return "", nil, err
		}
		for _, event := range zaiCompactionEvents(response) {
			data, _ := json.Marshal(event)
			if err := downstream.WriteMessage(websocket.TextMessage, data); err != nil {
				return "", nil, err
			}
		}
		return response.ID, response.Output, nil
	}
	body, err = normaliseZAIRequest(body)
	if err != nil {
		data, _ := json.Marshal(map[string]any{"type": "error", "status": http.StatusBadRequest, "error": map[string]string{"type": "invalid_request_error", "code": "zai_invalid_request", "message": err.Error()}})
		_ = downstream.WriteMessage(websocket.TextMessage, data)
		return "", nil, err
	}
	if len(body) > maxRequestBody {
		const message = "Z.ai request body exceeds 10 MiB"
		data, _ := json.Marshal(map[string]any{"type": "error", "status": http.StatusRequestEntityTooLarge, "error": map[string]string{"type": "invalid_request_error", "code": "zai_request_too_large", "message": message}})
		_ = downstream.WriteMessage(websocket.TextMessage, data)
		return "", nil, errors.New(message)
	}
	accounts, err := s.zaiAccounts()
	if err != nil || len(accounts) == 0 {
		return "", nil, errors.New("Z.ai API key unavailable")
	}
	target, err := s.zaiResponsesURL()
	if err != nil {
		return "", nil, err
	}
	header = header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	header.Set("Accept", "text/event-stream")
	response, err := s.doZAIRequest(ctx, target, header, body, accounts)
	if err != nil {
		return "", nil, errors.New("Z.ai HTTP stream request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := json.Marshal(map[string]any{"type": "error", "status": response.StatusCode, "error": map[string]string{"type": "api_error", "code": "zai_upstream_error", "message": "Z.ai request failed"}})
		_ = downstream.WriteMessage(websocket.TextMessage, data)
		return "", nil, fmt.Errorf("Z.ai HTTP stream returned status %d", response.StatusCode)
	}
	return relayResponsesSSE(response.Body, "Z.ai", normaliseZAIResponse, func(_ string, data []byte) error {
		return downstream.WriteMessage(websocket.TextMessage, data)
	})
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
	normalise    func([]byte) ([]byte, error)
}

func (c *providerBoundWebSocketConn) ReadMessage() (int, []byte, error) {
	kind, payload, err := c.websocketRelayConn.ReadMessage()
	if err != nil {
		return kind, payload, err
	}
	if kind == websocket.TextMessage {
		if c.normalise != nil {
			payload, err = c.normalise(payload)
			if err != nil {
				return 0, nil, err
			}
		}
		model := extractCodexWebSocketFrameModel(payload)
		if model != "" && c.acceptsModel != nil && !c.acceptsModel(model) {
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "model provider changed; reconnect"), time.Now().Add(time.Second))
			_ = c.Close()
			return 0, nil, errors.New("model provider changed; reconnect WebSocket")
		}
	}
	return kind, payload, nil
}
