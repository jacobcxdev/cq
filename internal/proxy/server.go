package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const maxRequestBody = 10 << 20 // 10 MiB

const defaultServerShutdownGracePeriod = 5 * time.Second

const (
	codexResponsesPath              = "/v1/responses"
	legacyCodexResponsesPath        = "/responses"
	codexCompactResponsesPath       = "/v1/responses/compact"
	legacyCodexCompactResponsesPath = "/responses/compact"
	codexAppServerPath              = "/app-server"
	codexImagesPathPrefix           = "/v1/images/"
	legacyCodexImagesPathPrefix     = "/images/"
	codexSearchPath                 = "/alpha/search"
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

type CodexSourceHealth struct {
	Name           string `json:"name"`
	CandidateCount int    `json:"candidate_count"`
	HealthCode     string `json:"health_code"`
}

type CodexRoutingDefaultHealth struct {
	Configured bool   `json:"configured"`
	Resolved   bool   `json:"resolved"`
	Routable   bool   `json:"routable"`
	Status     string `json:"status"`
}

const (
	CodexRoutingDefaultStatusUnconfigured = "unconfigured"
	CodexRoutingDefaultStatusResolved     = "resolved"
	CodexRoutingDefaultStatusUnroutable   = "unroutable"
	CodexRoutingDefaultStatusUnresolved   = "unresolved"
	CodexRoutingDefaultStatusUnknown      = "unknown"
)

type CodexHealth struct {
	AccountCount      int                       `json:"account_count"`
	AccountCountKnown bool                      `json:"-"`
	HealthCode        string                    `json:"-"`
	ExternalSources   []CodexSourceHealth       `json:"external_sources"`
	RoutingDefault    CodexRoutingDefaultHealth `json:"-"`
}

// Server is the reverse proxy HTTP server.
type Server struct {
	Config    *Config
	Selector  ClaudeSelector
	Discover  ClaudeDiscoverer
	Transport http.RoundTripper
	// RuntimeNormalHandler is set only by the socket supervisor. It forwards
	// public work to the selected private worker instead of running normal
	// proxy semantics in the supervisor process.
	RuntimeNormalHandler http.Handler
	// RuntimeCallerCredentials remain worker-local. The runtime handler uses
	// safe caller subject IDs to restore only the credential authorised by the
	// supervisor's consumed admission.
	RuntimeCallerCredentials []NormalCallerCredentialV1
	CodexDiscover            CodexDiscoverer
	CodexHealth              func() CodexHealth
	CodexRequests            *CodexRequestRouter
	CodexWebSocketExecutor   ExplicitWebSocketExecutor
	// CodexWebSocketBroker owns readiness-gated terminating WebSocket routing.
	// Nil fails closed when WebSocket enforcement is effective.
	CodexWebSocketBroker CodexWebSocketRoutingHandler
	// CodexNativeHTTP may claim readiness-gated or retained-fence native traffic.
	// Nil or a declined untouched request preserves the off/observe path below.
	CodexNativeHTTP CodexNativeHTTPRoutingHandler
	// codexLegacyNativeHTTP is the package-private fallback extraction seam.
	// Nil selects the production legacy handler bound to this Server.
	codexLegacyNativeHTTP codexLegacyNativeHTTPFallback
	// Deprecated compatibility seams. Production routing never sets these.
	CodexTransport            http.RoundTripper
	CodexUpgradeTransport     http.RoundTripper // HTTP/1.1-only transport for WebSocket upgrades
	codexLiveSidebandUpstream string
	codexLive                 codexLiveState
	Headroom                  *HeadroomBridge
	Diag                      *DiagnosticsWriter
	PayloadDiag               *PayloadWriter
	// CodexCanary enforces the always-on Codex diagnostics privacy boundary
	// even when route diagnostics persistence is disabled.
	CodexCanary *CodexCanaryRecorder
	// CodexCanaryStop is non-nil only while one active canary is owned by this
	// exact serving process.
	CodexCanaryStop CodexCanaryStopFunc
	// ServingAttestor binds maintenance finalise proof authority to the exact
	// pre-bound loopback listener passed to http.Server.Serve.
	ServingAttestor *ServingAttestor
	// CodexHTTPStartupValidation is an explicit one-shot validation mode. Nil
	// preserves normal long-running server startup.
	CodexHTTPStartupValidation CodexHTTPStartupValidationFunc
	// codexInstalledHTTPRouteAudit is non-nil only for the explicit one-shot
	// installed validation process. Normal startup never attaches it.
	codexInstalledHTTPRouteAudit *codexInstalledHTTPRouteAudit
	// shutdownGracePeriod is test-configurable; zero selects the production
	// default. It is deliberately unexported so callers cannot weaken shutdown.
	shutdownGracePeriod time.Duration
	// CodexRouting is resolved once at startup. Config reload never mutates it.
	CodexRouting *CodexRoutingRuntime
	// CodexObserver mirrors Responses lifecycle and preserves an exact strong
	// turn's first actual route without consuming prospective shadow choices.
	CodexObserver *CodexTurnObserver
	// CodexWebSocketObserver is the mode-specific WebSocket view over the same
	// live coordinator core. CodexWebSocketObserverConfigured distinguishes an
	// explicitly disabled WebSocket observer from the compatibility fallback.
	CodexWebSocketObserver           *CodexTurnObserver
	CodexWebSocketObserverConfigured bool
	// CodexHTTPEnforcer owns readiness-gated turns and retained authority fences.
	CodexHTTPEnforcer *CodexHTTPEnforcer
	// CodexPrimer is non-nil only in credential-coordinator owner process.
	CodexPrimer *CodexPrimer
	// HeadroomMode is the resolved compression mode. Only meaningful when
	// Headroom is non-nil. Reported in the /health response.
	HeadroomMode HeadroomMode
	// Catalog is the optional model registry catalog. When non-nil, it backs
	// /v1/models projections, /v1/registry, and routing decisions.
	Catalog *modelregistry.Catalog
	// Refresher is the optional registry refresher. When non-nil, it backs
	// the /v1/registry/refresh endpoint.
	Refresher RegistryRefresher
	// RoutingPolicy remains worker-owned while local authenticated control
	// reads or publishes policy through this process.
	RoutingPolicy *RoutingPolicyStore
	SessionPolicy *SessionPolicyResolver
	// CodexTurnReceipts remains worker-owned and process-local. It exposes only
	// privacy-safe post-turn route facts through authenticated local control.
	CodexTurnReceipts *CodexTurnReceiptStore
}

type codexWebSocketRoutingProvider interface {
	codexWebSocketRouting() (*CodexRequestRouter, ExplicitWebSocketExecutor)
}

func (s *Server) codexWebSocketRouting() (*CodexRequestRouter, ExplicitWebSocketExecutor) {
	if s == nil {
		return nil, nil
	}
	if s.CodexRequests != nil && s.CodexWebSocketExecutor != nil {
		return s.CodexRequests, s.CodexWebSocketExecutor
	}
	if provider, ok := s.CodexUpgradeTransport.(codexWebSocketRoutingProvider); ok {
		return provider.codexWebSocketRouting()
	}
	return nil, nil
}

// ListenAndServe starts the proxy and blocks until the context is cancelled or a signal is received.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s == nil || s.Config == nil {
		return errors.New("proxy server configuration is unavailable")
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{
		IP: net.IPv4(127, 0, 0, 1), Port: s.Config.Port,
	})
	if err != nil {
		return err
	}
	return s.serve(ctx, listener)
}

// ServeAdoptedListener serves an already-bound listener without rebinding it.
// The caller retains lifecycle authority; Server closes only this inherited
// descriptor when serving terminates.
func (s *Server) ServeAdoptedListener(ctx context.Context, listener net.Listener) error {
	return s.serve(ctx, listener)
}

func (s *Server) serve(ctx context.Context, listener net.Listener) error {
	if s == nil || listener == nil || ctx == nil {
		if listener != nil {
			_ = listener.Close()
		}
		return errors.New("proxy server listener is unavailable")
	}
	if (s.CodexHTTPStartupValidation != nil || s.CodexCanaryStop != nil) && s.ServingAttestor == nil {
		_ = listener.Close()
		return errors.New("Codex HTTP startup validation requires a serving attestor")
	}
	handler, err := s.handler()
	if err != nil {
		_ = listener.Close()
		return err
	}
	serveListener := net.Listener(listener)
	if s.ServingAttestor != nil {
		tcpListener, ok := listener.(*net.TCPListener)
		if !ok {
			_ = listener.Close()
			return ErrServingAttestorUnavailable
		}
		serveListener, err = s.ServingAttestor.ActivateListener(tcpListener)
		if err != nil {
			_ = listener.Close()
			return err
		}
	}
	var servingReady <-chan struct{}
	if s.CodexHTTPStartupValidation != nil || s.CodexCanaryStop != nil {
		readyListener := newCodexHTTPStartupReadyListener(serveListener)
		serveListener = readyListener
		servingReady = readyListener.ready
	}
	srv := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	var startupValidation codexHTTPStartupValidationRun
	if s.CodexHTTPStartupValidation != nil {
		startupValidation = runCodexHTTPStartupValidation(ctx, stop, servingReady, CodexHTTPStartupValidationRuntime{
			ListenerAddress: listener.Addr().String(),
			ServingAttestor: s.ServingAttestor,
		}, s.CodexHTTPStartupValidation)
	}
	var canaryStopResult <-chan error
	if s.CodexCanaryStop != nil {
		result := make(chan error, 1)
		canaryStopResult = result
		go func() {
			var stopErr error
			defer func() {
				if recover() != nil {
					stopErr = ErrCodexCanaryStopUnavailable
				}
				result <- stopErr
				stop()
			}()
			select {
			case <-servingReady:
				stopErr = s.CodexCanaryStop(ctx, CodexCanaryStopRuntime{
					ListenerAddress: listener.Addr().String(),
					ServingAttestor: s.ServingAttestor,
				})
			case <-ctx.Done():
				stopErr = ctx.Err()
			}
		}()
	}

	shutdownResult := make(chan error, 1)
	go func() {
		<-ctx.Done()
		if s.ServingAttestor != nil {
			if s.CodexHTTPStartupValidation != nil && !codexHTTPStartupValidationCompleted(startupValidation) {
				<-s.ServingAttestor.abortUnexpected()
			} else {
				<-s.ServingAttestor.BeginClose()
			}
		}
		gracePeriod := s.shutdownGracePeriod
		if gracePeriod <= 0 {
			gracePeriod = defaultServerShutdownGracePeriod
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracePeriod)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			closeErr := srv.Close()
			shutdownResult <- errors.Join(fmt.Errorf("proxy server graceful shutdown: %w", err), closeErr)
			return
		}
		shutdownResult <- nil
	}()

	fmt.Fprintf(os.Stderr, "cq: proxy listening on %s\n", listener.Addr().String())

	serveErr := srv.Serve(serveListener)
	if !errors.Is(serveErr, http.ErrServerClosed) && s.ServingAttestor != nil {
		<-s.ServingAttestor.abortUnexpected()
	}
	stop()
	if s.ServingAttestor != nil {
		<-s.ServingAttestor.BeginClose()
	}
	shutdownErr := <-shutdownResult
	if !errors.Is(serveErr, http.ErrServerClosed) {
		return errors.Join(serveErr, shutdownErr)
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	if canaryStopResult != nil {
		if stopErr := <-canaryStopResult; stopErr != nil && !errors.Is(stopErr, context.Canceled) && !errors.Is(stopErr, context.DeadlineExceeded) {
			return errors.Join(errors.New("Codex canary stop failed"), stopErr)
		}
	}
	if s.CodexHTTPStartupValidation != nil {
		if err := codexHTTPStartupValidationResult(startupValidation); err != nil {
			return errors.Join(errors.New("Codex HTTP startup validation failed"), err)
		}
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// servingAttestedTCP4Listener delegates the exact pre-bound TCP4 listener and
// revokes unsealed proof authority before an unexpected Accept error reaches
// http.Server.Serve.
type servingAttestedTCP4Listener struct {
	net.Listener
	attestor *ServingAttestor
}

func (l *servingAttestedTCP4Listener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil && l.attestor != nil {
		if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
			return connection, err
		}
		<-l.attestor.abortUnexpected()
	}
	return connection, err
}

func (s *Server) handler() (http.Handler, error) {
	if s.RuntimeNormalHandler != nil {
		return s.RuntimeNormalHandler, nil
	}
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
	mux.HandleFunc("GET "+RuntimePolicyPath, s.handlePolicyControl)
	mux.HandleFunc("PUT "+RuntimePolicyPath, s.handlePolicyControl)
	mux.HandleFunc("POST "+RuntimePolicySessionDigestPath, s.handlePolicySessionDigest)
	mux.HandleFunc("POST "+RuntimeCodexTurnReceiptPath, s.handleCodexTurnReceipt)
	mux.HandleFunc("POST "+RuntimeCodexTurnReceiptV2Path, s.handleCodexTurnReceipt)
	mux.HandleFunc(codexResponsesPath, s.handleCodexResponsesRoute)
	mux.HandleFunc(legacyCodexResponsesPath, s.handleLegacyCodexResponsesRoute)
	mux.HandleFunc("GET "+codexCompactResponsesPath, s.handleCodexCompactResponsesGetRoute)
	mux.HandleFunc("GET "+legacyCodexCompactResponsesPath, s.handleLegacyCodexCompactResponsesGetRoute)
	mux.HandleFunc("POST "+codexCompactResponsesPath, s.handleCodexCompactResponsesRoute)
	mux.HandleFunc("POST "+legacyCodexCompactResponsesPath, s.handleLegacyCodexCompactResponsesRoute)
	mux.HandleFunc(codexAppServerPath, s.handleCodexAppServerRoute)
	mux.HandleFunc(codexImagesPathPrefix, s.handleCodexImagesRoute)
	mux.HandleFunc(legacyCodexImagesPathPrefix, s.handleCodexImagesRoute)
	mux.HandleFunc("POST "+codexSearchPath, s.handleCodexSearchRoute)
	mux.HandleFunc("POST /live", s.handleCodexLiveCall)
	mux.HandleFunc("POST /v1/live", s.handleCodexLiveCall)
	mux.HandleFunc("POST /realtime/calls", s.handleCodexLiveCall)
	mux.HandleFunc("POST /v1/realtime/calls", s.handleCodexLiveCall)
	mux.HandleFunc("GET /live/", s.handleCodexLiveSideband)
	mux.HandleFunc("GET /v1/live/", s.handleCodexLiveSideband)
	mux.HandleFunc("GET /realtime/calls/", s.handleCodexLiveSideband)
	mux.HandleFunc("GET /v1/realtime/calls/", s.handleCodexLiveSideband)
	mux.HandleFunc("GET /realtime", s.handleCodexLiveSideband)
	mux.HandleFunc("GET /v1/realtime", s.handleCodexLiveSideband)
	for _, pattern := range unsupportedOpenAICompatibleRoutePatterns() {
		mux.HandleFunc(pattern, s.handleUnsupportedOpenAICompatibleRoute)
	}
	mux.HandleFunc("/", s.proxyHandler(upstream))
	if s.codexInstalledHTTPRouteAudit != nil {
		return s.codexInstalledHTTPRouteAudit.guard(mux), nil
	}
	return mux, nil
}

// RuntimeHandler returns this process's complete normal proxy semantics for
// execution by a private runtime worker.
func (s *Server) RuntimeHandler() (http.Handler, error) {
	return s.handler()
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

	var upstreamModels []ModelMetadata
	if s.isValidToken(bearerToken(r)) {
		upstreamModels = s.fetchUpstreamModels(r)
	}

	// /v1/models keeps runtime/API model metadata compatible with ANTHROPIC_BASE_URL.
	// Claude Code's interactive /model picker is populated separately from
	// additionalModelOptionsCache in ~/.claude.json; bootstrap/OAuth discovery does
	// not use ANTHROPIC_BASE_URL and custom OAuth hosts are allowlist-restricted.
	//
	// Local OpenAI-compatible clients also probe /v1/models when openai_base_url
	// points at CQ, but they do not know CQ's local_token. Serve local metadata
	// without auth and only query Claude upstream when the supplied bearer token
	// is a valid CQ/Claude token, so arbitrary OpenAI API keys are never relayed.
	models := mergeModelMetadata(SyntheticModelCatalog(), registryCatalogModelMetadata(s.Catalog), upstreamModels)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": models,
	})
}

func (s *Server) rejectInvalidProxyToken(w http.ResponseWriter, r *http.Request, routeKind string, start time.Time, emitDiag bool) {
	fmt.Fprintf(os.Stderr, "cq: reject %s %s: invalid proxy token\n", r.Method, r.URL.Path)
	writeError(w, http.StatusForbidden, "authentication_error", "invalid proxy token")
	if !emitDiag {
		return
	}
	s.emitDiagnostics(RouteEvent{
		Time:       start.UTC(),
		Method:     r.Method,
		Path:       r.URL.Path,
		Provider:   "unknown",
		RouteKind:  routeKind,
		StatusCode: http.StatusForbidden,
		LatencyMS:  time.Since(start).Milliseconds(),
		Error:      diagnosticsErrorCode("authentication_error", "invalid proxy token"),
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
		s.rejectInvalidProxyToken(w, r, "registry", time.Now(), true)
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
		s.rejectInvalidProxyToken(w, r, "registry_refresh", time.Now(), true)
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

func (s *Server) codexRoutingHealth() (CodexModeStatus, CodexModeStatus) {
	if s.CodexRouting != nil {
		return s.CodexRouting.HTTP, s.CodexRouting.WebSocket
	}
	httpConfigured := CodexRoutingOff
	wsConfigured := CodexRoutingOff
	if s.Config != nil {
		if s.Config.CodexTurnRouting != "" {
			httpConfigured = s.Config.CodexTurnRouting
		}
		if s.Config.CodexWSTurnRouting != "" {
			wsConfigured = s.Config.CodexWSTurnRouting
		}
	}
	status := func(configured CodexRoutingMode) CodexModeStatus {
		result := CodexModeStatus{Configured: configured, Effective: CodexRoutingOff}
		if configured != CodexRoutingOff {
			result.InhibitionReason = "routing runtime unavailable"
		}
		return result
	}
	return status(httpConfigured), status(wsConfigured)
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
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("websocket transport is not supported on %s; use %s", codexResponsesPath, legacyCodexResponsesPath))
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
	start := time.Now()
	message := fmt.Sprintf("%s is retired; run Codex app-server locally and route its outbound Responses traffic through %s", codexAppServerPath, legacyCodexResponsesPath)
	writeError(w, http.StatusGone, "invalid_request_error", message)
	event := RouteEvent{
		Time:       start.UTC(),
		Method:     r.Method,
		Path:       r.URL.Path,
		Provider:   "codex",
		RouteKind:  "codex_app_server",
		StatusCode: http.StatusGone,
		LatencyMS:  time.Since(start).Milliseconds(),
		Error:      diagnosticsErrorCode("invalid_request_error", message),
	}
	event.applySessionCorrelation(r.Header)
	s.emitDiagnostics(event)
}

func (s *Server) handleCodexImagesRoute(w http.ResponseWriter, r *http.Request) {
	s.handleCodexHTTPRoute(w, r, "codex_images", "images", codexImagesUpstreamPath(r.URL.EscapedPath()))
}

func (s *Server) handleCodexSearchRoute(w http.ResponseWriter, r *http.Request) {
	s.handleCodexHTTPRoute(w, r, "codex_search", "search", r.URL.EscapedPath())
}

func (s *Server) handleCodexHTTPRoute(w http.ResponseWriter, r *http.Request, routeKind, logKind, upstreamPath string) {
	start := time.Now()
	statusCode := 0
	diagError := ""
	defer func() {
		s.emitDiagnostics(RouteEvent{
			Time:       start.UTC(),
			Method:     r.Method,
			Path:       r.URL.Path,
			Provider:   "codex",
			RouteKind:  routeKind,
			StatusCode: statusCode,
			LatencyMS:  time.Since(start).Milliseconds(),
			Error:      diagError,
		})
	}()

	if r.Method != http.MethodPost {
		statusCode = http.StatusMethodNotAllowed
		message := fmt.Sprintf("%s only supports POST", r.URL.Path)
		diagError = diagnosticsErrorCode("invalid_request_error", message)
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", message)
		return
	}

	if !s.codexHTTPAvailable() {
		statusCode = http.StatusServiceUnavailable
		diagError = diagnosticsErrorCode("api_error", "no codex accounts configured")
		writeError(w, http.StatusServiceUnavailable, "api_error", "no codex accounts configured")
		return
	}

	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		r.Body.Close()
		if err != nil {
			statusCode = http.StatusBadRequest
			diagError = diagnosticsErrorCode("invalid_request_error", "failed to read request body")
			writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		if len(body) > maxRequestBody {
			statusCode = http.StatusRequestEntityTooLarge
			diagError = diagnosticsErrorCode("invalid_request_error", "request body exceeds 10 MiB")
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds 10 MiB")
			return
		}
	}

	upstreamURL := strings.TrimRight(s.Config.CodexUpstream, "/") + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		statusCode = http.StatusInternalServerError
		diagError = diagnosticsErrorCode("api_error", fmt.Sprintf("create upstream request: %v", err))
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return
	}
	upReq.ContentLength = int64(len(body))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	for key, vals := range r.Header {
		for _, v := range vals {
			upReq.Header.Add(key, v)
		}
	}

	fmt.Fprintf(os.Stderr, "cq: route %s %s provider=codex (%s)\n", r.Method, r.URL.Path, logKind)
	resp, _, _, err := s.doCodexRequest(r.Context(), extractModel(body), upReq)
	if err != nil {
		statusCode = http.StatusBadGateway
		diagError = diagnosticsErrorCode("api_error", fmt.Sprintf("codex upstream error: %v", err))
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "cq: proxy %s %s → %d (codex %s)\n", r.Method, r.URL.Path, resp.StatusCode, logKind)
	statusCode = resp.StatusCode
	if err := relayCodexHTTPResponse(w, resp, false); err != nil {
		fmt.Fprintf(os.Stderr, "cq: codex %s response copy: %v\n", logKind, err)
	}
}

func codexImagesUpstreamPath(path string) string {
	if strings.HasPrefix(path, codexImagesPathPrefix) {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

func unsupportedOpenAICompatibleRoutePatterns() []string {
	return []string{
		"/v1/responses/",
		"/responses/",
		"/v1/chat/completions",
		"/chat/completions",
		"/v1/completions",
		"/completions",
		"/v1/audio/",
		"/audio/",
		"/v1/embeddings",
		"/embeddings",
		"/v1/moderations",
		"/moderations",
		"/v1/realtime/",
		"/realtime/",
		"/v1/files",
		"/v1/files/",
		"/files",
		"/files/",
		"/v1/uploads",
		"/v1/uploads/",
		"/uploads",
		"/uploads/",
		"/v1/vector_stores",
		"/v1/vector_stores/",
		"/vector_stores",
		"/vector_stores/",
		"/v1/batches",
		"/v1/batches/",
		"/batches",
		"/batches/",
		"/v1/assistants",
		"/v1/assistants/",
		"/assistants",
		"/assistants/",
		"/v1/threads",
		"/v1/threads/",
		"/threads",
		"/threads/",
		"/v1/fine_tuning/",
		"/fine_tuning/",
		"/v1/evals",
		"/v1/evals/",
		"/evals",
		"/evals/",
		"/v1/containers",
		"/v1/containers/",
		"/containers",
		"/containers/",
		"/v1/conversations",
		"/v1/conversations/",
		"/conversations",
		"/conversations/",
	}
}

func (s *Server) handleUnsupportedOpenAICompatibleRoute(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	message := fmt.Sprintf("%s is an OpenAI-compatible endpoint that CQ does not support through Codex OAuth; supported local Codex endpoints are /v1/responses, /responses, /v1/images/*, /images/*, /alpha/search, /models, and /app-server", r.URL.Path)
	writeError(w, http.StatusNotImplemented, "invalid_request_error", message)
	s.emitDiagnostics(RouteEvent{
		Time:       start.UTC(),
		Method:     r.Method,
		Path:       r.URL.Path,
		Provider:   "codex",
		RouteKind:  "openai_unsupported",
		StatusCode: http.StatusNotImplemented,
		LatencyMS:  time.Since(start).Milliseconds(),
		Error:      diagnosticsErrorCode("invalid_request_error", message),
	})
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
	var codexCount any = 0
	var codexHealth CodexHealth
	if s.CodexHealth != nil {
		codexHealth = s.CodexHealth()
		codexHealth.RoutingDefault = normaliseCodexRoutingDefaultHealth(codexHealth.RoutingDefault)
		if codexHealth.AccountCountKnown {
			codexCount = codexHealth.AccountCount
		} else {
			codexCount = nil
		}
	} else if s.CodexDiscover != nil {
		codexCount = len(s.CodexDiscover())
	}
	status := "ok"
	if codexHealth.HealthCode != "" && codexHealth.HealthCode != "ok" {
		status = "degraded"
	}
	for _, source := range codexHealth.ExternalSources {
		if source.HealthCode != "ok" {
			status = "degraded"
			break
		}
	}
	if codexHealth.RoutingDefault.Configured && codexHealth.RoutingDefault.Status != CodexRoutingDefaultStatusResolved {
		status = "degraded"
	}
	resp := map[string]any{
		"status":                      status,
		"headroom":                    s.Headroom != nil,
		"codex_runtime_observability": codexProcessRuntimeObservability.snapshot(),
		"accounts": map[string]any{
			"claude": claudeCount,
			"codex":  codexCount,
		},
		"diagnostics": map[string]bool{
			"enabled": s.Diag != nil,
			"payload": s.PayloadDiag != nil,
		},
	}
	if s.CodexHealth != nil {
		resp["codex_external_sources"] = codexHealth.ExternalSources
		resp["codex_inventory_health"] = codexHealth.HealthCode
		resp["codex_routing_default"] = codexHealth.RoutingDefault
	}
	httpMode, wsMode := s.codexRoutingHealth()
	s.annotateCodexWebSocketSkew(r.Context(), &wsMode)
	resp["codex_turn_routing"] = httpMode
	resp["codex_ws_turn_routing"] = wsMode
	if s.CodexObserver != nil {
		resp["codex_turn_observation"] = s.CodexObserver.Health()
	}
	if s.CodexWebSocketObserverConfigured && s.CodexWebSocketObserver != nil && s.CodexWebSocketObserver != s.CodexObserver {
		resp["codex_ws_turn_observation"] = s.CodexWebSocketObserver.Health()
	}
	if s.CodexHTTPEnforcer != nil {
		resp["codex_turn_enforcement"] = s.CodexHTTPEnforcer.Observer.Health()
	}
	primerConfigured := s.Config != nil && s.Config.CodexWindowPriming.Enabled
	resp["codex_window_priming"] = s.CodexPrimer.Health(primerConfigured)
	if s.Headroom != nil {
		switch s.HeadroomMode {
		case HeadroomModeCache:
			resp["headroom_mode"] = "cache"
		default:
			resp["headroom_mode"] = "token"
		}
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(resp); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "encode health response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.ServingAttestor != nil {
		if proof, ok := s.ServingAttestor.ProveHealth(r, body.Bytes()); ok {
			w.Header().Set(ServingProofResponseHeader, proof)
		}
	}
	_, _ = w.Write(body.Bytes())
}

func (s *Server) annotateCodexWebSocketSkew(ctx context.Context, mode *CodexModeStatus) {
	if s == nil || mode == nil || mode.Effective != CodexRoutingObserve || s.CodexRequests == nil || s.CodexRequests.Capacity == nil {
		return
	}
	keys, err := s.CodexRequests.AccountKeys(ctx)
	if err != nil && s.CodexDiscover != nil {
		for _, account := range s.CodexDiscover() {
			if account.AccountKey != "" {
				keys = append(keys, account.AccountKey)
			}
		}
	}
	minRemaining, maxRemaining, known := 101, -1, 0
	for _, key := range keys {
		view := s.CodexRequests.Capacity.Capacity(key, CapacityBucketBase)
		if view.State == CapacityUnknown || view.RemainingPct < 0 {
			continue
		}
		minRemaining = min(minRemaining, view.RemainingPct)
		maxRemaining = max(maxRemaining, view.RemainingPct)
		known++
	}
	if known < 2 || maxRemaining <= minRemaining {
		return
	}
	mode.ConnectionSticky = true
	mode.CapacitySkewPct = maxRemaining - minRemaining
	mode.Limitation = "account selected once per WebSocket connection; later turns cannot rebalance"
}

func normaliseCodexRoutingDefaultHealth(health CodexRoutingDefaultHealth) CodexRoutingDefaultHealth {
	unconfigured := CodexRoutingDefaultHealth{Status: CodexRoutingDefaultStatusUnconfigured}
	resolved := CodexRoutingDefaultHealth{
		Configured: true,
		Resolved:   true,
		Routable:   true,
		Status:     CodexRoutingDefaultStatusResolved,
	}
	unroutable := CodexRoutingDefaultHealth{
		Configured: true,
		Resolved:   true,
		Status:     CodexRoutingDefaultStatusUnroutable,
	}
	unresolved := CodexRoutingDefaultHealth{
		Configured: true,
		Status:     CodexRoutingDefaultStatusUnresolved,
	}
	unknown := CodexRoutingDefaultHealth{
		Configured: true,
		Status:     CodexRoutingDefaultStatusUnknown,
	}

	switch health {
	case CodexRoutingDefaultHealth{}, unconfigured:
		return unconfigured
	case resolved:
		return resolved
	case unroutable:
		return unroutable
	case unresolved:
		return unresolved
	case unknown:
		return unknown
	default:
		return unknown
	}
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
// forwarded as-is through explicit-account execution.
//
// Security: no proxy token auth is required. The proxy binds to 127.0.0.1 only,
// so only local processes can reach this endpoint. Codex CLI in ChatGPT auth
// mode doesn't support custom API keys, so we can't require the proxy token.
func (s *Server) handleNativeCodex(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var model string
	ctx, routeDiag := withRouteDiagnostics(r.Context())
	r = r.WithContext(ctx)
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
	if s.CodexNativeHTTP != nil {
		if handled, routedModel := s.CodexNativeHTTP.TryServe(w, r, false); handled {
			model = routedModel
			return
		}
	}

	legacy := s.codexLegacyNativeHTTP
	if legacy == nil {
		legacy = newLegacyCodexNativeHTTPHandler(s)
	}
	model = legacy.Handle(w, r)
}

func (s *Server) parseCodexHTTPEnforcement(body []byte, header http.Header) (CodexProtocolRequest, bool, error) {
	if s == nil || s.CodexHTTPEnforcer == nil {
		return CodexProtocolRequest{}, false, nil
	}
	return s.CodexHTTPEnforcer.Parse(body, header)
}

func (s *Server) parseCodexHTTPEnforcementDecoded(body []byte, header http.Header) (CodexProtocolRequest, bool, error) {
	if s == nil || s.CodexHTTPEnforcer == nil {
		return CodexProtocolRequest{}, false, nil
	}
	return s.CodexHTTPEnforcer.parseDecoded(body, header)
}

// proxyCodexUpgrade handles WebSocket upgrade requests to /responses. Enforced
// routing accepts the local downstream first, then the terminating broker reads
// the strong response.create frame, selects its durable account, and owns the
// account-bound upstream lifecycle. Legacy routing retains first-frame dial and
// blind relay behaviour.
//
// Note: native Codex WebSocket traffic is intentionally out of scope for
// headroom compression — the handshake body is minimal and the subsequent
// binary/text frames are not buffered by this proxy.
func (s *Server) proxyCodexUpgrade(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	statusCode := 0
	diagError := ""
	routeKind := "codex_legacy_websocket"
	_, wsMode := s.codexRoutingHealth()
	webSocketEnforcing := wsMode.Effective == CodexRoutingEnforce
	if webSocketEnforcing {
		routeKind = "codex_websocket_broker"
	}
	ctx, routeDiag := withRouteDiagnostics(r.Context())
	r = r.WithContext(ctx)
	defer func() {
		event := RouteEvent{
			Time:       start.UTC(),
			Method:     r.Method,
			Path:       r.URL.Path,
			Provider:   "codex",
			RouteKind:  routeKind,
			StatusCode: statusCode,
			LatencyMS:  time.Since(start).Milliseconds(),
			Error:      diagError,
		}
		event.applyRouteDiagnostics(routeDiag)
		event.applySessionCorrelation(r.Header)
		s.emitDiagnostics(event)
	}()

	if webSocketEnforcing && s.CodexWebSocketBroker == nil {
		statusCode = http.StatusServiceUnavailable
		diagError = diagnosticsErrorCode("api_error", "Codex WebSocket routing unavailable")
		writeError(w, http.StatusServiceUnavailable, "api_error", "Codex WebSocket routing unavailable")
		return
	}
	if !webSocketEnforcing {
		if router, executor := s.codexWebSocketRouting(); router == nil || executor == nil {
			statusCode = http.StatusServiceUnavailable
			diagError = diagnosticsErrorCode("api_error", "no codex accounts configured")
			writeError(w, http.StatusServiceUnavailable, "api_error", "no codex accounts configured")
			return
		}
	}
	upstreamURL := ""
	if !webSocketEnforcing {
		var err error
		upstreamURL, err = codexAppServerWebSocketURL(s.Config.CodexUpstream)
		if err != nil {
			statusCode = http.StatusInternalServerError
			diagError = diagnosticsErrorCode("api_error", "invalid codex upstream URL")
			writeError(w, http.StatusInternalServerError, "api_error", "invalid codex upstream URL")
			return
		}
	}

	fmt.Fprintf(os.Stderr, "cq: route %s /responses (websocket upgrade) provider=codex (native)\n", r.Method)

	upgrader := websocket.Upgrader{
		CheckOrigin:       func(_ *http.Request) bool { return true },
		Subprotocols:      websocket.Subprotocols(r),
		EnableCompression: true,
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	statusCode = http.StatusSwitchingProtocols
	defer clientConn.Close()
	clientConn.SetReadLimit(codexWebSocketMessageMaxBytes)

	if webSocketEnforcing {
		brokerContext := withCodexWSFrameObservationSink(r.Context(), func(diagnostics *routeDiagnostics) {
			s.emitCodexWebSocketFrameObservation(diagnostics)
		})
		if err := s.CodexWebSocketBroker.Serve(brokerContext, clientConn, r.Header); err != nil {
			failure := classifyCodexWebSocketFailure(err)
			decision := "broker_failed"
			if failure.plan {
				decision = "plan_failed"
			}
			noteCodexObservation(r.Context(), codexObservationFields{Decision: decision, Reason: string(failure.Reason)})
			sessionKey, _ := sessionCorrelation(r.Header)
			if sessionKey == "" {
				sessionKey = "none"
			}
			fmt.Fprintf(os.Stderr, "cq: Codex route trace transport=websocket session=%s event=%s stage=%s reason=%s\n", sessionKey, decision, failure.Stage, failure.Reason)
			diagError = diagnosticsErrorCode("api_error", "Codex WebSocket routing failed")
			_ = clientConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream error"), time.Now().Add(time.Second))
		}
		return
	}

	messageType, message, err := clientConn.ReadMessage()
	if err != nil {
		return
	}
	requestedModel := ""
	if messageType == websocket.TextMessage {
		requestedModel = extractCodexWebSocketFrameModel(message)
		s.emitCodexWebSocketPayloadDiagnostics(r, legacyCodexResponsesPath, requestedModel, message, 1)
	}
	if diagnostics, accepted := inspectCodexLegacyWSClientFrame(messageType, message); accepted {
		s.emitCodexWebSocketFrameObservation(diagnostics)
	}
	wsObserver := s.codexWebSocketObserver()
	if wsObserver != nil {
		r = r.WithContext(withCodexObservation(r.Context(), wsObserver))
	}
	upstreamConn, choice, _, capacity, err := s.dialCodexWebSocketWithCapacity(r.Context(), upstreamURL, r.Header, requestedModel)
	if err != nil {
		_ = clientConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream error"), time.Now().Add(time.Second))
		return
	}
	defer upstreamConn.Close()
	upstreamConn.SetReadLimit(codexWebSocketMessageMaxBytes)
	observation := newCodexWSObservationSession(wsObserver, r.Context(), choice, capacity)
	if observation != nil && messageType == websocket.TextMessage {
		observation.ObserveClient(message)
	}
	if err := upstreamConn.WriteMessage(messageType, message); err != nil {
		if observation != nil {
			observation.Close(err)
		}
		return
	}
	relayErr := relayWebSocketPairObserved(r.Context(), clientConn, upstreamConn, func(fromClient bool, messageType int, message []byte) {
		if fromClient {
			if diagnostics, accepted := inspectCodexLegacyWSClientFrame(messageType, message); accepted {
				s.emitCodexWebSocketFrameObservation(diagnostics)
			}
		}
		if observation == nil || messageType != websocket.TextMessage {
			return
		}
		if fromClient {
			observation.ObserveClient(message)
		} else {
			observation.ObserveUpstream(message)
		}
	})
	if observation != nil {
		observation.Close(relayErr)
	}
}

func inspectCodexLegacyWSClientFrame(messageType int, frame []byte) (*routeDiagnostics, bool) {
	if messageType != websocket.TextMessage {
		return nil, false
	}
	var envelope struct {
		Type   string `json:"type"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil ||
		(envelope.Type != "response.create" && envelope.Method != "response/create") {
		return nil, false
	}
	request, err := parseCodexObservationRequest(frame, nil)
	if err != nil {
		return nil, false
	}
	diagnostics := &routeDiagnostics{}
	diagnostics.codex = codexObservationFieldsForRequestShape(classifyCodexRequestShape(request, nil))
	return diagnostics, true
}

func (s *Server) emitCodexWebSocketFrameObservation(diagnostics *routeDiagnostics) {
	event := RouteEvent{
		Time:      time.Now().UTC(),
		Method:    http.MethodPost,
		Path:      legacyCodexResponsesPath,
		Provider:  "codex",
		RouteKind: "codex_websocket_frame",
	}
	event.applyRouteDiagnostics(diagnostics)
	s.emitDiagnostics(event)
}

func (s *Server) codexWebSocketObserver() *CodexTurnObserver {
	if s == nil {
		return nil
	}
	if s.CodexWebSocketObserverConfigured {
		return s.CodexWebSocketObserver
	}
	if s.CodexWebSocketObserver != nil {
		return s.CodexWebSocketObserver
	}
	return s.CodexObserver
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

func (s *Server) dialCodexWebSocket(ctx context.Context, upstreamURL string, incomingHeaders http.Header, requestedModel string) (*websocket.Conn, RouteChoice, CandidateAttempt, error) {
	connection, choice, attempt, _, err := s.dialCodexWebSocketBeforeDownstreamUpgrade(ctx, upstreamURL, incomingHeaders, requestedModel)
	return connection, choice, attempt, err
}

// dialCodexWebSocketWithCapacity runs after the downstream WebSocket upgrade.
// At that point the selected logical account and credential candidate are
// immutable for this connection. Exact resolution may update only that same
// candidate's revision before the single upstream dispatch.
func (s *Server) dialCodexWebSocketWithCapacity(ctx context.Context, upstreamURL string, incomingHeaders http.Header, requestedModel string) (*websocket.Conn, RouteChoice, CandidateAttempt, *codexRateLimitProducer, error) {
	router, executor := s.codexWebSocketRouting()
	if router == nil || executor == nil {
		return nil, RouteChoice{}, CandidateAttempt{}, nil, fmt.Errorf("no Codex accounts configured")
	}
	plan, err := router.Plan(ctx, CodexRouteRequirements{RequestedModel: requestedModel}, "")
	if err != nil {
		return nil, RouteChoice{}, CandidateAttempt{}, nil, err
	}
	noteRouteAccount(ctx, redactedAccountHint("codex", string(plan.Choice.AccountKey)), false)
	if len(plan.Attempts) == 0 {
		return nil, plan.Choice, CandidateAttempt{}, nil, fmt.Errorf("no Codex credential candidate available for WebSocket")
	}
	attempt := plan.Attempts[0]
	conn, resp, body, actual, dialErr := executeCodexWebSocketAttempt(
		executor, ctx, plan.Choice, attempt, upstreamURL, incomingHeaders,
		func(actual CandidateAttempt) { observeCodexAttempt(ctx, plan.Choice, actual) },
	)
	capacity := router.newRateLimitProducer(plan.Choice, true)
	if capacity != nil && resp != nil {
		_ = capacity.ObserveHeaders(resp.Header)
	}
	if dialErr == nil {
		return conn, plan.Choice, actual, capacity, nil
	}
	if resp == nil {
		return nil, plan.Choice, actual, nil, dialErr
	}
	if resp.StatusCode == http.StatusTooManyRequests && !resp.Uncompressed && codexAttemptResponseHasIdentityEncoding(resp.Header) {
		wrapped, parseErr := parseCodexHTTPError(body, resp.StatusCode)
		if parseErr == nil && wrapped.HardUsageLimit {
			router.observeHardLimit(plan.Choice, resp, capacity)
		}
	}
	return nil, plan.Choice, actual, nil, fmt.Errorf("codex websocket upgrade failed: %s", resp.Status)
}

// dialCodexWebSocketBeforeDownstreamUpgrade may try bounded alternate
// candidates and accounts because no downstream WebSocket has been admitted.
func (s *Server) dialCodexWebSocketBeforeDownstreamUpgrade(ctx context.Context, upstreamURL string, incomingHeaders http.Header, requestedModel string) (*websocket.Conn, RouteChoice, CandidateAttempt, *codexRateLimitProducer, error) {
	router, executor := s.codexWebSocketRouting()
	if router == nil || executor == nil {
		return nil, RouteChoice{}, CandidateAttempt{}, nil, fmt.Errorf("no Codex accounts configured")
	}
	var excluded []codex.SelectionExclusion
	for {
		plan, err := router.Plan(ctx, CodexRouteRequirements{RequestedModel: requestedModel}, "", excluded...)
		if err != nil {
			if len(excluded) == 0 {
				return nil, RouteChoice{}, CandidateAttempt{}, nil, err
			}
			return nil, RouteChoice{}, CandidateAttempt{}, nil, fmt.Errorf("no alternate codex account available for WebSocket")
		}
		noteRouteAccount(ctx, redactedAccountHint("codex", string(plan.Choice.AccountKey)), len(excluded) > 0)
		hardLimited := false
		for _, attempt := range plan.Attempts {
			conn, resp, body, actual, dialErr := executeCodexWebSocketAttempt(
				executor, ctx, plan.Choice, attempt, upstreamURL, incomingHeaders,
				func(actual CandidateAttempt) { observeCodexAttempt(ctx, plan.Choice, actual) },
			)
			capacity := router.newRateLimitProducer(plan.Choice, true)
			if capacity != nil && resp != nil {
				_ = capacity.ObserveHeaders(resp.Header)
			}
			if dialErr == nil {
				return conn, plan.Choice, actual, capacity, nil
			}
			if resp == nil {
				return nil, plan.Choice, actual, nil, dialErr
			}
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				continue
			case http.StatusTooManyRequests:
				if !resp.Uncompressed && codexAttemptResponseHasIdentityEncoding(resp.Header) {
					wrapped, parseErr := parseCodexHTTPError(body, resp.StatusCode)
					if parseErr == nil && wrapped.HardUsageLimit {
						router.observeHardLimit(plan.Choice, resp, capacity)
						hardLimited = true
						break
					}
				}
				return nil, plan.Choice, actual, nil, fmt.Errorf("codex websocket upgrade failed: %s", resp.Status)
			default:
				return nil, plan.Choice, actual, nil, fmt.Errorf("codex websocket upgrade failed: %s", resp.Status)
			}
			if hardLimited {
				break
			}
		}
		excluded = append(excluded, codex.SelectionExclusion{AccountKey: plan.Choice.AccountKey})
	}
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
			s.rejectInvalidProxyToken(w, r, "proxy_auth", start, diagnosticsAnthropicRouteKind(r.URL.Path) == "")
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
		if routeProvider == ProviderCodex {
			fmt.Fprintf(os.Stderr, "cq: route %s %s model_family=%s provider=codex\n",
				r.Method, r.URL.Path, projectCodexDiagnosticsModel(routeModel))
		} else {
			fmt.Fprintf(os.Stderr, "cq: route %s %s model=%q provider=%s\n",
				r.Method, r.URL.Path, routeModel, providerName(routeProvider))
		}
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
	if s == nil {
		return
	}
	event = projectCodexDiagnostics(event)
	recorder := s.CodexCanary
	if recorder == nil {
		recorder = s.Diag.codexCanary()
	}
	if err := rejectUnsafeCodexDiagnostics(event, recorder); err != nil {
		fmt.Fprintln(os.Stderr, "cq: diagnostics: dropped unsafe Codex event")
		return
	}
	if s.Diag == nil {
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
	case strings.Contains(msg, "openai-compatible endpoint") && strings.Contains(msg, "does not support"):
		return errType + ":unsupported_openai_endpoint"
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
