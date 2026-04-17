package proxy

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

const countTokensPath = "/v1/messages/count_tokens"

func RouteRequest(method, path, model string) Provider {
	if method == http.MethodPost && path == countTokensPath {
		return RouteModel(model)
	}
	return RouteModel(model)
}

// Provider identifies the upstream API provider for routing.
type Provider int

const (
	// ProviderClaude routes to the Anthropic API.
	ProviderClaude Provider = iota
	// ProviderCodex routes to the OpenAI Responses API.
	ProviderCodex
)

// RouteModel maps a model name to the provider that serves it.
// The [1m] suffix is stripped before matching; all other name characters are preserved.
func RouteModel(model string) Provider {
	lower := strings.ToLower(ParseModel(model))
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

var oneMillionSuffix = regexp.MustCompile(`(?i)\[1m\]$`)

// ParseModel normalises a model name by stripping a trailing [1m] suffix (case-insensitive).
// No other transformations are applied: effort-like substrings such as "-xhigh" are
// preserved as part of the model name and passed to upstream unchanged.
func ParseModel(model string) string {
	return oneMillionSuffix.ReplaceAllString(model, "")
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
