package proxy

import (
	"strings"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexRouteProjectionErrorCode classifies secret-free projection failures.
type CodexRouteProjectionErrorCode string

const (
	CodexRouteProjectionInvalidInventory        CodexRouteProjectionErrorCode = "invalid_inventory"
	CodexRouteProjectionInvalidProvisionalCount CodexRouteProjectionErrorCode = "invalid_provisional_count"
)

// CodexRouteProjectionError omits account and credential metadata by design.
type CodexRouteProjectionError struct {
	Code CodexRouteProjectionErrorCode
}

func (e *CodexRouteProjectionError) Error() string {
	return "Codex route projection failed: " + string(e.Code)
}

// ProjectCodexRoutePolicyCandidates converts one secret-free inventory view
// into policy candidates without selecting credentials.
func ProjectCodexRoutePolicyCandidates(
	inventory codex.Inventory,
	capacity *CodexCapacityLedger,
	requirements CodexRouteRequirements,
	provisional map[codex.AccountKey]int,
) ([]CodexRoutePolicyCandidate, error) {
	for _, count := range provisional {
		if count < 0 {
			return nil, &CodexRouteProjectionError{Code: CodexRouteProjectionInvalidProvisionalCount}
		}
	}

	seen := make(map[codex.AccountKey]bool, len(inventory.Accounts))
	candidates := make([]CodexRoutePolicyCandidate, 0, len(inventory.Accounts))
	for _, account := range inventory.Accounts {
		if account.Key == "" {
			continue
		}
		if seen[account.Key] {
			return nil, &CodexRouteProjectionError{Code: CodexRouteProjectionInvalidInventory}
		}
		seen[account.Key] = true

		effectiveModel, compatible := projectCodexRouteModel(account.Identity.PlanType, requirements.RequestedModel)
		buckets := routeBuckets(effectiveModel, requirements.RequiredModels, account.Identity.PlanType)
		views := make([]CapacityView, len(buckets))
		for i, bucket := range buckets {
			views[i] = capacity.Capacity(account.Key, bucket)
		}
		candidates = append(candidates, CodexRoutePolicyCandidate{
			Choice: RouteChoice{
				AccountKey:      account.Key,
				RequestedModel:  requirements.RequestedModel,
				EffectiveModel:  effectiveModel,
				RequiredBuckets: append([]CapacityBucket(nil), buckets...),
			},
			RequiredCapacity:  views,
			Compatible:        compatible,
			Routable:          codexRouteProjectionAccountRoutable(account),
			ProvisionalLeases: provisional[account.Key],
		})
	}
	return candidates, nil
}

func projectCodexRouteModel(plan, requested string) (string, bool) {
	if strings.TrimSpace(ParseModel(requested)) == "" {
		return requested, false
	}
	if codexPlanSupportsModel(plan, requested) {
		return requested, true
	}
	rewritten, ok := rewriteCodexModelName(requested)
	if !ok || !codexPlanSupportsModel(plan, rewritten) {
		return requested, false
	}
	return rewritten, true
}

func codexRouteProjectionAccountRoutable(account codex.LogicalAccount) bool {
	if !account.Routable || account.Unstable || account.Identity.AccountID == "" || account.Identity.UserID == "" {
		return false
	}
	for _, candidate := range account.Candidates {
		if codexRouteProjectionCandidateReady(account.Key, candidate) {
			return true
		}
	}
	return false
}

func codexRouteProjectionCandidateReady(accountKey codex.AccountKey, candidate codex.CredentialCandidate) bool {
	return candidate.Routable && !candidate.DispatchBlocked &&
		candidate.Ref.AccountKey == accountKey && candidate.Ref.CandidateID != "" &&
		candidate.Revision != "" && candidate.Source >= codex.SourceSystem && candidate.Source <= codex.SourceExternal
}
