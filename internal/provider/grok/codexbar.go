package grok

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/codexbar"
)

type codexBarAuthority struct {
	owned  Authority
	fs     fsutil.FileSystem
	env    func(string) string
	home   string
	client httputil.Doer
}

// WithCodexBar adds borrowed, dynamically reread credentials. Discovery never
// imports a refresh token, changes source preference, or modifies CodexBar.
func WithCodexBar(owned Authority, fs fsutil.FileSystem, env func(string) string, home string, client httputil.Doer) Authority {
	if concrete, ok := client.(*http.Client); ok {
		copy := *concrete
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	return &codexBarAuthority{owned: owned, fs: fs, env: env, home: home, client: client}
}

type borrowedGrok struct {
	account Account
	token   string
}

func (a *codexBarAuthority) Accounts(ctx context.Context) ([]Account, error) {
	accounts, err := a.owned.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	borrowed, err := a.borrowed(ctx)
	if err != nil {
		if len(accounts) > 0 {
			return append(accounts, Account{ID: "grok_codexbar_unavailable", Label: "CodexBar source unavailable", Source: "codexbar", ReportingOnly: true}), nil
		}
		return nil, err
	}
	seen := make(map[AccountID]bool, len(accounts))
	for _, account := range accounts {
		seen[account.ID] = true
	}
	for _, external := range borrowed {
		if !seen[external.account.ID] {
			external.account.Active = len(accounts) == 0
			accounts = append(accounts, external.account)
			seen[external.account.ID] = true
		}
	}
	return accounts, nil
}

func (a *codexBarAuthority) borrowed(ctx context.Context) ([]borrowedGrok, error) {
	configured, err := codexbar.ReadAccounts(a.fs, a.env, a.home, "grok")
	if err != nil {
		return nil, err
	}
	result := make([]borrowedGrok, 0, len(configured))
	for _, source := range configured {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h := sha256.Sum256([]byte("codexbar:grok:" + source.ID))
		account := Account{ID: AccountID("grok_codexbar_" + hex.EncodeToString(h[:16])), Label: source.Label, Source: "codexbar", ReportingOnly: true}
		token := strings.TrimSpace(source.Token)
		if strings.HasPrefix(strings.ToLower(token), "bearer ") {
			token = strings.TrimSpace(token[7:])
		}
		if !validToken(token) || strings.HasPrefix(strings.ToLower(token), "xai-") || strings.Contains(token, "=") {
			result = append(result, borrowedGrok{account: account})
			continue
		}
		verified, err := readSubscriptionProfile(ctx, a.client, token)
		if err == nil {
			verified.Label = source.Label
			verified.Source = "codexbar"
			verified.ID = accountID(verified)
			if validAccount(verified) {
				account = verified
			}
		}
		result = append(result, borrowedGrok{account: account, token: token})
	}
	return result, nil
}

func (a *codexBarAuthority) Do(ctx context.Context, id AccountID, request *http.Request) (*http.Response, error) {
	owned, err := a.owned.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, account := range owned {
		if account.ID == id {
			return a.owned.Do(ctx, id, request)
		}
	}
	if request == nil || request.URL == nil || request.URL.Scheme != "https" || request.URL.Host != "cli-chat-proxy.grok.com" || request.URL.User != nil || request.URL.Fragment != "" || !strings.HasPrefix(request.URL.Path, "/v1/") {
		return nil, errors.New("invalid Grok subscription endpoint")
	}
	borrowed, err := a.borrowed(ctx)
	if err != nil {
		return nil, err
	}
	for _, candidate := range borrowed {
		if candidate.account.ID == id {
			if candidate.account.ReportingOnly || candidate.token == "" {
				return nil, errors.New("CodexBar Grok credential requires a verified subscription bearer")
			}
			upstream := request.Clone(ctx)
			upstream.Header = make(http.Header)
			for _, name := range []string{"Accept", "Content-Type", "X-Grok-Conv-Id"} {
				if v := request.Header.Get(name); v != "" {
					upstream.Header.Set(name, v)
				}
			}
			setSubscriptionHeaders(upstream, candidate.token, candidate.account.UserID)
			return a.client.Do(upstream)
		}
	}
	return nil, ErrAuthExpired
}

// DoResetRead delegates only the exact read-only reward-inventory operation.
// Borrowed cookie sessions never enter a bearer-authenticated endpoint.
func (a *codexBarAuthority) DoResetRead(ctx context.Context, id AccountID, request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.String() != resetInventoryURL {
		return nil, errors.New("invalid Grok reset inventory endpoint")
	}
	owned, err := a.owned.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, account := range owned {
		if account.ID == id {
			scoped, ok := a.owned.(resetReadAuthority)
			if !ok {
				return nil, errors.New("Grok reset inventory authority unavailable")
			}
			return scoped.DoResetRead(ctx, id, request)
		}
	}
	borrowed, err := a.borrowed(ctx)
	if err != nil {
		return nil, err
	}
	for _, candidate := range borrowed {
		if candidate.account.ID == id && !candidate.account.ReportingOnly && candidate.token != "" {
			upstream := request.Clone(ctx)
			upstream.Body = io.NopCloser(bytes.NewReader([]byte{0, 0, 0, 0, 0}))
			upstream.ContentLength = 5
			upstream.Header = make(http.Header)
			for _, name := range []string{"Content-Type", "X-Grpc-Web", "X-User-Agent", "Origin", "Referer", "Accept"} {
				if value := request.Header.Get(name); value != "" {
					upstream.Header.Set(name, value)
				}
			}
			upstream.Header.Set("Authorization", "Bearer "+candidate.token)
			return a.client.Do(upstream)
		}
	}
	return nil, ErrAuthExpired
}
