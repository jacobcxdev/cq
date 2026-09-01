package codex

import (
	"context"
	"encoding/json"
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

const (
	credentialAuthorityUnavailableMessage = "Codex credential coordinator unavailable"
	credentialInventoryStaleMessage       = "Codex credential inventory stale"
	credentialInventoryDegradedMessage    = "Codex credential inventory degraded"
	credentialOwnerRefreshRequiredMessage = "access expired — credential owner must refresh"
	cqRefreshRequiredMessage              = "access expired — CQ refresh required"
	credentialRejectedMessage             = "credential rejected by provider"
)

type logicalInventoryAccount struct {
	key       AccountKey
	accountID string
	userID    string
	email     string
	active    bool
}

// Provider implements provider.Provider for Codex (OpenAI).
type Provider struct {
	client        httputil.Doer
	fs            fsutil.FileSystem
	inventory     CredentialInventory
	secrets       ExactSecretResolver
	refreshBroker CredentialRefreshBroker

	inventoryMu      sync.RWMutex
	lastInventory    []logicalInventoryAccount
	hasLastInventory bool
}

// New creates a Provider that uses the given HTTP client for API calls.
func New(client httputil.Doer) *Provider {
	return &Provider{client: client, fs: fsutil.OSFileSystem{}}
}

// Fetch discovers all Codex accounts and fetches quota for each in parallel.
func (p *Provider) Fetch(ctx context.Context, now time.Time) ([]quota.Result, error) {
	broker := p.refreshBroker
	inventoryReader := p.inventory
	secrets := p.secrets
	var control *CredentialControl
	if broker == nil && inventoryReader == nil && secrets == nil {
		if durableFS, ok := p.fs.(fsutil.DurableFileSystem); ok {
			var err error
			control, err = OpenDefaultCredentialRefreshControl(ctx, durableFS, p.client)
			if err != nil {
				return p.credentialInventoryFailureResults(), nil
			}
			defer control.Close()
			broker = control
			inventoryReader = control
			secrets = control
		}
	}
	if broker != nil || inventoryReader != nil || secrets != nil {
		if inventoryReader == nil || secrets == nil {
			return p.credentialInventoryFailureResults(), nil
		}
	}
	var inventory Inventory
	if inventoryReader != nil {
		listed, err := inventoryReader.List(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return p.credentialInventoryFailureResults(), nil
		}
		inventory = listed
	} else {
		inventory = DiscoverInventory(p.fs)
	}
	inventoryDegraded := inventoryHasDegradedExternalSource(inventory)
	if inventoryReader != nil && !inventoryDegraded {
		p.rememberLogicalInventory(inventory)
	}
	if len(inventory.Accounts) == 0 {
		if inventoryDegraded {
			return p.appendMissingDegradedInventoryResults(inventory, nil), nil
		}
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
			results[i] = p.fetchLogicalAccount(ctx, logical, inventoryReader, secrets, broker, now)
			results[i].Active = logical.Active
		}(i, logical)
	}
	wg.Wait()
	if inventoryDegraded {
		for i, result := range results {
			if result.Error == nil || (result.Error.Code != "auth_expired" && result.Error.Code != "no_token") {
				continue
			}
			results[i] = credentialInventoryDegradedResult(inventory.Accounts[i].Identity, inventory.Accounts[i].Active)
		}
		results = p.appendMissingDegradedInventoryResults(inventory, results)
	}

	return results, nil
}

func (p *Provider) appendMissingDegradedInventoryResults(inventory Inventory, results []quota.Result) []quota.Result {
	p.inventoryMu.RLock()
	hasLast := p.hasLastInventory
	accounts := append([]logicalInventoryAccount(nil), p.lastInventory...)
	p.inventoryMu.RUnlock()
	for _, account := range accounts {
		present := false
		for _, logical := range inventory.Accounts {
			if sameLogicalInventoryAccount(logical, account) {
				present = true
				break
			}
		}
		if present {
			continue
		}
		results = append(results, credentialInventoryDegradedResult(AccountIdentity{
			AccountID: account.accountID,
			Email:     account.email,
		}, account.active))
	}
	if len(results) == 0 && (!hasLast || len(accounts) == 0) {
		return []quota.Result{credentialInventoryDegradedResult(AccountIdentity{}, false)}
	}
	return results
}

func sameLogicalInventoryAccount(logical LogicalAccount, account logicalInventoryAccount) bool {
	if logical.Key != "" && account.key != "" && logical.Key == account.key {
		return true
	}
	identity := logical.Identity
	return identity.AccountID != "" && identity.UserID != "" && account.accountID != "" && account.userID != "" &&
		identity.AccountID == account.accountID && identity.UserID == account.userID
}

func (p *Provider) fetchLogicalAccount(ctx context.Context, logical LogicalAccount, inventory CredentialInventory, secrets ExactSecretResolver, broker CredentialRefreshBroker, now time.Time) quota.Result {
	if !logical.Routable {
		return noTokenResult(logical)
	}
	candidates := ResolveCandidate(logical, "", now)
	last := noTokenResult(logical)
	hasProviderRejection := false
	inventoryDegraded := false
	refreshable := make([]PlannedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		switch CandidateAvailabilityAt(candidate, now) {
		case CandidateRefreshRequired:
			refreshable = append(refreshable, PlanCandidate(logical, candidate))
			if !hasProviderRejection {
				last = authExpiredResult(logical.Identity, cqRefreshRequiredMessage, 0)
			}
			continue
		case CandidateUnavailable:
			if !hasProviderRejection && candidate.Routable && !candidate.AccessExpiresAt.IsZero() && !candidate.AccessExpiresAt.After(now) {
				last = credentialOwnerRefreshRequiredResult(logical.Identity)
			}
			continue
		}
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
				if errors.Is(err, ErrCredentialInventoryDegraded) {
					inventoryDegraded = true
					continue
				}
				inventoryDegraded = true
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
		hasProviderRejection = true
		if candidate.Source == SourceManaged && candidate.CQAuthored && candidate.RefreshEligible {
			refreshable = append(refreshable, planned)
			last.Error.Message = cqRefreshRequiredMessage
		}
	}
	if broker == nil {
		if inventoryDegraded {
			return credentialInventoryDegradedResult(logical.Identity, logical.Active)
		}
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
	if inventoryDegraded {
		return credentialInventoryDegradedResult(logical.Identity, logical.Active)
	}
	return last
}

func authExpiredResult(identity AccountIdentity, message string, status int) quota.Result {
	result := quota.ErrorResult("auth_expired", message, status)
	result.Email = identity.Email
	result.AccountID = identity.AccountID
	return result
}

func credentialOwnerRefreshRequiredResult(identity AccountIdentity) quota.Result {
	return authExpiredResult(identity, credentialOwnerRefreshRequiredMessage, 0)
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

func (p *Provider) rememberLogicalInventory(inventory Inventory) {
	accounts := make([]logicalInventoryAccount, len(inventory.Accounts))
	for i, logical := range inventory.Accounts {
		accounts[i] = logicalInventoryAccount{
			key:       logical.Key,
			accountID: logical.Identity.AccountID,
			userID:    logical.Identity.UserID,
			email:     logical.Identity.Email,
			active:    logical.Active,
		}
	}
	p.inventoryMu.Lock()
	p.lastInventory = accounts
	p.hasLastInventory = true
	p.inventoryMu.Unlock()
}

func (p *Provider) credentialInventoryFailureResults() []quota.Result {
	p.inventoryMu.RLock()
	hasLast := p.hasLastInventory
	accounts := append([]logicalInventoryAccount(nil), p.lastInventory...)
	p.inventoryMu.RUnlock()
	if !hasLast {
		return []quota.Result{credentialAuthorityUnavailableResult(AccountIdentity{})}
	}
	if len(accounts) == 0 {
		return []quota.Result{credentialInventoryStaleResult(AccountIdentity{}, false)}
	}
	results := make([]quota.Result, len(accounts))
	for i, account := range accounts {
		results[i] = credentialInventoryStaleResult(AccountIdentity{
			AccountID: account.accountID,
			Email:     account.email,
		}, account.active)
	}
	return results
}

func credentialInventoryStaleResult(identity AccountIdentity, active bool) quota.Result {
	result := quota.ErrorResult("fetch_error", credentialInventoryStaleMessage, 0)
	result.Email = identity.Email
	result.AccountID = identity.AccountID
	result.Active = active
	return result
}

func credentialInventoryDegradedResult(identity AccountIdentity, active bool) quota.Result {
	result := quota.ErrorResult("fetch_error", credentialInventoryDegradedMessage, 0)
	result.Email = identity.Email
	result.AccountID = identity.AccountID
	result.Active = active
	return result
}

func inventoryHasDegradedExternalSource(inventory Inventory) bool {
	for _, source := range inventory.ExternalSources {
		if source.ErrorCode != "" && !source.OptionalAbsent {
			return true
		}
	}
	return false
}

func (p *Provider) discoverAccountInventory(ctx context.Context) (Inventory, error) {
	inventoryReader := p.inventory
	var control *CredentialControl
	if inventoryReader == nil {
		if durableFS, ok := p.fs.(fsutil.DurableFileSystem); ok {
			var err error
			control, err = OpenDefaultCredentialRefreshControl(ctx, durableFS, p.client)
			if err != nil {
				return Inventory{}, fmt.Errorf("open Codex credential inventory: %w", err)
			}
			defer control.Close()
			inventoryReader = control
		}
	}
	if inventoryReader != nil {
		listed, err := inventoryReader.List(ctx)
		if err != nil {
			return Inventory{}, fmt.Errorf("list Codex credential inventory: %w", err)
		}
		return listed, nil
	} else {
		return DiscoverInventory(p.fs), nil
	}
}

// DiscoverAccounts returns the coordinator's Codex account inventory without
// making network calls. It implements provider.Discoverer so the runner can
// report accounts whose usage is absent from the cache.
func (p *Provider) DiscoverAccounts(ctx context.Context) ([]provider.Account, error) {
	inventory, err := p.discoverAccountInventory(ctx)
	if err != nil {
		return nil, err
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

type RoutingAccount struct {
	Key       AccountKey
	AccountID string
	Email     string
	Active    bool
}

// RoutingAccounts exposes secret-free identities needed to compare provider
// discovery with proxy routing policy.
func (p *Provider) RoutingAccounts(ctx context.Context) ([]RoutingAccount, error) {
	inventory, err := p.discoverAccountInventory(ctx)
	if err != nil {
		return nil, err
	}
	accounts := make([]RoutingAccount, len(inventory.Accounts))
	for i, logical := range inventory.Accounts {
		accounts[i] = RoutingAccount{
			Key:       logical.Key,
			AccountID: logical.Identity.AccountID,
			Email:     logical.Identity.Email,
			Active:    logical.Active,
		}
	}
	return accounts, nil
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

	if codexUsageAuthRejected(code, body) {
		r := quota.ErrorResult("auth_expired", credentialRejectedMessage, code)
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

func codexUsageAuthRejected(status int, body []byte) bool {
	if status == 401 {
		return true
	}
	if status != 403 {
		return false
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Error.Type == "authentication_error"
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
