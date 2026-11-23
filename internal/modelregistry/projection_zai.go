package modelregistry

import "encoding/json"

func zaiCodexEntryRaw(e Entry) json.RawMessage {
	if e.ID == "glm-5.4" {
		// Z.ai reports a 1M window but gives Codex a 50% input budget.
		// Use Codex's usual 5% headroom for this catalogue entry.
		var raw map[string]json.RawMessage
		if json.Unmarshal(e.Raw, &raw) != nil || string(raw["context_window"]) != "1048576" ||
			string(raw["effective_context_window_percent"]) != "50" {
			return e.Raw
		}
		raw["effective_context_window_percent"] = json.RawMessage("95")
		encoded, _ := json.Marshal(raw)
		return encoded
	}
	if e.ID != "glm-5.3" {
		return e.Raw
	}
	// Compact GLM-5.3 at 70% of its usable window to avoid late-context reasoning loops.
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
