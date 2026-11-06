package grok

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// SessionAuthority is CQ's single credential boundary for Grok subscription calls.
// Disk ownership is serialized with the same secure filesystem lock used by
// other CQ credential owners, including across separate CLI/service processes.
type SessionAuthority struct {
	Store      Store
	Client     httputil.Doer
	Exchange   RefreshExchange
	Now        func() time.Time
	retainedMu sync.Mutex
	retained   map[AccountID]retainedOperation
}

func NewAuthority(store Store, client httputil.Doer, exchange RefreshExchange) *SessionAuthority {
	if httpClient, ok := client.(*http.Client); ok {
		owned := *httpClient
		owned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &owned
	}
	return &SessionAuthority{Store: store, Client: client, Exchange: exchange, Now: time.Now}
}
func (a *SessionAuthority) Accounts(ctx context.Context) ([]Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := a.Store.read()
	if err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(records.Accounts))
	for i, record := range records.Accounts {
		account := record.Account
		account.Active = i == 0
		accounts = append(accounts, account)
	}
	return accounts, nil
}
func (a *SessionAuthority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a *SessionAuthority) Do(ctx context.Context, id AccountID, request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.URL.Scheme != "https" || request.URL.Host != "cli-chat-proxy.grok.com" || request.URL.User != nil || request.URL.Fragment != "" || request.URL.Opaque != "" || !strings.HasPrefix(request.URL.Path, "/v1/") {
		return nil, errors.New("invalid Grok subscription endpoint")
	}
	return a.doAuthenticated(ctx, id, request, false)
}

// DoResetRead grants one read-only billing operation, not general Grok web access.
func (a *SessionAuthority) DoResetRead(ctx context.Context, id AccountID, request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.String() != "https://grok.com/prod_mc_billing.ConsumerUiSvc/GetRemainingResets" {
		return nil, errors.New("invalid Grok reset-read endpoint")
	}
	read := request.Clone(ctx)
	read.Body = io.NopCloser(bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	read.ContentLength = 5
	read.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte{0, 0, 0, 0, 0})), nil }
	return a.doAuthenticated(ctx, id, read, true)
}

func (a *SessionAuthority) doAuthenticated(ctx context.Context, id AccountID, request *http.Request, resetRead bool) (*http.Response, error) {
	if a.Client == nil {
		return nil, errors.New("Grok HTTP client unavailable")
	}
	lock, err := a.Store.lock(ctx)
	if err != nil {
		return nil, err
	}
	records, err := lock.store.read()
	if err != nil {
		lock.Close()
		return nil, err
	}
	index, err := findAccount(records, string(id))
	if err != nil {
		lock.Close()
		return nil, err
	}
	record := &records.Accounts[index]
	if record.Operation != nil || !record.Tokens.Expiry.After(a.now().Add(time.Minute)) {
		if err := a.refreshLocked(ctx, lock.store, &records, index); err != nil {
			lock.Close()
			return nil, err
		}
	}
	token, user := record.Tokens.Access, record.Account.UserID
	if err := lock.Close(); err != nil {
		return nil, err
	}
	upstream := request.Clone(ctx)
	upstream.Header = make(http.Header)
	for _, name := range []string{"Content-Type", "Accept", "X-Grok-Conv-Id"} {
		if v := request.Header.Get(name); v != "" {
			upstream.Header.Set(name, v)
		}
	}
	if resetRead {
		upstream.Header.Set("Content-Type", "application/grpc-web+proto")
		upstream.Header.Set("X-Grpc-Web", "1")
	}
	upstream.Header.Set("Authorization", "Bearer "+token)
	upstream.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	upstream.Header.Set("X-Userid", user)
	upstream.Header.Set("X-Grok-User-Id", user)
	upstream.Header.Set("X-Grok-Client-Mode", "headless")
	upstream.Header.Set("X-Grok-Client-Version", "1.0.77")
	upstream.Header.Set("X-Authenticateresponse", "authenticate-response")
	return a.Client.Do(upstream)
}
