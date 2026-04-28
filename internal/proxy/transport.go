package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// TokenTransport is an http.RoundTripper that injects OAuth tokens
// and handles 401 (refresh) and 429 (immediate replay across accounts).
type TokenTransport struct {
	Selector    ClaudeSelector
	Refresher   RefreshFunc
	Persister   PersistFunc
	Switcher    AccountSwitcher
	RefreshHTTP httputil.Doer
	Quota       *QuotaCache
	Inner       http.RoundTripper

	mu                     sync.Mutex
	knownTokens            map[string]string // acctIdentifier → current access token
	suppressFailoverForKey string
}

func (t *TokenTransport) inner() http.RoundTripper {
	if t.Inner != nil {
		return t.Inner
	}
	return http.DefaultTransport
}

// RoundTrip implements http.RoundTripper.
func (t *TokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	acct, err := t.Selector.Select(req.Context())
	if err != nil {
		return nil, err
	}
	noteRouteAccount(req.Context(), claudeAccountHint(acct), false)

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
		t.clearFailoverSuppression()
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
	resp, err := t.doRequest(req, newToken)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 400 {
		t.clearFailoverSuppression()
	}
	return resp, nil
}

// handle429 implements immediate replay-on-first-429 across all candidate accounts.
// On the first 429 for a /v1/messages request, the transport immediately tries every
// alternate account before surfacing a 429 to the client.
//
// Quota classification controls whether the switch is persisted:
//   - confirmed exhausted (MinRemainingPct == 0):  persist switch
//   - fresh remaining capacity (MinRemainingPct > 0): replay but do NOT persist switch
//   - unknown (no data, stale, or nil cache):          replay, do not persist switch
//
// Failover suppression is set after a full walk where all candidates returned 429,
// preventing ping-pong until a later non-429 success clears it.
func (t *TokenTransport) handle429(req *http.Request, resp *http.Response, failedAcct *keyring.ClaudeOAuth) (*http.Response, error) {
	if !tracksExhaustion(req) {
		return resp, nil
	}

	if t.isFailoverSuppressed(failedAcct) {
		return resp, nil
	}

	// Walk alternates until one succeeds or none remain.
	excluded := acctExcludeKeys(failedAcct)
	last429Resp := resp
	var fallbackResp *http.Response

	for {
		alt, err := t.Selector.Select(req.Context(), excluded...)
		if err != nil {
			if fallbackResp == nil {
				t.setFailoverSuppression(failedAcct)
				return last429Resp, nil
			}
			last429Resp.Body.Close()
			return fallbackResp, nil
		}

		token := alt.AccessToken
		if alt.ExpiresAt > 0 && alt.ExpiresAt <= time.Now().UnixMilli() {
			refreshed, err := t.refreshAccount(alt, token)
			if err != nil {
				// Can't use this alternate — skip it.
				excluded = append(excluded, acctExcludeKeys(alt)...)
				continue
			}
			token = refreshed
		}

		noteRouteAccount(req.Context(), claudeAccountHint(alt), true)
		altResp, err := t.doRequest(req, token)
		if err != nil {
			last429Resp.Body.Close()
			if fallbackResp != nil {
				fallbackResp.Body.Close()
			}
			return nil, err
		}

		switch altResp.StatusCode {
		case http.StatusTooManyRequests:
			last429Resp.Body.Close()
			last429Resp = altResp
			excluded = append(excluded, acctExcludeKeys(alt)...)
		case http.StatusUnauthorized:
			altResp.Body.Close()
			altResp, err = t.handleUnauthorized(req, alt, alt.AccessToken)
			if err != nil || altResp == nil {
				excluded = append(excluded, acctExcludeKeys(alt)...)
				continue
			}
			if altResp.StatusCode < 400 {
				if t.isConfirmedExhausted(req.Context(), failedAcct) {
					t.persistSwitch(req.Context(), alt)
				}
				last429Resp.Body.Close()
				if fallbackResp != nil {
					fallbackResp.Body.Close()
				}
				t.clearFailoverSuppression()
				return altResp, nil
			}
			if altResp.StatusCode == http.StatusTooManyRequests {
				last429Resp.Body.Close()
				last429Resp = altResp
				excluded = append(excluded, acctExcludeKeys(alt)...)
				continue
			}
			body, readErr := httputil.ReadBody(altResp.Body)
			altResp.Body.Close()
			if readErr != nil {
				last429Resp.Body.Close()
				if fallbackResp != nil {
					fallbackResp.Body.Close()
				}
				return nil, readErr
			}
			altResp.Body = io.NopCloser(bytes.NewReader(body))
			if fallbackResp != nil {
				fallbackResp.Body.Close()
			}
			fallbackResp = altResp
			excluded = append(excluded, acctExcludeKeys(alt)...)
		default:
			if altResp.StatusCode < 400 {
				if t.isConfirmedExhausted(req.Context(), failedAcct) {
					t.persistSwitch(req.Context(), alt)
				}
				last429Resp.Body.Close()
				if fallbackResp != nil {
					fallbackResp.Body.Close()
				}
				t.clearFailoverSuppression()
				return altResp, nil
			}
			body, readErr := httputil.ReadBody(altResp.Body)
			altResp.Body.Close()
			if readErr != nil {
				last429Resp.Body.Close()
				if fallbackResp != nil {
					fallbackResp.Body.Close()
				}
				return nil, readErr
			}
			altResp.Body = io.NopCloser(bytes.NewReader(body))
			if fallbackResp != nil {
				fallbackResp.Body.Close()
			}
			fallbackResp = altResp
			excluded = append(excluded, acctExcludeKeys(alt)...)
		}
	}
}

// isConfirmedExhausted returns true only when fresh quota data positively
// confirms the account has 0% remaining capacity.
func (t *TokenTransport) isConfirmedExhausted(ctx context.Context, acct *keyring.ClaudeOAuth) bool {
	if t.Quota == nil {
		return false
	}
	snap, ok := t.Quota.Refresh(ctx, acct)
	if !ok {
		return false
	}
	if time.Since(snap.FetchedAt) > transientQuotaMaxAge {
		return false
	}
	if !snap.Result.IsUsable() {
		return false
	}
	return snap.Result.MinRemainingPct() == 0
}

func (t *TokenTransport) isFailoverSuppressed(acct *keyring.ClaudeOAuth) bool {
	key := acctIdentifier(acct)
	if key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.suppressFailoverForKey == key
}

func (t *TokenTransport) setFailoverSuppression(acct *keyring.ClaudeOAuth) {
	key := acctIdentifier(acct)
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.suppressFailoverForKey = key
}

func (t *TokenTransport) clearFailoverSuppression() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.suppressFailoverForKey = ""
}

// persistSwitch persists the account switch asynchronously (best-effort).
func (t *TokenTransport) persistSwitch(ctx context.Context, alt *keyring.ClaudeOAuth) {
	if t.Switcher != nil && alt.Email != "" {
		go func() {
			if err := t.Switcher(context.Background(), alt.Email); err != nil {
				fmt.Fprintf(os.Stderr, "cq: proxy switch persist failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "cq: proxy active account is now %s\n", alt.Email)
			}
		}()
	}
}

// transientQuotaMaxAge is the maximum age of a quota snapshot to trust
// for exhaustion detection.
const transientQuotaMaxAge = 5 * time.Minute

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

func claudeAccountHint(a *keyring.ClaudeOAuth) string {
	if a == nil {
		return ""
	}
	return redactedAccountHint("claude", a.AccountUUID, a.Email, a.AccessToken)
}

func tracksExhaustion(req *http.Request) bool {
	return req != nil && req.URL != nil && req.URL.Path == "/v1/messages"
}

func acctExcludeKeys(a *keyring.ClaudeOAuth) []string {
	var keys []string
	if a.Email != "" {
		keys = append(keys, a.Email)
	}
	if a.AccountUUID != "" {
		keys = append(keys, a.AccountUUID)
	}
	if len(keys) == 0 && a.AccessToken != "" {
		keys = append(keys, a.AccessToken)
	}
	return keys
}
