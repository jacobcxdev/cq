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
	quota       QuotaReader
}

// NewAccountSelector creates a ClaudeSelector backed by the given discovery function.
// If activeEmail is non-nil, Select() prefers the active account over others.
// If quota is non-nil, Select() prefers accounts with more remaining quota.
func NewAccountSelector(discover ClaudeDiscoverer, activeEmail ActiveEmailFunc, quota QuotaReader) ClaudeSelector {
	return &accountSelector{discover: discover, activeEmail: activeEmail, quota: quota}
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
	bestRemaining := -1
	var bestExpired *keyring.ClaudeOAuth
	bestExpiredRemaining := -1

	for i := range accounts {
		a := &accounts[i]
		if isExcluded(a, excludeSet) || a.AccessToken == "" {
			continue
		}

		remaining := s.accountRemaining(a)

		if a.ExpiresAt == 0 || a.ExpiresAt > now {
			if s.betterCandidate(a, remaining, best, bestRemaining, activeEmail) {
				best = a
				bestRemaining = remaining
			}
		} else if a.RefreshToken != "" {
			if s.betterCandidate(a, remaining, bestExpired, bestExpiredRemaining, activeEmail) {
				bestExpired = a
				bestExpiredRemaining = remaining
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

// accountRemaining returns the MinRemainingPct for an account from the quota
// cache, or -1 if no data is available.
func (s *accountSelector) accountRemaining(a *keyring.ClaudeOAuth) int {
	if s.quota == nil {
		return -1
	}
	key := acctIdentifier(a)
	snap, ok := s.quota.Snapshot(key)
	if !ok {
		return -1
	}
	return snap.Result.MinRemainingPct()
}

// betterCandidate returns true if candidate (with candidateRemaining) should
// replace current (with currentRemaining). Selection priority:
//  1. Higher remaining quota (when both have data)
//  2. Known quota state beats unknown
//  3. Active account preference
//  4. Later token expiry
func (s *accountSelector) betterCandidate(candidate *keyring.ClaudeOAuth, candidateRemaining int, current *keyring.ClaudeOAuth, currentRemaining int, activeEmail string) bool {
	if current == nil {
		return true
	}

	// Quota-based comparison (when at least one has data).
	if candidateRemaining >= 0 || currentRemaining >= 0 {
		if candidateRemaining != currentRemaining {
			if candidateRemaining == 0 && currentRemaining < 0 {
				return false
			}
			if currentRemaining == 0 && candidateRemaining < 0 {
				return true
			}
			return candidateRemaining > currentRemaining
		}
		// Equal remaining — fall through to tiebreakers.
	}

	// Active account preference.
	if activeEmail != "" {
		candidateActive := candidate.Email == activeEmail
		currentActive := current.Email == activeEmail
		if candidateActive != currentActive {
			return candidateActive
		}
	}

	// Latest expiry.
	return candidate.ExpiresAt > current.ExpiresAt
}

func isExcluded(a *keyring.ClaudeOAuth, excludeSet map[string]bool) bool {
	return (a.Email != "" && excludeSet[a.Email]) ||
		(a.AccountUUID != "" && excludeSet[a.AccountUUID]) ||
		(a.AccessToken != "" && excludeSet[a.AccessToken])
}
