package proxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexDiscoverer abstracts Codex account discovery for testability.
type CodexDiscoverer func() []codex.CodexAccount

// CodexSelector picks a Codex account for a request.
type CodexSelector interface {
	Select(ctx context.Context, exclude ...string) (*codex.CodexAccount, error)
}

type codexQuotaReader interface {
	Snapshot(identifier string) (QuotaSnapshot, bool)
}

type codexSelector struct {
	discover CodexDiscoverer
	quota    codexQuotaReader
}

type codexModelContextKey struct{}

// NewCodexSelector creates a CodexSelector backed by the given discovery function.
func NewCodexSelector(discover CodexDiscoverer, quota codexQuotaReader) CodexSelector {
	return &codexSelector{discover: discover, quota: quota}
}

func (s *codexSelector) Select(ctx context.Context, exclude ...string) (*codex.CodexAccount, error) {
	accounts := s.discover()
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no codex accounts available")
	}

	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}

	requestedModel := codexRequestedModel(ctx)
	if codexModelRequiresPro(requestedModel) {
		if acct := s.selectAccount(accounts, excludeSet, requestedModel, true); acct != nil {
			return acct, nil
		}
	}
	if acct := s.selectAccount(accounts, excludeSet, requestedModel, false); acct != nil {
		return acct, nil
	}

	return nil, fmt.Errorf("no codex accounts with valid tokens and quota")
}

func (s *codexSelector) selectAccount(accounts []codex.CodexAccount, excludeSet map[string]bool, requestedModel string, requireCompatible bool) *codex.CodexAccount {
	for i := range accounts {
		a := &accounts[i]
		if !s.isEligible(a, excludeSet, requestedModel, requireCompatible) {
			continue
		}
		if a.IsActive {
			result := *a
			return &result
		}
	}

	for i := range accounts {
		a := &accounts[i]
		if !s.isEligible(a, excludeSet, requestedModel, requireCompatible) {
			continue
		}
		result := *a
		return &result
	}
	return nil
}

func (s *codexSelector) isEligible(a *codex.CodexAccount, excludeSet map[string]bool, requestedModel string, requireCompatible bool) bool {
	if codexAcctExcluded(a, excludeSet) || a.AccessToken == "" || !s.hasQuota(a) {
		return false
	}
	if requireCompatible && !codexPlanSupportsModel(a.PlanType, requestedModel) {
		return false
	}
	return true
}

func codexRequestedModel(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexModelContextKey{}).(string)
	return model
}

func codexModelRequiresPro(model string) bool {
	baseModel, _ := ParseModelEffort(model)
	return strings.EqualFold(baseModel, codexSparkModel)
}

func codexPlanSupportsModel(plan, model string) bool {
	if !codexModelRequiresPro(model) {
		return true
	}
	return strings.EqualFold(plan, "pro")
}

// codexAcctExcluded returns true if the account matches any key in the exclude set.
func (s *codexSelector) hasQuota(a *codex.CodexAccount) bool {
	if s.quota == nil {
		return true
	}
	snap, ok := s.quota.Snapshot(a.AccountID)
	if !ok {
		snap, ok = s.quota.Snapshot(a.Email)
	}
	if !ok {
		return true
	}
	// Only treat as exhausted when the snapshot is fresh AND MinRemainingPct()==0.
	// Stale snapshots are treated as unknown — eligible for selection.
	// MinRemainingPct returns -1 when there is no window data; treat that as eligible too.
	if time.Since(snap.FetchedAt) > transientQuotaMaxAge {
		return true // stale — unknown status, assume has quota
	}
	return snap.Result.MinRemainingPct() != 0
}

func codexAcctExcluded(a *codex.CodexAccount, excludeSet map[string]bool) bool {
	return (a.Email != "" && excludeSet[a.Email]) ||
		(a.AccountID != "" && excludeSet[a.AccountID]) ||
		(a.RecordKey != "" && excludeSet[a.RecordKey])
}

// codexAcctExcludeKeys returns keys that can be used to exclude this account from selection.
func codexAcctExcludeKeys(a *codex.CodexAccount) []string {
	var keys []string
	if a.Email != "" {
		keys = append(keys, a.Email)
	}
	if a.AccountID != "" {
		keys = append(keys, a.AccountID)
	}
	if a.RecordKey != "" {
		keys = append(keys, a.RecordKey)
	}
	return keys
}

// codexAcctIdentifier returns a stable identifier for tracking per-account state.
func codexAcctIdentifier(a *codex.CodexAccount) string {
	if a.AccountID != "" {
		return a.AccountID
	}
	if a.Email != "" {
		return a.Email
	}
	return a.AccessToken
}
