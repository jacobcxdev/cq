package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

func (s *Server) doCodexRequest(ctx context.Context, requestedModel string, req *http.Request, additionalModels ...string) (*http.Response, RouteChoice, CandidateAttempt, error) {
	if s == nil {
		return nil, RouteChoice{}, CandidateAttempt{}, fmt.Errorf("no Codex accounts configured")
	}
	// Compatibility seam for embedders supplying an already-authenticated
	// transport. CQ production always uses CodexRequests.
	if s.CodexRequests == nil {
		if s.CodexTransport == nil {
			return nil, RouteChoice{}, CandidateAttempt{}, fmt.Errorf("no Codex accounts configured")
		}
		response, err := s.CodexTransport.RoundTrip(req)
		return response, RouteChoice{}, CandidateAttempt{}, err
	}
	return s.CodexRequests.Do(ctx, CodexRouteRequirements{
		RequestedModel: requestedModel,
		RequiredModels: additionalModels,
	}, req)
}

func (s *Server) codexHTTPAvailable() bool {
	return s != nil && (s.CodexRequests != nil || s.CodexTransport != nil)
}

func relayCodexHTTPResponse(w http.ResponseWriter, response *http.Response, flush bool) error {
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if !flush {
		_, err := io.Copy(w, response.Body)
		return err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, err := io.Copy(w, response.Body)
		return err
	}
	buffer := make([]byte, 4096)
	for {
		read, err := response.Body.Read(buffer)
		if read > 0 {
			if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
				return writeErr
			}
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
