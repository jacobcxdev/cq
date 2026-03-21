package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// testDoer rewrites request URLs to point at a local httptest.Server,
// preserving the original path and query, so functions that use hardcoded
// production URLs can be tested without modification.
type testDoer struct {
	client  *http.Client
	baseURL string
}

func (d *testDoer) Do(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	base, _ := url.Parse(d.baseURL)
	req.URL.Scheme = base.Scheme
	req.URL.Host = base.Host
	return d.client.Do(req)
}

// --- DecodeEmail ---

func TestDecodeEmail(t *testing.T) {
	makeJWT := func(payload map[string]any) string {
		b, _ := json.Marshal(payload)
		encoded := base64.RawURLEncoding.EncodeToString(b)
		return "header." + encoded + ".signature"
	}

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{
			name:  "valid token with email",
			token: makeJWT(map[string]any{"email": "user@example.com", "sub": "abc"}),
			want:  "user@example.com",
		},
		{
			name:  "valid token without email field",
			token: makeJWT(map[string]any{"sub": "abc"}),
			want:  "",
		},
		{
			name:  "not enough parts",
			token: "onlyone",
			want:  "",
		},
		{
			name:  "only two parts",
			token: "header.payload",
			want:  "",
		},
		{
			name:  "invalid base64 payload",
			token: "header.!!!invalid!!!.signature",
			want:  "",
		},
		{
			name:  "payload is not JSON",
			token: "header." + base64.RawURLEncoding.EncodeToString([]byte("notjson")) + ".sig",
			want:  "",
		},
		{
			name:  "empty string",
			token: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeEmail(tc.token)
			if got != tc.want {
				t.Errorf("DecodeEmail() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- generatePKCE ---

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE() error = %v", err)
	}
	if verifier == "" {
		t.Error("verifier is empty")
	}
	if challenge == "" {
		t.Error("challenge is empty")
	}
	if verifier == challenge {
		t.Error("verifier and challenge must be different")
	}
}

func TestGeneratePKCEUnique(t *testing.T) {
	v1, c1, err1 := generatePKCE()
	v2, c2, err2 := generatePKCE()
	if err1 != nil || err2 != nil {
		t.Fatalf("generatePKCE() errors: %v, %v", err1, err2)
	}
	if v1 == v2 {
		t.Error("two calls produced the same verifier")
	}
	if c1 == c2 {
		t.Error("two calls produced the same challenge")
	}
}

// --- buildAuthorizeURL ---

func TestBuildAuthorizeURL(t *testing.T) {
	challenge := "test-challenge"
	state := "test-state"
	port := 54321

	raw := buildAuthorizeURL(challenge, state, port)

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("buildAuthorizeURL produced invalid URL: %v", err)
	}

	if !strings.HasPrefix(raw, claudeAIAuthURL) {
		t.Errorf("URL does not start with claudeAIAuthURL: %q", raw)
	}

	q := u.Query()

	tests := []struct {
		param string
		want  string
	}{
		{"client_id", ClaudeClientID},
		{"response_type", "code"},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"state", state},
		{"code", "true"},
	}
	for _, tc := range tests {
		t.Run("param_"+tc.param, func(t *testing.T) {
			if got := q.Get(tc.param); got != tc.want {
				t.Errorf("query param %q = %q, want %q", tc.param, got, tc.want)
			}
		})
	}

	// redirect_uri must include the port
	redirectURI := q.Get("redirect_uri")
	if !strings.Contains(redirectURI, "54321") {
		t.Errorf("redirect_uri %q does not contain port 54321", redirectURI)
	}

	// scope must be non-empty and contain at least one expected scope
	scope := q.Get("scope")
	if scope == "" {
		t.Error("scope is empty")
	}
	if !strings.Contains(scope, "user:profile") {
		t.Errorf("scope %q does not contain user:profile", scope)
	}
}

// --- exchangeCode ---

func TestExchangeCode(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("method = %q, want POST", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("unmarshal request body: %v", err)
			}
			if req["grant_type"] != "authorization_code" {
				t.Errorf("grant_type = %q, want authorization_code", req["grant_type"])
			}
			if req["client_id"] != ClaudeClientID {
				t.Errorf("client_id = %q, want %q", req["client_id"], ClaudeClientID)
			}
			if req["code"] != "mycode" {
				t.Errorf("code = %q, want mycode", req["code"])
			}
			if req["code_verifier"] != "myverifier" {
				t.Errorf("code_verifier = %q, want myverifier", req["code_verifier"])
			}
			if req["state"] != "mystate" {
				t.Errorf("state = %q, want mystate", req["state"])
			}

			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"tok123","refresh_token":"ref456","expires_in":3600,"token_type":"Bearer","scope":"user:profile"}`))
		}))
		defer srv.Close()

		client := &testDoer{client: srv.Client(), baseURL: srv.URL}
		tokens, err := exchangeCode(context.Background(), client, "mycode", "http://localhost:12345/callback", "myverifier", "mystate")
		if err != nil {
			t.Fatalf("exchangeCode() error = %v", err)
		}
		if tokens.AccessToken != "tok123" {
			t.Errorf("AccessToken = %q, want tok123", tokens.AccessToken)
		}
		if tokens.RefreshToken != "ref456" {
			t.Errorf("RefreshToken = %q, want ref456", tokens.RefreshToken)
		}
		if tokens.ExpiresIn != 3600 {
			t.Errorf("ExpiresIn = %d, want 3600", tokens.ExpiresIn)
		}
	})

	t.Run("non-200 response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_client"}`))
		}))
		defer srv.Close()

		client := &testDoer{client: srv.Client(), baseURL: srv.URL}
		_, err := exchangeCode(context.Background(), client, "bad", "http://localhost:0/callback", "v", "s")
		if err == nil {
			t.Fatal("expected error for non-200 response")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error = %q, want it to mention 401", err.Error())
		}
	})

	t.Run("empty access token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"","refresh_token":"ref","expires_in":3600}`))
		}))
		defer srv.Close()

		client := &testDoer{client: srv.Client(), baseURL: srv.URL}
		_, err := exchangeCode(context.Background(), client, "c", "http://localhost:0/callback", "v", "s")
		if err == nil {
			t.Fatal("expected error for empty access token")
		}
	})
}

// --- fetchProfile ---

func TestFetchProfile(t *testing.T) {
	t.Run("happy path with claude_ prefix stripped", func(t *testing.T) {
		profileJSON := `{
			"account": {"uuid": "acct-uuid-1", "email": "alice@example.com"},
			"organization": {"uuid": "org-uuid-1", "organization_type": "claude_pro", "rate_limit_tier": "pro_tier"}
		}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer mytoken" {
				t.Errorf("Authorization = %q, want %q", got, "Bearer mytoken")
			}
			if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
				t.Errorf("anthropic-beta = %q, want %q", got, "oauth-2025-04-20")
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(profileJSON))
		}))
		defer srv.Close()

		client := &testDoer{client: srv.Client(), baseURL: srv.URL}
		p, err := fetchProfile(context.Background(), client, "mytoken")
		if err != nil {
			t.Fatalf("fetchProfile() error = %v", err)
		}
		if p.Email != "alice@example.com" {
			t.Errorf("Email = %q, want alice@example.com", p.Email)
		}
		if p.AccountUUID != "acct-uuid-1" {
			t.Errorf("AccountUUID = %q, want acct-uuid-1", p.AccountUUID)
		}
		if p.OrgUUID != "org-uuid-1" {
			t.Errorf("OrgUUID = %q, want org-uuid-1", p.OrgUUID)
		}
		// "claude_pro" → "pro" after prefix strip
		if p.Plan != "pro" {
			t.Errorf("Plan = %q, want pro (claude_ prefix should be stripped)", p.Plan)
		}
		if p.RateLimitTier != "pro_tier" {
			t.Errorf("RateLimitTier = %q, want pro_tier", p.RateLimitTier)
		}
		if len(p.RawJSON) == 0 {
			t.Error("RawJSON is empty")
		}
	})

	t.Run("plan without claude_ prefix is preserved", func(t *testing.T) {
		profileJSON := `{
			"account": {"uuid": "u", "email": "b@example.com"},
			"organization": {"uuid": "o", "organization_type": "enterprise", "rate_limit_tier": "ent"}
		}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(profileJSON))
		}))
		defer srv.Close()

		client := &testDoer{client: srv.Client(), baseURL: srv.URL}
		p, err := fetchProfile(context.Background(), client, "tok")
		if err != nil {
			t.Fatalf("fetchProfile() error = %v", err)
		}
		if p.Plan != "enterprise" {
			t.Errorf("Plan = %q, want enterprise", p.Plan)
		}
	})

	t.Run("non-200 response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		}))
		defer srv.Close()

		client := &testDoer{client: srv.Client(), baseURL: srv.URL}
		_, err := fetchProfile(context.Background(), client, "bad-tok")
		if err == nil {
			t.Fatal("expected error for non-200 response")
		}
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("error = %q, want it to mention 403", err.Error())
		}
	})
}

// --- generateState ---

func TestGenerateState(t *testing.T) {
	t.Run("returns non-empty string", func(t *testing.T) {
		s, err := generateState()
		if err != nil {
			t.Fatalf("generateState() error = %v", err)
		}
		if s == "" {
			t.Error("generateState() returned empty string")
		}
	})

	t.Run("two calls produce different values", func(t *testing.T) {
		s1, err1 := generateState()
		s2, err2 := generateState()
		if err1 != nil || err2 != nil {
			t.Fatalf("generateState() errors: %v, %v", err1, err2)
		}
		if s1 == s2 {
			t.Errorf("two calls produced the same state: %q", s1)
		}
	})
}

// --- callback handler ---

// makeCallbackMux mirrors the validation logic in Login() for isolated testing.
func makeCallbackMux(state string, codeCh chan string, errCh chan error) *http.ServeMux {
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			if r.URL.Query().Get("state") != state {
				errCh <- fmt.Errorf("state mismatch in callback")
				fmt.Fprintf(w, "<html><body><h2>Login failed</h2><p>Invalid state parameter.</p></body></html>")
				return
			}
			code := r.URL.Query().Get("code")
			if code == "" {
				errCh <- fmt.Errorf("no code in callback")
				fmt.Fprintf(w, "<html><body><h2>Login failed</h2><p>No authorization code received.</p></body></html>")
				return
			}
			if len(code) > 512 || !validOAuthCode.MatchString(code) {
				errCh <- fmt.Errorf("invalid authorization code format")
				fmt.Fprintf(w, "<html><body><h2>Login failed</h2><p>Invalid authorization code.</p></body></html>")
				return
			}
			codeCh <- code
			fmt.Fprintf(w, "<html><body><h2>Login successful!</h2><p>You can close this tab.</p></body></html>")
		})
	})
	return mux
}

func TestCallbackStateMismatch(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(makeCallbackMux("correct-state", codeCh, errCh))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/callback?state=wrong-state&code=validcode123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	select {
	case e := <-errCh:
		if !strings.Contains(e.Error(), "state mismatch") {
			t.Errorf("err = %q, want state mismatch", e.Error())
		}
	case c := <-codeCh:
		t.Errorf("expected error, got code %q", c)
	}
}

func TestCallbackNoCode(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(makeCallbackMux("my-state", codeCh, errCh))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/callback?state=my-state")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	select {
	case e := <-errCh:
		if !strings.Contains(e.Error(), "no code") {
			t.Errorf("err = %q, want 'no code'", e.Error())
		}
	case c := <-codeCh:
		t.Errorf("expected error, got code %q", c)
	}
}

func TestCallbackInvalidCodeFormat(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(makeCallbackMux("my-state", codeCh, errCh))
	defer srv.Close()

	// Code contains invalid characters (spaces)
	resp, err := http.Get(srv.URL + "/callback?state=my-state&code=invalid%20code%20with%20spaces")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	select {
	case e := <-errCh:
		if !strings.Contains(e.Error(), "invalid authorization code format") {
			t.Errorf("err = %q, want 'invalid authorization code format'", e.Error())
		}
	case c := <-codeCh:
		t.Errorf("expected error, got code %q", c)
	}
}

func TestCallbackValidCode(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(makeCallbackMux("my-state", codeCh, errCh))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/callback?state=my-state&code=valid_code_ABC-123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Login successful") {
		t.Errorf("body = %q, want 'Login successful'", string(body))
	}

	select {
	case code := <-codeCh:
		if code != "valid_code_ABC-123" {
			t.Errorf("code = %q, want valid_code_ABC-123", code)
		}
	case e := <-errCh:
		t.Errorf("unexpected error: %v", e)
	}
}
