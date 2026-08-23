package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const (
	RuntimeCodexTurnReceiptPath   = "/_cq/control/codex/turn-receipt"
	RuntimeCodexTurnReceiptV2Path = "/_cq/control/codex/turn-receipt/v2"
	codexTurnReceiptRequestMax    = 16 << 10
)

func (s *Server) handleCodexTurnReceipt(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.CodexTurnReceipts == nil {
		http.Error(writer, "Codex turn receipts unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, codexTurnReceiptRequestMax+1))
	if err != nil || len(body) > codexTurnReceiptRequestMax {
		zeroRuntimeBytes(body)
		http.Error(writer, "invalid Codex turn receipt lookup", http.StatusBadRequest)
		return
	}
	defer zeroRuntimeBytes(body)
	var input struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(writer, "invalid Codex turn receipt lookup", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(writer, "invalid Codex turn receipt lookup", http.StatusBadRequest)
		return
	}
	session := []byte(input.SessionID)
	turn := []byte(input.TurnID)
	input.SessionID = ""
	input.TurnID = ""
	defer zeroRuntimeBytes(session)
	defer zeroRuntimeBytes(turn)
	if !validCanonicalSessionID(session) || !validCanonicalSessionID(turn) {
		http.Error(writer, "invalid Codex turn receipt lookup", http.StatusBadRequest)
		return
	}
	receipt, found := s.CodexTurnReceipts.lookup(session, turn)
	if request.URL.Path == RuntimeCodexTurnReceiptV2Path {
		response := CodexTurnReceiptLookupV2{SchemaVersion: 2, Found: found}
		if found {
			response.Receipt = &receipt
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			http.Error(writer, "Codex turn receipt response unavailable", http.StatusInternalServerError)
		}
		return
	}
	response := CodexTurnReceiptLookupV1{SchemaVersion: 1, Found: found}
	if found {
		response.Receipt = &receipt.CodexTurnReceiptV1
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		http.Error(writer, "Codex turn receipt response unavailable", http.StatusInternalServerError)
	}
}
