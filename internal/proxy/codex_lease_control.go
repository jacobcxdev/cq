package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/codex"
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

const RuntimeCodexLeaseRedistributionPath = "/_cq/control/codex/leases/redistribute"

type CodexLeaseRedistributor interface {
	RedistributeTaskAffinities(context.Context) (CodexLeaseRedistributionResult, error)
	RedistributeTaskAffinitiesForReset(context.Context, codex.AccountKey, string) (CodexLeaseRedistributionResult, error)
}

// CodexLeaseResetRequest identifies a reset already consumed by the CLI.
// Empty request bodies are the independent manual redistribution control.
type CodexLeaseResetRequest struct {
	AccountKey codex.AccountKey `json:"reset_account_key"`
	EventID    string           `json:"event_id"`
}

func (s *Server) handleCodexLeaseRedistribution(writer http.ResponseWriter, request *http.Request) {
	var reset CodexLeaseResetRequest
	if request.Body != nil {
		body, err := httputil.ReadBody(request.Body)
		if err != nil {
			http.Error(writer, "Codex redistribution request invalid", http.StatusBadRequest)
			return
		}
		if len(bytes.TrimSpace(body)) != 0 {
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&reset) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
				reset.AccountKey == "" || reset.EventID == "" || len(reset.AccountKey) > 256 || len(reset.EventID) > 256 ||
				strings.TrimSpace(string(reset.AccountKey)) != string(reset.AccountKey) || strings.TrimSpace(reset.EventID) != reset.EventID {
				http.Error(writer, "Codex redistribution request invalid", http.StatusBadRequest)
				return
			}
		}
	}
	if s == nil || s.CodexLeaseRedistributor == nil {
		http.Error(writer, "Codex lease redistribution unavailable", http.StatusServiceUnavailable)
		return
	}
	var result CodexLeaseRedistributionResult
	var err error
	if reset.AccountKey != "" {
		if s.CodexResetCapacityRefresh == nil || s.CodexResetCapacityRefresh(request.Context(), reset.AccountKey) != nil {
			http.Error(writer, "Codex reset capacity unavailable", http.StatusServiceUnavailable)
			return
		}
		result, err = s.CodexLeaseRedistributor.RedistributeTaskAffinitiesForReset(request.Context(), reset.AccountKey, reset.EventID)
	} else {
		result, err = s.CodexLeaseRedistributor.RedistributeTaskAffinities(request.Context())
	}
	if err != nil {
		http.Error(writer, "Codex lease redistribution unavailable", http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(result); err != nil {
		http.Error(writer, "Codex lease redistribution response unavailable", http.StatusInternalServerError)
	}
}
