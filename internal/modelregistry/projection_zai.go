package modelregistry

import "encoding/json"

// Keep GLM-5.3 below reported late-context reasoning loops. Compact at 70% of the
// usable window shown by Codex, without reducing the provider's context size.
// Recompute from native metadata so catalogue updates retain the percentage;
// other models, including future GLM releases, keep their provider defaults.
func zaiCodexEntryRaw(e Entry) json.RawMessage {
	if e.ID != "glm-5.3" {
		return e.Raw
	}
	var info struct {
		ContextWindow    *int64 `json:"context_window"`
		MaxWindow        *int64 `json:"max_context_window"`
		EffectivePct     *int64 `json:"effective_context_window_percent"`
		AutoCompactLimit *int64 `json:"auto_compact_token_limit"`
	}
	if json.Unmarshal(e.Raw, &info) != nil {
		return e.Raw
	}
	window := info.ContextWindow
	if window == nil {
		window = info.MaxWindow
	}
	percent := int64(95) // Codex's default input headroom.
	if info.EffectivePct != nil {
		percent = *info.EffectivePct
	}
	if window == nil || *window <= 0 || percent <= 0 || percent > 100 {
		return e.Raw
	}
	// Divide first to keep integer arithmetic within the original window size.
	usable := *window/100*percent + (*window%100)*percent/100
	limit := usable/10*7 + usable%10*7/10
	if limit <= 0 || info.AutoCompactLimit != nil && *info.AutoCompactLimit > 0 && *info.AutoCompactLimit <= limit {
		return e.Raw
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(e.Raw, &raw) != nil {
		return e.Raw
	}
	raw["auto_compact_token_limit"], _ = json.Marshal(limit)
	encoded, err := json.Marshal(raw)
	if err != nil {
		return e.Raw
	}
	return encoded
}
