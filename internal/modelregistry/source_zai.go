package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// ZAISource fetches native Codex model metadata from Z.ai's Responses endpoint.
type ZAISource struct {
	Client httputil.Doer
	Token  func(context.Context) (string, error)
	// Tokens returns preferred-account order and takes precedence over Token.
	Tokens func(context.Context) ([]string, error)
}

func (s *ZAISource) Fetch(ctx context.Context) (result SourceResult, err error) {
	defer func() {
		if recover() != nil {
			result = SourceResult{}
			err = errors.New("zai source: model discovery panicked")
		}
	}()

	var tokens []string
	if s.Tokens != nil {
		tokens, err = s.Tokens(ctx)
	} else if s.Token != nil {
		var token string
		token, err = s.Token(ctx)
		tokens = []string{token}
	}
	if err != nil {
		return SourceResult{}, errors.New("zai source: API key unavailable")
	}
	keys := make([]string, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token != "" && !seen[token] {
			keys = append(keys, token)
			seen[token] = true
		}
	}
	if len(keys) == 0 {
		return SourceResult{}, errors.New("zai source: API key unavailable")
	}
	var resp *http.Response
	for i, key := range keys {
		if ctx.Err() != nil {
			return SourceResult{}, errors.New("zai source: model request cancelled")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.z.ai/api/v1/models", nil)
		if err != nil {
			return SourceResult{}, errors.New("zai source: build request failed")
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err = s.Client.Do(req)
		if err != nil {
			return SourceResult{}, errors.New("zai source: model request failed")
		}
		rejected := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests
		if !rejected || i == len(keys)-1 {
			break
		}
		_ = resp.Body.Close()
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
		if strings.HasPrefix(strings.ToLower(info.Slug), "glm-") &&
			(info.DisplayName == "" || strings.EqualFold(info.DisplayName, info.Slug)) {
			name := info.DisplayName
			if name == "" {
				name = info.Slug
			}
			parts := strings.Split(name, "-")
			parts[0] = "GLM"
			for i := 1; i < len(parts); i++ {
				part := []rune(parts[i])
				if len(part) > 0 {
					part[0] = unicode.ToUpper(part[0])
					parts[i] = string(part)
				}
			}
			info.DisplayName = strings.Join(parts, "-")
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
