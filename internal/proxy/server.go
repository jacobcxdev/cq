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
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const maxRequestBody = 10 << 20 // 10 MiB

const (
	codexResponsesPath              = "/v1/responses"
	legacyCodexResponsesPath        = "/responses"
	codexCompactResponsesPath       = "/v1/responses/compact"
	legacyCodexCompactResponsesPath = "/responses/compact"
	codexAppServerPath              = "/app-server"
)

// RegistryRefresher is the interface for triggering a registry refresh.
// Implementations must be safe for concurrent calls.
type RegistryRefresher interface {
	Refresh(context.Context) (modelregistry.RefreshDiagnostics, error)
}

// RegistryRefresherFunc is a function adapter for RegistryRefresher.
type RegistryRefresherFunc func(context.Context) (modelregistry.RefreshDiagnostics, error)

// Refresh implements RegistryRefresher.
func (f RegistryRefresherFunc) Refresh(ctx context.Context) (modelregistry.RefreshDiagnostics, error) {
	return f(ctx)
}

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
	Diag                  *DiagnosticsWriter
	PayloadDiag           *PayloadWriter
	// HeadroomMode is the resolved compression mode. Only meaningful when
	// Headroom is non-nil. Reported in the /health response.
	HeadroomMode HeadroomMode
	// Catalog is the optional model registry catalog. When non-nil, it backs
	// /v1/models projections, /v1/registry, and routing decisions.
	Catalog *modelregistry.Catalog
	// Refresher is the optional registry refresher. When non-nil, it backs
	// the /v1/registry/refresh endpoint.
	Refresher RegistryRefresher
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
	mux.HandleFunc("GET /models", s.handleCodexNativeModels)
	mux.HandleFunc("GET /v1/registry", s.handleRegistry)
	mux.HandleFunc("POST /v1/registry/refresh", s.handleRegistryRefresh)
	mux.HandleFunc(codexResponsesPath, s.handleCodexResponsesRoute)
	mux.HandleFunc(legacyCodexResponsesPath, s.handleLegacyCodexResponsesRoute)
	mux.HandleFunc("GET "+codexCompactResponsesPath, s.handleCodexCompactResponsesGetRoute)
	mux.HandleFunc("GET "+legacyCodexCompactResponsesPath, s.handleLegacyCodexCompactResponsesGetRoute)
	mux.HandleFunc("POST "+codexCompactResponsesPath, s.handleCodexCompactResponsesRoute)
	mux.HandleFunc("POST "+legacyCodexCompactResponsesPath, s.handleLegacyCodexCompactResponsesRoute)
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

	// /v1/models keeps runtime/API model metadata compatible with ANTHROPIC_BASE_URL.
	// Claude Code's interactive /model picker is populated separately from
	// additionalModelOptionsCache in ~/.claude.json; bootstrap/OAuth discovery does
	// not use ANTHROPIC_BASE_URL and custom OAuth hosts are allowlist-restricted.
	models := mergeModelMetadata(SyntheticModelCatalog(), registryCatalogModelMetadata(s.Catalog), s.fetchUpstreamModels(r))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": models,
	})
}

// handleCodexNativeModels serves GET /models?client_version=... in the native
// Codex ModelsResponse shape. This endpoint is used by Codex CLI to fetch the
// available model list. No proxy token is required — the proxy binds to 127.0.0.1
// only, so only local processes can reach this endpoint.
func (s *Server) handleCodexNativeModels(w http.ResponseWriter, r *http.Request) {
	if s.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "model registry not configured")
		return
	}
	snap := s.Catalog.Snapshot()
	resp := modelregistry.CodexModelsResponse(snap)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleRegistry serves GET /v1/registry and returns the current registry
// snapshot as JSON. Requires a valid local proxy token.
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	if !s.isValidToken(bearerToken(r)) {
		writeError(w, http.StatusForbidden, "authentication_error", "invalid proxy token")
		return
	}
	if s.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "model registry not configured")
		return
	}
	snap := s.Catalog.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// handleRegistryRefresh serves POST /v1/registry/refresh. It calls the
// injected Refresher and returns the result. Requires a valid local proxy token.
// Concurrent refresh requests are safe — the Refresher is responsible for
// serialisation (Refresher.Refresh acquires its own mutex).
func (s *Server) handleRegistryRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.isValidToken(bearerToken(r)) {
		writeError(w, http.StatusForbidden, "authentication_error", "invalid proxy token")
		return
	}
	if s.Refresher == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "model registry refresher not configured")
		return
	}
	diag, err := s.Refresher.Refresh(r.Context())
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: registry refresh: %v\n", err)
		writeError(w, http.StatusInternalServerError, "api_error", "registry refresh failed")
		return
	}
	resp := map[string]any{
		"ok":     true,
		"counts": diag.Counts,
	}
	if se := refreshSourceErrors(diag); se != nil {
		resp["source_errors"] = se
	}
	if mc := refreshMalformedCounts(diag); mc != nil {
		resp["malformed"] = mc
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// refreshSourceErrors converts the SourceErrors map in RefreshDiagnostics to a
// map[string]string suitable for JSON serialisation. Returns nil when there are
// no errors so the field is omitted from the response.
func refreshSourceErrors(diag modelregistry.RefreshDiagnostics) map[string]string {
	var out map[string]string
	for provider, err := range diag.SourceErrors {
		if err == nil {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[string(provider)] = err.Error()
	}
	return out
}

// refreshMalformedCounts converts the MalformedCounts map in RefreshDiagnostics
// to a map[string]int suitable for JSON serialisation. Returns nil when there
// are no malformed entries so the field is omitted from the response.
func refreshMalformedCounts(diag modelregistry.RefreshDiagnostics) map[string]int {
	var out map[string]int
	for provider, count := range diag.MalformedCounts {
		if count <= 0 {
			continue
		}
		if out == nil {
			out = make(map[string]int)
		}
		out[string(provider)] = count
	}
	return out
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
		start := time.Now()
		message := fmt.Sprintf("%s requires websocket upgrade", codexAppServerPath)
		w.Header().Set("Upgrade", "websocket")
		writeError(w, http.StatusUpgradeRequired, "invalid_request_error", message)
		event := RouteEvent{
			Time:       start.UTC(),
			Method:     r.Method,
			Path:       r.URL.Path,
			Provider:   "codex",
			RouteKind:  "codex_app_server",
			StatusCode: http.StatusUpgradeRequired,
			LatencyMS:  time.Since(start).Milliseconds(),
			Error:      diagnosticsErrorCode("invalid_request_error", message),
		}
		event.applySessionCorrelation(r.Header)
		s.emitDiagnostics(event)
		return
	}
	s.proxyCodexAppServer(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if wrapped, rec := s.wrapDiagnosticsResponseWriter(w); rec != nil {
		w = wrapped
		defer func() {
			s.emitDiagnostics(RouteEvent{
				Time:       start.UTC(),
				Method:     r.Method,
				Path:       r.URL.Path,
				Provider:   "proxy",
				RouteKind:  "health",
				StatusCode: rec.statusCode(),
				LatencyMS:  time.Since(start).Milliseconds(),
			})
		}()
	}

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
		"diagnostics": map[string]bool{
			"enabled": s.Diag != nil,
			"payload": s.PayloadDiag != nil,
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
	start := time.Now()
	var model string
	ctx, routeDiag := withRouteDiagnostics(r.Context())
	if wrapped, rec := s.wrapDiagnosticsResponseWriter(w); rec != nil {
		w = wrapped
		defer func() {
			event := RouteEvent{
				Time:       start.UTC(),
				Method:     r.Method,
				Path:       r.URL.Path,
				Provider:   "codex",
				RouteKind:  "codex_native",
				Model:      model,
				StatusCode: rec.statusCode(),
				LatencyMS:  time.Since(start).Milliseconds(),
				Error:      rec.diagnosticsError(),
			}
			event.applyRouteDiagnostics(routeDiag)
			event.applySessionCorrelation(r.Header)
			s.emitDiagnostics(event)
		}()
	}

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

	model = extractModel(body)
	fmt.Fprintf(os.Stderr, "cq: route POST /responses model=%q provider=codex (native)\n", model)

	// Emit payload diagnostics before any body rewrite.
	if s.PayloadDiag != nil {
		sessionKey, sessionSource := payloadSessionCorrelation(r.Header, body)
		s.emitPayloadDiagnostics(PayloadEvent{
			Time:          time.Now().UTC(),
			Method:        r.Method,
			Path:          r.URL.Path,
			Provider:      "codex",
			RouteKind:     "codex_native",
			Model:         model,
			ClientKind:    clientKindFromUserAgent(r.Header.Get("User-Agent")),
			SessionKey:    sessionKey,
			SessionSource: sessionSource,
			BodyBytes:     len(body),
			Body:          encodeBody(body),
		})
	}

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
	upReq, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
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
			RouteKind:  "codex_legacy_websocket",
			StatusCode: statusCode,
			LatencyMS:  time.Since(start).Milliseconds(),
			Error:      diagError,
		}
		event.applyRouteDiagnostics(routeDiag)
		event.applySessionCorrelation(r.Header)
		s.emitDiagnostics(event)
	}()

	transport, err := s.codexAppServerTransport()
	if err != nil {
		statusCode = http.StatusServiceUnavailable
		diagError = diagnosticsErrorCode("api_error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "api_error", err.Error())
		return
	}
	upstreamURL, err := codexAppServerWebSocketURL(s.Config.CodexUpstream)
	if err != nil {
		statusCode = http.StatusInternalServerError
		diagError = diagnosticsErrorCode("api_error", "invalid codex upstream URL")
		writeError(w, http.StatusInternalServerError, "api_error", "invalid codex upstream URL")
		return
	}

	fmt.Fprintf(os.Stderr, "cq: route %s /responses (websocket upgrade) provider=codex (native)\n", r.Method)

	upgrader := websocket.Upgrader{
		CheckOrigin:  func(_ *http.Request) bool { return true },
		Subprotocols: websocket.Subprotocols(r),
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	statusCode = http.StatusSwitchingProtocols
	defer clientConn.Close()
	clientConn.SetReadLimit(maxRequestBody)

	messageType, message, err := clientConn.ReadMessage()
	if err != nil {
		return
	}
	requestedModel := ""
	if messageType == websocket.TextMessage {
		requestedModel = extractCodexWebSocketFrameModel(message)
		s.emitCodexWebSocketPayloadDiagnostics(r, legacyCodexResponsesPath, requestedModel, message, 1)
	}
	upstreamConn, _, err := s.dialCodexAppServer(r.Context(), transport, upstreamURL, r.Header, requestedModel)
	if err != nil {
		_ = clientConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream error"), time.Now().Add(time.Second))
		return
	}
	defer upstreamConn.Close()
	upstreamConn.SetReadLimit(maxRequestBody)
	if err := upstreamConn.WriteMessage(messageType, message); err != nil {
		return
	}
	errCh := make(chan error, 2)
	go func() { errCh <- relayWebSocketMessages(clientConn, upstreamConn) }()
	go func() { errCh <- relayWebSocketMessages(upstreamConn, clientConn) }()
	<-errCh
}

// proxyCodexAppServer handles the Codex /app-server websocket path. Unlike the
// legacy /responses websocket proxy, the app-server chooses the model in the
// initial JSON-RPC thread/start frame after the upgrade, so the proxy must
// inspect that frame before selecting an account and opening the upstream
// websocket.
func (s *Server) proxyCodexAppServer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	statusCode := 0
	requestedModel := ""
	diagError := ""
	ctx, routeDiag := withRouteDiagnostics(r.Context())
	r = r.WithContext(ctx)
	defer func() {
		event := RouteEvent{
			Time:       start.UTC(),
			Method:     r.Method,
			Path:       r.URL.Path,
			Provider:   "codex",
			RouteKind:  "codex_app_server",
			Model:      requestedModel,
			StatusCode: statusCode,
			LatencyMS:  time.Since(start).Milliseconds(),
			Error:      diagError,
		}
		event.applyRouteDiagnostics(routeDiag)
		event.applySessionCorrelation(r.Header)
		s.emitDiagnostics(event)
	}()

	transport, err := s.codexAppServerTransport()
	if err != nil {
		statusCode = http.StatusServiceUnavailable
		diagError = diagnosticsErrorCode("api_error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "api_error", err.Error())
		return
	}
	upstreamURL, err := codexAppServerWebSocketURL(s.Config.CodexUpstream)
	if err != nil {
		statusCode = http.StatusInternalServerError
		message := "invalid codex upstream URL"
		diagError = diagnosticsErrorCode("api_error", message)
		writeError(w, http.StatusInternalServerError, "api_error", message)
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
	statusCode = http.StatusSwitchingProtocols
	defer clientConn.Close()
	clientConn.SetReadLimit(maxRequestBody)

	messageType, message, err := clientConn.ReadMessage()
	if err != nil {
		return
	}
	if messageType == websocket.TextMessage {
		requestedModel = extractCodexAppServerThreadStartModel(message)
		s.emitCodexWebSocketPayloadDiagnostics(r, codexAppServerPath, requestedModel, message, 1)
	}

	fmt.Fprintf(os.Stderr, "cq: route %s %s model=%q provider=codex (native)\n", r.Method, codexAppServerPath, requestedModel)

	upstreamConn, acct, err := s.dialCodexAppServer(r.Context(), transport, upstreamURL, r.Header, requestedModel)
	if err != nil {
		diagError = diagnosticsErrorCode("api_error", "codex upstream error: "+err.Error())
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
		noteRouteAccount(ctx, codexAccountHint(acct), len(excluded) > 0)
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

func extractCodexWebSocketFrameModel(message []byte) string {
	var payload struct {
		Model  string `json:"model"`
		Params struct {
			Model string `json:"model"`
		} `json:"params"`
	}
	if json.Unmarshal(message, &payload) != nil {
		return ""
	}
	if payload.Model != "" {
		return payload.Model
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
		start := time.Now()
		var routeModel string
		var routeProvider Provider
		var buf []byte
		ctx, routeDiag := withRouteDiagnostics(r.Context())
		r = r.WithContext(ctx)
		if diagnosticsAnthropicRouteKind(r.URL.Path) != "" {
			if wrapped, rec := s.wrapDiagnosticsResponseWriter(w); rec != nil {
				w = wrapped
				defer func() {
					provider := providerName(routeProvider)
					event := RouteEvent{
						Time:       start.UTC(),
						Method:     r.Method,
						Path:       r.URL.Path,
						Provider:   provider,
						RouteKind:  diagnosticsAnthropicRouteKind(r.URL.Path),
						Model:      routeModel,
						PinActive:  provider == "claude" && s.claudePinActive(),
						StatusCode: rec.statusCode(),
						LatencyMS:  time.Since(start).Milliseconds(),
						Error:      rec.diagnosticsError(),
					}
					event.applyRouteDiagnostics(routeDiag)
					event.applySessionCorrelation(r.Header)
					s.emitDiagnostics(event)
				}()
			}
		}

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

		// Emit payload diagnostics before any body rewrite, while buf still holds
		// the original request body. Only emitted for buffered Anthropic endpoints.
		if diagnosticsAnthropicRouteKind(r.URL.Path) != "" && s.PayloadDiag != nil {
			sessionKey, sessionSource := payloadSessionCorrelation(r.Header, buf)
			routeProvider = RouteRequestWithCatalog(r.Method, r.URL.Path, routeModel, s.Catalog)
			s.emitPayloadDiagnostics(PayloadEvent{
				Time:          start.UTC(),
				Method:        r.Method,
				Path:          r.URL.Path,
				Provider:      providerName(routeProvider),
				RouteKind:     diagnosticsAnthropicRouteKind(r.URL.Path),
				Model:         routeModel,
				ClientKind:    clientKindFromUserAgent(r.Header.Get("User-Agent")),
				SessionKey:    sessionKey,
				SessionSource: sessionSource,
				BodyBytes:     len(buf),
				Body:          encodeBody(buf),
			})
		}

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

		routeProvider = RouteRequestWithCatalog(r.Method, r.URL.Path, routeModel, s.Catalog)
		fmt.Fprintf(os.Stderr, "cq: route %s %s model=%q provider=%s\n",
			r.Method, r.URL.Path, routeModel, providerName(routeProvider))
		if routeProvider == ProviderCodex {
			s.handleCodex(w, r, buf)
			return
		}

		rp.ServeHTTP(w, r)
	}
}

type diagnosticsResponseWriter struct {
	http.ResponseWriter
	status          int
	diagnosticError string
}

func (w *diagnosticsResponseWriter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *diagnosticsResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *diagnosticsResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *diagnosticsResponseWriter) SetDiagnosticsError(err string) {
	w.diagnosticError = err
}

func (w *diagnosticsResponseWriter) diagnosticsError() string {
	return w.diagnosticError
}

func (w *diagnosticsResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type diagnosticsFlushWriter struct {
	*diagnosticsResponseWriter
}

func (w diagnosticsFlushWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) wrapDiagnosticsResponseWriter(w http.ResponseWriter) (http.ResponseWriter, *diagnosticsResponseWriter) {
	if s == nil || s.Diag == nil {
		return w, nil
	}
	rec := &diagnosticsResponseWriter{ResponseWriter: w}
	if _, ok := w.(http.Flusher); ok {
		return diagnosticsFlushWriter{diagnosticsResponseWriter: rec}, rec
	}
	return rec, rec
}

func (s *Server) emitDiagnostics(event RouteEvent) {
	if s == nil || s.Diag == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if err := s.Diag.Write(event); err != nil {
		fmt.Fprintf(os.Stderr, "cq: diagnostics: write: %v\n", err)
	}
}

func (s *Server) emitPayloadDiagnostics(event PayloadEvent) {
	if s == nil || s.PayloadDiag == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if err := s.PayloadDiag.Write(event); err != nil {
		fmt.Fprintf(os.Stderr, "cq: payload diagnostics: write: %v\n", err)
	}
}

func (s *Server) emitCodexWebSocketPayloadDiagnostics(r *http.Request, path, model string, frame []byte, frameIndex int) {
	if s == nil || s.PayloadDiag == nil || r == nil {
		return
	}
	sessionKey, sessionSource, signal := codexWebSocketFrameCorrelation(r.Header, frame)
	s.emitPayloadDiagnostics(PayloadEvent{
		Time:          time.Now().UTC(),
		Method:        r.Method,
		Path:          path,
		Provider:      "codex",
		RouteKind:     "codex_websocket_frame",
		Model:         model,
		ClientKind:    clientKindFromUserAgent(r.Header.Get("User-Agent")),
		SessionKey:    sessionKey,
		SessionSource: sessionSource,
		SessionSignal: signal,
		FrameIndex:    frameIndex,
		BodyBytes:     len(frame),
		Body:          encodeBody(frame),
	})
}

func (s *Server) claudePinActive() bool {
	if s == nil {
		return false
	}
	if selector, ok := s.Selector.(interface{ Pin() string }); ok {
		return selector.Pin() != ""
	}
	return s.Config != nil && s.Config.PinnedClaudeAccount != ""
}

func diagnosticsAnthropicRouteKind(path string) string {
	switch path {
	case "/v1/messages":
		return "anthropic_messages"
	case countTokensPath:
		return "anthropic_count_tokens"
	default:
		return ""
	}
}

// clientKindFromUserAgent classifies the client type from a User-Agent string.
// Returns a short lowercase label suitable for diagnostics.
func clientKindFromUserAgent(ua string) string {
	lower := strings.ToLower(ua)
	switch {
	case strings.Contains(lower, "claude-code"):
		return "claude-code"
	case strings.Contains(lower, "codex"):
		return "codex"
	case strings.Contains(lower, "anthropic"):
		return "anthropic-sdk"
	case ua == "":
		return ""
	default:
		return "other"
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
	if rec, ok := w.(interface{ SetDiagnosticsError(string) }); ok {
		rec.SetDiagnosticsError(diagnosticsErrorCode(errType, message))
	}
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

func diagnosticsErrorCode(errType, message string) string {
	msg := strings.ToLower(message)
	switch {
	case strings.Contains(msg, "invalid proxy token"):
		return errType + ":invalid_proxy_token"
	case strings.Contains(msg, "no codex accounts configured") ||
		strings.Contains(msg, "no codex accounts available") ||
		strings.Contains(msg, "no codex accounts with valid tokens and quota"):
		return errType + ":no_codex_accounts"
	case strings.Contains(msg, "websocket transport is not supported"):
		return errType + ":unsupported_websocket_transport"
	case strings.Contains(msg, "requires websocket upgrade"):
		return errType + ":websocket_upgrade_required"
	case strings.Contains(msg, "only supports"):
		return errType + ":method_not_allowed"
	case strings.Contains(msg, "invalid codex upstream url") ||
		strings.Contains(msg, "unsupported codex upstream scheme"):
		return errType + ":invalid_codex_upstream"
	case strings.Contains(msg, "create upstream request"):
		return errType + ":invalid_upstream"
	case strings.Contains(msg, "codex upstream error") ||
		strings.Contains(msg, "codex upstream:") ||
		strings.Contains(msg, "codex websocket upgrade failed"):
		return errType + ":codex_upstream_error"
	case strings.Contains(msg, "request translation failed"):
		return errType + ":request_translation_failed"
	case strings.Contains(msg, "failed to read request body"):
		return errType + ":read_request_body"
	case strings.Contains(msg, "request body exceeds"):
		return errType + ":request_body_too_large"
	case strings.Contains(msg, "not a codex model"):
		return errType + ":invalid_route_model"
	case strings.Contains(msg, "stream collection failed"):
		return errType + ":stream_collection_failed"
	case strings.Contains(msg, "response assembly failed"):
		return errType + ":response_assembly_failed"
	case strings.Contains(msg, "decode count_tokens response"):
		return errType + ":decode_count_tokens_response"
	case strings.Contains(msg, "model registry refresher not configured"):
		return errType + ":model_registry_refresher_not_configured"
	case strings.Contains(msg, "model registry not configured"):
		return errType + ":model_registry_not_configured"
	case strings.Contains(msg, "registry refresh failed"):
		return errType + ":registry_refresh_failed"
	case errType == "api_error":
		return errType + ":upstream_error"
	default:
		return errType
	}
}
