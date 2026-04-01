package proxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	claude "github.com/jacobcxdev/cq/internal/provider/claude"
)

// RefreshFunc exchanges a refresh token for new credentials.
type RefreshFunc func(ctx context.Context, client httputil.Doer, refreshToken string, scopes []string) (*claude.RefreshResult, error)

// PersistFunc persists refreshed account credentials (best-effort).
type PersistFunc func(acct *keyring.ClaudeOAuth)

// DefaultPersister persists refreshed tokens to all credential stores.
func DefaultPersister(acct *keyring.ClaudeOAuth) {
	keyring.PersistRefreshedToken(acct)
}

// AccountSwitcher persists an account switch (credentials file + keychain + cq keyring).
type AccountSwitcher func(ctx context.Context, email string) error

const (
	// exhaustionThreshold is how many consecutive 429s declare an account exhausted.
	exhaustionThreshold = 3
	// exhaustionWindow is the time window within which consecutive 429s must occur.
	exhaustionWindow = 60 * time.Second
)

// failure429 tracks consecutive 429 responses for one account.
type failure429 struct {
	count int
	first time.Time
}

// TokenTransport is an http.RoundTripper that injects OAuth tokens
// and handles 401 (refresh) and 429 (exhaustion-based failover).
type TokenTransport struct {
	Selector    ClaudeSelector
	Refresher   RefreshFunc
	Persister   PersistFunc
	Switcher    AccountSwitcher
	RefreshHTTP httputil.Doer
	Inner       http.RoundTripper

	mu          sync.Mutex
	knownTokens map[string]string          // acctIdentifier → current access token
	failures    map[string]*failure429      // acctIdentifier → 429 tracker
}

func (t *TokenTransport) inner() http.RoundTripper {
	if t.Inner != nil {
		return t.Inner
	}
	return http.DefaultTransport
}

// record429 records a 429 for the account and returns true if the account is exhausted.
func (t *TokenTransport) record429(acct *keyring.ClaudeOAuth) bool {
	key := acctIdentifier(acct)
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.failures == nil {
		t.failures = make(map[string]*failure429)
	}
	f := t.failures[key]
	if f == nil {
		t.failures[key] = &failure429{count: 1, first: now}
		return false
	}
	// Reset if window expired.
	if now.Sub(f.first) > exhaustionWindow {
		f.count = 1
		f.first = now
		return false
	}
	f.count++
	return f.count >= exhaustionThreshold
}

// reset429 clears the 429 tracker for the account (called on non-429 responses).
func (t *TokenTransport) reset429(acct *keyring.ClaudeOAuth) {
	key := acctIdentifier(acct)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
}

// RoundTrip implements http.RoundTripper.
func (t *TokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	acct, err := t.Selector.Select(req.Context())
	if err != nil {
		return nil, err
	}

	// Refresh upfront if token is already expired.
	token := acct.AccessToken
	if acct.ExpiresAt > 0 && acct.ExpiresAt <= time.Now().UnixMilli() {
		refreshed, err := t.refreshAccount(acct, token)
		if err != nil {
			return nil, fmt.Errorf("token expired and refresh failed: %w", err)
		}
		token = refreshed
	}

	resp, err := t.doRequest(req, token)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		resp.Body.Close()
		return t.handleUnauthorized(req, acct, token)
	case http.StatusTooManyRequests:
		return t.handle429(req, resp, acct)
	default:
		t.reset429(acct)
		return resp, nil
	}
}

func (t *TokenTransport) doRequest(req *http.Request, token string) (*http.Response, error) {
	out := shallowCloneRequest(req)
	out.Header.Set("Authorization", "Bearer "+token)
	appendBeta(out)
	out.Header.Del("x-api-key")
	return t.inner().RoundTrip(out)
}

func (t *TokenTransport) handleUnauthorized(req *http.Request, acct *keyring.ClaudeOAuth, failedToken string) (*http.Response, error) {
	if acct.RefreshToken == "" {
		return nil, fmt.Errorf("token expired and no refresh token available")
	}
	newToken, err := t.refreshAccount(acct, failedToken)
	if err != nil {
		return nil, fmt.Errorf("token refresh failed: %w", err)
	}
	return t.doRequest(req, newToken)
}

func (t *TokenTransport) handle429(req *http.Request, resp *http.Response, failedAcct *keyring.ClaudeOAuth) (*http.Response, error) {
	exhausted := t.record429(failedAcct)
	if !exhausted {
		// Transient 429 — forward to client (Claude Code handles backoff).
		return resp, nil
	}

	// Account exhausted — attempt failover to alternate.
	alt, err := t.Selector.Select(req.Context(), acctExcludeKeys(failedAcct)...)
	if err != nil {
		return resp, nil // no alternate — forward upstream 429
	}

	fmt.Fprintf(os.Stderr, "cq: proxy account %s exhausted (%d consecutive 429s), switching to %s\n",
		failedAcct.Email, exhaustionThreshold, alt.Email)

	// Persist the switch (best-effort, async).
	if t.Switcher != nil && alt.Email != "" {
		go func() {
			if err := t.Switcher(context.Background(), alt.Email); err != nil {
				fmt.Fprintf(os.Stderr, "cq: proxy switch persist failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "cq: proxy active account is now %s\n", alt.Email)
			}
		}()
	}

	token := alt.AccessToken
	if alt.ExpiresAt > 0 && alt.ExpiresAt <= time.Now().UnixMilli() {
		refreshed, err := t.refreshAccount(alt, token)
		if err != nil {
			return resp, nil // refresh failed — forward upstream 429
		}
		token = refreshed
	}

	resp.Body.Close()
	return t.doRequest(req, token)
}

// refreshAccount obtains a fresh token, with double-check to avoid redundant refreshes.
func (t *TokenTransport) refreshAccount(acct *keyring.ClaudeOAuth, failedToken string) (string, error) {
	key := acctIdentifier(acct)

	t.mu.Lock()
	defer t.mu.Unlock()

	// Double-check: another goroutine may have already refreshed.
	if tok, ok := t.knownTokens[key]; ok && tok != failedToken {
		return tok, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rr, err := t.Refresher(ctx, t.RefreshHTTP, acct.RefreshToken, acct.Scopes)
	if err != nil {
		return "", err
	}

	if t.knownTokens == nil {
		t.knownTokens = make(map[string]string)
	}
	t.knownTokens[key] = rr.AccessToken

	// Persist best-effort (async, never fails the in-flight request).
	now := time.Now().UnixMilli()
	updated := *acct
	updated.AccessToken = rr.AccessToken
	updated.ExpiresAt = now + rr.ExpiresIn*1000
	if rr.RefreshToken != "" {
		updated.RefreshToken = rr.RefreshToken
	}
	go t.persist(&updated)

	return rr.AccessToken, nil
}

func (t *TokenTransport) persist(acct *keyring.ClaudeOAuth) {
	if t.Persister != nil {
		t.Persister(acct)
	}
}

// shallowCloneRequest creates a shallow copy with cloned headers and a replayed body.
func shallowCloneRequest(req *http.Request) *http.Request {
	out := new(http.Request)
	*out = *req
	out.Header = req.Header.Clone()
	if req.URL != nil {
		u := *req.URL
		out.URL = &u
	}
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			out.Body = body
		}
	}
	return out
}

func appendBeta(req *http.Request) {
	const beta = "oauth-2025-04-20"
	existing := req.Header.Get("anthropic-beta")
	if existing == "" {
		req.Header.Set("anthropic-beta", beta)
	} else if !strings.Contains(existing, beta) {
		req.Header.Set("anthropic-beta", existing+","+beta)
	}
}

func acctIdentifier(a *keyring.ClaudeOAuth) string {
	if a.AccountUUID != "" {
		return a.AccountUUID
	}
	if a.Email != "" {
		return a.Email
	}
	return a.AccessToken
}

func acctExcludeKeys(a *keyring.ClaudeOAuth) []string {
	var keys []string
	if a.Email != "" {
		keys = append(keys, a.Email)
	}
	if a.AccountUUID != "" {
		keys = append(keys, a.AccountUUID)
	}
	return keys
}
