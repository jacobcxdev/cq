package proxy

import (
	"context"
	"fmt"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
)

// ClaudeDiscoverer abstracts keyring discovery for testability.
type ClaudeDiscoverer func() []keyring.ClaudeOAuth

// ActiveEmailFunc returns the email of the currently active Claude account.
type ActiveEmailFunc func() string

// ClaudeSelector picks the best account for a request.
type ClaudeSelector interface {
	Select(ctx context.Context, exclude ...string) (*keyring.ClaudeOAuth, error)
}

type accountSelector struct {
	discover    ClaudeDiscoverer
	activeEmail ActiveEmailFunc
}

// NewAccountSelector creates a ClaudeSelector backed by the given discovery function.
// If activeEmail is non-nil, Select() prefers the active account over others.
func NewAccountSelector(discover ClaudeDiscoverer, activeEmail ActiveEmailFunc) ClaudeSelector {
	return &accountSelector{discover: discover, activeEmail: activeEmail}
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

	var activeEmail string
	if s.activeEmail != nil {
		activeEmail = s.activeEmail()
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
			// Non-expired (or unknown expiry).
			// Prefer the active account; otherwise pick latest expiry.
			if best == nil {
				best = a
			} else if activeEmail != "" && a.Email == activeEmail && best.Email != activeEmail {
				best = a
			} else if best.Email != activeEmail && a.ExpiresAt > best.ExpiresAt {
				best = a
			}
		} else if a.RefreshToken != "" {
			// Expired but refreshable.
			if bestExpired == nil {
				bestExpired = a
			} else if activeEmail != "" && a.Email == activeEmail && bestExpired.Email != activeEmail {
				bestExpired = a
			} else if bestExpired.Email != activeEmail && a.ExpiresAt > bestExpired.ExpiresAt {
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
