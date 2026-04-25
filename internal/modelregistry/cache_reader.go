package modelregistry

import (
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
	entries := make([]Entry, 0, len(envelope.Models))
	for _, raw := range envelope.Models {
		var info codexModelInfo
		if err := json.Unmarshal(raw, &info); err != nil || info.Slug == "" {
			continue
		}
		entries = append(entries, Entry{
			Provider:         ProviderCodex,
			ID:               info.Slug,
			DisplayName:      info.DisplayName,
			Description:      info.Description,
			ContextWindow:    info.ContextWindow,
			MaxContextWindow: info.MaxContextWindow,
			Priority:         info.Priority,
			Visibility:       info.Visibility,
			Source:           SourceNative,
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
		Models []claudeCapability `json:"models"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parse claude capabilities %s: %w", path, err)
	}
	entries := make([]Entry, 0, len(cache.Models))
	for _, m := range cache.Models {
		if m.ID == "" {
			continue
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
