package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SanitisedCodexFixture struct {
	SchemaVersion  int                     `json:"schema_version"`
	CapturedAt     time.Time               `json:"captured_at"`
	BodyBytes      int                     `json:"body_bytes"`
	BodyHash       string                  `json:"body_hash"`
	MetadataSource CodexTurnMetadataSource `json:"metadata_source"`
	RequestKind    CodexRequestKind        `json:"request_kind,omitempty"`
	SessionHint    string                  `json:"session_hint,omitempty"`
	ThreadHint     string                  `json:"thread_hint,omitempty"`
	TurnHint       string                  `json:"turn_hint,omitempty"`
	HasPreviousID  bool                    `json:"has_previous_response_id,omitempty"`
	HasEncrypted   bool                    `json:"has_encrypted_state,omitempty"`
}

func BuildSanitisedCodexFixture(body []byte, contentEncoding, directMetadata string, now time.Time) (SanitisedCodexFixture, error) {
	decoded, err := DecodeCodexRequest(body, contentEncoding, DefaultCodexZstdLimits)
	if err != nil {
		return SanitisedCodexFixture{}, err
	}
	request, err := ParseCodexProtocolRequest(decoded.Decoded(), directMetadata, nil)
	if err != nil {
		return SanitisedCodexFixture{}, err
	}
	if !request.Metadata.Found {
		return SanitisedCodexFixture{}, errors.New("Codex fixture has no turn metadata")
	}
	sum := sha256.Sum256(body)
	metadata := request.Metadata.Metadata
	return SanitisedCodexFixture{
		SchemaVersion:  CurrentCodexParserSchema,
		CapturedAt:     now.UTC(),
		BodyBytes:      len(body),
		BodyHash:       hex.EncodeToString(sum[:]),
		MetadataSource: request.Metadata.Source,
		RequestKind:    metadata.RequestKind,
		SessionHint:    hashPrefix("session", metadata.SessionID),
		ThreadHint:     hashPrefix("thread", metadata.ThreadID),
		TurnHint:       hashPrefix("turn", metadata.TurnID),
		HasPreviousID:  request.PreviousResponseID != "",
		HasEncrypted:   request.HasEncryptedState,
	}, nil
}

func WriteSanitisedCodexFixture(path string, fixture SanitisedCodexFixture) error {
	if path == "" {
		return errors.New("Codex fixture path required")
	}
	data, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sanitised Codex fixture: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Codex fixture directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write sanitised Codex fixture: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace sanitised Codex fixture: %w", err)
	}
	return nil
}
