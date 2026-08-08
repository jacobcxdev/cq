package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	codexTurnMetadataKey      = "x-codex-turn-metadata"
	codexTurnMetadataMaxBytes = 64 << 10
	codexTurnIDMaxBytes       = 4 << 10
)

type CodexRequestKind string

const (
	CodexRequestTurn       CodexRequestKind = "turn"
	CodexRequestPrewarm    CodexRequestKind = "prewarm"
	CodexRequestCompaction CodexRequestKind = "compaction"
	CodexRequestMemory     CodexRequestKind = "memory"
)

type CodexCompactionPhase string

const (
	CodexCompactionStandalone CodexCompactionPhase = "standalone_turn"
	CodexCompactionPreTurn    CodexCompactionPhase = "pre_turn"
	CodexCompactionMidTurn    CodexCompactionPhase = "mid_turn"
)

type CodexTurnMetadataSource string

const (
	CodexTurnMetadataNone      CodexTurnMetadataSource = "none"
	CodexTurnMetadataNested    CodexTurnMetadataSource = "nested"
	CodexTurnMetadataHeader    CodexTurnMetadataSource = "header"
	CodexTurnMetadataFlat      CodexTurnMetadataSource = "flat"
	CodexTurnMetadataHandshake CodexTurnMetadataSource = "handshake"
)

type CodexTurnMetadata struct {
	SessionID       string               `json:"session_id"`
	ThreadID        string               `json:"thread_id"`
	TurnID          string               `json:"turn_id"`
	WindowID        string               `json:"window_id,omitempty"`
	RequestKind     CodexRequestKind     `json:"request_kind"`
	CompactionPhase CodexCompactionPhase `json:"compaction,omitempty"`
}

type CodexTurnMetadataResult struct {
	Metadata CodexTurnMetadata
	Source   CodexTurnMetadataSource
	Found    bool
	Strong   bool
}

func ParseCodexTurnMetadata(body []byte, directHeader string, handshake *CodexTurnMetadata) (CodexTurnMetadataResult, error) {
	var envelope struct {
		ClientMetadata json.RawMessage `json:"client_metadata"`
	}
	if len(body) != 0 {
		if err := json.Unmarshal(body, &envelope); err != nil {
			return CodexTurnMetadataResult{}, fmt.Errorf("decode Codex request metadata envelope: %w", err)
		}
	}

	if len(envelope.ClientMetadata) != 0 && !bytes.Equal(envelope.ClientMetadata, []byte("null")) {
		var client map[string]json.RawMessage
		if err := json.Unmarshal(envelope.ClientMetadata, &client); err != nil {
			return CodexTurnMetadataResult{}, fmt.Errorf("decode Codex client metadata: %w", err)
		}
		if raw, ok := client[codexTurnMetadataKey]; ok {
			metadata, err := decodeCodexTurnMetadata(raw)
			return codexTurnMetadataResult(metadata, CodexTurnMetadataNested, true, err)
		}
	}

	if directHeader != "" {
		if len(directHeader) > codexTurnMetadataMaxBytes {
			return CodexTurnMetadataResult{}, errors.New("Codex turn metadata header exceeds limit")
		}
		metadata, err := decodeCodexTurnMetadata([]byte(directHeader))
		return codexTurnMetadataResult(metadata, CodexTurnMetadataHeader, true, err)
	}

	if len(envelope.ClientMetadata) != 0 && !bytes.Equal(envelope.ClientMetadata, []byte("null")) {
		var metadata CodexTurnMetadata
		if err := json.Unmarshal(envelope.ClientMetadata, &metadata); err != nil {
			return CodexTurnMetadataResult{}, fmt.Errorf("decode flat Codex turn metadata: %w", err)
		}
		if metadataHasAnyField(metadata) {
			return codexTurnMetadataResult(metadata, CodexTurnMetadataFlat, true, nil)
		}
	}

	if handshake != nil && metadataHasAnyField(*handshake) {
		return codexTurnMetadataResult(*handshake, CodexTurnMetadataHandshake, false, nil)
	}
	return CodexTurnMetadataResult{Source: CodexTurnMetadataNone}, nil
}

func decodeCodexTurnMetadata(raw []byte) (CodexTurnMetadata, error) {
	if len(raw) > codexTurnMetadataMaxBytes {
		return CodexTurnMetadata{}, errors.New("Codex turn metadata exceeds limit")
	}
	var encoded string
	if len(raw) != 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return CodexTurnMetadata{}, fmt.Errorf("decode Codex turn metadata string: %w", err)
		}
		raw = []byte(encoded)
		if len(raw) > codexTurnMetadataMaxBytes {
			return CodexTurnMetadata{}, errors.New("Codex turn metadata exceeds limit")
		}
	}
	var metadata CodexTurnMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return CodexTurnMetadata{}, fmt.Errorf("decode Codex turn metadata: %w", err)
	}
	return metadata, nil
}

func codexTurnMetadataResult(metadata CodexTurnMetadata, source CodexTurnMetadataSource, strong bool, err error) (CodexTurnMetadataResult, error) {
	if err != nil {
		return CodexTurnMetadataResult{}, err
	}
	if err := validateCodexTurnMetadata(metadata); err != nil {
		return CodexTurnMetadataResult{}, err
	}
	return CodexTurnMetadataResult{Metadata: metadata, Source: source, Found: true, Strong: strong}, nil
}

func validateCodexTurnMetadata(metadata CodexTurnMetadata) error {
	for name, value := range map[string]string{
		"session_id": metadata.SessionID,
		"thread_id":  metadata.ThreadID,
		"turn_id":    metadata.TurnID,
		"window_id":  metadata.WindowID,
	} {
		if len(value) > codexTurnIDMaxBytes {
			return fmt.Errorf("Codex %s exceeds limit", name)
		}
	}

	switch metadata.RequestKind {
	case CodexRequestTurn:
		if metadata.SessionID == "" || metadata.ThreadID == "" || metadata.TurnID == "" {
			return errors.New("Codex turn metadata requires session_id, thread_id, and turn_id")
		}
	case CodexRequestPrewarm:
		if metadata.SessionID == "" || metadata.ThreadID == "" || metadata.TurnID != "" {
			return errors.New("Codex prewarm metadata requires session_id and thread_id with empty turn_id")
		}
	case CodexRequestCompaction:
		if metadata.SessionID == "" || metadata.ThreadID == "" || metadata.TurnID == "" {
			return errors.New("Codex compaction metadata requires session_id, thread_id, and turn_id")
		}
	case CodexRequestMemory:
		// Memory requests deliberately may not identify a turn.
	default:
		return fmt.Errorf("unknown Codex request_kind %q", metadata.RequestKind)
	}
	return nil
}

func metadataHasAnyField(metadata CodexTurnMetadata) bool {
	return metadata.SessionID != "" || metadata.ThreadID != "" || metadata.TurnID != "" ||
		metadata.WindowID != "" || metadata.RequestKind != "" || metadata.CompactionPhase != ""
}
