package proxy

import (
	"encoding/json"
	"strings"
)

// Provider identifies the upstream API provider for routing.
type Provider int

const (
	// ProviderClaude routes to the Anthropic API.
	ProviderClaude Provider = iota
	// ProviderCodex routes to the OpenAI Responses API.
	ProviderCodex
)

// RouteModel maps a model name to the provider that serves it.
// Effort suffixes (e.g. "-xhigh") are stripped before matching.
func RouteModel(model string) Provider {
	base, _ := ParseModelEffort(model)
	lower := strings.ToLower(base)
	switch {
	case strings.HasPrefix(lower, "gpt-"):
		return ProviderCodex
	case strings.HasPrefix(lower, "o1") ||
		strings.HasPrefix(lower, "o3") ||
		strings.HasPrefix(lower, "o4"):
		return ProviderCodex
	case strings.HasPrefix(lower, "codex-"):
		return ProviderCodex
	default:
		return ProviderClaude
	}
}

// effortSuffixes maps model name suffixes to OpenAI reasoning effort levels.
// Users can append these to any Codex model name (e.g. "gpt-5.4-xhigh")
// to force a specific reasoning effort without the dynamic effort selector.
var effortSuffixes = []struct {
	suffix string
	effort string
}{
	{"-xhigh", "xhigh"},
	{"-high", "high"},
	{"-medium", "medium"},
	{"-low", "low"},
}

// ParseModelEffort splits a model name into the base model and an optional
// effort override from a recognised suffix. Returns ("gpt-5.4", "xhigh")
// for input "gpt-5.4-xhigh", or ("gpt-5.4", "") for input "gpt-5.4".
func ParseModelEffort(model string) (baseModel, effort string) {
	lower := strings.ToLower(model)
	for _, es := range effortSuffixes {
		if strings.HasSuffix(lower, es.suffix) {
			return model[:len(model)-len(es.suffix)], es.effort
		}
	}
	return model, ""
}

// extractModel performs a quick JSON parse to extract the "model" field
// from a request body without fully unmarshalling.
func extractModel(body []byte) string {
	var partial struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &partial) != nil {
		return ""
	}
	return partial.Model
}
