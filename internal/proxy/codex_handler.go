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
	if s.CodexSelector == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "no codex accounts configured")
		return
	}

	acct, err := s.CodexSelector.Select(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", fmt.Sprintf("codex account selection failed: %v", err))
		return
	}

	rawModel := extractModel(body)
	baseModel, effortOverride := ParseModelEffort(rawModel)

	// Translate Anthropic → OpenAI Responses.
	translated, err := translateRequest(body, effortOverride)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("request translation failed: %v", err))
		return
	}

	// Determine if streaming.
	streaming := extractStream(body)
	// Use base model (without effort suffix) for response translation.
	model := baseModel

	// Build upstream request.
	upstreamURL := s.Config.CodexUpstream + "/responses"
	upReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("create upstream request: %v", err))
		return
	}

	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	if acct.AccountID != "" {
		upReq.Header.Set("ChatGPT-Account-ID", acct.AccountID)
	}

	// Set body.
	upReq.Body = io.NopCloser(bytes.NewReader(translated))
	upReq.ContentLength = int64(len(translated))

	// Send to upstream.
	transport := s.codexTransport()
	resp, err := transport.RoundTrip(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("codex upstream error: %v", err))
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "cq: proxy POST %s → %d (codex)\n", upstreamURL, resp.StatusCode)

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

func (s *Server) codexTransport() http.RoundTripper {
	if s.CodexTransport != nil {
		return s.CodexTransport
	}
	return http.DefaultTransport
}

// extractStream does a quick JSON parse to check the "stream" field.
func extractStream(body []byte) bool {
	var partial struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &partial)
	return partial.Stream
}
