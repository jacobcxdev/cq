package proxy

import (
	"context"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// redistributionInventory deliberately requires known included quota before
// replacing a current account. Unknown capacity cannot justify extra spend.
func (factory *CodexHTTPRequestPlanFactory) redistributionInventory(ctx context.Context, protocol CodexProtocolRequest, snapshot CodexLeaseRouteSnapshot, inventory codex.Inventory) codex.Inventory {
	if factory.PinnedAccountKey != "" {
		return codex.Inventory{}
	}
	accounts := codexHTTPRequestPlanAccountKeys(inventory)
	now := time.Now()
	if factory.Now != nil {
		now = factory.Now()
	}
	caller, _ := runtimeCallerAuthority(ctx)
	allowed := accounts
	if factory.SessionPolicy != nil {
		var decision SessionPolicyDecision
		var err error
		if factory.CyberEligibility != nil && protocol.CyberAccessProgram != "" {
			decision, err = enforceSessionCyberPolicy(factory.SessionPolicy, caller, []byte(protocol.Metadata.Metadata.SessionID), accounts, "", now)
		} else {
			decision, err = enforceSessionPolicy(factory.SessionPolicy, caller, []byte(protocol.Metadata.Metadata.SessionID), accounts, "", now)
		}
		if err != nil {
			return codex.Inventory{}
		}
		allowed = decision.Allowed
	}
	inventory = filterCodexHTTPRequestInventory(inventory, allowed)
	if factory.CyberEligibility != nil && protocol.CyberAccessProgram != "" {
		inventory, _ = filterCodexCyberInventory(factory.CyberEligibility, inventory, protocol)
	}
	candidates, err := ProjectCodexRoutePolicyCandidates(inventory, factory.Capacity, codexHTTPRequestPlanRequirements(protocol), nil, now)
	if err != nil {
		return codex.Inventory{}
	}
	positive := make([]codex.AccountKey, 0, len(candidates))
	for _, candidate := range candidates {
		account := candidate.Choice.AccountKey
		if !candidate.Routable || !candidate.Compatible || codexRoutePolicyCapacity(candidate).State != CapacityPositive {
			continue
		}
		included := true
		for _, bucket := range candidate.Choice.RequiredBuckets {
			if factory.Capacity.IncludedCapacity(account, bucket).State != CapacityPositive {
				included = false
				break
			}
		}
		if included {
			positive = append(positive, account)
		}
	}
	return filterCodexHTTPRequestInventory(inventory, positive)
}

// ShouldRedistribute asks a delta client to provide a portable full create,
// before any bytes of that delta are dispatched to an account.
func (factory *CodexHTTPRequestPlanFactory) ShouldRedistribute(ctx context.Context, protocol CodexProtocolRequest, active codex.AccountKey) bool {
	if factory == nil || factory.Inventory == nil || factory.Routes == nil || !protocol.Metadata.Strong || protocol.Metadata.Metadata.RequestKind != CodexRequestTurn {
		return false
	}
	inventory, err := factory.Inventory.List(ctx)
	if err != nil {
		return false
	}
	snapshot, err := factory.Routes.LoadRouteSnapshot(ctx, NewCodexLeaseKey(protocol.Metadata.Metadata), codexHTTPRequestPlanAccountKeys(inventory), factory.Authority)
	if err != nil || snapshot.RedistributionGeneration == 0 {
		return false
	}
	if active == "" {
		active = snapshot.BoundAccountKey
		if active == "" {
			active = snapshot.AffinityAccountKey
		}
	}
	positive := factory.redistributionInventory(ctx, protocol, snapshot, inventory)
	accounts := codexHTTPRequestPlanAccountKeys(positive)
	return !containsCodexHTTPRequestAccountKey(accounts, active) && len(accounts) != 0
}
