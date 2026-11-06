package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/provider/grok"
)

// GrokSource adds subscription model metadata to the existing registry. The
// merged display is not account entitlement; dispatch must read its own account.
type GrokSource struct{ Authority grok.Authority }

func (s *GrokSource) Fetch(ctx context.Context) (result SourceResult, err error) {
	defer func() {
		if recover() != nil {
			result = SourceResult{}
			err = errors.New("grok source: model discovery failed")
		}
	}()
	if s == nil || s.Authority == nil {
		return SourceResult{}, errors.New("grok source: subscription authority unavailable")
	}
	accounts, err := s.Authority.Accounts(ctx)
	if err != nil {
		return SourceResult{}, errors.New("grok source: accounts unavailable")
	}
	result = SourceResult{Entries: []Entry{}, CodexRawByID: make(map[string]json.RawMessage), FetchedAt: time.Now()}
	seen := make(map[string]bool)
	succeeded := len(accounts) == 0
	eligible := 0
	for _, account := range accounts {
		if account.ReportingOnly {
			continue
		}
		eligible++
		if err := ctx.Err(); err != nil {
			return SourceResult{}, err
		}
		models, fetchErr := grok.FetchModels(ctx, s.Authority, account.ID)
		if fetchErr != nil {
			continue
		}
		succeeded = true
		for _, model := range models {
			if model.Backend != "responses" || model.Hidden || model.ContextWindow <= 0 {
				continue
			}
			key := strings.ToLower(model.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			entry := Entry{Provider: ProviderGrok, ID: model.ID, Aliases: model.Aliases, DisplayName: model.Name, Description: model.Description, ContextWindow: model.ContextWindow, MaxContextWindow: model.MaxContextWindow, MaxOutputTokens: model.MaxOutputTokens, Visibility: "list", Source: SourceNative, Raw: model.Raw}
			result.Entries = append(result.Entries, entry)
			result.CodexRawByID[model.ID] = grokCodexEntryRaw(entry)
		}
	}
	if eligible > 0 && !succeeded {
		return SourceResult{}, errors.New("grok source: subscription model catalogue unavailable")
	}
	result.NotConfigured = eligible == 0
	return result, nil
}
