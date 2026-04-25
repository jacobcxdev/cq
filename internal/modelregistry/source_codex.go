package modelregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// codexModelsResponse is the shape of GET /models?client_version=<v> from the Codex API.
type codexModelsResponse struct {
	Models []json.RawMessage `json:"models"`
}

// codexModelInfo captures the fields cq maps onto Entry.
// The full raw JSON is preserved separately in CodexRawByID.
type codexModelInfo struct {
	Slug             string `json:"slug"`
	DisplayName      string `json:"display_name"`
	Description      string `json:"description"`
	ContextWindow    int    `json:"context_window"`
	MaxContextWindow int    `json:"max_context_window"`
	Priority         int    `json:"priority"`
	Visibility       string `json:"visibility"`
}

// CodexSource fetches the Codex model catalogue from the upstream API.
// All fields are injected for testability.
type CodexSource struct {
	// Client is the HTTP doer used to make requests.
	Client httputil.Doer
	// BaseURL is the root URL, e.g. "https://api.openai.com".
	// Defaults to "https://api.openai.com" when empty.
	BaseURL string
	// Token returns a bearer token for the request.
	Token func(ctx context.Context) (string, error)
	// ClientVersion is sent as the client_version query parameter.
	ClientVersion string
}

// Fetch implements NativeSource.
func (s *CodexSource) Fetch(ctx context.Context) (SourceResult, error) {
	token, err := s.Token(ctx)
	if err != nil {
		return SourceResult{}, fmt.Errorf("codex source: get token: %w", err)
	}

	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	url := baseURL + "/models"
	if s.ClientVersion != "" {
		url += "?client_version=" + s.ClientVersion
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SourceResult{}, fmt.Errorf("codex source: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.Client.Do(req)
	if err != nil {
		return SourceResult{}, fmt.Errorf("codex source: request: %w", err)
	}
	defer resp.Body.Close()

	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return SourceResult{}, fmt.Errorf("codex source: read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SourceResult{}, fmt.Errorf("codex source: HTTP %d: %s",
			resp.StatusCode, httputil.TruncateBody(body, httputil.MaxErrorBodyLen))
	}

	var parsed codexModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return SourceResult{}, fmt.Errorf("codex source: parse response: %w", err)
	}

	entries := make([]Entry, 0, len(parsed.Models))
	rawByID := make(map[string]json.RawMessage, len(parsed.Models))
	malformed := 0

	for _, raw := range parsed.Models {
		var info codexModelInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			malformed++
			continue
		}
		if info.Slug == "" {
			malformed++
			continue
		}

		e := Entry{
			Provider:         ProviderCodex,
			ID:               info.Slug,
			DisplayName:      info.DisplayName,
			Description:      info.Description,
			ContextWindow:    info.ContextWindow,
			MaxContextWindow: info.MaxContextWindow,
			Priority:         info.Priority,
			Visibility:       info.Visibility,
			Source:           SourceNative,
		}
		entries = append(entries, e)
		rawByID[info.Slug] = raw
	}

	return SourceResult{
		Entries:          entries,
		CodexRawByID:     rawByID,
		FetchedAt:        time.Now(),
		MalformedEntries: malformed,
	}, nil
}
