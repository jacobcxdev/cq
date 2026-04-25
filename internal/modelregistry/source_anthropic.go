package modelregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// anthropicModelsResponse is the shape of GET /v1/models from the Anthropic API.
type anthropicModelsResponse struct {
	Data []anthropicModel `json:"data"`
}

// anthropicModel is one element from the Anthropic /v1/models response.
// Only the fields cq uses are decoded; the full raw JSON is preserved separately.
type anthropicModel struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// AnthropicSource fetches the Anthropic model catalogue from the upstream API.
// All fields are injected for testability.
type AnthropicSource struct {
	// Client is the HTTP doer used to make requests.
	Client httputil.Doer
	// BaseURL is the root URL, e.g. "https://api.anthropic.com".
	// Defaults to "https://api.anthropic.com" when empty.
	BaseURL string
	// Token returns a bearer token for the request.
	Token func(ctx context.Context) (string, error)
}

// Fetch implements NativeSource.
func (s *AnthropicSource) Fetch(ctx context.Context) (SourceResult, error) {
	token, err := s.Token(ctx)
	if err != nil {
		return SourceResult{}, fmt.Errorf("anthropic source: get token: %w", err)
	}

	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return SourceResult{}, fmt.Errorf("anthropic source: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := s.Client.Do(req)
	if err != nil {
		return SourceResult{}, fmt.Errorf("anthropic source: request: %w", err)
	}
	defer resp.Body.Close()

	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return SourceResult{}, fmt.Errorf("anthropic source: read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SourceResult{}, fmt.Errorf("anthropic source: HTTP %d: %s",
			resp.StatusCode, httputil.TruncateBody(body, httputil.MaxErrorBodyLen))
	}

	var rawResp struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &rawResp); err != nil {
		return SourceResult{}, fmt.Errorf("anthropic source: parse response: %w", err)
	}

	entries := make([]Entry, 0, len(rawResp.Data))
	rawByID := make(map[string]json.RawMessage, len(rawResp.Data))

	for _, raw := range rawResp.Data {
		var m anthropicModel
		if err := json.Unmarshal(raw, &m); err != nil {
			return SourceResult{}, fmt.Errorf("anthropic source: parse model: %w", err)
		}
		if m.ID == "" {
			continue
		}
		e := Entry{
			Provider:        ProviderAnthropic,
			ID:              m.ID,
			DisplayName:     m.DisplayName,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
			Source:          SourceNative,
		}
		entries = append(entries, e)
		rawByID[m.ID] = raw
	}

	return SourceResult{
		Entries:          entries,
		AnthropicRawByID: rawByID,
		FetchedAt:        time.Now(),
	}, nil
}
