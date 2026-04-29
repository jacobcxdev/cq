package proxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
)

type sessionAffinityStore struct {
	mu       sync.RWMutex
	accounts map[string]string
}

func newSessionAffinityStore() *sessionAffinityStore {
	return &sessionAffinityStore{accounts: make(map[string]string)}
}

func (s *sessionAffinityStore) remember(sessionKey string, acct *keyring.ClaudeOAuth) {
	if s == nil || sessionKey == "" || acct == nil {
		return
	}
	identifier := acctIdentifier(acct)
	if identifier == "" {
		return
	}
	s.mu.Lock()
	s.accounts[sessionKey] = identifier
	s.mu.Unlock()
}

func (s *sessionAffinityStore) lookup(sessionKey string) (string, bool) {
	if s == nil || sessionKey == "" {
		return "", false
	}
	s.mu.RLock()
	identifier, ok := s.accounts[sessionKey]
	s.mu.RUnlock()
	return identifier, ok
}

type SessionAffinitySelector struct {
	inner    ClaudeSelector
	discover ClaudeDiscoverer
	quota    QuotaReader
	store    *sessionAffinityStore
}

func NewSessionAffinitySelector(inner ClaudeSelector, discover ClaudeDiscoverer, quota QuotaReader) *SessionAffinitySelector {
	return &SessionAffinitySelector{
		inner:    inner,
		discover: discover,
		quota:    quota,
		store:    newSessionAffinityStore(),
	}
}

func (s *SessionAffinitySelector) Select(ctx context.Context, exclude ...string) (*keyring.ClaudeOAuth, error) {
	if sessionKey, _ := sessionCorrelation(headersFromContext(ctx)); sessionKey != "" {
		if acct := s.affinityAccount(sessionKey, exclude); acct != nil {
			return acct, nil
		}
	}
	return s.inner.Select(ctx, exclude...)
}

func (s *SessionAffinitySelector) affinityAccount(sessionKey string, exclude []string) *keyring.ClaudeOAuth {
	identifier, ok := s.store.lookup(sessionKey)
	if !ok {
		return nil
	}
	excludeSet := make(map[string]bool, len(exclude))
	for _, key := range exclude {
		excludeSet[key] = true
	}
	for _, acct := range s.discover() {
		if acctIdentifier(&acct) != identifier || !affinityAccountUsable(&acct, s.quota, excludeSet) {
			continue
		}
		result := acct
		return &result
	}
	return nil
}

func (s *SessionAffinitySelector) Remember(sessionKey string, acct *keyring.ClaudeOAuth) {
	if s == nil {
		return
	}
	s.store.remember(sessionKey, acct)
}

func affinityAccountUsable(acct *keyring.ClaudeOAuth, quota QuotaReader, excludeSet map[string]bool) bool {
	if acct == nil || isExcluded(acct, excludeSet) || acct.AccessToken == "" {
		return false
	}
	if acct.ExpiresAt != 0 && acct.ExpiresAt <= time.Now().UnixMilli() && acct.RefreshToken == "" {
		return false
	}
	if quota == nil {
		return true
	}
	snap, ok := quota.Snapshot(acctIdentifier(acct))
	if !ok || time.Since(snap.FetchedAt) > transientQuotaMaxAge {
		return true
	}
	return snap.Result.MinRemainingPct() != 0
}

type requestHeadersContextKey struct{}

func contextWithRequestHeaders(ctx context.Context, headers http.Header) context.Context {
	return context.WithValue(ctx, requestHeadersContextKey{}, headers)
}

func headersFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	headers, _ := ctx.Value(requestHeadersContextKey{}).(http.Header)
	return headers
}
