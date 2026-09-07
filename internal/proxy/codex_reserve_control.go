package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jacobcxdev/cq/internal/quota"
)

const RuntimeReservePath = "/_cq/control/reserve"

type CodexReserveControlRequest struct {
	Action  string           `json:"action"`
	Window  quota.WindowName `json:"window,omitempty"`
	Percent float64          `json:"percent,omitempty"`
}

func (s *Server) handleReserveControl(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.Reserve == nil {
		http.Error(w, "system account reserve unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		writePolicyControlJSON(w, s.Reserve.Status())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(body) > 4096 {
		http.Error(w, "invalid reserve control", http.StatusBadRequest)
		return
	}
	var mutation CodexReserveControlRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&mutation) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		http.Error(w, "invalid reserve control", http.StatusBadRequest)
		return
	}
	if mutation.Action != "set" && (mutation.Window != "" || mutation.Percent != 0) {
		http.Error(w, "invalid reserve control", http.StatusBadRequest)
		return
	}
	status, err := s.Reserve.Control(mutation.Action, mutation.Window, mutation.Percent)
	if err != nil {
		http.Error(w, "reserve control rejected", http.StatusConflict)
		return
	}
	writePolicyControlJSON(w, status)
}
