package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type grokTestDoer func(*http.Request) (*http.Response, error)

func (f grokTestDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type grokTestTransport func(*http.Request) (*http.Response, error)

func (f grokTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGrokOAuthRefreshNeverRedirectsTokenBody(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: grokTestTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		w := httptest.NewRecorder()
		if r.URL.Host != "auth.x.ai" {
			t.Error("OAuth refresh token crossed trusted origin")
		}
		if r.URL.Path == "/.well-known/openid-configuration" {
			w.WriteString(`{"issuer":"https://auth.x.ai","authorization_endpoint":"https://auth.x.ai/oauth2/authorize","token_endpoint":"https://auth.x.ai/oauth2/token","jwks_uri":"https://auth.x.ai/.well-known/jwks.json","id_token_signing_alg_values_supported":["ES256"]}`)
		} else {
			w.Header().Set("Location", "https://other.example/token")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
		return w.Result(), nil
	})}
	_, err := (GrokOAuth{Client: client}).Refresh(context.Background(), "private-refresh-token")
	if err == nil || requests != 2 {
		t.Fatalf("redirect followed: requests=%d err=%v", requests, err)
	}
}

func TestGrokOAuthLogin(t *testing.T) {
	for _, broken := range []string{"", "nonce", "signature", "issuer", "audience", "expired", "discovery_origin"} {
		t.Run(broken, func(t *testing.T) {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			var nonce, verifier string
			client := grokTestDoer(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				if r.URL.Host != "auth.x.ai" {
					t.Fatalf("unexpected host %s", r.URL.Host)
				}
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					authorize := "https://auth.x.ai/oauth2/authorize"
					if broken == "discovery_origin" {
						authorize = "https://other.example/authorize"
					}
					fmt.Fprintf(w, `{"issuer":"https://auth.x.ai","authorization_endpoint":%q,"token_endpoint":"https://auth.x.ai/oauth2/token","jwks_uri":"https://auth.x.ai/.well-known/jwks.json","id_token_signing_alg_values_supported":["ES256"]}`, authorize)
				case "/.well-known/jwks.json":
					fmt.Fprintf(w, `{"keys":[{"kty":"EC","crv":"P-256","alg":"ES256","use":"sig","kid":"test","x":%q,"y":%q}]}`, b64(key.X.FillBytes(make([]byte, 32))), b64(key.Y.FillBytes(make([]byte, 32))))
				case "/oauth2/token":
					if r.Method != "POST" {
						t.Fatal("token method")
					}
					_ = r.ParseForm()
					verifier = r.Form.Get("code_verifier")
					if verifier == "" || r.Form.Get("client_id") != GrokClientID || r.Form.Get("code") != "good-code" {
						t.Fatal("invalid token exchange")
					}
					claims := map[string]any{"iss": GrokIssuer, "aud": GrokClientID, "sub": "user-1", "email": "test@example.com", "exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce}
					if broken == "nonce" {
						claims["nonce"] = "wrong"
					}
					if broken == "issuer" {
						claims["iss"] = "https://other.example"
					}
					if broken == "audience" {
						claims["aud"] = "wrong"
					}
					if broken == "expired" {
						claims["exp"] = time.Now().Add(-time.Hour).Unix()
					}
					jwt := grokTestJWT(t, key, claims)
					if broken == "signature" {
						jwt = jwt[:strings.LastIndex(jwt, ".")+1] + b64(make([]byte, 64))
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-test", "refresh_token": "refresh-test", "id_token": jwt, "expires_in": 3600, "token_type": "Bearer"})
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				return w.Result(), nil
			})
			oauth := GrokOAuth{Client: client, OpenURL: func(ctx context.Context, raw string) error {
				u, e := url.Parse(raw)
				if e != nil {
					return e
				}
				q := u.Query()
				nonce = q.Get("nonce")
				if nonce == "" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != GrokClientID {
					t.Fatal("invalid authorize URL")
				}
				redirect, e := url.Parse(q.Get("redirect_uri"))
				if e != nil {
					return e
				}
				if redirect.Hostname() != "127.0.0.1" {
					t.Fatal("not loopback")
				}
				// Invalid state must not consume the valid callback.
				for _, state := range []string{"wrong", q.Get("state")} {
					redirect.RawQuery = url.Values{"code": {"good-code"}, "state": {state}}.Encode()
					resp, e := http.Get(redirect.String())
					if e != nil {
						return e
					}
					resp.Body.Close()
				}
				return nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			login, err := oauth.Login(ctx)
			if broken != "" {
				if err == nil {
					t.Fatal("accepted invalid OAuth response")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if login.UserID != "user-1" || login.Email != "test@example.com" || login.Tokens.AccessToken != "access-test" || len(verifier) < 43 {
				t.Fatalf("invalid login metadata: user=%q", login.UserID)
			}
		})
	}
}

func TestGrokOAuthCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (GrokOAuth{Client: grokTestDoer(func(*http.Request) (*http.Response, error) { t.Fatal("request after cancellation"); return nil, nil })}).Login(ctx)
	if err == nil {
		t.Fatal("expected cancellation")
	}
}

func grokTestJWT(t *testing.T, key *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "test"})
	payload, _ := json.Marshal(claims)
	unsigned := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(unsigned))
	r, s, e := ecdsa.Sign(rand.Reader, key, sum[:])
	if e != nil {
		t.Fatal(e)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return unsigned + "." + b64(sig)
}
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestGrokOAuthTeamLoginWithoutIDToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claims  any
		wantErr bool
	}{
		{"team", map[string]any{"principal_type": "Team", "principal_id": "team-1", "team_id": "team-1"}, false},
		{"camelcase", map[string]any{"principalType": "Team", "principalId": "team-1"}, false},
		{"missing_principal", map[string]any{"principal_type": "Team"}, true},
		{"wrong_claim_type", map[string]any{"principal_type": "Team", "principal_id": 7}, true},
		{"personal_missing_id", map[string]any{"principal_type": "User", "principal_id": "user-1"}, true},
		{"empty_team", map[string]any{"principal_type": "Team", "principal_id": ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(tc.claims)
			access := b64([]byte(`{"alg":"ES256"}`)) + "." + b64(payload) + ".signature"
			oauth := GrokOAuth{Client: grokTestDoer(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					fmt.Fprint(w, `{"issuer":"https://auth.x.ai","authorization_endpoint":"https://auth.x.ai/oauth2/authorize","token_endpoint":"https://auth.x.ai/oauth2/token","jwks_uri":"https://auth.x.ai/.well-known/jwks.json","id_token_signing_alg_values_supported":["ES256"]}`)
				case "/oauth2/token":
					json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "refresh-team", "expires_in": 3600, "token_type": "Bearer"})
				default:
					t.Fatalf("unexpected endpoint %s", r.URL.Path)
				}
				return w.Result(), nil
			}), OpenURL: func(ctx context.Context, raw string) error {
				u, _ := url.Parse(raw)
				q := u.Query()
				callback, _ := url.Parse(q.Get("redirect_uri"))
				callback.RawQuery = url.Values{"code": {"team-code"}, "state": {q.Get("state")}}.Encode()
				response, err := http.Get(callback.String())
				if err != nil {
					return err
				}
				return response.Body.Close()
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			login, err := oauth.Login(ctx)
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted malformed/team or personal missing ID token")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if login.UserID != "team-1" || login.PrincipalType != "Team" || login.PrincipalID != "team-1" || login.TeamID != "team-1" || login.Tokens.UserID != "team-1" || login.Tokens.IDToken != "" {
				t.Fatalf("wrong team login identity: %+v", login)
			}
		})
	}
}

func TestGrokOAuthRefreshExtractsTeamPrincipal(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"principal_type": "Team", "principal_id": "team-refresh"})
	access := b64([]byte(`{"alg":"ES256"}`)) + "." + b64(payload) + ".signature"
	oauth := GrokOAuth{Client: grokTestDoer(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if r.URL.Path == "/.well-known/openid-configuration" {
			fmt.Fprint(w, `{"issuer":"https://auth.x.ai","authorization_endpoint":"https://auth.x.ai/oauth2/authorize","token_endpoint":"https://auth.x.ai/oauth2/token","jwks_uri":"https://auth.x.ai/.well-known/jwks.json","id_token_signing_alg_values_supported":["ES256"]}`)
		} else if r.URL.Path == "/oauth2/token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "rotated", "expires_in": 3600, "token_type": "Bearer"})
		} else {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		return w.Result(), nil
	})}
	tokens, err := oauth.Refresh(context.Background(), "refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.UserID != "team-refresh" || tokens.PrincipalType != "Team" || tokens.PrincipalID != "team-refresh" || tokens.TeamID != "team-refresh" {
		t.Fatalf("lost team principal: %+v", tokens)
	}
}
