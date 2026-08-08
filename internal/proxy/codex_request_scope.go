package proxy

import (
	"context"
	"fmt"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CandidateAttempt identifies one credential attempt without carrying secrets.
type CandidateAttempt struct {
	AccountKey codex.AccountKey
	Candidate  codex.CandidateRef
	Revision   codex.Revision
	Ordinal    int
}

// CodexRequestPlan binds one route choice to bounded same-identity candidates.
type CodexRequestPlan struct {
	Choice   RouteChoice
	Attempts []CandidateAttempt

	refreshAttempt *CandidateAttempt
}

// CodexRequestScoper creates explicit route and candidate plans.
type CodexRequestScoper interface {
	Plan(context.Context, CodexRouteRequirements, codex.Revision, ...codex.SelectionExclusion) (CodexRequestPlan, error)
}

// CodexRequestScope combines quota-aware account choice with secret-free inventory.
type CodexRequestScope struct {
	Chooser   CodexRouteChooser
	Inventory codex.CredentialInventory
	Now       func() time.Time
}

// Plan selects one identity and returns its candidates in deterministic retry order.
func (s *CodexRequestScope) Plan(ctx context.Context, requirements CodexRouteRequirements, accepted codex.Revision, exclude ...codex.SelectionExclusion) (CodexRequestPlan, error) {
	if s == nil || s.Chooser == nil || s.Inventory == nil {
		return CodexRequestPlan{}, fmt.Errorf("Codex request scope unavailable")
	}
	choice, err := s.Chooser.Choose(ctx, requirements, exclude...)
	if err != nil {
		return CodexRequestPlan{}, err
	}
	inventory, err := s.Inventory.List(ctx)
	if err != nil {
		return CodexRequestPlan{}, fmt.Errorf("list Codex credential inventory: %w", err)
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	for _, logical := range inventory.Accounts {
		if logical.Key != choice.AccountKey {
			continue
		}
		candidates := codex.ResolveCandidate(logical, accepted, now)
		plan := CodexRequestPlan{Choice: choice, Attempts: make([]CandidateAttempt, 0, len(candidates))}
		for _, candidate := range candidates {
			attempt := CandidateAttempt{
				AccountKey: logical.Key,
				Candidate:  candidate.Ref,
				Revision:   candidate.Revision,
				Ordinal:    len(plan.Attempts) + 1,
			}
			plan.Attempts = append(plan.Attempts, attempt)
			if plan.refreshAttempt == nil && candidate.Source == codex.SourceManaged && candidate.CQAuthored {
				copy := attempt
				plan.refreshAttempt = &copy
			}
		}
		if len(plan.Attempts) == 0 {
			return CodexRequestPlan{}, fmt.Errorf("selected Codex account has no dispatchable credential candidates")
		}
		return plan, nil
	}
	return CodexRequestPlan{}, fmt.Errorf("selected Codex account disappeared from inventory")
}
