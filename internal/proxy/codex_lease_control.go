package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const CodexLeaseErrorHeader = "X-CQ-Lease-Error"

const RuntimeCodexLeaseInvalidationPath = "/_cq/control/codex/leases/invalidate"

type CodexLeaseInvalidator interface {
	InvalidateTaskAffinities(context.Context) (CodexLeaseInvalidationResult, error)
}

func (s *Server) handleCodexLeaseInvalidation(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.CodexLeaseInvalidator == nil {
		writer.Header().Set(CodexLeaseErrorHeader, "lease_journal_unavailable")
		http.Error(writer, "Codex lease invalidation unavailable", http.StatusServiceUnavailable)
		return
	}
	result, err := s.CodexLeaseInvalidator.InvalidateTaskAffinities(request.Context())
	if err != nil {
		writer.Header().Set(CodexLeaseErrorHeader, codexLeaseInvalidationErrorCode(err))
		http.Error(writer, "Codex lease invalidation unavailable", http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(result); err != nil {
		http.Error(writer, "Codex lease invalidation response unavailable", http.StatusInternalServerError)
	}
}

// Receipts classify existing failures without changing the legacy HTTP response.
func codexLeaseInvalidationErrorCode(err error) string {
	var commit *fsutil.CommitError
	switch {
	case errors.As(err, &commit):
		return "routing_io_failed"
	case errors.Is(err, ErrCodexLeaseWriterUnavailable), errors.Is(err, ErrCodexLeaseStorePoisoned), errors.Is(err, ErrCodexLegacyQuarantine), errors.Is(err, ErrCodexLeaseTrustLost):
		return "lease_journal_unavailable"
	case errors.Is(err, ErrCodexLeaseStaleMutation), errors.Is(err, ErrCodexLeaseAuthorityMismatch), errors.Is(err, ErrCodexLeaseInvalidMutation):
		return "routing_conflict"
	default:
		return "routing_io_failed"
	}
}
