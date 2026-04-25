package modelregistry

import (
	"encoding/json"
	"errors"
)

// Provider identifies the upstream API provider for a model.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderCodex     Provider = "codex"
)

// Source indicates where a registry entry originated.
type Source string

const (
	SourceNative  Source = "native"
	SourceOverlay Source = "overlay"
)

// Entry is a single model record in the registry.
type Entry struct {
	Provider         Provider `json:"provider"`
	ID               string   `json:"id"`
	Aliases          []string `json:"aliases,omitempty"`
	DisplayName      string   `json:"display_name,omitempty"`
	Description      string   `json:"description,omitempty"`
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxContextWindow int      `json:"max_context_window,omitempty"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
	Visibility       string   `json:"visibility,omitempty"`
	Priority         int      `json:"priority,omitempty"`
	Source           Source   `json:"source"`
	CloneFrom        string   `json:"clone_from,omitempty"`
	InferredFrom     string   `json:"inferred_from,omitempty"`
	// Raw holds the original provider JSON for pass-through use.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Validate returns an error if the entry is missing required fields or contains
// unrecognised enumeration values.
func (e Entry) Validate() error {
	if e.Provider == "" {
		return errors.New("modelregistry: entry missing provider")
	}
	switch e.Provider {
	case ProviderAnthropic, ProviderCodex:
	default:
		return errors.New("modelregistry: unknown provider " + string(e.Provider))
	}
	if e.ID == "" {
		return errors.New("modelregistry: entry missing id")
	}
	// Source is optional (omitempty); only validate when non-empty.
	if e.Source != "" {
		switch e.Source {
		case SourceNative, SourceOverlay:
		default:
			return errors.New("modelregistry: unknown source " + string(e.Source))
		}
	}
	return nil
}
