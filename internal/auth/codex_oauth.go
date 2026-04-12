package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	// CodexClientID is the OAuth client ID for Codex (Auth0/OpenAI).
	CodexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexIssuer      = "https://auth.openai.com"
	codexPort        = 1455
	codexCallbackPath = "/auth/callback"
)

// codexScopes returns the OAuth scopes for Codex login.
func codexScopes() string {
	return "openid profile email offline_access api.connectors.read api.connectors.invoke"
}

// CodexTokenResponse is returned by the Codex OAuth token exchange.
type CodexTokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// CodexLogin performs the OAuth PKCE flow for Codex/ChatGPT via Auth0.
// It starts a local server on port 1455, opens the browser, and returns
// tokens + decoded claims on success.
func CodexLogin(ctx context.Context, client httputil.Doer) (*CodexTokenResponse, *CodexClaims, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, nil, fmt.Errorf("generate PKCE: %w", err)
	}

	state, err := generateState()
	if err != nil {
		return nil, nil, fmt.Errorf("generate state: %w", err)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", codexPort))
	if err != nil {
		return nil, nil, fmt.Errorf("listen on port %d (is codex already running?): %w", codexPort, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	redirectURI := fmt.Sprintf("http://localhost:%d%s", port, codexCallbackPath)
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	var accepted sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc(codexCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if r.URL.Query().Get("state") != state {
			fmt.Fprintf(w, "<html><body><h2>Login failed</h2><p>Invalid state parameter.</p></body></html>")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			fmt.Fprintf(w, "<html><body><h2>Login failed</h2><p>No authorization code received.</p></body></html>")
			return
		}
		if len(code) > 512 || !validOAuthCode.MatchString(code) {
			fmt.Fprintf(w, "<html><body><h2>Login failed</h2><p>Invalid authorization code.</p></body></html>")
			return
		}

		sent := false
		accepted.Do(func() {
			codeCh <- code
			sent = true
		})
		if sent {
			fmt.Fprintf(w, "<html><body><h2>Login successful!</h2><p>You can close this tab.</p><script>try{window.open('','_self','');window.close()}catch(e){}</script></body></html>")
		} else {
			fmt.Fprintf(w, "<html><body><p>You can close this tab.</p></body></html>")
		}
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case errCh <- err:
			default:
			}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := buildCodexAuthorizeURL(challenge, state, port)

	fmt.Printf("Starting local login server on http://localhost:%d.\n", port)
	fmt.Println("If your browser did not open, navigate to this URL to authenticate:")
	fmt.Println()
	if err := openBrowser(authURL); err != nil {
		// Browser failed to open — URL is already printed below
	}
	fmt.Printf("  %s\n\n", authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, nil, err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-time.After(5 * time.Minute):
		return nil, nil, fmt.Errorf("login timed out after 5 minutes")
	}

	tokens, err := exchangeCodexCode(ctx, client, code, redirectURI, verifier)
	if err != nil {
		return nil, nil, fmt.Errorf("exchange code: %w", err)
	}

	claims := DecodeCodexClaims(tokens.IDToken)
	return tokens, &claims, nil
}

func buildCodexAuthorizeURL(challenge, state string, port int) string {
	u, _ := url.Parse(codexIssuer + "/oauth/authorize")
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", CodexClientID)
	q.Set("redirect_uri", fmt.Sprintf("http://localhost:%d%s", port, codexCallbackPath))
	q.Set("scope", codexScopes())
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", "cq")
	u.RawQuery = q.Encode()
	return u.String()
}

// RefreshCodexToken exchanges a refresh_token for a new set of tokens using
// the Auth0 form-encoded POST (same endpoint/client-id as code exchange).
// Returns the new tokens, or an error if the refresh fails.
func RefreshCodexToken(ctx context.Context, client httputil.Doer, refreshToken string) (*CodexTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {CodexClientID},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", codexIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token refresh failed: HTTP %d", resp.StatusCode)
	}

	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var tokens CodexTokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, err
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in refresh response")
	}
	if tokens.ExpiresIn <= 0 {
		tokens.ExpiresIn = DefaultExpiresInSec
	}
	return &tokens, nil
}

// exchangeCodexCode exchanges an authorization code for tokens using
// form-urlencoded POST (Auth0 convention, unlike Claude's JSON body).
func exchangeCodexCode(ctx context.Context, client httputil.Doer, code, redirectURI, verifier string) (*CodexTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {CodexClientID},
		"code_verifier": {verifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", codexIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange failed: HTTP %d", resp.StatusCode)
	}

	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var tokens CodexTokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, err
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("empty access token")
	}
	return &tokens, nil
}
