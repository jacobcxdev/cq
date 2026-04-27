package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
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
		fmt.Sprintf("websocket transport is not supported on %s; use %s", requestPath, codexAppServerPath))
}

// handleNativeCodexCompact forwards a compact request to the upstream
// /responses/compact endpoint using CodexTransport for auth injection.
// No headroom compression is applied — compact requests already represent
// a summarisation boundary; compressing them further is counterproductive.
func (s *Server) handleNativeCodexCompact(w http.ResponseWriter, r *http.Request, requestPath string) {
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
	fmt.Fprintf(os.Stderr, "cq: route POST %s model=%q provider=codex (native compact)\n", requestPath, model)

	// Build upstream request targeting /responses/compact (no headroom applied).
	upstreamURL := s.Config.CodexUpstream + "/responses/compact"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return
	}
	upReq.ContentLength = int64(len(body))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
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

	// Transport handles auth injection and account rotation.
	resp, err := s.CodexTransport.RoundTrip(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "cq: proxy POST %s → %d (codex native compact)\n", upstreamURL, resp.StatusCode)

	// Forward response headers, status, and body.
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

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
