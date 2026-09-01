package proxy

import (
	"context"
	"encoding/json"
	"net/http"
)

const RuntimeCodexLeaseInvalidationPath = "/_cq/control/codex/leases/invalidate"

type CodexLeaseInvalidator interface {
	InvalidateTaskAffinities(context.Context) (CodexLeaseInvalidationResult, error)
}

func (s *Server) handleCodexLeaseInvalidation(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.CodexLeaseInvalidator == nil {
		http.Error(writer, "Codex lease invalidation unavailable", http.StatusServiceUnavailable)
		return
	}
	result, err := s.CodexLeaseInvalidator.InvalidateTaskAffinities(request.Context())
	if err != nil {
		http.Error(writer, "Codex lease invalidation unavailable", http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(result); err != nil {
		http.Error(writer, "Codex lease invalidation response unavailable", http.StatusInternalServerError)
	}
}
