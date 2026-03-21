package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

var validOAuthCode = regexp.MustCompile(`^[A-Za-z0-9\-._~+/]+=*$`)

const (
	// DefaultExpiresInSec is the fallback token expiry when the server omits expires_in.
	DefaultExpiresInSec = 3600

	// ClaudeClientID is the OAuth client ID for Claude.
	ClaudeClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeAIAuthURL = "https://claude.ai/oauth/authorize"
	tokenURL        = "https://platform.claude.com/v1/oauth/token"
	apiBaseURL      = "https://api.anthropic.com"
)

// DefaultScopes returns the full set of OAuth scopes matching Claude Code.
func DefaultScopes() []string {
	return []string{
		"org:create_api_key", "user:profile", "user:inference",
		"user:sessions:claude_code", "user:mcp_servers", "user:file_upload",
	}
}

// TokenResponse is returned by the OAuth token exchange.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// Profile holds the user's Claude profile.
type Profile struct {
	Email         string
	AccountUUID   string
	OrgUUID       string
	Plan          string
	RateLimitTier string
	RawJSON       json.RawMessage
}

// Login performs the OAuth PKCE flow: starts a local server,
// opens the browser, and returns tokens + profile on success.
func Login(ctx context.Context, client httputil.Doer) (*TokenResponse, *Profile, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, nil, fmt.Errorf("generate PKCE: %w", err)
	}

	state, err := generateState()
	if err != nil {
		return nil, nil, fmt.Errorf("generate state: %w", err)
	}

	// Use ephemeral port (OS-assigned)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	var accepted sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// Validate every request independently — bad requests must NOT
		// consume the sync.Once so the real OAuth redirect can still succeed.
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

		// Only the first valid callback is accepted; duplicates get a benign response.
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

	autoURL := buildAuthorizeURL(challenge, state, port)

	fmt.Println("Opening browser to sign in\u2026")
	if err := openBrowser(autoURL); err != nil {
		fmt.Printf("Open this URL to sign in:\n  %s\n", autoURL)
	}

	// Wait for callback
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

	// Exchange code for tokens (JSON body with state)
	tokens, err := exchangeCode(ctx, client, code, redirectURI, verifier, state)
	if err != nil {
		return nil, nil, fmt.Errorf("exchange code: %w", err)
	}

	// Fetch profile
	profile, err := fetchProfile(ctx, client, tokens.AccessToken)
	if err != nil {
		return tokens, nil, fmt.Errorf("fetch profile: %w", err)
	}

	return tokens, profile, nil
}

func generatePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

func generateState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func buildAuthorizeURL(challenge, state string, port int) string {
	u, _ := url.Parse(claudeAIAuthURL) // constant URL, will never fail
	q := u.Query()
	q.Set("code", "true")
	q.Set("client_id", ClaudeClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d/callback", port))
	q.Set("scope", strings.Join(DefaultScopes(), " "))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}

func exchangeCode(ctx context.Context, client httputil.Doer, code, redirectURI, verifier, state string) (*TokenResponse, error) {
	body := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     ClaudeClientID,
		"code_verifier": verifier,
		"state":         state,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange failed: HTTP %d", resp.StatusCode)
	}

	tokenBody, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var tokens TokenResponse
	if err := json.Unmarshal(tokenBody, &tokens); err != nil {
		return nil, err
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("empty access token")
	}
	return &tokens, nil
}

func fetchProfile(ctx context.Context, client httputil.Doer, token string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiBaseURL+"/api/oauth/profile", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("profile API: HTTP %d", resp.StatusCode)
	}

	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, err
	}

	// NOTE: this profile parsing mirrors internal/provider/claude/parser.go:parseProfile.
	// The two serve different callers (auth login vs provider fetch) and carry
	// different fields, so the duplication is intentional.
	var raw struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			UUID             string `json:"uuid"`
			OrganizationType string `json:"organization_type"`
			RateLimitTier    string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	plan := raw.Organization.OrganizationType
	if strings.HasPrefix(plan, "claude_") {
		plan = plan[len("claude_"):]
	}

	return &Profile{
		Email:         raw.Account.Email,
		AccountUUID:   raw.Account.UUID,
		OrgUUID:       raw.Organization.UUID,
		Plan:          plan,
		RateLimitTier: raw.Organization.RateLimitTier,
		RawJSON:       json.RawMessage(body),
	}, nil
}
