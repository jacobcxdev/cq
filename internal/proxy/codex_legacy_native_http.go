package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// codexLegacyNativeHTTPFallback owns the pre-enforcement native Responses path.
// It is invoked only after the authoritative handler is absent or declines.
type codexLegacyNativeHTTPFallback interface {
	Handle(http.ResponseWriter, *http.Request) string
}

type legacyCodexNativeHTTPHandler struct {
	server *Server
}

func newLegacyCodexNativeHTTPHandler(server *Server) codexLegacyNativeHTTPFallback {
	return &legacyCodexNativeHTTPHandler{server: server}
}

func (handler *legacyCodexNativeHTTPHandler) Handle(w http.ResponseWriter, r *http.Request) string {
	s := handler.server
	ctx := r.Context()
	var model string
	if !s.codexHTTPAvailable() {
		writeError(w, http.StatusServiceUnavailable, "api_error", "no codex accounts configured")
		return model
	}

	// Buffer request body.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return model
	}
	if len(body) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds 10 MiB")
		return model
	}

	contentEncoding, err := parseCodexContentEncoding(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return model
	}
	inspectionLimits := DefaultCodexZstdLimits
	inspectionLimits.MaxEncodedBytes = maxRequestBody
	inspectionLimits.MaxDecodedBytes = maxRequestBody
	decodedRequest, err := DecodeCodexRequest(body, contentEncoding, inspectionLimits)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return model
	}
	decodedBody := decodedRequest.Decoded()
	protocolRequest, enforce, err := s.parseCodexHTTPEnforcementDecoded(decodedBody, r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return model
	}
	model = extractModel(decodedBody)
	if enforce && protocolRequest.Model != "" {
		model = protocolRequest.Model
	}
	fmt.Fprintf(os.Stderr, "cq: route POST /responses model_family=%s provider=codex (native)\n", projectCodexDiagnosticsModel(model))

	// Emit payload diagnostics before any body rewrite.
	if s.PayloadDiag != nil {
		sessionKey, sessionSource := payloadSessionCorrelation(r.Header, decodedBody)
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
	forwardBody := decodedRequest.Replay()
	bodyRewritten := false
	if s.Headroom != nil {
		var compressed []byte
		var saved int
		var err error
		if s.HeadroomMode == HeadroomModeCache {
			compressed, saved, err = s.Headroom.CompressResponsesCache(decodedBody)
		} else {
			compressed, saved, err = s.Headroom.CompressResponses(decodedBody, HeadroomModeToken)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: headroom: %v\n", err)
		} else if saved > 0 {
			prepared, encodeErr := EncodeCodexRequest(compressed, decodedRequest.Encoding(), inspectionLimits)
			if encodeErr != nil {
				writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("encode compressed Codex request: %v", encodeErr))
				return model
			}
			fmt.Fprintf(os.Stderr, "cq: headroom saved %d tokens\n", saved)
			forwardBody = prepared
			bodyRewritten = true
		}
	}

	// Build upstream request — forward as-is, no translation.
	upstreamURL := s.Config.CodexUpstream + "/responses"
	upReq, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(forwardBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return model
	}
	upReq.ContentLength = int64(len(forwardBody))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(forwardBody)), nil
	}

	// Forward all original headers — the transport will override auth headers.
	// This preserves Codex CLI-specific headers the ChatGPT backend may require.
	for key, vals := range r.Header {
		for _, v := range vals {
			upReq.Header.Add(key, v)
		}
	}
	if bodyRewritten {
		upReq.Header.Set("Content-Length", fmt.Sprint(len(forwardBody)))
	}
	if upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}

	resp, choice, observation, err := s.doCodexHTTPRouteDecoded(ctx, model, protocolRequest, upReq, body, decodedBody, r.Header, false, enforce)
	if err != nil {
		if observation != nil {
			observation.Finish(err)
		}
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return model
	}
	if observation != nil {
		routeDiag, _ := ctx.Value(routeDiagnosticsContextKey{}).(*routeDiagnostics)
		_, failover := routeDiag.fields()
		observation.Selected(choice, failover)
		if err := observation.PrepareV2Response(resp); err != nil {
			closeErr := closeCodexHTTPResponseBody(resp.Body)
			observation.Finish(errors.Join(err, closeErr))
			fmt.Fprintf(os.Stderr, "cq: Codex v2 observation: %v\n", err)
			writeError(w, http.StatusBadGateway, "api_error", "codex continuity observation failed")
			return model
		}
		observation.Response(resp)
		observeCodexResponseBody(resp, observation)
	}
	defer closeCodexHTTPResponseBody(resp.Body)

	fmt.Fprintf(os.Stderr, "cq: proxy POST %s → %d (codex native)\n", upstreamURL, resp.StatusCode)

	if err := relayCodexHTTPResponse(w, resp, true); err != nil {
		fmt.Fprintf(os.Stderr, "cq: codex native response copy: %v\n", err)
	}
	return model
}
