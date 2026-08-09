package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

const credentialAuthorityUnavailableMessage = "Codex credential coordinator unavailable"

// Provider implements provider.Provider for Codex (OpenAI).
type Provider struct {
	client        httputil.Doer
	fs            fsutil.FileSystem
	inventory     CredentialInventory
	secrets       ExactSecretResolver
	refreshBroker CredentialRefreshBroker
}

// New creates a Provider that uses the given HTTP client for API calls.
func New(client httputil.Doer) *Provider {
	return &Provider{client: client, fs: fsutil.OSFileSystem{}}
}

// Fetch discovers all Codex accounts and fetches quota for each in parallel.
func (p *Provider) Fetch(ctx context.Context, _ time.Time) ([]quota.Result, error) {
	broker := p.refreshBroker
	inventoryReader := p.inventory
	secrets := p.secrets
	var control *CredentialControl
	if broker == nil && inventoryReader == nil && secrets == nil {
		if durableFS, ok := p.fs.(fsutil.DurableFileSystem); ok {
			var err error
			control, err = OpenDefaultCredentialRefreshControl(ctx, durableFS, p.client)
			if err != nil {
				return []quota.Result{credentialAuthorityUnavailableResult(AccountIdentity{})}, nil
			}
			defer control.Close()
			broker = control
			inventoryReader = control
			secrets = control
		}
	}
	var inventory Inventory
	if inventoryReader != nil {
		listed, err := inventoryReader.List(ctx)
		if err != nil {
			if errors.Is(err, ErrCredentialAuthorityUnavailable) {
				return []quota.Result{credentialAuthorityUnavailableResult(AccountIdentity{})}, nil
			}
			return nil, fmt.Errorf("list Codex credential inventory: %w", err)
		}
		inventory = listed
	} else {
		inventory = DiscoverInventory(p.fs)
	}
	if len(inventory.Accounts) == 0 {
		return []quota.Result{quota.ErrorResult("not_configured", "not configured", 0)}, nil
	}

	results := make([]quota.Result, len(inventory.Accounts))
	var wg sync.WaitGroup
	for i, logical := range inventory.Accounts {
		wg.Add(1)
		go func(i int, logical LogicalAccount) {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					fmt.Fprintf(os.Stderr, "cq: panic in codex provider: %v\n%s\n", rv, debug.Stack())
					results[i] = quota.ErrorResult("panic", fmt.Sprintf("%v", rv), 0)
				}
			}()
			results[i] = p.fetchLogicalAccount(ctx, logical, inventoryReader, secrets, broker)
			results[i].Active = logical.Active
		}(i, logical)
	}
	wg.Wait()

	return results, nil
}

func (p *Provider) fetchLogicalAccount(ctx context.Context, logical LogicalAccount, inventory CredentialInventory, secrets ExactSecretResolver, broker CredentialRefreshBroker) quota.Result {
	if !logical.Routable {
		return noTokenResult(logical)
	}
	candidates := ResolveCandidate(logical, "", time.Now())
	dispatchable := candidates[:0]
	for _, candidate := range candidates {
		if candidate.Routable {
			dispatchable = append(dispatchable, candidate)
		}
	}
	candidates = dispatchable
	if len(candidates) == 0 {
		return noTokenResult(logical)
	}
	var last quota.Result
	refreshable := make([]PlannedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		planned := PlanCandidate(logical, candidate)
		credential := candidate.Credential
		if secrets != nil {
			material, resolved, err := ResolvePlannedCandidate(ctx, inventory, secrets, planned)
			if err != nil {
				if ctx.Err() != nil {
					last = quota.ErrorResult("transport_error", ctx.Err().Error(), 0)
					last.Email = logical.Identity.Email
					last.AccountID = logical.Identity.AccountID
					return last
				}
				if errors.Is(err, ErrCredentialAuthorityUnavailable) {
					return credentialAuthorityUnavailableResult(logical.Identity)
				}
				last = quota.ErrorResult("auth_expired", "auth expired — re-authenticate via codex login", 0)
				last.Email = logical.Identity.Email
				last.AccountID = logical.Identity.AccountID
				continue
			}
			planned = resolved
			credential = CodexAccount{
				AccessToken: material.AccessToken, RefreshToken: material.RefreshToken,
				IDToken: material.IDToken, AccountID: material.AccountID,
				Email: logical.Identity.Email, UserID: logical.Identity.UserID,
				PlanType: logical.Identity.PlanType, RecordKey: logical.Identity.RecordKey,
			}
		}
		if credential.AccessToken == "" {
			last = quota.ErrorResult("no_token", "no token", 0)
			last.Email = logical.Identity.Email
			last.AccountID = logical.Identity.AccountID
			continue
		}
		last = p.fetchAccount(ctx, credential)
		if last.Error == nil || last.Error.Code != "auth_expired" {
			return last
		}
		if planned.Source == SourceManaged {
			refreshable = append(refreshable, planned)
		}
	}
	if broker == nil {
		return last
	}
	for _, planned := range refreshable {
		refreshed, err := broker.Refresh(ctx, planned.Ref, planned.Revision)
		if ctxErr := ctx.Err(); ctxErr != nil {
			last = quota.ErrorResult("transport_error", ctxErr.Error(), 0)
			last.Email = logical.Identity.Email
			last.AccountID = logical.Identity.AccountID
			return last
		}
		if err != nil {
			if errors.Is(err, ErrCredentialAuthorityUnavailable) {
				return credentialAuthorityUnavailableResult(logical.Identity)
			}
			continue
		}
		if refreshed.Ref != planned.Ref || refreshed.Revision == "" ||
			!credentialMaterialMatchesIdentity(refreshed.Material, planned.Identity) {
			continue
		}
		credential := CodexAccount{
			AccessToken: refreshed.Material.AccessToken, RefreshToken: refreshed.Material.RefreshToken,
			IDToken: refreshed.Material.IDToken, AccountID: refreshed.Material.AccountID,
			Email: logical.Identity.Email, UserID: logical.Identity.UserID,
			PlanType: logical.Identity.PlanType, RecordKey: logical.Identity.RecordKey,
		}
		last = p.fetchAccount(ctx, credential)
		if last.Error == nil || last.Error.Code != "auth_expired" {
			return last
		}
	}
	return last
}

func noTokenResult(logical LogicalAccount) quota.Result {
	r := quota.ErrorResult("no_token", "no token", 0)
	r.Email = logical.Identity.Email
	r.AccountID = logical.Identity.AccountID
	return r
}

func credentialAuthorityUnavailableResult(identity AccountIdentity) quota.Result {
	result := quota.ErrorResult("fetch_error", credentialAuthorityUnavailableMessage, 0)
	result.Email = identity.Email
	result.AccountID = identity.AccountID
	return result
}

// DiscoverAccounts returns the coordinator's Codex account inventory without
// making network calls. It implements provider.Discoverer so the runner can
// synthesise auth_expired rows for accounts absent from the cache.
func (p *Provider) DiscoverAccounts(ctx context.Context) ([]provider.Account, error) {
	inventoryReader := p.inventory
	var control *CredentialControl
	if inventoryReader == nil {
		if durableFS, ok := p.fs.(fsutil.DurableFileSystem); ok {
			var err error
			control, err = OpenDefaultCredentialRefreshControl(ctx, durableFS, p.client)
			if err != nil {
				return nil, fmt.Errorf("open Codex credential inventory: %w", err)
			}
			defer control.Close()
			inventoryReader = control
		}
	}
	var inventory Inventory
	if inventoryReader != nil {
		listed, err := inventoryReader.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list Codex credential inventory: %w", err)
		}
		inventory = listed
	} else {
		inventory = DiscoverInventory(p.fs)
	}
	out := make([]provider.Account, len(inventory.Accounts))
	for i, logical := range inventory.Accounts {
		out[i] = provider.Account{
			AccountID: logical.Identity.AccountID,
			Email:     logical.Identity.Email,
			Label:     logical.Identity.PlanType,
			Active:    logical.Active,
		}
	}
	return out, nil
}

// fetchAccount fetches quota for one concrete candidate. Refresh decisions
// stay at the logical-account broker boundary; this method never mutates auth.
func (p *Provider) fetchAccount(ctx context.Context, acct CodexAccount) quota.Result {
	if acct.AccessToken == "" {
		r := quota.ErrorResult("no_token", "no token", 0)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	body, code, err := fetchUsage(ctx, p.client, acct.AccessToken, acct.AccountID)
	if err != nil {
		r := quota.ErrorResult("transport_error", err.Error(), 0)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	if code == 401 || code == 403 {
		r := quota.ErrorResult("auth_expired", "auth expired — re-authenticate via codex login", code)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	if code != 200 {
		r := quota.ErrorResult("api_error", "api error", code)
		r.Email = acct.Email
		r.AccountID = acct.AccountID
		return r
	}

	return parseUsage(body, acct.Email, acct.AccountID)
}

// dedup removes duplicate results by AccountID, preferring usable results
// over errors when both exist for the same account.
func dedup(results []quota.Result) []quota.Result {
	if len(results) <= 1 {
		return results
	}
	seen := make(map[string]int) // key -> index in out
	var out []quota.Result
	for _, r := range results {
		key := r.AccountID
		if key == "" {
			key = r.Email
		}
		if key == "" {
			out = append(out, r)
			continue
		}
		if idx, exists := seen[key]; exists {
			if r.IsUsable() && !out[idx].IsUsable() {
				out[idx] = r
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, r)
	}
	return out
}
