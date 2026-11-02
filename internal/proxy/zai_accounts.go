package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/quota"
)

// PrioritiseZAIAccounts keeps preferred order among healthy or unknown accounts,
// followed by subscriptions known to be unavailable. Callers supply fresh quota
// results. Every credential remains available for explicit rejection fallback.
func PrioritiseZAIAccounts(accounts []zaiprov.Account, results []quota.Result, now time.Time) []zaiprov.Account {
	unavailable := make(map[string]bool)
	for _, result := range results {
		if result.AccountID == "" {
			continue
		}
		if result.Status == quota.StatusError && result.Error != nil && result.Error.Code == "auth_expired" {
			unavailable[result.AccountID] = true
			continue
		}
		if !result.IsUsable() {
			continue
		}
		for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
			window, exists := result.Windows[name]
			if !exists || window.ResetAtUnix > 0 && window.ResetAtUnix <= now.Unix() {
				continue
			}
			remaining := float64(window.RemainingPct)
			if window.RemainingPctExact != nil {
				remaining = *window.RemainingPctExact
			}
			if remaining == 0 {
				unavailable[result.AccountID] = true
			}
		}
	}
	ordered := make([]zaiprov.Account, 0, len(accounts))
	for _, account := range accounts {
		if !unavailable[account.ID] {
			ordered = append(ordered, account)
		}
	}
	for _, account := range accounts {
		if unavailable[account.ID] {
			ordered = append(ordered, account)
		}
	}
	return ordered
}

// Read credentials for each request so account changes do not require a proxy
// restart. The legacy reader remains available for single-account callers.
func (s *Server) zaiAccounts() ([]zaiprov.Account, error) {
	var accounts []zaiprov.Account
	var err error
	if s.ZAIAccounts != nil {
		accounts, err = s.ZAIAccounts()
	} else if s.ZAIAPIKey != nil {
		var key string
		key, err = s.ZAIAPIKey()
		accounts = []zaiprov.Account{{APIKey: key}}
	}
	if err != nil {
		return nil, err
	}
	unique := make([]zaiprov.Account, 0, len(accounts))
	seen := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		account.APIKey = strings.TrimSpace(account.APIKey)
		if account.APIKey != "" && !seen[account.APIKey] {
			unique = append(unique, account)
			seen[account.APIKey] = true
		}
	}
	return unique, nil
}

// A rejected request may move to another subscription before any response is
// forwarded. Network errors and accepted streams are never replayed: the
// upstream may already have consumed them. Opaque response IDs stay on the
// preferred account; WebSocket requests instead replay their complete history.
func (s *Server) doZAIRequest(ctx context.Context, target string, header http.Header, body []byte, accounts []zaiprov.Account) (*http.Response, error) {
	var request struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	_ = json.Unmarshal(body, &request)
	if request.PreviousResponseID != "" {
		accounts = accounts[:1]
	} else if s.ZAIQuota != nil {
		accounts = PrioritiseZAIAccounts(accounts, s.ZAIQuota(), time.Now())
	}
	transport := s.ZAITransport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i, account := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		setZAIHeaders(request.Header, header, account.APIKey)
		noteRouteAccount(ctx, redactedAccountHint("zai", account.ID), i > 0)
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		rejected := response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests
		if !rejected || i == len(accounts)-1 {
			return response, nil
		}
		// Closing without draining keeps a rejected or unbounded upstream body
		// from delaying account failover. No rejected payload reaches diagnostics.
		_ = response.Body.Close()
		fmt.Fprintf(os.Stderr, "cq: Z.ai account=%q rejected status=%d; trying next account\n", account.ID, response.StatusCode)
	}
	return nil, fmt.Errorf("Z.ai API key unavailable")
}
