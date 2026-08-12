package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	codexLiveSidebandRoot = "https://api.openai.com/v1"
	codexLiveCallTTL      = time.Hour
)

var codexLiveProtocolHeaders = []string{
	"OpenAI-Alpha",
	"X-Session-ID",
	"Session-ID",
	"Thread-ID",
	"Originator",
	"X-OAI-Attestation",
}

type codexLiveCall struct {
	choice    RouteChoice
	attempt   CandidateAttempt
	expiresAt time.Time
}

type codexLiveState struct {
	mu    sync.Mutex
	calls map[string]codexLiveCall
}

type codexLiveBackendRequest struct {
	SDP     string          `json:"sdp"`
	Session json.RawMessage `json:"session,omitempty"`
}

type codexLiveSidebandTarget struct {
	callID string
	style  string
}

func (s *Server) handleCodexLiveCall(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	statusCode := 0
	diagError := ""
	ctx, routeDiag := withRouteDiagnostics(r.Context())
	r = r.WithContext(ctx)
	defer func() {
		event := RouteEvent{
			Time:       start.UTC(),
			Method:     r.Method,
			Path:       r.URL.Path,
			Provider:   "codex",
			RouteKind:  "codex_live_call",
			StatusCode: statusCode,
			LatencyMS:  time.Since(start).Milliseconds(),
			Error:      diagError,
		}
		event.applyRouteDiagnostics(routeDiag)
		event.applySessionCorrelation(r.Header)
		s.emitDiagnostics(event)
	}()

	body, err := codexLiveBackendBody(r)
	if err != nil {
		statusCode = http.StatusBadRequest
		diagError = diagnosticsErrorCode("invalid_request_error", err.Error())
		writeError(w, statusCode, "invalid_request_error", err.Error())
		return
	}

	upstreamURL := strings.TrimRight(s.Config.CodexUpstream, "/") +
		"/realtime/calls?intent=quicksilver&architecture=avas"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		statusCode = http.StatusInternalServerError
		diagError = diagnosticsErrorCode("api_error", "create codex live request")
		writeError(w, statusCode, "api_error", "failed to create codex live request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	copyCodexLiveProtocolHeaders(upReq.Header, r.Header)
	upReq.ContentLength = int64(len(body))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	resp, choice, attempt, err := s.doCodexRequest(r.Context(), "", upReq)
	if err != nil {
		statusCode = http.StatusBadGateway
		diagError = diagnosticsErrorCode("api_error", "codex live upstream error")
		writeError(w, statusCode, "api_error", fmt.Sprintf("codex live upstream error: %v", err))
		return
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode

	responseBody, err := readCodexLiveBody(resp.Body)
	if err != nil {
		statusCode = http.StatusBadGateway
		diagError = diagnosticsErrorCode("api_error", err.Error())
		writeError(w, statusCode, "api_error", err.Error())
		return
	}
	if callID := codexLiveCallID(resp.Header.Get("Location")); callID != "" && resp.StatusCode < 400 {
		s.codexLive.remember(callID, choice, attempt)
	}
	copyCodexLiveResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

func (s *Server) handleCodexLiveSideband(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	statusCode := 0
	diagError := ""
	ctx, routeDiag := withRouteDiagnostics(r.Context())
	r = r.WithContext(ctx)
	defer func() {
		event := RouteEvent{
			Time:       start.UTC(),
			Method:     r.Method,
			Path:       r.URL.Path,
			Provider:   "codex",
			RouteKind:  "codex_live_sideband",
			StatusCode: statusCode,
			LatencyMS:  time.Since(start).Milliseconds(),
			Error:      diagError,
		}
		event.applyRouteDiagnostics(routeDiag)
		event.applySessionCorrelation(r.Header)
		s.emitDiagnostics(event)
	}()

	target, ok := parseCodexLiveSidebandTarget(r.URL)
	if !ok {
		statusCode = http.StatusNotFound
		diagError = diagnosticsErrorCode("invalid_request_error", "invalid codex live call ID")
		writeError(w, statusCode, "invalid_request_error", "invalid codex live call ID")
		return
	}

	router, executor := s.codexWebSocketRouting()
	if router == nil || executor == nil {
		statusCode = http.StatusServiceUnavailable
		diagError = diagnosticsErrorCode("api_error", "no codex accounts configured")
		writeError(w, statusCode, "api_error", "no codex accounts configured")
		return
	}

	upstreamURL := s.codexLiveSidebandURL(target)
	headers := http.Header{}
	copyCodexLiveProtocolHeaders(headers, r.Header)
	for _, subprotocol := range websocket.Subprotocols(r) {
		headers.Add("Sec-WebSocket-Protocol", subprotocol)
	}
	var upstreamConn *websocket.Conn
	var resp *http.Response
	var err error
	if choice, attempt, ok := s.codexLive.affinity(target.callID); ok {
		noteRouteAccount(r.Context(), redactedAccountHint("codex", string(choice.AccountKey)), false)
		upstreamConn, resp, _, _, err = executeCodexWebSocketAttempt(
			executor, r.Context(), choice, attempt, upstreamURL, headers,
			func(actual CandidateAttempt) { s.codexLive.remember(target.callID, choice, actual) },
		)
	} else {
		upstreamConn, _, _, err = s.dialCodexWebSocket(r.Context(), upstreamURL, headers, "")
	}
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		statusCode = http.StatusBadGateway
		diagError = diagnosticsErrorCode("api_error", "codex live sideband upstream error")
		writeError(w, statusCode, "api_error", "codex live sideband upstream error")
		return
	}
	defer upstreamConn.Close()
	upstreamConn.SetReadLimit(maxRequestBody)

	var selectedSubprotocol []string
	if subprotocol := upstreamConn.Subprotocol(); subprotocol != "" {
		selectedSubprotocol = []string{subprotocol}
	}
	upgrader := websocket.Upgrader{
		CheckOrigin:  func(_ *http.Request) bool { return true },
		Subprotocols: selectedSubprotocol,
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	statusCode = http.StatusSwitchingProtocols
	defer clientConn.Close()
	clientConn.SetReadLimit(maxRequestBody)

	_ = relayWebSocketPair(r.Context(), clientConn, upstreamConn)
}

func codexLiveBackendBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to read codex live request")
	}
	if len(raw) > maxRequestBody {
		return nil, fmt.Errorf("codex live request exceeds 10 MiB")
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("invalid codex live content type")
	}
	var request codexLiveBackendRequest
	switch mediaType {
	case "multipart/form-data":
		boundary := params["boundary"]
		if boundary == "" {
			return nil, fmt.Errorf("codex live multipart boundary is missing")
		}
		reader := multipart.NewReader(bytes.NewReader(raw), boundary)
		for {
			part, partErr := reader.NextPart()
			if partErr == io.EOF {
				break
			}
			if partErr != nil {
				return nil, fmt.Errorf("invalid codex live multipart body")
			}
			value, readErr := io.ReadAll(part)
			part.Close()
			if readErr != nil {
				return nil, fmt.Errorf("invalid codex live multipart field")
			}
			switch part.FormName() {
			case "sdp":
				request.SDP = string(value)
			case "session":
				if !json.Valid(value) {
					return nil, fmt.Errorf("codex live session must be valid JSON")
				}
				request.Session = append(json.RawMessage(nil), value...)
			}
		}
	case "application/sdp":
		request.SDP = string(raw)
	case "application/json":
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("invalid codex live JSON body")
		}
	default:
		return nil, fmt.Errorf("unsupported codex live content type %q", mediaType)
	}
	if strings.TrimSpace(request.SDP) == "" {
		return nil, fmt.Errorf("codex live request is missing sdp")
	}
	return json.Marshal(request)
}

func readCodexLiveBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read codex live response")
	}
	if len(data) > maxRequestBody {
		return nil, fmt.Errorf("codex live response exceeds 10 MiB")
	}
	return data, nil
}

func copyCodexLiveProtocolHeaders(dst, src http.Header) {
	for _, name := range codexLiveProtocolHeaders {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

func copyCodexLiveResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Location"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

func codexLiveCallID(location string) string {
	if location == "" {
		return ""
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 {
		return ""
	}
	callID := segments[len(segments)-1]
	if !validCodexLiveCallID(callID) {
		return ""
	}
	return callID
}

func parseCodexLiveSidebandTarget(requestURL *url.URL) (codexLiveSidebandTarget, bool) {
	path := strings.TrimPrefix(requestURL.Path, "/v1")
	if strings.HasPrefix(path, "/live/") {
		callID := strings.TrimPrefix(path, "/live/")
		return codexLiveSidebandTarget{callID: callID, style: "live"}, validCodexLiveCallID(callID)
	}
	if strings.HasPrefix(path, "/realtime/calls/") {
		callID := strings.TrimPrefix(path, "/realtime/calls/")
		return codexLiveSidebandTarget{callID: callID, style: "realtime-calls"}, validCodexLiveCallID(callID)
	}
	if path == "/realtime" {
		callID := requestURL.Query().Get("call_id")
		return codexLiveSidebandTarget{callID: callID, style: "realtime-query"}, validCodexLiveCallID(callID)
	}
	return codexLiveSidebandTarget{}, false
}

func validCodexLiveCallID(callID string) bool {
	if len(callID) == 0 || len(callID) > 128 {
		return false
	}
	for _, char := range callID {
		if (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') &&
			char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func (s *Server) codexLiveSidebandURL(target codexLiveSidebandTarget) string {
	root := s.codexLiveSidebandUpstream
	if root == "" {
		root = codexLiveSidebandRoot
	}
	root = strings.TrimRight(root, "/")
	switch {
	case strings.HasPrefix(root, "https://"):
		root = "wss://" + strings.TrimPrefix(root, "https://")
	case strings.HasPrefix(root, "http://"):
		root = "ws://" + strings.TrimPrefix(root, "http://")
	}
	switch target.style {
	case "live":
		return root + "/live/" + url.PathEscape(target.callID)
	case "realtime-calls":
		return root + "/realtime/calls/" + url.PathEscape(target.callID)
	default:
		return root + "/realtime?intent=quicksilver&call_id=" + url.QueryEscape(target.callID)
	}
}

func (s *codexLiveState) remember(callID string, choice RouteChoice, attempt CandidateAttempt) {
	if choice.AccountKey == "" || attempt.Candidate.CandidateID == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = make(map[string]codexLiveCall)
	}
	for id, call := range s.calls {
		if call.expiresAt.Before(now) {
			delete(s.calls, id)
		}
	}
	s.calls[callID] = codexLiveCall{choice: choice, attempt: attempt, expiresAt: now.Add(codexLiveCallTTL)}
}

func (s *codexLiveState) affinity(callID string) (RouteChoice, CandidateAttempt, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	call, ok := s.calls[callID]
	if !ok || call.expiresAt.Before(now) {
		delete(s.calls, callID)
		return RouteChoice{}, CandidateAttempt{}, false
	}
	return call.choice, call.attempt, true
}
