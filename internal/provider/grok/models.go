package grok

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Model is account-scoped native Grok metadata, not a pooled entitlement.
type Model struct {
	ID                     string
	Name                   string
	Description            string
	Backend                string
	ContextWindow          int
	MaxContextWindow       int
	MaxOutputTokens        int
	ReasoningEfforts       []string
	DefaultReasoningEffort string
	Aliases                []string
	Hidden                 bool
	Raw                    json.RawMessage
}

// FetchModels reads the selected subscription's own model catalogue.
func FetchModels(ctx context.Context, authority Authority, account AccountID) ([]Model, error) {
	body, err := readSubscriptionJSON(ctx, authority, account, "/models")
	if err != nil {
		return nil, err
	}
	return ParseModels(body)
}

// ParseModels preserves native metadata while rejecting ambiguous identities
// and endpoint overrides that could send a subscription bearer elsewhere.
func ParseModels(body []byte) ([]Model, error) {
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	invalid := errors.New("invalid Grok subscription model catalogue")
	if json.Unmarshal(body, &response) != nil || response.Data == nil {
		return nil, invalid
	}
	models := make([]Model, 0, len(response.Data))
	seen := make(map[string]bool, len(response.Data))
	for _, raw := range response.Data {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			return nil, invalid
		}
		var meta map[string]json.RawMessage
		if value := obj["_meta"]; len(value) != 0 && string(value) != "null" {
			if json.Unmarshal(value, &meta) != nil {
				return nil, invalid
			}
		}
		field := func(keys ...string) json.RawMessage {
			for _, source := range []map[string]json.RawMessage{obj, meta} {
				for _, key := range keys {
					if v, ok := source[key]; ok {
						return v
					}
				}
			}
			return nil
		}
		text := func(keys ...string) (string, error) {
			v := field(keys...)
			if len(v) == 0 || string(v) == "null" {
				return "", nil
			}
			var s string
			if json.Unmarshal(v, &s) != nil {
				return "", invalid
			}
			return s, nil
		}
		integer := func(keys ...string) (int, error) {
			v := field(keys...)
			if len(v) == 0 || string(v) == "null" {
				return 0, nil
			}
			var n int
			if json.Unmarshal(v, &n) != nil || n < 0 {
				return 0, invalid
			}
			return n, nil
		}
		id, err := text("model", "modelId", "id")
		if err != nil || !validModelID(id) || seen[strings.ToLower(id)] {
			return nil, invalid
		}
		seen[strings.ToLower(id)] = true
		backend, err := text("apiBackend", "api_backend")
		if err != nil {
			return nil, invalid
		}
		if backend == "" {
			backend = "chat_completions"
		} // Grok Build's native default.
		if backend != "responses" && backend != "chat_completions" && backend != "messages" {
			return nil, invalid
		}
		for _, key := range []string{"baseUrl", "base_url", "apiBaseUrl", "api_base_url"} {
			base, err := text(key)
			if err != nil || base != "" && strings.TrimRight(base, "/") != BaseURL {
				return nil, invalid
			}
		}
		name, err := text("name", "displayName", "display_name")
		if err != nil {
			return nil, invalid
		}
		if name == "" {
			name = id
		}
		description, err := text("description")
		if err != nil {
			return nil, invalid
		}
		contextWindow, err := integer("contextWindow", "context_window", "totalContextTokens")
		if err != nil {
			return nil, invalid
		}
		maxContextWindow := contextWindow
		if choices := field("contextWindows", "context_windows"); len(choices) > 0 {
			var windows []int
			if json.Unmarshal(choices, &windows) != nil {
				return nil, invalid
			}
			for _, window := range windows {
				if window <= 0 {
					return nil, invalid
				}
				if contextWindow == 0 {
					contextWindow = window
				}
				maxContextWindow = max(maxContextWindow, window)
			}
		}
		maxOutput, err := integer("maxCompletionTokens", "max_completion_tokens")
		if err != nil {
			return nil, invalid
		}
		// Native model schemas can carry per-model credential configuration.
		// CQ authenticates through its authority and must not publish those
		// fields in model listing/cache metadata.
		redacted := false
		for _, source := range []map[string]json.RawMessage{obj, meta} {
			for _, key := range []string{"apiKey", "api_key", "envKey", "env_key", "extraHeaders", "extra_headers"} {
				if _, exists := source[key]; exists {
					delete(source, key)
					redacted = true
				}
			}
		}
		if redacted {
			if meta != nil {
				obj["_meta"], _ = json.Marshal(meta)
			}
			raw, _ = json.Marshal(obj)
		}
		m := Model{ID: id, Name: name, Description: description, Backend: backend, ContextWindow: contextWindow, MaxContextWindow: maxContextWindow, MaxOutputTokens: maxOutput, Raw: append(json.RawMessage(nil), raw...)}
		if v := field("hidden"); len(v) > 0 && json.Unmarshal(v, &m.Hidden) != nil {
			return nil, invalid
		}
		if v := field("aliases"); len(v) > 0 {
			if json.Unmarshal(v, &m.Aliases) != nil {
				return nil, invalid
			}
			for _, alias := range m.Aliases {
				if !validModelID(alias) {
					return nil, invalid
				}
			}
		}
		if v := field("reasoningEfforts", "reasoning_efforts"); len(v) > 0 {
			var efforts []json.RawMessage
			if json.Unmarshal(v, &efforts) != nil {
				return nil, invalid
			}
			seenEfforts := make(map[string]bool, len(efforts))
			for _, effort := range efforts {
				var name string
				isDefault := false
				if json.Unmarshal(effort, &name) != nil {
					var obj struct {
						Effort  string `json:"effort"`
						ID      string `json:"id"`
						Value   string `json:"value"`
						Default bool   `json:"default"`
					}
					if json.Unmarshal(effort, &obj) != nil {
						return nil, invalid
					}
					for _, candidate := range []string{obj.Effort, obj.Value, obj.ID} {
						if candidate == "" {
							continue
						}
						if name != "" && name != candidate {
							return nil, invalid
						}
						name = candidate
					}
					isDefault = obj.Default
				}
				if !validEffort(name) || seenEfforts[name] {
					return nil, invalid
				}
				seenEfforts[name] = true
				if isDefault {
					if m.DefaultReasoningEffort != "" {
						return nil, invalid
					}
					m.DefaultReasoningEffort = name
				}
				m.ReasoningEfforts = append(m.ReasoningEfforts, name)
			}
		}
		for _, key := range []string{"defaultReasoningEffort", "default_reasoning_effort", "reasoningEffort", "reasoning_effort"} {
			declared, readErr := text(key)
			if readErr != nil || declared != "" && (!validEffort(declared) || m.DefaultReasoningEffort != "" && m.DefaultReasoningEffort != declared) {
				return nil, invalid
			}
			if declared != "" {
				m.DefaultReasoningEffort = declared
			}
		}
		if len(m.ReasoningEfforts) == 0 {
			var caps struct {
				Efforts []string `json:"reasoning_effort"`
				Default string   `json:"default_reasoning_effort"`
			}
			if rawCaps := field("capabilities"); len(rawCaps) > 0 {
				if json.Unmarshal(rawCaps, &caps) != nil {
					return nil, invalid
				}
				for _, effort := range caps.Efforts {
					if !validEffort(effort) {
						return nil, invalid
					}
				}
				m.ReasoningEfforts = caps.Efforts
				if caps.Default != "" && m.DefaultReasoningEffort != "" && caps.Default != m.DefaultReasoningEffort {
					return nil, invalid
				}
				if m.DefaultReasoningEffort == "" {
					m.DefaultReasoningEffort = caps.Default
				}
			}
		}
		if m.DefaultReasoningEffort != "" {
			found := false
			for _, effort := range m.ReasoningEfforts {
				found = found || effort == m.DefaultReasoningEffort
			}
			if !found {
				return nil, invalid
			}
		}
		models = append(models, m)
	}
	return models, nil
}

func validModelID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if r <= ' ' || r > 126 || strings.ContainsRune("/\\?#", r) {
			return false
		}
	}
	return true
}

func validEffort(s string) bool {
	switch s {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}
