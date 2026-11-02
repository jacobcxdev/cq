package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// ZAISource fetches native Codex model metadata from Z.ai's Responses endpoint.
type ZAISource struct {
	Client httputil.Doer
	Token  func(context.Context) (string, error)
}

func (s *ZAISource) Fetch(ctx context.Context) (result SourceResult, err error) {
	defer func() {
		if recover() != nil {
			result = SourceResult{}
			err = errors.New("zai source: model discovery panicked")
		}
	}()

	token, err := s.Token(ctx)
	if err != nil || token == "" {
		return SourceResult{}, errors.New("zai source: API key unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.z.ai/api/v1/models", nil)
	if err != nil {
		return SourceResult{}, errors.New("zai source: build request failed")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.Client.Do(req)
	if err != nil {
		return SourceResult{}, errors.New("zai source: model request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SourceResult{}, fmt.Errorf("zai source: HTTP %d", resp.StatusCode)
	}
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return SourceResult{}, errors.New("zai source: read model response failed")
	}
	var parsed codexModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Models == nil {
		return SourceResult{}, errors.New("zai source: invalid model response")
	}
	result = SourceResult{
		Entries:      make([]Entry, 0, len(parsed.Models)),
		CodexRawByID: make(map[string]json.RawMessage, len(parsed.Models)),
		FetchedAt:    time.Now(),
	}
	for _, raw := range parsed.Models {
		var info codexModelInfo
		if err := json.Unmarshal(raw, &info); err != nil || info.Slug == "" {
			result.MalformedEntries++
			continue
		}
		displayName := info.DisplayName
		switch info.Slug {
		case "glm-5.3":
			info.DisplayName = "GLM-5.3"
		case "glm-5.3-flash":
			info.DisplayName = "GLM-5.3-Flash"
		}
		if info.DisplayName != displayName {
			raw = cloneRawWithOverrides(raw, Entry{ID: info.Slug, DisplayName: info.DisplayName})
		}
		result.Entries = append(result.Entries, Entry{
			Provider: ProviderZAI, ID: info.Slug, DisplayName: info.DisplayName,
			Description: info.Description, ContextWindow: info.ContextWindow,
			MaxContextWindow: info.MaxContextWindow, Priority: info.Priority,
			Visibility: info.Visibility, Source: SourceNative, Raw: raw,
		})
		result.CodexRawByID[info.Slug] = raw
	}
	return result, nil
}
