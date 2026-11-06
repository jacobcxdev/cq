package modelregistry

import (
	"encoding/json"

	"github.com/jacobcxdev/cq/internal/provider/grok"
)

// grokCodexEntryRaw reuses the existing Codex projection baseline while mapping
// Grok's native metadata. It does not inherit a GPT context or effort default.
func grokCodexEntryRaw(e Entry) json.RawMessage {
	if e.ContextWindow <= 0 {
		return nil
	}
	// A native client cache already contains the projected object. CQ's own
	// registry cache instead retains the original subscription metadata.
	var existing struct {
		Slug string `json:"slug"`
	}
	if json.Unmarshal(e.Raw, &existing) == nil && existing.Slug == e.ID {
		return e.Raw
	}
	models, err := grok.ParseModels(append(append([]byte(`{"data":[`), e.Raw...), []byte(`]}`)...))
	if err != nil || len(models) != 1 || models[0].Backend != "responses" {
		return nil
	}
	m := models[0]
	var record map[string]any
	if json.Unmarshal(synthesiseCodexEntry(e), &record) != nil {
		return nil
	}
	levels := make([]map[string]string, 0, len(m.ReasoningEfforts))
	for _, effort := range m.ReasoningEfforts {
		levels = append(levels, map[string]string{"effort": effort, "description": effort})
	}
	record["supported_reasoning_levels"] = levels
	if m.DefaultReasoningEffort != "" {
		record["default_reasoning_level"] = m.DefaultReasoningEffort
	}
	record["max_context_window"] = max(e.ContextWindow, e.MaxContextWindow)
	if e.MaxOutputTokens > 0 {
		record["max_output_tokens"] = e.MaxOutputTokens
	}
	// These are Codex wire types, not Grok API options. Do not inherit older
	// synthetic enum/bool defaults that current Codex cannot deserialize.
	record["apply_patch_tool_type"] = nil
	record["default_verbosity"] = nil
	record["default_reasoning_summary"] = "none"
	record["web_search_tool_type"] = "text"
	record["truncation_policy"] = map[string]any{"mode": "bytes", "limit": codexFallbackTruncationBytesLimit}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil
	}
	return encoded
}
