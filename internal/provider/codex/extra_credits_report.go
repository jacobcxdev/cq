package codex

import (
	"context"

	"github.com/jacobcxdev/cq/internal/quota"
)

// EnrichResults overlays current local permission after quota caching. Credit
// availability stays provider-owned; cached permission never controls display.
func (p *Provider) EnrichResults(ctx context.Context, results []quota.Result) []quota.Result {
	policy := p.ExtraCreditsPolicy
	var policyErr error
	if policy == nil {
		policy, policyErr = DefaultExtraCreditsPolicy()
	}
	inventory, inventoryErr := p.discoverAccountInventory(ctx)
	out := append([]quota.Result(nil), results...)
	for i := range out {
		result := &out[i]
		hasCurrentKey := false
		for _, account := range inventory.Accounts {
			if result.AccountKey != "" && string(account.Key) == result.AccountKey {
				hasCurrentKey = true
				break
			}
		}
		var matched *LogicalAccount
		matches := 0
		for index := range inventory.Accounts {
			account := &inventory.Accounts[index]
			if (hasCurrentKey && string(account.Key) != result.AccountKey) ||
				(!hasCurrentKey && result.AccountID == "" && result.Email == "") ||
				(result.AccountID != "" && account.Identity.AccountID != result.AccountID) ||
				(result.Email != "" && account.Identity.Email != result.Email) {
				continue
			}
			matched = account
			matches++
		}
		credits := quota.ExtraCredits{}
		if result.ExtraCredits != nil {
			credits = *result.ExtraCredits
		}
		credits.Enabled = false
		if inventoryErr == nil && policyErr == nil && policy != nil && matches == 1 {
			if key, err := ExtraCreditsIdentityKey(matched.Identity); err == nil {
				credits.Enabled, _ = policy.Allowed(key)
				if result.AccountKey == "" {
					result.AccountKey = string(matched.Key)
				}
			}
		}
		result.ExtraCredits = &credits
	}
	return out
}
