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
	sessionKey, sessionSource := sessionCorrelation(r.Header)
	ctx := withCodexTrace(r.Context(), s.Diag, s.PayloadDiag, CodexTraceStart{
		Transport: "http", SessionKey: sessionKey, SessionSource: sessionSource,
	})
	ctx, routeDiag := withRouteDiagnostics(ctx)
	routeDiag.applyCodexTrace(ctx)
	ctx = s.withCodexRequestIngressObservation(ctx, r.Method, r.URL.Path, "codex_compact_ingress")
	r = r.WithContext(ctx)
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

	// Buffer request body using the native HTTP transport contract.
	body, err := readCodexNativeHTTPRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	contentEncoding, err := parseCodexContentEncoding(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	inspectionLimits := codexHTTPZstdLimits()
	decodedRequest, err := DecodeCodexRequest(body, contentEncoding, inspectionLimits)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	decodedBody := decodedRequest.Decoded()
	telemetryRequest, telemetryErr := parseCodexObservationRequest(decodedBody, r.Header)
	telemetryShape := classifyCodexRequestShape(telemetryRequest, telemetryErr)
	noteCodexObservation(ctx, codexObservationFieldsForRequestShape(telemetryShape))
	if telemetryErr != nil {
		defer replaceCodexRequestShapeObservation(ctx, telemetryShape)
	}
	protocolRequest, enforce, err := s.parseCodexHTTPEnforcementDecoded(decodedBody, r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model = extractModel(decodedBody)
	if enforce && protocolRequest.Model != "" {
		model = protocolRequest.Model
	}
	fmt.Fprintf(os.Stderr, "cq: route POST %s model_family=%s provider=codex (native compact)\n", requestPath, projectCodexDiagnosticsModel(model))

	// Emit payload diagnostics before forwarding.
	payloadSessionKey, payloadSessionSource := payloadSessionCorrelation(r.Header, decodedBody)
	emitCodexRawTracePayload(ctx, PayloadEvent{
		Method: r.Method, Path: r.URL.Path, Provider: "codex", RouteKind: "codex_compact", Direction: "downstream_request",
		Model: model, ClientKind: clientKindFromUserAgent(r.Header.Get("User-Agent")),
		SessionKey: payloadSessionKey, SessionSource: payloadSessionSource, Headers: codexTraceHeaders(r.Header), Complete: true,
	}, body)

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
	captureCodexHTTPResponsePayloadWithEvent(ctx, resp, PayloadEvent{
		Method: r.Method, Path: r.URL.Path, Provider: "codex", RouteKind: "codex_compact", Direction: "upstream_response",
		Model: model, ClientKind: clientKindFromUserAgent(r.Header.Get("User-Agent")), AccountHint: codexTraceAccountHint(choice.AccountKey),
	})
	if observation != nil {
		_, failover := routeDiag.fields()
		observation.Selected(choice, failover)
		if err := observation.PrepareV2Response(resp); err != nil {
			observation.Finish(err)
			fmt.Fprintf(os.Stderr, "cq: Codex v2 compact observation: %v\n", err)
		} else {
			observation.Response(resp)
			observeCodexResponseBody(resp, observation)
		}
	}
	defer closeCodexHTTPResponseBody(resp.Body)

	fmt.Fprintf(os.Stderr, "cq: proxy POST %s → %d (codex native compact)\n", upstreamURL, resp.StatusCode)

	if err := relayCodexHTTPResponse(w, resp, true); err != nil {
		fmt.Fprintf(os.Stderr, "cq: codex compact response copy: %v\n", err)
	}
}
