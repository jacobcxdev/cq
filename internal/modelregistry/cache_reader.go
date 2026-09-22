package modelregistry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// LoadCodexEntriesFromCache reads a previously-published Codex models_cache.json
// envelope and returns the models as native Entry records. A missing file
// returns (nil, nil). Malformed JSON returns an error.
func LoadCodexEntriesFromCache(fsys fsutil.FileSystem, path string) ([]Entry, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read codex cache %s: %w", path, err)
	}
	var envelope codexCacheEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse codex cache %s: %w", path, err)
	}
	if entries, ok, err := cachedNatives(envelope.CQNative, ProviderCodex, envelope.Models); ok || err != nil {
		if err != nil {
			return nil, err
		}
		rawByID := map[string]json.RawMessage{}
		for _, raw := range envelope.Models {
			var info codexModelInfo
			if json.Unmarshal(raw, &info) != nil || ValidateModelID(info.Slug) != nil {
				return nil, fmt.Errorf("invalid Codex model cache entry")
			}
			rawByID[info.Slug] = raw
		}
		for i := range entries {
			raw, exists := rawByID[entries[i].ID]
			if !exists {
				return nil, fmt.Errorf("invalid native cache provenance")
			}
			entries[i].Raw = raw
		}
		return entries, nil
	}
	if envelope.Models == nil {
		return nil, fmt.Errorf("invalid Codex model cache envelope")
	}
	entries := make([]Entry, 0, len(envelope.Models))
	for _, raw := range envelope.Models {
		var info codexModelInfo
		if err := json.Unmarshal(raw, &info); err != nil || ValidateModelID(info.Slug) != nil {
			return nil, fmt.Errorf("invalid Codex model cache entry")
		}
		entries = append(entries, Entry{
			Provider:         ProviderCodex,
			ID:               info.Slug,
			DisplayName:      info.DisplayName,
			Description:      info.Description,
			ContextWindow:    info.ContextWindow,
			MaxContextWindow: info.MaxContextWindow,
			Priority:         info.Priority,
			PriorityKnown:    info.PriorityKnown,
			Visibility:       info.Visibility,
			Source:           SourceNative,
			Raw:              raw,
		})
	}
	return entries, nil
}

// LoadClaudeEntriesFromCapabilities reads Claude Code's model-capabilities
// cache and returns the models as native Entry records. A missing file
// returns (nil, nil). Malformed JSON returns an error.
func LoadClaudeEntriesFromCapabilities(fsys fsutil.FileSystem, path string) ([]Entry, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read claude capabilities %s: %w", path, err)
	}
	var cache struct {
		Models   []json.RawMessage      `json:"models"`
		CQNative *cacheNativeProvenance `json:"cq_native"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parse claude capabilities %s: %w", path, err)
	}
	if entries, ok, err := cachedNatives(cache.CQNative, ProviderAnthropic, cache.Models); ok || err != nil {
		return entries, err
	}
	if cache.Models == nil {
		return nil, fmt.Errorf("invalid Claude model cache envelope")
	}
	entries := make([]Entry, 0, len(cache.Models))
	for _, raw := range cache.Models {
		var m claudeCapability
		if json.Unmarshal(raw, &m) != nil || ValidateModelID(m.ID) != nil {
			return nil, fmt.Errorf("invalid Claude model cache entry")
		}
		entries = append(entries, Entry{
			Provider:        ProviderAnthropic,
			ID:              m.ID,
			ContextWindow:   m.MaxInputTokens,
			MaxOutputTokens: m.MaxTokens,
			Source:          SourceNative,
		})
	}
	return entries, nil
}

// Provenance is bound to the published model payload. Client rewrites invalidate
// it, so stale CQ metadata can never supersede a newer vendor cache.
type cacheNativeProvenance struct {
	Digest  string  `json:"digest"`
	Entries []Entry `json:"entries"`
}

func cachePayloadDigest(models any) string {
	data, err := json.Marshal(models)
	if err != nil {
		return ""
	}
	// Hash every field, including unknown vendor metadata, independently of
	// object key order and whitespace. Preserve numbers without float rounding.
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return ""
	}
	data, _ = json.Marshal(payload)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
func nativeProvenance(snap Snapshot, provider Provider, models any) *cacheNativeProvenance {
	p := &cacheNativeProvenance{Digest: cachePayloadDigest(models), Entries: []Entry{}}
	for _, e := range snap.Entries {
		if e.Provider == provider && e.Source == SourceNative {
			e.Raw = nil
			p.Entries = append(p.Entries, e)
		}
	}
	return p
}
func cachedNatives(p *cacheNativeProvenance, provider Provider, models []json.RawMessage) ([]Entry, bool, error) {
	if p == nil {
		return nil, false, nil
	}
	decoded, decodeErr := hex.DecodeString(p.Digest)
	if decodeErr != nil || len(decoded) != 32 || p.Entries == nil {
		return nil, true, fmt.Errorf("invalid native cache provenance")
	}
	if p.Digest != cachePayloadDigest(models) {
		return nil, false, nil
	}
	claudeIDs := map[string]bool{}
	if provider == ProviderAnthropic {
		for _, raw := range models {
			var m claudeCapability
			if json.Unmarshal(raw, &m) != nil || ValidateModelID(m.ID) != nil {
				return nil, true, fmt.Errorf("invalid Claude model cache entry")
			}
			claudeIDs[m.ID] = true
		}
	}
	for _, e := range p.Entries {
		if e.Provider != provider || e.Source != SourceNative || e.Validate() != nil || ValidateModelID(e.ID) != nil || e.ContextWindow < 0 || e.MaxContextWindow < 0 || e.MaxOutputTokens < 0 {
			return nil, true, fmt.Errorf("invalid native cache provenance")
		}
		if provider == ProviderAnthropic && !claudeIDs[e.ID] {
			return nil, true, fmt.Errorf("invalid native cache provenance")
		}
	}
	return p.Entries, true, nil
}
