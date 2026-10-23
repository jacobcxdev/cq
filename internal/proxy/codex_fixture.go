package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

type SanitisedCodexFixture struct {
	SchemaVersion  int                     `json:"schema_version"`
	CapturedAt     time.Time               `json:"captured_at"`
	BodyBytes      int                     `json:"body_bytes"`
	BodyHash       string                  `json:"body_hash"`
	MetadataSource CodexTurnMetadataSource `json:"metadata_source"`
	RequestKind    CodexRequestKind        `json:"request_kind"`
	SessionHint    string                  `json:"session_hint"`
	ThreadHint     string                  `json:"thread_hint"`
	TurnHint       string                  `json:"turn_hint"`
	HasPreviousID  bool                    `json:"has_previous_response_id"`
	HasEncrypted   bool                    `json:"has_encrypted_state"`
}

func BuildSanitisedCodexFixture(body []byte, contentEncoding, directMetadata string, now time.Time) (SanitisedCodexFixture, error) {
	if contentEncoding == "auto" {
		contentEncoding = ""
	}
	decoded, err := DecodeCodexRequest(body, contentEncoding, DefaultCodexZstdLimits)
	if err != nil {
		return SanitisedCodexFixture{}, err
	}
	metadataResult, err := strictCodexFixtureMetadata(decoded.Decoded(), directMetadata)
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
	request.Metadata = metadataResult
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
	tmp, err := os.CreateTemp(dir, ".cq-fixture-*")
	if err != nil {
		return fmt.Errorf("create sanitised Codex fixture: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Linking a complete temporary file is an atomic no-replace publication.
	if err := os.Link(tmp.Name(), path); err != nil {
		return fmt.Errorf("create sanitised Codex fixture: %w", err)
	}
	return nil
}

// Fixture input is intentionally stricter than the live vendor protocol.
func fixtureObject(raw []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid fixture UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("fixture object required")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid fixture key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate fixture key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing fixture JSON")
	}
	return fields, nil
}

func strictFixtureMetadata(raw []byte, nested bool) (CodexTurnMetadata, error) {
	if !utf8.Valid(raw) || len(raw) > codexTurnMetadataMaxBytes {
		return CodexTurnMetadata{}, errors.New("invalid fixture metadata size or UTF-8")
	}
	if nested && len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return CodexTurnMetadata{}, err
		}
		raw = []byte(encoded)
	}
	if len(raw) > codexTurnMetadataMaxBytes {
		return CodexTurnMetadata{}, errors.New("fixture metadata exceeds limit")
	}
	fields, err := fixtureObject(raw)
	if err != nil {
		return CodexTurnMetadata{}, err
	}
	for key, value := range fields {
		switch key {
		case "session_id", "thread_id", "turn_id", "window_id", "request_kind", "compaction":
		default:
			return CodexTurnMetadata{}, errors.New("unknown fixture metadata field")
		}
		var text string
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil {
			return CodexTurnMetadata{}, errors.New("fixture metadata requires strings")
		}
	}
	var metadata CodexTurnMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return metadata, err
	}
	if _, present := fields["compaction"]; present && metadata.RequestKind != CodexRequestCompaction {
		return metadata, errors.New("unexpected fixture compaction")
	}
	return metadata, validateCodexTurnMetadata(metadata)
}

func strictCodexFixtureMetadata(body []byte, explicit string) (CodexTurnMetadataResult, error) {
	envelope, err := fixtureObject(body)
	if err != nil {
		return CodexTurnMetadataResult{}, err
	}
	result := CodexTurnMetadataResult{Source: CodexTurnMetadataNone}
	accept := func(raw []byte, source CodexTurnMetadataSource) error {
		metadata, err := strictFixtureMetadata(raw, source == CodexTurnMetadataNested)
		if err != nil {
			return err
		}
		if result.Found && metadata != result.Metadata {
			return errors.New("conflicting fixture metadata")
		}
		result = CodexTurnMetadataResult{Metadata: metadata, Source: source, Found: true, Strong: true}
		return nil
	}
	if raw, ok := envelope["client_metadata"]; ok {
		client, err := fixtureObject(raw)
		if err != nil {
			return result, err
		}
		nested, hasNested := client[codexTurnMetadataKey]
		delete(client, codexTurnMetadataKey)
		if len(client) > 0 || !hasNested {
			flat, _ := json.Marshal(client)
			if err := accept(flat, CodexTurnMetadataFlat); err != nil {
				return result, err
			}
		}
		if hasNested {
			if err := accept(nested, CodexTurnMetadataNested); err != nil {
				return result, err
			}
		}
	}
	if explicit != "" {
		if err := accept([]byte(explicit), CodexTurnMetadataHeader); err != nil {
			return result, err
		}
	}
	if !result.Found {
		return result, errors.New("Codex fixture has no turn metadata")
	}
	return result, nil
}
