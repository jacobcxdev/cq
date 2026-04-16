package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexAccountSwitcher persists a Codex account switch (best-effort).
type CodexAccountSwitcher func(ctx context.Context, email string) error

// CodexTokenTransport is an http.RoundTripper that injects Codex OAuth tokens
// and handles 401 (failover) and 429 (immediate replay across accounts).
//
// Unlike TokenTransport, Codex tokens cannot be refreshed — the only
// recovery from auth failure is failover to an alternate account.
type CodexTokenTransport struct {
	Selector CodexSelector
	Switcher CodexAccountSwitcher
	Quota    *QuotaCache
	Inner    http.RoundTripper

	mu                     sync.Mutex
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
	req = withCodexModelContext(req)
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
		t.clearFailoverSuppression()
		return resp, nil
	}
}

const (
	codexSparkModel    = "gpt-5.3-codex-spark"
	codexFallbackModel = "gpt-5.3-codex"
)

func (t *CodexTokenTransport) doRequest(req *http.Request, acct *codex.CodexAccount) (*http.Response, error) {
	out := shallowCloneRequest(req)
	rewriteCodexModelForAccount(out, acct)
	out.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	if acct.AccountID != "" {
		out.Header.Set("ChatGPT-Account-ID", acct.AccountID)
	}
	out.Header.Del("x-api-key")
	return t.inner().RoundTrip(out)
}

func rewriteCodexModelForAccount(req *http.Request, acct *codex.CodexAccount) {
	if acct != nil && codexPlanSupportsModel(acct.PlanType, codexRequestedModel(req.Context())) {
		return
	}
	if req.GetBody == nil {
		return
	}
	body, err := req.GetBody()
	if err != nil {
		return
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return
	}

	rewritten, ok := rewriteCodexModelBody(data)
	if !ok {
		return
	}

	req.Body = io.NopCloser(bytes.NewReader(rewritten))
	req.ContentLength = int64(len(rewritten))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(rewritten)), nil
	}
}

func rewriteCodexModelBody(body []byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, false
	}

	rawModel, ok := payload["model"]
	if !ok {
		return nil, false
	}

	var model string
	if json.Unmarshal(rawModel, &model) != nil {
		return nil, false
	}

	rewrittenModel, ok := rewriteCodexModelName(model)
	if !ok {
		return nil, false
	}
	rawRewrittenModel, err := json.Marshal(rewrittenModel)
	if err != nil {
		return nil, false
	}

	payload["model"] = rawRewrittenModel
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return rewritten, true
}

func rewriteCodexModelName(model string) (string, bool) {
	normalised := ParseModel(model)
	lower := strings.ToLower(normalised)
	spark := strings.ToLower(codexSparkModel)
	if lower == spark {
		return codexFallbackModel, true
	}
	if strings.HasPrefix(lower, spark+"-") {
		suffix := normalised[len(codexSparkModel):]
		return codexFallbackModel + suffix, true
	}
	return "", false
}

func withCodexModelContext(req *http.Request) *http.Request {
	if req == nil || codexRequestedModel(req.Context()) != "" || req.GetBody == nil {
		return req
	}
	body, err := req.GetBody()
	if err != nil {
		return req
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return req
	}
	model := extractModel(data)
	if model == "" {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), codexModelContextKey{}, model))
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
	resp, err := t.doRequest(req, alt)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 400 {
		t.clearFailoverSuppression()
	}
	return resp, nil
}

// handle429 implements immediate replay-on-first-429 across all candidate accounts.
// On the first 429, the transport immediately tries every alternate account before
// surfacing a 429 to the client.
//
// Exhaustion classification controls whether the switch is persisted:
//   - hard exhaustion (insufficient_quota in body):        persist switch
//   - fresh snapshot MinRemainingPct == 0:                 persist switch
//   - fresh snapshot MinRemainingPct > 0 (has capacity):  replay but do NOT persist switch
//   - stale/missing snapshot (unknown):                    replay, do not persist switch
//
// Failover suppression is set after a full walk where all candidates returned 429,
// preventing ping-pong until a later non-429 success clears it.
func (t *CodexTokenTransport) handle429(req *http.Request, resp *http.Response, failedAcct *codex.CodexAccount) (*http.Response, error) {
	// Read the body once for exhaustion classification; preserve it for forwarding.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	if t.isFailoverSuppressed(failedAcct) {
		return makeBufferedResponse(resp, body), nil
	}

	// Classify exhaustion of the failing account.
	hardExhausted := isHardExhaustion(body) || t.isSnapshotExhausted(failedAcct)

	// Walk alternates until one succeeds or none remain.
	excluded := codexAcctExcludeKeys(failedAcct)
	last429Body := body
	last429Resp := resp
	var fallbackBody []byte
	var fallbackResp *http.Response

	for {
		alt, err := t.Selector.Select(req.Context(), excluded...)
		if err != nil {
			if fallbackResp == nil {
				t.setFailoverSuppression(failedAcct)
				return makeBufferedResponse(last429Resp, last429Body), nil
			}
			return makeBufferedResponse(fallbackResp, fallbackBody), nil
		}

		altResp, err := t.doRequest(req, alt)
		if err != nil {
			return nil, err
		}

		switch altResp.StatusCode {
		case http.StatusTooManyRequests:
			altBody, _ := io.ReadAll(io.LimitReader(altResp.Body, 1<<20))
			altResp.Body.Close()
			last429Body = altBody
			last429Resp = altResp
			excluded = append(excluded, codexAcctExcludeKeys(alt)...)
		default:
			if altResp.StatusCode < 400 {
				if hardExhausted {
					t.persistSwitch(alt)
				}
				t.clearFailoverSuppression()
				return altResp, nil
			}
			altBody, _ := io.ReadAll(io.LimitReader(altResp.Body, 1<<20))
			altResp.Body.Close()
			fallbackBody = altBody
			fallbackResp = altResp
			excluded = append(excluded, codexAcctExcludeKeys(alt)...)
		}
	}
}

// isSnapshotExhausted returns true when a fresh quota snapshot positively
// confirms the account has 0% remaining capacity (MinRemainingPct == 0).
// Returns false for stale/missing snapshots (unknown status).
func (t *CodexTokenTransport) isSnapshotExhausted(acct *codex.CodexAccount) bool {
	if t.Quota == nil {
		return false
	}
	// Try by AccountID first, then email.
	id := acct.AccountID
	if id == "" {
		id = acct.Email
	}
	if id == "" {
		return false
	}
	snap, ok := t.Quota.Snapshot(id)
	if !ok {
		return false
	}
	if time.Since(snap.FetchedAt) > transientQuotaMaxAge {
		return false // stale — unknown status, not confirmed exhausted
	}
	if !snap.Result.IsUsable() {
		return false
	}
	return snap.Result.MinRemainingPct() == 0
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

func (t *CodexTokenTransport) clearFailoverSuppression() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.suppressFailoverForKey = ""
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
