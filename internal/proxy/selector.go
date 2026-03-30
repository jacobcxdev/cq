package proxy

import (
	"context"
	"fmt"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
)

// ClaudeDiscoverer abstracts keyring discovery for testability.
type ClaudeDiscoverer func() []keyring.ClaudeOAuth

// ClaudeSelector picks the best account for a request.
type ClaudeSelector interface {
	Select(ctx context.Context, exclude ...string) (*keyring.ClaudeOAuth, error)
}

type accountSelector struct {
	discover ClaudeDiscoverer
}

// NewAccountSelector creates a ClaudeSelector backed by the given discovery function.
func NewAccountSelector(discover ClaudeDiscoverer) ClaudeSelector {
	return &accountSelector{discover: discover}
}

func (s *accountSelector) Select(_ context.Context, exclude ...string) (*keyring.ClaudeOAuth, error) {
	accounts := s.discover()
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no claude accounts available")
	}

	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}

	now := time.Now().UnixMilli()
	var best *keyring.ClaudeOAuth
	var bestExpired *keyring.ClaudeOAuth

	for i := range accounts {
		a := &accounts[i]
		if isExcluded(a, excludeSet) || a.AccessToken == "" {
			continue
		}

		if a.ExpiresAt == 0 || a.ExpiresAt > now {
			// Non-expired (or unknown expiry): pick latest expiry.
			if best == nil || a.ExpiresAt > best.ExpiresAt {
				best = a
			}
		} else if a.RefreshToken != "" {
			// Expired but refreshable: pick newest.
			if bestExpired == nil || a.ExpiresAt > bestExpired.ExpiresAt {
				bestExpired = a
			}
		}
	}

	if best != nil {
		result := *best
		return &result, nil
	}
	if bestExpired != nil {
		result := *bestExpired
		return &result, nil
	}

	return nil, fmt.Errorf("no claude accounts available")
}

func isExcluded(a *keyring.ClaudeOAuth, excludeSet map[string]bool) bool {
	return (a.Email != "" && excludeSet[a.Email]) ||
		(a.AccountUUID != "" && excludeSet[a.AccountUUID])
}
