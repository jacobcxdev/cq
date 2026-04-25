package modelregistry

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// claudeCapability is the shape Claude Code's model-capabilities cache expects.
type claudeCapability struct {
	ID             string `json:"id"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
}

// claudeCapabilitiesCache is the on-disk format of Claude Code's
// model-capabilities cache. The timestamp must be a numeric Unix second.
type claudeCapabilitiesCache struct {
	Timestamp int64              `json:"timestamp"`
	Models    []claudeCapability `json:"models"`
}

// ClaudeCapabilitiesProjection builds the model-capabilities payload from the
// snapshot, using the given Unix timestamp. Only Anthropic entries are included.
// The models slice is ordered longest-ID-first, with ties broken lexicographically.
func ClaudeCapabilitiesProjection(snap Snapshot, timestamp int64) claudeCapabilitiesCache {
	models := make([]claudeCapability, 0)
	for _, e := range snap.Entries {
		if e.Provider != ProviderAnthropic {
			continue
		}
		models = append(models, claudeCapability{
			ID:             e.ID,
			MaxInputTokens: e.ContextWindow,
			MaxTokens:      e.MaxOutputTokens,
		})
	}

	sort.Slice(models, func(i, j int) bool {
		if len(models[i].ID) != len(models[j].ID) {
			return len(models[i].ID) > len(models[j].ID)
		}
		return models[i].ID < models[j].ID
	})

	return claudeCapabilitiesCache{
		Timestamp: timestamp,
		Models:    models,
	}
}

// PublishClaudeCapabilities writes the model-capabilities cache to path using
// an atomic tmp+rename write. Parent directories are created with 0o700; the
// file is written with 0o600.
//
// The existing cache is not read — the snapshot is the sole source of truth.
// Any previous string-formatted timestamp is replaced by a numeric Unix second.
func PublishClaudeCapabilities(fsys fsutil.FileSystem, path string, snap Snapshot, now time.Time) error {
	payload := ClaudeCapabilitiesProjection(snap, now.Unix())

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("publish claude capabilities: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("publish claude capabilities: mkdir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	if err := fsys.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("publish claude capabilities: write tmp: %w", err)
	}

	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp)
		return fmt.Errorf("publish claude capabilities: rename: %w", err)
	}

	return nil
}
