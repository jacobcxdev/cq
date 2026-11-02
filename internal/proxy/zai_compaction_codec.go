package proxy

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	zaiCompactionPrefix          = "cq_compaction_v1:"
	zaiCompactionMaxSummaryBytes = 128 << 10
)

func sealZAICompaction(localToken, summary string) (json.RawMessage, error) {
	if !validZAICompactionSummary(summary) {
		return nil, errors.New("invalid CQ compaction summary")
	}
	aead, err := zaiCompactionAEAD(localToken)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("create CQ compaction nonce")
	}
	sealed := aead.Seal(append([]byte(nil), nonce...), nonce, []byte(summary), []byte(zaiCompactionPrefix))
	return json.Marshal(struct {
		Type             string `json:"type"`
		ID               string `json:"id"`
		EncryptedContent string `json:"encrypted_content"`
	}{"compaction", "cq_zai_compaction_" + hex.EncodeToString(nonce), zaiCompactionPrefix + base64.RawURLEncoding.EncodeToString(sealed)})
}

// Saved CQ compaction state requires the existing LocalToken to survive upgrades
// and reinstalls. Changing accounts preserves access; rotating or losing that
// proxy token makes previously sealed state unreadable.
func zaiCompactionAEAD(localToken string) (cipher.AEAD, error) {
	if localToken == "" {
		return nil, errors.New("CQ compaction key unavailable")
	}
	key := sha256.Sum256([]byte("cq:zai:compaction:v1\x00" + localToken))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, errors.New("initialise CQ compaction encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialise CQ compaction encryption")
	}
	return aead, nil
}

func validZAICompactionSummary(summary string) bool {
	return len(summary) <= zaiCompactionMaxSummaryBytes && utf8.ValidString(summary) && strings.TrimSpace(summary) != ""
}

func openZAICompaction(localToken, encrypted string) (string, error) {
	aead, err := zaiCompactionAEAD(localToken)
	if err != nil {
		return "", err
	}
	encoded := strings.TrimPrefix(encrypted, zaiCompactionPrefix)
	limit := base64.RawURLEncoding.EncodedLen(aead.NonceSize() + aead.Overhead() + zaiCompactionMaxSummaryBytes)
	if len(encoded) > limit {
		return "", errors.New("invalid CQ compaction state")
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() || base64.RawURLEncoding.EncodeToString(sealed) != encoded {
		return "", errors.New("invalid CQ compaction state")
	}
	nonce := sealed[:aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[aead.NonceSize():], []byte(zaiCompactionPrefix))
	if err != nil || !validZAICompactionSummary(string(plain)) {
		return "", errors.New("invalid CQ compaction state")
	}
	return string(plain), nil
}

// CQ-owned compaction state is restored only in the top-level input sequence.
// Foreign opaque state and requests without CQ items retain their exact bytes.
// The selected provider owns the overall request budget; only individual
// encrypted summaries are bounded here.
func normaliseCQCompactionInput(body []byte, localToken string) ([]byte, bool, error) {
	// Ordinary Codex requests have their own validation and larger body budget.
	// Unicode escapes may encode the CQ marker, so probe those rare requests too.
	if !bytes.Contains(body, []byte(zaiCompactionPrefix)) && !bytes.Contains(body, []byte(`\u`)) {
		return body, false, nil
	}
	owned, err := hasCQCompactionInput(body)
	if err != nil {
		return nil, false, errors.New("invalid CQ compaction request")
	}
	if !owned {
		return body, false, nil
	}
	err = validateCodexJSONAuthority(body, "CQ compaction input", func(path []string, name string) bool {
		if len(path) == 0 {
			return codexJSONNameEqual(name, "input")
		}
		return len(path) == 1 && path[0] == "input" && (codexJSONNameEqual(name, "type") || codexJSONNameEqual(name, "id") || codexJSONNameEqual(name, "encrypted_content"))
	})
	if err != nil {
		return nil, false, errors.New("invalid CQ compaction request")
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return nil, false, errors.New("invalid CQ compaction request")
	}
	raw, ok := codexJSONRawField(request, "input")
	if !ok || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte{'['}) {
		return body, false, nil
	}
	var input []json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		return nil, false, errors.New("invalid CQ compaction input")
	}
	changed := false
	for i, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			continue
		}
		encryptedRaw, _ := codexJSONRawField(item, "encrypted_content")
		var encrypted string
		if json.Unmarshal(encryptedRaw, &encrypted) != nil || !strings.HasPrefix(encrypted, zaiCompactionPrefix) {
			continue
		}
		typeRaw, _ := codexJSONRawField(item, "type")
		var kind string
		if json.Unmarshal(typeRaw, &kind) != nil || kind == "" {
			return nil, false, errors.New("invalid CQ compaction item")
		}
		if kind != "compaction" {
			continue
		}
		if idRaw, exists := codexJSONRawField(item, "id"); exists {
			var id *string
			if json.Unmarshal(idRaw, &id) != nil {
				return nil, false, errors.New("invalid CQ compaction item")
			}
		}
		summary, err := openZAICompaction(localToken, encrypted)
		if err != nil {
			return nil, false, err
		}
		input[i], err = json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": summary}}})
		if err != nil {
			return nil, false, errors.New("encode CQ compaction input")
		}
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	updated, err := json.Marshal(input)
	if err != nil {
		return nil, false, errors.New("encode CQ compaction input")
	}
	// Preserve the original field spelling rather than creating conflicting input authority.
	for key := range request {
		if codexJSONNameEqual(key, "input") {
			request[key] = updated
		}
	}
	updated, err = json.Marshal(request)
	if err != nil {
		return nil, false, errors.New("encode CQ compaction request")
	}
	return updated, true, nil
}

// Keep duplicate fields while probing ownership; otherwise a later input/type
// field could hide an earlier CQ carrier before strict authority validation.
type cqCompactionJSONField struct {
	name  string
	value json.RawMessage
}

func cqCompactionObjectFields(raw []byte) ([]cqCompactionJSONField, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte{'{'}) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var fields []cqCompactionJSONField
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, cqCompactionJSONField{name.(string), value})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid compaction object")
	}
	return fields, nil
}

func hasCQCompactionInput(body []byte) (bool, error) {
	fields, err := cqCompactionObjectFields(body)
	if err != nil {
		return false, err
	}
	for _, field := range fields {
		if !codexJSONNameEqual(field.name, "input") || !bytes.HasPrefix(bytes.TrimSpace(field.value), []byte{'['}) {
			continue
		}
		var input []json.RawMessage
		if json.Unmarshal(field.value, &input) != nil {
			return false, errors.New("invalid compaction input")
		}
		for _, item := range input {
			fields, err := cqCompactionObjectFields(item)
			if err != nil {
				return false, err
			}
			hasType, badType, compaction, owned := false, false, false, false
			for _, field := range fields {
				var value string
				switch {
				case codexJSONNameEqual(field.name, "type"):
					hasType = true
					if json.Unmarshal(field.value, &value) != nil || value == "" {
						badType = true
					} else if value == "compaction" {
						compaction = true
					}
				case codexJSONNameEqual(field.name, "encrypted_content"):
					if json.Unmarshal(field.value, &value) == nil && strings.HasPrefix(value, zaiCompactionPrefix) {
						owned = true
					}
				}
			}
			if owned && (compaction || badType || !hasType) {
				return true, nil
			}
		}
	}
	return false, nil
}
