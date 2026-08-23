package gemini

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

const (
	antigravityOAuthClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	tokenExpirySkew          = 30 * time.Second
)

var errLocalInputPanic = errors.New("Antigravity local input panic")

// Provider implements provider.Provider through Antigravity's HTTP API.
type Provider struct {
	client       httputil.Doer
	fs           fsutil.FileSystem
	credentials  credentialReader
	clientSecret string
}

type localInputs struct {
	Credentials credentials
	ProjectID   string
}

// New creates an HTTP-backed Gemini provider.
func New(client httputil.Doer, clientSecret string) *Provider {
	return newProvider(client, fsutil.OSFileSystem{}, systemCredentialReader{}, clientSecret)
}

func newProvider(client httputil.Doer, fsys fsutil.FileSystem, reader credentialReader, clientSecret string) *Provider {
	return &Provider{
		client:       client,
		fs:           fsys,
		credentials:  reader,
		clientSecret: clientSecret,
	}
}

// DiscoverAccounts reports the externally managed Antigravity identity when
// its Keychain entry is present. It does not parse credentials or use network.
func (p *Provider) DiscoverAccounts(_ context.Context) ([]provider.Account, error) {
	_, err := p.credentials.Get(antigravityKeychainService, antigravityKeychainAccount)
	if err != nil {
		if isCredentialNotFound(err) {
			return nil, nil
		}
		return nil, errCredentialRead
	}
	return []provider.Account{{
		AccountID: antigravityAccountID,
		Label:     "Antigravity CLI",
		Active:    true,
	}}, nil
}

func (p *Provider) readLocalInputs() (localInputs, error) {
	var (
		inputs        localInputs
		credentialErr error
		projectErr    error
		wg            sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if recover() != nil {
				credentialErr = errLocalInputPanic
			}
		}()
		inputs.Credentials, credentialErr = readCredentials(p.credentials)
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if recover() != nil {
				projectErr = errLocalInputPanic
			}
		}()
		inputs.ProjectID, projectErr = readProjectID(p.fs)
	}()
	wg.Wait()
	return inputs, errors.Join(credentialErr, projectErr)
}

// Fetch reads Antigravity-owned local state and retrieves Gemini quota directly.
func (p *Provider) Fetch(ctx context.Context, now time.Time) (results []quota.Result, err error) {
	defer func() {
		if recover() != nil {
			results = singleError("fetch_panic", "Gemini provider failed", 0)
			err = nil
		}
	}()
	inputs, err := p.readLocalInputs()
	if err != nil {
		switch {
		case errors.Is(err, errLocalInputPanic):
			return singleError("fetch_panic", "Gemini local input failed", 0), nil
		case errors.Is(err, errInvalidCredentials), errors.Is(err, errInvalidProjectID):
			return singleError("parse_error", "invalid Antigravity configuration", 0), nil
		case errors.Is(err, errCredentialsNotFound):
			return singleError("not_configured", "Antigravity credentials not found", 0), nil
		default:
			return singleError("fetch_error", "read Antigravity configuration", 0), nil
		}
	}

	credential := inputs.Credentials
	if credential.AccessToken == "" {
		return singleError("no_token", "no token", 0), nil
	}
	accessToken := credential.AccessToken
	if !credential.Expiry.After(now.Add(tokenExpirySkew)) {
		if credential.RefreshToken == "" || p.clientSecret == "" {
			return singleError("auth_expired", "auth expired", 0), nil
		}
		refreshed, status, refreshErr := refreshAccessToken(
			ctx,
			p.client,
			antigravityOAuthClientID,
			p.clientSecret,
			credential.RefreshToken,
		)
		if refreshErr != nil {
			if errors.Is(refreshErr, errOAuthRejected) || errors.Is(refreshErr, errInvalidRefreshResponse) {
				return singleError("auth_expired", "auth expired", status), nil
			}
			return singleError("fetch_error", "Gemini token refresh failed", 0), nil
		}
		accessToken = refreshed.AccessToken
		credential.AccessToken = refreshed.AccessToken
		credential.Expiry = now.Add(time.Duration(refreshed.ExpiresIn) * time.Second)
		if refreshed.RefreshToken != "" {
			credential.RefreshToken = refreshed.RefreshToken
		}
	}

	projectID := inputs.ProjectID
	if projectID == "" {
		var status int
		projectID, status, err = loadCodeAssist(ctx, p.client, accessToken)
		if err != nil {
			if errors.Is(err, errInvalidProjectResponse) {
				return singleError("parse_error", "invalid Antigravity project response", 0), nil
			}
			return singleError("fetch_error", "Gemini project request failed", 0), nil
		}
		switch {
		case status == http.StatusUnauthorized:
			return singleError("auth_expired", "auth expired", status), nil
		case status != http.StatusOK:
			return singleError("api_error", "Gemini API error", status), nil
		}
	}

	body, status, err := retrieveUserQuotaSummary(ctx, p.client, accessToken, projectID)
	if err != nil {
		return singleError("fetch_error", "Gemini quota request failed", 0), nil
	}
	switch {
	case status == http.StatusUnauthorized:
		return singleError("auth_expired", "auth expired", status), nil
	case status != http.StatusOK:
		return singleError("api_error", "Gemini API error", status), nil
	}

	result, err := parseQuotaSummary(body)
	if err != nil {
		return singleError("parse_error", "invalid Gemini quota response", 0), nil
	}
	return []quota.Result{result}, nil
}

func singleError(code, message string, status int) []quota.Result {
	result := quota.ErrorResult(code, message, status)
	result.AccountID = antigravityAccountID
	result.Active = true
	return []quota.Result{result}
}
