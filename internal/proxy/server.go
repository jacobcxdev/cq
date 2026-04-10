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
	s.proxyCodexUpgrade(w, r)
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"headroom": s.Headroom != nil,
		"accounts": map[string]int{
			"claude": claudeCount,
			"codex":  codexCount,
		},
	})
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
		if s.Headroom != nil && len(buf) > 0 {
			if compressed, saved, err := s.Headroom.Compress(buf); err != nil {
				fmt.Fprintf(os.Stderr, "cq: headroom: %v\n", err)
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
