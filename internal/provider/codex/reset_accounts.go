package codex

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type ResetAccount struct {
	AccountKey AccountKey `json:"-"`
	AccountID  string     `json:"account_id,omitempty"`
	Email      string     `json:"email,omitempty"`
	PlanType   string     `json:"plan_type,omitempty"`
	Active     bool       `json:"active"`

	planned     PlannedCandidate
	refreshable bool
}

type ResetAccountSnapshot struct {
	Inventory Inventory
	Aliases   AccountAliasIndex
	Accounts  []ResetAccount
}

type ResetBackend struct {
	Inventory CredentialInventory
	Resolver  ExactSecretResolver
	Refresh   CredentialRefreshBroker
	Aliases   func() (AccountAliasIndex, error)
	Credits   ResetCreditClient
	Now       func() time.Time
}

func ProjectVisibleAccounts(inventory Inventory, now time.Time) []ResetAccount {
	accounts := make([]ResetAccount, 0, len(inventory.Accounts))
	for _, logical := range inventory.Accounts {
		candidates := ResolveCandidate(logical, "", now)
		if len(candidates) == 0 {
			continue
		}
		candidate := candidates[0]
		accounts = append(accounts, ResetAccount{
			AccountKey:  logical.Key,
			AccountID:   logical.Identity.AccountID,
			Email:       logical.Identity.Email,
			PlanType:    logical.Identity.PlanType,
			Active:      logical.Active,
			planned:     PlanCandidate(logical, candidate),
			refreshable: candidate.Source == SourceManaged && candidate.RefreshEligible,
		})
	}
	return accounts
}

func (b *ResetBackend) Snapshot(ctx context.Context) (ResetAccountSnapshot, error) {
	if b == nil || b.Inventory == nil {
		return ResetAccountSnapshot{}, errors.New("Codex credential inventory unavailable")
	}
	inventory, err := b.Inventory.List(ctx)
	if err != nil {
		return ResetAccountSnapshot{}, err
	}
	aliases := AccountAliasIndex{}
	if b.Aliases != nil {
		aliases, err = b.Aliases()
		if err != nil {
			return ResetAccountSnapshot{}, err
		}
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	return ResetAccountSnapshot{
		Inventory: inventory,
		Aliases:   aliases,
		Accounts:  ProjectVisibleAccounts(inventory, now),
	}, nil
}

func (s ResetAccountSnapshot) ResolveReference(reference string) (ResetAccount, error) {
	key, err := ResolveAccountReference(s.Inventory, s.Aliases, reference)
	if err != nil {
		return ResetAccount{}, err
	}
	for _, account := range s.Accounts {
		if account.AccountKey == key {
			return account, nil
		}
	}
	return ResetAccount{}, &AccountReferenceError{Code: AccountReferenceMissing}
}

func (b *ResetBackend) ListCredits(ctx context.Context, account ResetAccount) (ResetCreditInventory, error) {
	material, account, err := b.resolve(ctx, account)
	if err != nil {
		return ResetCreditInventory{}, err
	}
	inventory, err := b.Credits.List(ctx, material)
	if err == nil || !resetAuthenticationFailure(err) || !account.refreshable {
		return inventory, err
	}
	material, _, err = b.refreshAndResolve(ctx, account)
	if err != nil {
		return ResetCreditInventory{}, err
	}
	return b.Credits.List(ctx, material)
}

func (b *ResetBackend) Consume(ctx context.Context, account ResetAccount, creditID, requestID string) (ConsumeResetResult, error) {
	material, account, err := b.resolve(ctx, account)
	if err != nil {
		return ConsumeResetResult{}, err
	}
	result, err := b.Credits.Consume(ctx, material, creditID, requestID)
	if err == nil || !resetAuthenticationFailure(err) || !account.refreshable {
		return result, err
	}
	material, _, err = b.refreshAndResolve(ctx, account)
	if err != nil {
		return ConsumeResetResult{}, err
	}
	return b.Credits.Consume(ctx, material, creditID, requestID)
}

func (b *ResetBackend) resolve(ctx context.Context, account ResetAccount) (CredentialMaterial, ResetAccount, error) {
	if b == nil || b.Inventory == nil || b.Resolver == nil {
		return CredentialMaterial{}, account, errors.New("Codex credential resolver unavailable")
	}
	material, planned, err := ResolvePlannedCandidate(ctx, b.Inventory, b.Resolver, account.planned)
	if err != nil {
		return CredentialMaterial{}, account, err
	}
	account.planned = planned
	return material, account, nil
}

func (b *ResetBackend) refreshAndResolve(ctx context.Context, account ResetAccount) (CredentialMaterial, ResetAccount, error) {
	if b.Refresh == nil {
		return CredentialMaterial{}, account, errors.New("Codex managed refresh unavailable")
	}
	refreshed, err := b.Refresh.Refresh(ctx, account.planned.Ref, account.planned.Revision)
	if err != nil {
		return CredentialMaterial{}, account, err
	}
	if refreshed.Ref.AccountKey != account.planned.Ref.AccountKey ||
		refreshed.Ref.CandidateID != account.planned.Ref.CandidateID || refreshed.Revision == "" {
		return CredentialMaterial{}, account, ErrCredentialIdentityMismatch
	}
	account.planned.Ref = refreshed.Ref
	account.planned.Revision = refreshed.Revision
	return b.resolve(ctx, account)
}

func resetAuthenticationFailure(err error) bool {
	var statusErr *ResetHTTPError
	return errors.As(err, &statusErr) &&
		(statusErr.Status == http.StatusUnauthorized || statusErr.Status == http.StatusForbidden)
}
