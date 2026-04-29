package proxy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
)

// PinnedClaudeSelector wraps an inner ClaudeSelector and, when a pin is set,
// routes all requests through the pinned account (by email or AccountUUID).
// If the pinned account is excluded or unusable, it delegates to the inner
// selector. Thread-safe: pin may be updated while the proxy is running.
type PinnedClaudeSelector struct {
	inner       ClaudeSelector
	discover    ClaudeDiscoverer
	quota       QuotaReader
	onPinExpire func(string)

	mu  sync.RWMutex
	pin string // email or AccountUUID; empty means no pin
}

// NewPinnedClaudeSelector creates a PinnedClaudeSelector.
// initialPin may be empty (no pin active).
func NewPinnedClaudeSelector(inner ClaudeSelector, discover ClaudeDiscoverer, initialPin string, quota QuotaReader) *PinnedClaudeSelector {
	return &PinnedClaudeSelector{
		inner:    inner,
		discover: discover,
		pin:      initialPin,
		quota:    quota,
	}
}

// SetPinExpireFunc configures a callback invoked after the selector clears an exhausted pin.
func (s *PinnedClaudeSelector) SetPinExpireFunc(f func(string)) {
	s.mu.Lock()
	s.onPinExpire = f
	s.mu.Unlock()
}

// SetPin atomically updates the active pin. An empty string clears the pin.
func (s *PinnedClaudeSelector) SetPin(pin string) {
	s.mu.Lock()
	s.pin = pin
	s.mu.Unlock()
}

// Pin returns the current pin value.
func (s *PinnedClaudeSelector) Pin() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pin
}

// Select implements ClaudeSelector.
//
// When a pin is set:
//   - Discovers accounts and finds one matching the pin by Email or AccountUUID.
//   - If the matched account is in the exclude set, delegates to inner.
//   - If matched and usable (non-empty access token, and either unexpired,
//     no ExpiresAt, or expired with a refresh token), returns a copy directly.
//   - If matched but unusable (no access token, or expired without refresh
//     token), returns an error.
//   - If not found, returns an error containing the pin value.
//
// When no pin is set, delegates to inner.Select.
func (s *PinnedClaudeSelector) Select(ctx context.Context, exclude ...string) (*keyring.ClaudeOAuth, error) {
	s.mu.RLock()
	pin := s.pin
	s.mu.RUnlock()

	if pin == "" {
		return s.inner.Select(ctx, exclude...)
	}

	accounts := s.discover()
	var matched *keyring.ClaudeOAuth
	for i := range accounts {
		a := &accounts[i]
		if a.Email == pin || a.AccountUUID == pin {
			matched = a
			break
		}
	}

	if matched == nil {
		return nil, fmt.Errorf("pinned Claude account %q not found", pin)
	}

	// If the pinned account is excluded, fall back to the inner selector.
	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}
	if isExcluded(matched, excludeSet) {
		return s.inner.Select(ctx, exclude...)
	}
	if s.pinExhausted(matched) {
		s.expirePin(pin)
		return s.inner.Select(ctx, exclude...)
	}

	// Usability check: must have an access token, and must be either unexpired,
	// have no expiry, or be expired with a refresh token (transport will refresh).
	if matched.AccessToken == "" {
		return nil, fmt.Errorf("pinned Claude account %q has no access token", pin)
	}
	now := time.Now().UnixMilli()
	if matched.ExpiresAt != 0 && matched.ExpiresAt <= now && matched.RefreshToken == "" {
		return nil, fmt.Errorf("pinned Claude account %q token is expired and has no refresh token", pin)
	}

	result := *matched
	return &result, nil
}

func (s *PinnedClaudeSelector) pinExhausted(acct *keyring.ClaudeOAuth) bool {
	if s.quota == nil {
		return false
	}
	snap, ok := s.quota.Snapshot(acctIdentifier(acct))
	if !ok || time.Since(snap.FetchedAt) > transientQuotaMaxAge {
		return false
	}
	return snap.Result.MinRemainingPct() == 0
}

func (s *PinnedClaudeSelector) expirePin(pin string) {
	var onExpire func(string)
	s.mu.Lock()
	if s.pin == pin {
		s.pin = ""
		onExpire = s.onPinExpire
	}
	s.mu.Unlock()
	if onExpire != nil {
		onExpire(pin)
	}
}
