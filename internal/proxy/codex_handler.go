package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// handleCodex processes a request destined for the OpenAI Responses API.
// It translates the Anthropic-format request body, forwards it with Codex credentials,
// and translates the response back to Anthropic format.
func (s *Server) handleCodex(w http.ResponseWriter, r *http.Request, body []byte) {
	if s.CodexTransport == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "no codex accounts configured")
		return
	}

	if r.URL.Path == countTokensPath {
		s.handleCodexCountTokens(w, r, body)
		return
	}

	rawModel := extractModel(body)
	if RouteModelWithCatalog(rawModel, s.Catalog) != ProviderCodex {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q is not a Codex model", rawModel))
		return
	}

	// Translate Anthropic → OpenAI Responses.
	translated, err := translateRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("request translation failed: %v", err))
		return
	}

	// Determine if streaming.
	streaming := extractStream(body)
	// Normalise [1m] suffix for response translation (effort suffixes are not stripped).
	model := ParseModel(rawModel)
	fmt.Fprintf(os.Stderr, "cq: route %s %s model=%q provider=codex protocol=anthropic-messages translated_upstream=/responses stream=%t\n", r.Method, r.URL.Path, rawModel, streaming)

	// Build upstream request.
	upstreamURL := s.Config.CodexUpstream + "/responses"
	upReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return
	}

	upReq.Header.Set("Content-Type", "application/json")

	// Set body (with GetBody for transport retry on 401/429).
	upReq.Body = io.NopCloser(bytes.NewReader(translated))
	upReq.ContentLength = int64(len(translated))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(translated)), nil
	}

	// Send to upstream — transport handles auth injection and account rotation.
	resp, err := s.CodexTransport.RoundTrip(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "cq: proxy POST %s → %d (codex translated)\n", upstreamURL, resp.StatusCode)

	// If upstream returned an error, forward it as an Anthropic-format error.
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		writeError(w, resp.StatusCode, "api_error", fmt.Sprintf("codex upstream: %s", string(respBody)))
		return
	}

	if streaming {
		// Client wants SSE — pipe translated events directly.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		translator := NewStreamTranslator(model)
		if err := translator.Translate(w, resp.Body); err != nil {
			// Can't write error after headers are sent — log it.
			fmt.Fprintf(os.Stderr, "cq: stream translation error: %v\n", err)
		}
	} else {
		// Client wants JSON but ChatGPT backend always streams.
		// Collect the SSE stream, then assemble a single JSON response.
		translator := NewStreamTranslator(model)
		if err := translator.Collect(resp.Body); err != nil {
			writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("stream collection failed: %v", err))
			return
		}

		assembled, err := translator.AssembleResponse(model)
		if err != nil {
			writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("response assembly failed: %v", err))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(assembled)
	}
}

func (s *Server) handleCodexCountTokens(w http.ResponseWriter, r *http.Request, body []byte) {
	rawModel := extractModel(body)
	if RouteModelWithCatalog(rawModel, s.Catalog) != ProviderCodex {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q is not a Codex model", rawModel))
		return
	}

	translated, err := translateCountTokensRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("request translation failed: %v", err))
		return
	}

	upstreamURL := s.Config.CodexUpstream + "/v1/responses/input_tokens"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Body = io.NopCloser(bytes.NewReader(translated))
	upReq.ContentLength = int64(len(translated))
	upReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(translated)), nil
	}

	resp, err := s.CodexTransport.RoundTrip(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		writeError(w, resp.StatusCode, "api_error", fmt.Sprintf("codex upstream: %s", string(respBody)))
		return
	}

	var upstreamResp openaiInputTokenCountResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&upstreamResp); err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("decode count_tokens response: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(anthropicCountTokensResponse{InputTokens: upstreamResp.InputTokens})
}

// extractStream does a quick JSON parse to check the "stream" field.
func extractStream(body []byte) bool {
	var partial struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &partial)
	return partial.Stream
}
