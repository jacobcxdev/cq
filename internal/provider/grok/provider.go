package grok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// Provider reports included Grok subscription quota through CQ's normal fetch
// and render path. The authority owns credentials and renewal.
type Provider struct{ authority Authority }

func New(authority Authority) *Provider { return &Provider{authority: authority} }

func (p *Provider) Fetch(ctx context.Context, now time.Time) (results []quota.Result, err error) {
	defer func() {
		if recover() != nil {
			results = []quota.Result{quota.ErrorResult("fetch_panic", "Grok provider failed", 0)}
			err = nil
		}
	}()
	if p == nil || p.authority == nil {
		return []quota.Result{quota.ErrorResult("not_configured", "run cq grok login", 0)}, nil
	}
	accounts, err := p.authority.Accounts(ctx)
	if err != nil {
		return []quota.Result{quota.ErrorResult("fetch_error", "read Grok accounts", 0)}, nil
	}
	seen := make(map[string]bool, len(accounts))
	unique := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		// A saved alias for the same user/principal/seat is not independent
		// capacity. Distinct seats within a team retain their own allowance.
		key := string(account.ID)
		if account.UserID != "" {
			key = account.UserID + "\x00" + account.PrincipalType + "\x00" + account.PrincipalID + "\x00" + account.TeamID
		}
		if account.ID == "" || seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, account)
	}
	if len(unique) == 0 {
		return []quota.Result{quota.ErrorResult("not_configured", "run cq grok login", 0)}, nil
	}
	results = make([]quota.Result, len(unique))
	var group sync.WaitGroup
	for i, account := range unique {
		group.Add(1)
		go func() { defer group.Done(); results[i] = p.fetchAccount(ctx, account, now) }()
	}
	group.Wait()
	return results, nil
}

func (p *Provider) fetchAccount(ctx context.Context, account Account, now time.Time) (result quota.Result) {
	defer func() {
		if recover() != nil {
			result = quota.ErrorResult("fetch_panic", "Grok provider failed", 0)
		}
		result.AccountID = string(account.ID)
		result.Email = account.Email
		if result.Email == "" {
			result.Email = account.Label
		}
		result.Plan = account.Plan
		result.Active = account.Active
	}()
	if account.ReportingOnly {
		return quota.ErrorResult("not_configured", "Grok source requires a verified subscription login; run cq grok login", 0)
	}
	body, err := readSubscriptionJSON(ctx, p.authority, account.ID, "/billing?format=credits")
	if err != nil {
		if errors.Is(err, ErrAuthExpired) {
			return quota.ErrorResult("auth_expired", "run cq grok login", 0)
		}
		var httpErr subscriptionHTTPError
		if errors.As(err, &httpErr) {
			if httpErr.status == http.StatusUnauthorized {
				return quota.ErrorResult("auth_expired", "Grok subscription login rejected; run cq grok login", httpErr.status)
			}
			return quota.ErrorResult("api_error", "Grok subscription quota request failed", httpErr.status)
		}
		return quota.ErrorResult("fetch_error", "Grok subscription quota request failed", 0)
	}
	result, err = parseQuota(body, now)
	if err != nil {
		return quota.ErrorResult("parse_error", "Grok included subscription quota unavailable", 0)
	}
	// The /user tier is an internal identifier (e.g. GrokPro). Follow the
	// official client's account-scoped display name rather than guessing aliases.
	// Optional presentation metadata must not hide otherwise usable quota.
	if settings, err := readSubscriptionJSON(ctx, p.authority, account.ID, "/settings"); err == nil {
		var display struct {
			Plan string `json:"subscription_tier_display"`
		}
		if json.Unmarshal(settings, &display) == nil && validText(display.Plan, 256) && strings.TrimSpace(display.Plan) != "" {
			account.Plan = display.Plan
		}
	}
	return result
}
