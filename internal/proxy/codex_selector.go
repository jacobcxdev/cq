package proxy

import (
	"context"
	"fmt"
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

// NewCodexSelector creates a CodexSelector backed by the given discovery function.
func NewCodexSelector(discover CodexDiscoverer, quota codexQuotaReader) CodexSelector {
	return &codexSelector{discover: discover, quota: quota}
}

func (s *codexSelector) Select(_ context.Context, exclude ...string) (*codex.CodexAccount, error) {
	accounts := s.discover()
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no codex accounts available")
	}

	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}

	// Prefer the active account when it still has quota.
	for i := range accounts {
		a := &accounts[i]
		if codexAcctExcluded(a, excludeSet) || a.AccessToken == "" || !s.hasQuota(a) {
			continue
		}
		if a.IsActive {
			result := *a
			return &result, nil
		}
	}

	// Fall back to first non-excluded account with a token and quota.
	for i := range accounts {
		a := &accounts[i]
		if codexAcctExcluded(a, excludeSet) || a.AccessToken == "" || !s.hasQuota(a) {
			continue
		}
		result := *a
		return &result, nil
	}

	return nil, fmt.Errorf("no codex accounts with valid tokens and quota")
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
