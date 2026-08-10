package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// handleCodexCompactResponsesRoute handles POST /v1/responses/compact.
func (s *Server) handleCodexCompactResponsesRoute(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		rejectCodexCompactWebSocket(w, codexCompactResponsesPath)
		return
	}
	s.handleNativeCodexCompact(w, r, codexCompactResponsesPath)
}

// handleCodexCompactResponsesGetRoute handles GET /v1/responses/compact.
func (s *Server) handleCodexCompactResponsesGetRoute(w http.ResponseWriter, r *http.Request) {
	handleCodexCompactGet(w, r, codexCompactResponsesPath)
}

// handleLegacyCodexCompactResponsesRoute handles POST /responses/compact.
func (s *Server) handleLegacyCodexCompactResponsesRoute(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		rejectCodexCompactWebSocket(w, legacyCodexCompactResponsesPath)
		return
	}
	s.handleNativeCodexCompact(w, r, legacyCodexCompactResponsesPath)
}

// handleLegacyCodexCompactResponsesGetRoute handles GET /responses/compact.
func (s *Server) handleLegacyCodexCompactResponsesGetRoute(w http.ResponseWriter, r *http.Request) {
	handleCodexCompactGet(w, r, legacyCodexCompactResponsesPath)
}

func handleCodexCompactGet(w http.ResponseWriter, r *http.Request, requestPath string) {
	if isWebSocketUpgrade(r) {
		rejectCodexCompactWebSocket(w, requestPath)
		return
	}
	w.Header().Set("Allow", http.MethodPost)
	writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", fmt.Sprintf("%s only supports POST", requestPath))
}

func rejectCodexCompactWebSocket(w http.ResponseWriter, requestPath string) {
	writeError(w, http.StatusBadRequest, "invalid_request_error",
		fmt.Sprintf("websocket transport is not supported on %s; use %s", requestPath, legacyCodexResponsesPath))
}

// handleNativeCodexCompact forwards a compact request to the upstream
// /responses/compact endpoint using explicit-account execution.
// No headroom compression is applied — compact requests already represent
// a summarisation boundary; compressing them further is counterproductive.
func (s *Server) handleNativeCodexCompact(w http.ResponseWriter, r *http.Request, requestPath string) {
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
				RouteKind:  "codex_compact",
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
		if handled, routedModel := s.CodexNativeHTTP.TryServe(w, r, true); handled {
			model = routedModel
			return
		}
	}

	if !s.codexHTTPAvailable() {
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

	contentEncoding, err := parseCodexContentEncoding(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	inspectionLimits := DefaultCodexZstdLimits
	inspectionLimits.MaxEncodedBytes = maxRequestBody
	inspectionLimits.MaxDecodedBytes = maxRequestBody
	decodedRequest, err := DecodeCodexRequest(body, contentEncoding, inspectionLimits)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	decodedBody := decodedRequest.Decoded()
	protocolRequest, enforce, err := s.parseCodexHTTPEnforcementDecoded(decodedBody, r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model = extractModel(decodedBody)
	if enforce && protocolRequest.Model != "" {
		model = protocolRequest.Model
	}
	fmt.Fprintf(os.Stderr, "cq: route POST %s model=%q provider=codex (native compact)\n", requestPath, model)

	// Emit payload diagnostics before forwarding.
	if s.PayloadDiag != nil {
		sessionKey, sessionSource := payloadSessionCorrelation(r.Header, decodedBody)
		s.emitPayloadDiagnostics(PayloadEvent{
			Time:          time.Now().UTC(),
			Method:        r.Method,
			Path:          r.URL.Path,
			Provider:      "codex",
			RouteKind:     "codex_compact",
			Model:         model,
			ClientKind:    clientKindFromUserAgent(r.Header.Get("User-Agent")),
			SessionKey:    sessionKey,
			SessionSource: sessionSource,
			BodyBytes:     len(body),
			Body:          encodeBody(body),
		})
	}

	// Build upstream request targeting /responses/compact (no headroom applied).
	forwardBody := decodedRequest.Replay()
	upstreamURL := s.Config.CodexUpstream + "/responses/compact"
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(forwardBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return
	}
	upReq.ContentLength = int64(len(forwardBody))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(forwardBody)), nil
	}

	// Forward all original headers; transport will override auth.
	for key, vals := range r.Header {
		for _, v := range vals {
			upReq.Header.Add(key, v)
		}
	}
	if upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}

	resp, choice, observation, err := s.doCodexHTTPRouteDecoded(ctx, model, protocolRequest, upReq, body, decodedBody, r.Header, true, enforce)
	if err != nil {
		if observation != nil {
			observation.Finish(err)
		}
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return
	}
	if observation != nil {
		_, failover := routeDiag.fields()
		observation.Selected(choice, failover)
		if err := observation.PrepareV2Response(resp); err != nil {
			_ = resp.Body.Close()
			observation.Finish(err)
			fmt.Fprintf(os.Stderr, "cq: Codex v2 compact observation: %v\n", err)
			writeError(w, http.StatusBadGateway, "api_error", "codex continuity observation failed")
			return
		}
		observation.Response(resp)
		observeCodexResponseBody(resp, observation)
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "cq: proxy POST %s → %d (codex native compact)\n", upstreamURL, resp.StatusCode)

	if err := relayCodexHTTPResponse(w, resp, true); err != nil {
		fmt.Fprintf(os.Stderr, "cq: codex compact response copy: %v\n", err)
	}
}
