package modelregistry

import (
	"encoding/json"
	"fmt"
)

// codexModelsResponseOut is the shape written to models_cache.json and
// returned by the proxy's /models endpoint.
type codexModelsResponseOut struct {
	Models []json.RawMessage `json:"models"`
}

// codexCacheEnvelope is the on-disk format of models_cache.json.
type codexCacheEnvelope struct {
	FetchedAt     string              `json:"fetched_at"`
	Etag          *string             `json:"etag,omitempty"`
	ClientVersion string              `json:"client_version,omitempty"`
	Models        []json.RawMessage   `json:"models"`
	CQProviders   map[string]Provider `json:"cq_providers,omitempty"`
}

// codexFallbackContextWindow is used for overlay entries that declare no
// context window. 272000 matches the upstream fallback observed in production.
const codexFallbackContextWindow = 272000

// codexFallbackTruncationBytesLimit is the bytes limit for synthesised
// truncation_policy objects.
const codexFallbackTruncationBytesLimit = 10000

// CodexModelsResponse builds the models response payload from the snapshot.
// Codex and Z.ai Responses entries are included.
//
// For native entries with raw JSON in CodexRawByID, the raw bytes are used
// verbatim (round-trip guarantee).
//
// For overlay/non-native entries:
//   - If CloneFrom refers to a native entry with raw JSON, the raw bytes are
//     cloned and slug/display_name are overridden.
//   - Otherwise a fallback synthetic object is built from the Entry fields.
func CodexModelsResponse(snap Snapshot) codexModelsResponseOut {
	models := make([]json.RawMessage, 0)
	for _, e := range snap.Entries {
		if e.Provider != ProviderCodex && e.Provider != ProviderZAI {
			continue
		}
		raw := codexEntryRaw(e, snap.CodexRawByID)
		models = append(models, raw)
	}
	return codexModelsResponseOut{Models: models}
}

// AugmentCodexModels adds CQ overlays and other providers to an account's own
// response. Pooled native entries and raw metadata never supply entitlements.
// Unknown upstream fields survive, and native model IDs take precedence.
func AugmentCodexModels(body []byte, snap Snapshot) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("parse Codex models envelope: %w", err)
	}
	var models []json.RawMessage
	if err := json.Unmarshal(envelope["models"], &models); err != nil || models == nil {
		return nil, fmt.Errorf("invalid Codex models list")
	}

	rawByID := make(map[string]json.RawMessage, len(models))
	for _, raw := range models {
		var info codexModelInfo
		if err := json.Unmarshal(raw, &info); err != nil || info.Slug == "" {
			return nil, fmt.Errorf("invalid Codex model")
		}
		if _, exists := rawByID[info.Slug]; exists {
			return nil, fmt.Errorf("duplicate Codex model")
		}
		rawByID[info.Slug] = raw
	}
	added := make(map[string]bool)
	for _, e := range snap.Entries {
		if e.Provider != ProviderZAI && (e.Provider != ProviderCodex || e.Source != SourceOverlay) {
			continue
		}
		if _, exists := rawByID[e.ID]; exists {
			continue
		}
		if added[e.ID] {
			continue
		}
		models = append(models, codexEntryRaw(e, rawByID))
		added[e.ID] = true
	}
	encodedModels, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	envelope["models"] = encodedModels
	return json.Marshal(envelope)
}

// codexEntryRaw returns the JSON bytes for a single Codex entry.
func codexEntryRaw(e Entry, rawByID map[string]json.RawMessage) json.RawMessage {
	if e.Provider == ProviderZAI && len(e.Raw) > 0 {
		return e.Raw
	}
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
