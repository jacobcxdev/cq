package modelregistry

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// codexModelsResponseOut is the shape written to models_cache.json and
// returned by the proxy's /models endpoint.
type codexModelsResponseOut struct {
	Models []json.RawMessage `json:"models"`
}

// codexCacheEnvelope is the on-disk format of models_cache.json.
type codexCacheEnvelope struct {
	FetchedAt     string            `json:"fetched_at"`
	Etag          *string           `json:"etag,omitempty"`
	ClientVersion string            `json:"client_version,omitempty"`
	Models        []json.RawMessage `json:"models"`
}

// codexFallbackContextWindow is used for overlay entries that declare no
// context window. 272000 matches the upstream fallback observed in production.
const codexFallbackContextWindow = 272000

// codexFallbackTruncationBytesLimit is the bytes limit for synthesised
// truncation_policy objects.
const codexFallbackTruncationBytesLimit = 10000

// CodexModelsResponse builds the models response payload from the snapshot.
// Only Codex entries are included.
//
// For native entries with raw JSON in CodexRawByID, the raw bytes are used
// verbatim (round-trip guarantee).
//
// For overlay/non-native entries:
//   - If CloneFrom refers to a native entry with raw JSON, the raw bytes are
//     cloned and slug/display_name are overridden.
//   - Otherwise a fallback synthetic object is built from the Entry fields.
func CodexModelsResponse(snap Snapshot) codexModelsResponseOut {
	var models []json.RawMessage
	for _, e := range snap.Entries {
		if e.Provider != ProviderCodex {
			continue
		}
		raw := codexEntryRaw(e, snap.CodexRawByID)
		models = append(models, raw)
	}
	return codexModelsResponseOut{Models: models}
}

// codexEntryRaw returns the JSON bytes for a single Codex entry.
func codexEntryRaw(e Entry, rawByID map[string]json.RawMessage) json.RawMessage {
	// Native entry: use the stored raw JSON verbatim.
	if e.Source == SourceNative {
		if raw, ok := rawByID[e.ID]; ok && raw != nil {
			return raw
		}
	}

	// Overlay: try to clone from the raw of CloneFrom target.
	if e.CloneFrom != "" {
		if baseRaw, ok := rawByID[e.CloneFrom]; ok && baseRaw != nil {
			return cloneRawWithOverrides(baseRaw, e)
		}
	}

	// Fallback: synthesise from Entry fields.
	return synthesiseCodexEntry(e)
}

// cloneRawWithOverrides deep-copies the base raw JSON object and overrides
// slug and display_name with values from the overlay entry.
func cloneRawWithOverrides(base json.RawMessage, e Entry) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return synthesiseCodexEntry(e)
	}

	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		out[k] = cp
	}

	slugBytes, _ := json.Marshal(e.ID)
	out["slug"] = json.RawMessage(slugBytes)

	label := e.DisplayName
	if label == "" {
		label = e.ID
	}
	labelBytes, _ := json.Marshal(label)
	out["display_name"] = json.RawMessage(labelBytes)

	result, err := json.Marshal(out)
	if err != nil {
		return synthesiseCodexEntry(e)
	}
	return json.RawMessage(result)
}

// synthesiseCodexEntry builds a synthetic Codex ModelInfo object from an
// Entry when no raw JSON is available. The fields use reasonable defaults
// consistent with upstream fallback behaviour.
func synthesiseCodexEntry(e Entry) json.RawMessage {
	cw := e.ContextWindow
	if cw == 0 {
		cw = codexFallbackContextWindow
	}

	priority := e.Priority
	if priority == 0 {
		priority = 99
	}

	visibility := e.Visibility
	if visibility == "" {
		visibility = "list"
	}

	label := e.DisplayName
	if label == "" {
		label = e.ID
	}

	truncationPolicy := map[string]any{
		"type":  "last_n_bytes",
		"bytes": codexFallbackTruncationBytesLimit,
	}

	obj := map[string]any{
		"slug":                          e.ID,
		"display_name":                  label,
		"description":                   e.Description,
		"context_window":                cw,
		"max_context_window":            cw,
		"effective_context_window_percent": 100,
		"shell_type":                    "default",
		"visibility":                    visibility,
		"supported_in_api":              true,
		"priority":                      priority,
		"base_instructions":             "",
		"supported_reasoning_levels":    []string{},
		"supports_reasoning_summaries":  false,
		"default_reasoning_summary":     false,
		"support_verbosity":             false,
		"default_verbosity":             false,
		"apply_patch_tool_type":         "default",
		"web_search_tool_type":          "default",
		"truncation_policy":             truncationPolicy,
		"supports_parallel_tool_calls":  false,
		"supports_image_detail_original": false,
		"experimental_supported_tools":  []string{},
		"input_modalities":              []string{"text"},
		"supports_search_tool":          false,
		"additional_speed_tiers":        []string{},
		"availability_nux":              nil,
		"upgrade":                       nil,
	}

	result, _ := json.Marshal(obj)
	return json.RawMessage(result)
}

// PublishCodexCache writes the Codex models_cache.json envelope to path
// using an atomic tmp+rename write. Parent directories are created with
// 0o700; the file is written with 0o600.
//
// Existing envelope fields (etag, client_version) are preserved when the
// caller passes an empty clientVersion string. fetched_at is always updated
// to now. clientVersion, when non-empty, replaces the stored value.
func PublishCodexCache(fsys fsutil.FileSystem, path string, snap Snapshot, now time.Time, clientVersion string) error {
	// Read existing envelope to preserve etag and other fields.
	var existing codexCacheEnvelope
	if data, err := fsys.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing) // best-effort; ignore parse errors
	}

	// Update envelope fields.
	existing.FetchedAt = now.UTC().Format(time.RFC3339)
	if clientVersion != "" {
		existing.ClientVersion = clientVersion
	}

	// Build the models list from the snapshot.
	resp := CodexModelsResponse(snap)
	existing.Models = resp.Models

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("publish codex cache: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("publish codex cache: mkdir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	if err := fsys.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("publish codex cache: write tmp: %w", err)
	}
	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp)
		return fmt.Errorf("publish codex cache: rename: %w", err)
	}

	return nil
}
