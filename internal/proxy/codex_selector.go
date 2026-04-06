package proxy

import (
	"context"
	"fmt"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexDiscoverer abstracts Codex account discovery for testability.
type CodexDiscoverer func() []codex.CodexAccount

// CodexSelector picks a Codex account for a request.
type CodexSelector interface {
	Select(ctx context.Context, exclude ...string) (*codex.CodexAccount, error)
}

type codexSelector struct {
	discover CodexDiscoverer
}

// NewCodexSelector creates a CodexSelector backed by the given discovery function.
func NewCodexSelector(discover CodexDiscoverer) CodexSelector {
	return &codexSelector{discover: discover}
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

	// Prefer the active account.
	for i := range accounts {
		a := &accounts[i]
		if codexAcctExcluded(a, excludeSet) || a.AccessToken == "" {
			continue
		}
		if a.IsActive {
			result := *a
			return &result, nil
		}
	}

	// Fall back to first non-excluded account with a token.
	for i := range accounts {
		a := &accounts[i]
		if codexAcctExcluded(a, excludeSet) || a.AccessToken == "" {
			continue
		}
		result := *a
		return &result, nil
	}

	return nil, fmt.Errorf("no codex accounts with valid tokens")
}

// codexAcctExcluded returns true if the account matches any key in the exclude set.
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
