package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	internalhttputil "github.com/jacobcxdev/cq/internal/httputil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const maxRequestBody = 10 << 20 // 10 MiB

const (
	codexResponsesPath       = "/v1/responses"
	legacyCodexResponsesPath = "/responses"
	codexAppServerPath       = "/app-server"
)

// Server is the reverse proxy HTTP server.
type Server struct {
	Config                *Config
	Selector              ClaudeSelector
	Discover              ClaudeDiscoverer
	Transport             http.RoundTripper
	CodexDiscover         CodexDiscoverer
	CodexTransport        http.RoundTripper
	CodexUpgradeTransport http.RoundTripper // HTTP/1.1-only transport for WebSocket upgrades
	Headroom              *HeadroomBridge
	// HeadroomMode is the resolved compression mode. Only meaningful when
	// Headroom is non-nil. Reported in the /health response.
	HeadroomMode HeadroomMode
}

// ListenAndServe starts the proxy and blocks until the context is cancelled or a signal is received.
func (s *Server) ListenAndServe(ctx context.Context) error {
	handler, err := s.handler()
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("127.0.0.1:%d", s.Config.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(os.Stderr, "cq: proxy listening on %s\n", addr)

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handler() (http.Handler, error) {
	upstream, err := url.Parse(s.Config.ClaudeUpstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc(codexResponsesPath, s.handleCodexResponsesRoute)
	mux.HandleFunc(legacyCodexResponsesPath, s.handleLegacyCodexResponsesRoute)
	mux.HandleFunc(codexAppServerPath, s.handleCodexAppServerRoute)
	mux.HandleFunc("/", s.proxyHandler(upstream))
	return mux, nil
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "/v1/models only supports GET")
		return
	}
	if !s.isValidToken(bearerToken(r)) {
		writeError(w, http.StatusForbidden, "authentication_error", "invalid proxy token")
		return
	}

	// Claude Code only refreshes live capabilities against first-party hosts, so
	// this endpoint is necessary Anthropic-compatible groundwork but not sufficient
	// on its own when ANTHROPIC_BASE_URL points at this proxy. We also write the
	// local model-capabilities cache on proxy startup.
	models := mergeModelMetadata(s.fetchUpstreamModels(r), SyntheticModelCatalog())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": models,
	})
}

func (s *Server) fetchUpstreamModels(r *http.Request) []ModelMetadata {
	if s.Config == nil || s.Config.ClaudeUpstream == "" {
		return nil
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.Config.ClaudeUpstream+"/v1/models", nil)
	if err != nil {
		return nil
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		upReq.Header.Set("Authorization", auth)
	}
	transport := s.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err := transport.RoundTrip(upReq)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var payload struct {
		Data []ModelMetadata `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	return payload.Data
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (s *Server) handleCodexResponsesRoute(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("websocket transport is not supported on %s; use %s", codexResponsesPath, codexAppServerPath))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", fmt.Sprintf("%s only supports POST", codexResponsesPath))
		return
	}
	s.handleNativeCodex(w, r)
}

func (s *Server) handleLegacyCodexResponsesRoute(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		s.proxyCodexUpgrade(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", fmt.Sprintf("%s only supports POST or websocket upgrade", legacyCodexResponsesPath))
		return
	}
	s.handleNativeCodex(w, r)
}

func (s *Server) handleCodexAppServerRoute(w http.ResponseWriter, r *http.Request) {
	if !isWebSocketUpgrade(r) {
		w.Header().Set("Upgrade", "websocket")
		writeError(w, http.StatusUpgradeRequired, "invalid_request_error", fmt.Sprintf("%s requires websocket upgrade", codexAppServerPath))
		return
	}
	s.proxyCodexAppServer(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	var claudeCount int
	if s.Discover != nil {
		claudeCount = len(s.Discover())
	}
	var codexCount int
	if s.CodexDiscover != nil {
		codexCount = len(s.CodexDiscover())
	}
	resp := map[string]any{
		"status":   "ok",
		"headroom": s.Headroom != nil,
		"accounts": map[string]int{
			"claude": claudeCount,
			"codex":  codexCount,
		},
	}
	if s.Headroom != nil {
		switch s.HeadroomMode {
		case HeadroomModeCache:
			resp["headroom_mode"] = "cache"
		default:
			resp["headroom_mode"] = "token"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// isValidToken returns true if token matches the local proxy token or the
// access token of any known Claude account. This allows Claude Code to
// authenticate with its own OAuth token (preserving subscriber detection)
// instead of requiring ANTHROPIC_API_KEY which disables OAuth features.
func (s *Server) isValidToken(token string) bool {
	if token == s.Config.LocalToken {
		return true
	}
	if s.Discover == nil {
		return false
	}
	for _, acct := range s.Discover() {
		if acct.AccessToken != "" && acct.AccessToken == token {
			return true
		}
	}
	return false
}

// handleNativeCodex handles requests from Codex CLI in native OpenAI Responses
// API format. No Anthropic↔OpenAI translation is performed — the request is
// forwarded as-is with auth injected by CodexTransport.
//
// Security: no proxy token auth is required. The proxy binds to 127.0.0.1 only,
// so only local processes can reach this endpoint. Codex CLI in ChatGPT auth
// mode doesn't support custom API keys, so we can't require the proxy token.
func (s *Server) handleNativeCodex(w http.ResponseWriter, r *http.Request) {
	if s.CodexTransport == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "no codex accounts configured")
		return
	}

	// Buffer request body.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	if len(body) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds 10 MiB")
		return
	}

	model := extractModel(body)
	fmt.Fprintf(os.Stderr, "cq: route POST /responses model=%q provider=codex (native)\n", model)

	// Compress Responses API input via headroom bridge if available.
	// Fail-open: on error, log and continue with original body.
	if s.Headroom != nil {
		var compressed []byte
		var saved int
		var err error
		if s.HeadroomMode == HeadroomModeCache {
			compressed, saved, err = s.Headroom.CompressResponsesCache(body)
		} else {
			compressed, saved, err = s.Headroom.CompressResponses(body, HeadroomModeToken)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: headroom: %v\n", err)
		} else if saved > 0 {
			fmt.Fprintf(os.Stderr, "cq: headroom saved %d tokens\n", saved)
			body = compressed
		}
	}

	// Build upstream request — forward as-is, no translation.
	upstreamURL := s.Config.CodexUpstream + "/responses"
	upReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return
	}
	upReq.ContentLength = int64(len(body))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	// Forward all original headers — the transport will override auth headers.
	// This preserves Codex CLI-specific headers the ChatGPT backend may require.
	for key, vals := range r.Header {
		for _, v := range vals {
			upReq.Header.Add(key, v)
		}
	}
	if upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}

	// Transport handles auth injection and account rotation.
	resp, err := s.CodexTransport.RoundTrip(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "cq: proxy POST %s → %d (codex native)\n", upstreamURL, resp.StatusCode)

	// Forward response as-is — headers, status code, body.
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the body through (supports SSE).
	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				f.Flush()
			}
			if readErr != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}

// proxyCodexUpgrade handles WebSocket upgrade requests to /responses by
// reverse-proxying to the Codex upstream. The CodexTokenTransport injects
// auth on the initial HTTP upgrade request; after the upgrade the raw TCP
// connection is relayed without further intervention.
//
// Note: native Codex WebSocket traffic is intentionally out of scope for
// headroom compression — the handshake body is minimal and the subsequent
// binary/text frames are not buffered by this proxy.
func (s *Server) proxyCodexUpgrade(w http.ResponseWriter, r *http.Request) {
	codexUpstream, err := url.Parse(s.Config.CodexUpstream)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "invalid codex upstream URL")
		return
	}

	fmt.Fprintf(os.Stderr, "cq: route %s /responses (websocket upgrade) provider=codex (native)\n", r.Method)

	transport := s.CodexUpgradeTransport
	if transport == nil {
		transport = s.CodexTransport
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(codexUpstream)
			pr.Out.URL.Path = codexUpstream.Path + "/responses"
			pr.Out.Host = codexUpstream.Host
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "api_error", "codex upstream error: "+err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

// proxyCodexAppServer handles the Codex /app-server websocket path. Unlike the
// legacy /responses websocket proxy, the app-server chooses the model in the
// initial JSON-RPC thread/start frame after the upgrade, so the proxy must
// inspect that frame before selecting an account and opening the upstream
// websocket.
func (s *Server) proxyCodexAppServer(w http.ResponseWriter, r *http.Request) {
	transport, err := s.codexAppServerTransport()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", err.Error())
		return
	}
	upstreamURL, err := codexAppServerWebSocketURL(s.Config.CodexUpstream)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "invalid codex upstream URL")
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin:  func(_ *http.Request) bool { return true },
		Subprotocols: websocket.Subprotocols(r),
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()
	clientConn.SetReadLimit(maxRequestBody)

	messageType, message, err := clientConn.ReadMessage()
	if err != nil {
		return
	}
	requestedModel := ""
	if messageType == websocket.TextMessage {
		requestedModel = extractCodexAppServerThreadStartModel(message)
	}

	fmt.Fprintf(os.Stderr, "cq: route %s %s model=%q provider=codex (native)\n", r.Method, codexAppServerPath, requestedModel)

	upstreamConn, acct, err := s.dialCodexAppServer(r.Context(), transport, upstreamURL, r.Header, requestedModel)
	if err != nil {
		_ = clientConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream error"), time.Now().Add(time.Second))
		return
	}
	defer upstreamConn.Close()
	upstreamConn.SetReadLimit(maxRequestBody)

	if messageType == websocket.TextMessage {
		message = rewriteCodexAppServerThreadStartMessage(message, acct)
	}
	if err := upstreamConn.WriteMessage(messageType, message); err != nil {
		return
	}

	errCh := make(chan error, 2)
	go func() { errCh <- relayWebSocketMessages(clientConn, upstreamConn) }()
	go func() { errCh <- relayWebSocketMessages(upstreamConn, clientConn) }()
	<-errCh
}

func (s *Server) codexAppServerTransport() (*CodexTokenTransport, error) {
	if t, ok := s.CodexUpgradeTransport.(*CodexTokenTransport); ok && t != nil && t.Selector != nil {
		return t, nil
	}
	if t, ok := s.CodexTransport.(*CodexTokenTransport); ok && t != nil && t.Selector != nil {
		return t, nil
	}
	return nil, fmt.Errorf("no codex accounts configured")
}

func codexAppServerWebSocketURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported codex upstream scheme %q", u.Scheme)
	}
	u.Path += "/responses"
	return u.String(), nil
}

func (s *Server) dialCodexAppServer(ctx context.Context, transport *CodexTokenTransport, upstreamURL string, incomingHeaders http.Header, requestedModel string) (*websocket.Conn, *codex.CodexAccount, error) {
	if requestedModel != "" {
		ctx = context.WithValue(ctx, codexModelContextKey{}, requestedModel)
	}
	var excluded []string
	persistSwitch := false
	for {
		acct, err := transport.Selector.Select(ctx, excluded...)
		if err != nil {
			if len(excluded) == 0 {
				return nil, nil, err
			}
			return nil, nil, fmt.Errorf("no alternate codex account available for app-server websocket")
		}
		conn, resp, body, err := dialCodexAppServerWithAccount(ctx, upstreamURL, incomingHeaders, acct)
		if err == nil {
			if persistSwitch {
				transport.persistSwitch(acct)
			}
			return conn, acct, nil
		}
		if resp == nil {
			return nil, nil, err
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			persistSwitch = true
		case http.StatusTooManyRequests:
			if isHardExhaustion(body) || transport.isSnapshotExhausted(acct) {
				persistSwitch = true
			}
		default:
			return nil, nil, fmt.Errorf("codex websocket upgrade failed: %s", resp.Status)
		}
		excluded = append(excluded, codexAcctExcludeKeys(acct)...)
	}
}

func dialCodexAppServerWithAccount(ctx context.Context, upstreamURL string, incomingHeaders http.Header, acct *codex.CodexAccount) (*websocket.Conn, *http.Response, []byte, error) {
	headers := cloneCodexAppServerHeaders(incomingHeaders)
	headers.Set("Authorization", "Bearer "+acct.AccessToken)
	headers.Del("x-api-key")
	if acct.AccountID != "" {
		headers.Set("ChatGPT-Account-ID", acct.AccountID)
	}
	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  30 * time.Second,
		EnableCompression: true,
	}
	conn, resp, err := dialer.DialContext(ctx, upstreamURL, headers)
	if err == nil {
		return conn, resp, nil, nil
	}
	var body []byte
	if resp != nil && resp.Body != nil {
		body, _ = internalhttputil.ReadBody(resp.Body)
		resp.Body.Close()
	}
	return nil, resp, body, err
}

func cloneCodexAppServerHeaders(incoming http.Header) http.Header {
	out := http.Header{}
	for key, values := range incoming {
		switch http.CanonicalHeaderKey(key) {
		case "Authorization", "Connection", "Content-Length", "Upgrade", "X-Api-Key", "Sec-Websocket-Extensions", "Sec-Websocket-Key", "Sec-Websocket-Version":
			continue
		}
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

func extractCodexAppServerThreadStartModel(message []byte) string {
	var payload struct {
		Method string `json:"method"`
		Params struct {
			Model string `json:"model"`
		} `json:"params"`
	}
	if json.Unmarshal(message, &payload) != nil {
		return ""
	}
	if payload.Method != "thread/start" {
		return ""
	}
	return payload.Params.Model
}

func rewriteCodexAppServerThreadStartMessage(message []byte, acct *codex.CodexAccount) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(message, &payload) != nil {
		return message
	}
	var method string
	if json.Unmarshal(payload["method"], &method) != nil || method != "thread/start" {
		return message
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(payload["params"], &params) != nil {
		return message
	}
	var model string
	if json.Unmarshal(params["model"], &model) != nil || model == "" {
		return message
	}
	if acct != nil && codexPlanSupportsModel(acct.PlanType, model) {
		return message
	}
	rewrittenModel, ok := rewriteCodexModelName(model)
	if !ok {
		return message
	}
	rawModel, err := json.Marshal(rewrittenModel)
	if err != nil {
		return message
	}
	params["model"] = rawModel
	rawParams, err := json.Marshal(params)
	if err != nil {
		return message
	}
	payload["params"] = rawParams
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return message
	}
	return rewritten
}

func relayWebSocketMessages(src, dst *websocket.Conn) error {
	for {
		messageType, message, err := src.ReadMessage()
		if err != nil {
			return err
		}
		if err := dst.WriteMessage(messageType, message); err != nil {
			return err
		}
	}
}

func (s *Server) proxyHandler(upstream *url.URL) http.HandlerFunc {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("x-api-key")
		},
		Transport:     s.Transport,
		FlushInterval: -1, // flush immediately for SSE streaming
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "api_error", err.Error())
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.Request != nil {
				fmt.Fprintf(os.Stderr, "cq: proxy %s %s → %d\n",
					resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			}
			return nil
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var routeModel string
		var routeProvider Provider
		var buf []byte

		// Auth check: accept local proxy token or a known Claude account token.
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.isValidToken(token) {
			writeError(w, http.StatusForbidden, "authentication_error", "invalid proxy token")
			return
		}

		// Buffer body for replay via GetBody on 401/429 retries.
		if r.Body != nil {
			var err error
			buf, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
			r.Body.Close()
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
				return
			}
			if len(buf) > maxRequestBody {
				writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds 10 MiB")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(buf))
			r.ContentLength = int64(len(buf))
			r.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(buf)), nil
			}
		}

		// Route based on the original endpoint and model before any body rewriting.
		routeModel = extractModel(buf)

		// Compress messages via headroom bridge if available.
		// Dispatch to the correct path based on the resolved headroom mode.
		if s.Headroom != nil && len(buf) > 0 {
			var compressed []byte
			var saved int
			var compErr error
			if s.HeadroomMode == HeadroomModeCache {
				compressed, saved, compErr = s.Headroom.CompressCache(buf)
			} else {
				compressed, saved, compErr = s.Headroom.Compress(buf)
			}
			if compErr != nil {
				fmt.Fprintf(os.Stderr, "cq: headroom: %v\n", compErr)
			} else if saved > 0 {
				fmt.Fprintf(os.Stderr, "cq: headroom saved %d tokens\n", saved)
				buf = compressed
				r.Body = io.NopCloser(bytes.NewReader(buf))
				r.ContentLength = int64(len(buf))
				r.GetBody = func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(buf)), nil
				}
			}
		}

		routeProvider = RouteRequest(r.Method, r.URL.Path, routeModel)
		fmt.Fprintf(os.Stderr, "cq: route %s %s model=%q provider=%s\n",
			r.Method, r.URL.Path, routeModel, providerName(routeProvider))
		if routeProvider == ProviderCodex {
			fmt.Fprintf(os.Stderr, "cq: codex debug marker=before_handleCodex method=%s path=%s model=%q\n", r.Method, r.URL.Path, routeModel)
			s.handleCodex(w, r, buf)
			return
		}

		rp.ServeHTTP(w, r)
	}
}

func providerName(provider Provider) string {
	switch provider {
	case ProviderCodex:
		return "codex"
	default:
		return "claude"
	}
}

func debugMessagePreview(body []byte) string {
	var partial struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &partial) != nil || len(partial.Messages) == 0 {
		return ""
	}
	for _, msg := range partial.Messages {
		if msg.Role != "user" {
			continue
		}
		if text := debugContentPreview(msg.Content); text != "" {
			return text
		}
	}
	return ""
}

func debugContentPreview(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return truncateDebugText(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			return truncateDebugText(block.Text)
		}
	}
	return ""
}

func truncateDebugText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	const maxLen = 120
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "…"
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
