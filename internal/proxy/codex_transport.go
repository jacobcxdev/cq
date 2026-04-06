package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexAccountSwitcher persists a Codex account switch (best-effort).
type CodexAccountSwitcher func(ctx context.Context, email string) error

// CodexTokenTransport is an http.RoundTripper that injects Codex OAuth tokens
// and handles 401 (failover) and 429 (exhaustion-based failover).
//
// Unlike TokenTransport, Codex tokens cannot be refreshed — the only
// recovery from auth failure is failover to an alternate account.
type CodexTokenTransport struct {
	Selector CodexSelector
	Switcher CodexAccountSwitcher
	Inner    http.RoundTripper

	mu                     sync.Mutex
	failures               map[string]*failure429
	suppressFailoverForKey string
}

func (t *CodexTokenTransport) inner() http.RoundTripper {
	if t.Inner != nil {
		return t.Inner
	}
	return http.DefaultTransport
}

// RoundTrip implements http.RoundTripper.
func (t *CodexTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	acct, err := t.Selector.Select(req.Context())
	if err != nil {
		return nil, err
	}

	resp, err := t.doRequest(req, acct)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		resp.Body.Close()
		return t.handleUnauthorized(req, acct)
	case http.StatusTooManyRequests:
		return t.handle429(req, resp, acct)
	default:
		t.reset429(acct)
		t.clearFailoverSuppression(acct)
		return resp, nil
	}
}

func (t *CodexTokenTransport) doRequest(req *http.Request, acct *codex.CodexAccount) (*http.Response, error) {
	out := shallowCloneRequest(req)
	out.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	if acct.AccountID != "" {
		out.Header.Set("ChatGPT-Account-ID", acct.AccountID)
	}
	out.Header.Del("x-api-key")
	return t.inner().RoundTrip(out)
}

func (t *CodexTokenTransport) handleUnauthorized(req *http.Request, failedAcct *codex.CodexAccount) (*http.Response, error) {
	// No refresh possible — attempt failover to alternate.
	alt, err := t.Selector.Select(req.Context(), codexAcctExcludeKeys(failedAcct)...)
	if err != nil {
		return nil, fmt.Errorf("codex token rejected and no alternate account available")
	}

	fmt.Fprintf(os.Stderr, "cq: proxy codex account %s got 401, switching to %s\n",
		codexAcctIdentifier(failedAcct), codexAcctIdentifier(alt))

	t.persistSwitch(alt)
	return t.doRequest(req, alt)
}

func (t *CodexTokenTransport) handle429(req *http.Request, resp *http.Response, failedAcct *codex.CodexAccount) (*http.Response, error) {
	// Read body to determine exhaustion type.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	hard := isHardExhaustion(body)
	if !hard {
		exhausted := t.record429(failedAcct)
		if !exhausted {
			// Transient 429 — reconstruct response and forward to client.
			return makeBufferedResponse(resp, body), nil
		}
	}

	if t.isFailoverSuppressed(failedAcct) {
		return makeBufferedResponse(resp, body), nil
	}

	// Account exhausted — attempt failover to alternate.
	alt, err := t.Selector.Select(req.Context(), codexAcctExcludeKeys(failedAcct)...)
	if err != nil {
		return makeBufferedResponse(resp, body), nil // no alternate
	}

	// Reset so a future rotation back gets a fresh window.
	t.reset429(failedAcct)

	reason := "counter"
	if hard {
		reason = "insufficient_quota"
	}
	fmt.Fprintf(os.Stderr, "cq: proxy codex account %s exhausted (%s), switching to %s\n",
		codexAcctIdentifier(failedAcct), reason, codexAcctIdentifier(alt))

	t.persistSwitch(alt)

	failoverResp, err := t.doRequest(req, alt)
	if err != nil {
		return nil, err
	}
	if failoverResp.StatusCode == http.StatusTooManyRequests {
		t.setFailoverSuppression(failedAcct)
	} else {
		t.clearFailoverSuppression(failedAcct)
	}
	return failoverResp, nil
}

// persistSwitch persists the account switch asynchronously (best-effort).
func (t *CodexTokenTransport) persistSwitch(acct *codex.CodexAccount) {
	if t.Switcher != nil && acct.Email != "" {
		go func() {
			if err := t.Switcher(context.Background(), acct.Email); err != nil {
				fmt.Fprintf(os.Stderr, "cq: proxy codex switch persist failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "cq: proxy codex active account is now %s\n", acct.Email)
			}
		}()
	}
}

// record429 records a 429 for the account and returns true if the account is exhausted.
func (t *CodexTokenTransport) record429(acct *codex.CodexAccount) bool {
	key := codexAcctIdentifier(acct)
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
	if now.Sub(f.first) > exhaustionWindow {
		f.count = 1
		f.first = now
		return false
	}
	f.count++
	return f.count >= exhaustionThreshold
}

// reset429 clears the 429 tracker for the account (called on non-429 responses).
func (t *CodexTokenTransport) reset429(acct *codex.CodexAccount) {
	key := codexAcctIdentifier(acct)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
}

func (t *CodexTokenTransport) isFailoverSuppressed(acct *codex.CodexAccount) bool {
	key := codexAcctIdentifier(acct)
	if key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.suppressFailoverForKey == key
}

func (t *CodexTokenTransport) setFailoverSuppression(acct *codex.CodexAccount) {
	key := codexAcctIdentifier(acct)
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.suppressFailoverForKey = key
}

func (t *CodexTokenTransport) clearFailoverSuppression(acct *codex.CodexAccount) {
	key := codexAcctIdentifier(acct)
	t.mu.Lock()
	defer t.mu.Unlock()
	if key == "" || t.suppressFailoverForKey == key {
		t.suppressFailoverForKey = ""
	}
}

// isHardExhaustion checks whether a 429 response body contains an OpenAI
// "insufficient_quota" error, which signals hard account exhaustion
// requiring immediate account switch (no counter needed).
func isHardExhaustion(body []byte) bool {
	var parsed struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return false
	}
	return parsed.Error.Type == "insufficient_quota"
}

// makeBufferedResponse reconstructs an http.Response with the already-read body.
func makeBufferedResponse(orig *http.Response, body []byte) *http.Response {
	return &http.Response{
		StatusCode: orig.StatusCode,
		Header:     orig.Header,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
