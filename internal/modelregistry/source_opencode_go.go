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
	Client      httputil.Doer
	URL         string
	Credential  func(context.Context) (bearer, account, workspaceID string, err error)
	Credentials func(context.Context) ([]OpenCodeGoCredential, error)
}

type OpenCodeGoCredential struct {
	Bearer, Account, WorkspaceID string
}

type openCodeGoModel struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	Type        string                     `json:"type"`
	Description string                     `json:"description"`
	API         map[string]json.RawMessage `json:"api"`
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
	if s.Credential == nil && s.Credentials == nil {
		return SourceResult{NotConfigured: true}, nil
	}
	var credentials []OpenCodeGoCredential
	var err error
	if s.Credentials != nil {
		credentials, err = s.Credentials(ctx)
	} else {
		var credential OpenCodeGoCredential
		credential.Bearer, credential.Account, credential.WorkspaceID, err = s.Credential(ctx)
		credentials = []OpenCodeGoCredential{credential}
	}
	if err != nil {
		return SourceResult{}, errors.New("OpenCode Go catalogue credential unavailable")
	}
	result := SourceResult{FetchedAt: time.Now()}
	var failures []error
	configured, succeeded := false, false
	for _, credential := range credentials {
		if err := ctx.Err(); err != nil {
			return SourceResult{}, err
		}
		if credential.Bearer == "" {
			continue
		}
		configured = true
		accountResult, fetchErr := s.fetchAccount(ctx, credential)
		if fetchErr != nil {
			failures = append(failures, fetchErr)
			continue
		}
		succeeded = true
		result.Entries = append(result.Entries, accountResult.Entries...)
		result.MalformedEntries += accountResult.MalformedEntries
	}
	if !succeeded && len(failures) > 0 {
		return SourceResult{}, errors.Join(failures...)
	}
	if !configured {
		return SourceResult{NotConfigured: true}, nil
	}
	return result, nil
}

func (s *OpenCodeGoSource) fetchAccount(ctx context.Context, credential OpenCodeGoCredential) (SourceResult, error) {
	bearer, account, workspaceID := credential.Bearer, credential.Account, credential.WorkspaceID
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
		var nativeAPI string
		if len(model.API) == 1 {
			for protocol := range model.API {
				switch protocol {
				case "responses", "chat-completions", "messages", "generate-content":
					nativeAPI = protocol
				}
			}
		}
		result.Entries = append(result.Entries, Entry{
			Provider: ProviderOpenCodeGo, ID: model.ID, DisplayName: name + " (" + openCodeGoName + ")",
			Description: model.Description, ContextWindow: model.Limit.Context, MaxContextWindow: model.Limit.Context,
			MaxOutputTokens: model.Limit.Output, Visibility: "list", Source: SourceNative, Raw: raw,
			SharedBudget: &SharedBudgetModel{
				ProviderName: openCodeGoName, ReasoningEfforts: model.Capabilities.Reasoning.Effort, NativeAPI: nativeAPI, CredentialScope: scope,
			},
		})
	}
	return result, nil
}
