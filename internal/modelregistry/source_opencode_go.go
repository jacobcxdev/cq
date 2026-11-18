package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	OpenCodeGoBaseURL           = "https://opencode.ai/inference/go"
	ProviderOpenCodeGo Provider = "opencode-go"
	openCodeGoName              = "OpenCode Go"
)

type OpenCodeGoSource struct {
	Client     httputil.Doer
	URL        string
	Credential func(context.Context) (bearer, account, workspaceID string, err error)
}

type openCodeGoModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Limit       struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Capabilities struct {
		Tools struct {
			Supported bool `json:"supported"`
		} `json:"tools"`
		Reasoning struct {
			Effort []string `json:"effort"`
		} `json:"reasoning"`
	} `json:"capabilities"`
	Go struct {
		MonthlyUsage struct {
			Go float64 `json:"go"`
		} `json:"monthly_usage"`
	} `json:"go"`
}

func (s *OpenCodeGoSource) Fetch(ctx context.Context) (SourceResult, error) {
	if s.Credential == nil {
		return SourceResult{NotConfigured: true}, nil
	}
	bearer, account, workspaceID, err := s.Credential(ctx)
	if err != nil {
		return SourceResult{}, errors.New("OpenCode Go catalogue credential unavailable")
	}
	if bearer == "" {
		return SourceResult{NotConfigured: true}, nil
	}
	scope := FingerprintSharedBudgetCredential(bearer, account, workspaceID)
	target := s.URL
	if target == "" {
		target = OpenCodeGoBaseURL + "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return SourceResult{}, errors.New("invalid OpenCode Go catalogue URL")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	if workspaceID != "" {
		req.Header.Set("X-OpenCode-Org-Id", workspaceID)
	}
	client := s.Client
	if httpClient, ok := client.(*http.Client); ok {
		noRedirects := *httpClient
		noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &noRedirects
	}
	resp, err := client.Do(req)
	if err != nil {
		return SourceResult{}, errors.New("OpenCode Go catalogue request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SourceResult{}, fmt.Errorf("OpenCode Go catalogue returned HTTP %d", resp.StatusCode)
	}
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return SourceResult{}, err
	}
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data == nil {
		return SourceResult{}, errors.New("invalid OpenCode Go catalogue")
	}
	result := SourceResult{FetchedAt: time.Now()}
	seen := make(map[string]bool)
	for _, raw := range envelope.Data {
		var model openCodeGoModel
		if json.Unmarshal(raw, &model) != nil || model.ID == "" || strings.TrimSpace(model.ID) != model.ID {
			result.MalformedEntries++
			continue
		}
		id := strings.ToLower(model.ID)
		if seen[id] || model.Type != "chat" || !model.Capabilities.Tools.Supported || model.Limit.Context <= 0 || model.Go.MonthlyUsage.Go <= 0 {
			continue
		}
		seen[id] = true
		name := model.Name
		if name == "" {
			name = model.ID
		}
		result.Entries = append(result.Entries, Entry{
			Provider: ProviderOpenCodeGo, ID: model.ID, DisplayName: name + " (" + openCodeGoName + ")",
			Description: model.Description, ContextWindow: model.Limit.Context, MaxContextWindow: model.Limit.Context,
			MaxOutputTokens: model.Limit.Output, Visibility: "list", Source: SourceNative, Raw: raw,
			SharedBudget: &SharedBudgetModel{
				ProviderName: openCodeGoName, ReasoningEfforts: model.Capabilities.Reasoning.Effort, CredentialScope: scope,
			},
		})
	}
	return result, nil
}
