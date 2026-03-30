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
	Select(ctx context.Context) (*codex.CodexAccount, error)
}

type codexSelector struct {
	discover CodexDiscoverer
}

// NewCodexSelector creates a CodexSelector backed by the given discovery function.
func NewCodexSelector(discover CodexDiscoverer) CodexSelector {
	return &codexSelector{discover: discover}
}

func (s *codexSelector) Select(_ context.Context) (*codex.CodexAccount, error) {
	accounts := s.discover()
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no codex accounts available")
	}

	// Prefer the active account.
	for i := range accounts {
		a := &accounts[i]
		if a.IsActive && a.AccessToken != "" {
			result := *a
			return &result, nil
		}
	}

	// Fall back to first account with a token.
	for i := range accounts {
		a := &accounts[i]
		if a.AccessToken != "" {
			result := *a
			return &result, nil
		}
	}

	return nil, fmt.Errorf("no codex accounts with valid tokens")
}
