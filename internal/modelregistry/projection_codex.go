package modelregistry

import (
	"encoding/json"
	"fmt"
	"strings"
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
// Includes first-party and shared-budget Responses models.
//
// Native fields are preserved, with a model-specific compaction cap for GLM-5.3
// and Codex Search MCP selection for third-party inference.
//
// For overlay/non-native entries:
//   - If CloneFrom refers to a native entry with raw JSON, the raw bytes are
//     cloned and slug/display_name are overridden.
//   - Otherwise a fallback synthetic object is built from the Entry fields.
func CodexModelsResponse(snap Snapshot) codexModelsResponseOut {
	models := make([]json.RawMessage, 0)
	for _, e := range snap.Entries {
		e = codexProjectionEntry(e, snap.SharedBudgetModels)
		if !isCodexProjectionEntry(e) {
			continue
		}
		raw := codexEntryRaw(e, snap.CodexRawByID)
		if len(raw) == 0 {
			continue
		}
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
	seen := make(map[string]bool, len(models))
	for _, raw := range models {
		var info codexModelInfo
		if err := json.Unmarshal(raw, &info); err != nil || info.Slug == "" {
			return nil, fmt.Errorf("invalid Codex model")
		}
		if _, exists := rawByID[info.Slug]; exists {
			return nil, fmt.Errorf("duplicate Codex model")
		}
		rawByID[info.Slug] = raw
		seen[strings.ToLower(info.Slug)] = true
	}
	for _, e := range snap.Entries {
		e = codexProjectionEntry(e, snap.SharedBudgetModels)
		if !isCodexProjectionEntry(e) || (e.Provider == ProviderCodex && e.Source != SourceOverlay) {
			continue
		}
		id := strings.ToLower(e.ID)
		if seen[id] {
			continue
		}
		raw := codexEntryRaw(e, rawByID)
		if len(raw) == 0 {
			continue
		}
		models = append(models, raw)
		seen[id] = true
	}
	encodedModels, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	envelope["models"] = encodedModels
	return json.Marshal(envelope)
}

func isCodexProjectionEntry(e Entry) bool {
	return e.Provider == ProviderCodex || e.Provider == ProviderZAI || e.Provider == ProviderGrok || e.SharedBudget != nil
}

// Anthropic's local route speaks Messages. Codex can use the first available
// shared-budget Responses route for that model.
func codexProjectionEntry(e Entry, sharedModels map[string][]Entry) Entry {
	if e.Provider == ProviderAnthropic {
		if routes := sharedModels[strings.ToLower(e.ID)]; len(routes) > 0 {
			return routes[0]
		}
	}
	return e
}

// codexEntryRaw returns the JSON bytes for a single Codex entry.
func codexEntryRaw(e Entry, rawByID map[string]json.RawMessage) json.RawMessage {
	raw := codexEntryMetadata(e, rawByID)
	if e.Provider == ProviderCodex && e.SharedBudget == nil {
		return raw
	}
	// Let Codex execute search through its own connector. The inference
	// endpoint receives an ordinary function call and its result.
	var record map[string]json.RawMessage
	if json.Unmarshal(raw, &record) != nil || record == nil {
		return raw
	}
	record["web_search_provider"] = json.RawMessage(`"search_mcp"`)
	body, _ := json.Marshal(record)
	return body
}

func codexEntryMetadata(e Entry, rawByID map[string]json.RawMessage) json.RawMessage {
	if e.SharedBudget != nil {
		return sharedBudgetCodexEntryRaw(e)
	}
	if e.Provider == ProviderGrok {
		return grokCodexEntryRaw(e)
	}
	if e.Provider == ProviderZAI && len(e.Raw) > 0 {
		return zaiCodexEntryRaw(e)
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

func sharedBudgetCodexEntryRaw(e Entry) json.RawMessage {
	if e.ContextWindow <= 0 {
		return nil
	}
	var record map[string]any
	if json.Unmarshal(synthesiseCodexEntry(e), &record) != nil {
		return nil
	}
	levels := make([]map[string]string, 0, len(e.SharedBudget.ReasoningEfforts))
	for _, effort := range e.SharedBudget.ReasoningEfforts {
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh":
			levels = append(levels, map[string]string{"effort": effort, "description": effort})
		}
	}
	if len(levels) > 0 {
		record["default_reasoning_level"] = levels[0]["effort"]
	}
	record["supported_reasoning_levels"] = levels
	record["apply_patch_tool_type"] = nil
	record["default_verbosity"] = nil
	record["default_reasoning_summary"] = "none"
	record["web_search_tool_type"] = "text"
	record["truncation_policy"] = map[string]any{"mode": "bytes", "limit": codexFallbackTruncationBytesLimit}
	if e.MaxOutputTokens > 0 {
		record["max_output_tokens"] = e.MaxOutputTokens
	}
	body, _ := json.Marshal(record)
	return body
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
		"slug":                             e.ID,
		"display_name":                     label,
		"description":                      e.Description,
		"context_window":                   cw,
		"max_context_window":               cw,
		"effective_context_window_percent": 100,
		"shell_type":                       "default",
		"visibility":                       visibility,
		"supported_in_api":                 true,
		"priority":                         priority,
		"base_instructions":                "",
		"supported_reasoning_levels":       []string{},
		"supports_reasoning_summaries":     false,
		"default_reasoning_summary":        false,
		"support_verbosity":                false,
		"default_verbosity":                false,
		"apply_patch_tool_type":            "default",
		"web_search_tool_type":             "default",
		"truncation_policy":                truncationPolicy,
		"supports_parallel_tool_calls":     false,
		"supports_image_detail_original":   false,
		"experimental_supported_tools":     []string{},
		"input_modalities":                 []string{"text"},
		"supports_search_tool":             false,
		"additional_speed_tiers":           []string{},
		"availability_nux":                 nil,
		"upgrade":                          nil,
	}

	result, _ := json.Marshal(obj)
	return json.RawMessage(result)
}
