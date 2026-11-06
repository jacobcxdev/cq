package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const GrokIssuer = "https://auth.x.ai"
const GrokClientID = "b1a00492-073a-47ea-816f-4c329264a828"
const grokScopes = "openid profile email offline_access grok-cli:access api:access"

// GrokTokens are private OAuth material. Never serialize them in command output.
type GrokTokens struct {
	AccessToken   string    `json:"-"`
	RefreshToken  string    `json:"-"`
	IDToken       string    `json:"-"`
	ExpiresAt     time.Time `json:"-"`
	UserID        string
	PrincipalType string
	PrincipalID   string
	TeamID        string
}

type GrokLogin struct {
	Tokens        GrokTokens `json:"-"`
	UserID        string
	Email         string
	PrincipalType string
	PrincipalID   string
	TeamID        string
	Plan          string
}

// GrokOAuth shares CQ's PKCE and browser helpers; provider discovery remains
// pinned to xAI's issuer. HTTP injection cannot change the trusted origins.
type GrokOAuth struct {
	Client  httputil.Doer
	OpenURL func(context.Context, string) error
}

func (o GrokOAuth) client() httputil.Doer {
	if client, ok := o.Client.(*http.Client); ok {
		copy := *client
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &copy
	}
	return o.Client
}

type grokDiscovery struct {
	Issuer        string   `json:"issuer"`
	Authorization string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	JWKS          string   `json:"jwks_uri"`
	Algorithms    []string `json:"id_token_signing_alg_values_supported"`
}

func (o GrokOAuth) discovery(ctx context.Context) (grokDiscovery, error) {
	var d grokDiscovery
	if err := o.readJSON(ctx, http.MethodGet, GrokIssuer+"/.well-known/openid-configuration", nil, &d); err != nil {
		return d, err
	}
	if d.Issuer != GrokIssuer || !grokIssuerURL(d.Authorization) || !grokIssuerURL(d.Token) || !grokIssuerURL(d.JWKS) || !slices.Contains(d.Algorithms, "ES256") {
		return d, errors.New("invalid Grok OAuth discovery")
	}
	return d, nil
}

func grokIssuerURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host == "auth.x.ai" && u.User == nil && u.Fragment == "" && u.RawQuery == "" && u.Path != ""
}

func (o GrokOAuth) Login(ctx context.Context) (GrokLogin, error) {
	if err := ctx.Err(); err != nil {
		return GrokLogin{}, err
	}
	d, err := o.discovery(ctx)
	if err != nil {
		return GrokLogin{}, err
	}
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return GrokLogin{}, err
	}
	state, err := generateState()
	if err != nil {
		return GrokLogin{}, err
	}
	nonce, err := generateState()
	if err != nil {
		return GrokLogin{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return GrokLogin{}, errors.New("listen for Grok login callback")
	}
	redirect := "http://" + listener.Addr().String() + "/callback"
	codes := make(chan string, 1)
	failures := make(chan error, 1)
	server := &http.Server{Handler: grokOAuthCallback(state, codes, failures), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		defer func() {
			if recover() != nil {
				select {
				case failures <- errors.New("Grok callback server failed"):
				default:
				}
			}
		}()
		if e := server.Serve(listener); e != nil && !errors.Is(e, http.ErrServerClosed) {
			select {
			case failures <- errors.New("Grok callback server failed"):
			default:
			}
		}
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = listener.Close()
	}()
	u, _ := url.Parse(d.Authorization)
	q := url.Values{"response_type": {"code"}, "client_id": {GrokClientID}, "redirect_uri": {redirect}, "scope": {grokScopes}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state}, "nonce": {nonce}}
	u.RawQuery = q.Encode()
	open := o.OpenURL
	if open == nil {
		open = OpenBrowserContext
	}
	if err = open(ctx, u.String()); err != nil {
		return GrokLogin{}, errors.New("open Grok login browser")
	}
	var code string
	select {
	case code = <-codes:
	case err = <-failures:
		return GrokLogin{}, err
	case <-ctx.Done():
		return GrokLogin{}, ctx.Err()
	}
	tokens, err := o.exchange(ctx, d.Token, url.Values{"grant_type": {"authorization_code"}, "client_id": {GrokClientID}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
	if err != nil {
		return GrokLogin{}, err
	}
	claims, err := o.loginIdentity(ctx, d, tokens, nonce)
	if err != nil {
		return GrokLogin{}, err
	}
	tokens.UserID = claims.Subject
	tokens.PrincipalType = claims.PrincipalType
	tokens.PrincipalID = claims.PrincipalID
	tokens.TeamID = claims.TeamID
	return GrokLogin{Tokens: tokens, UserID: claims.Subject, Email: claims.Email, PrincipalType: claims.PrincipalType, PrincipalID: claims.PrincipalID, TeamID: claims.TeamID}, nil
}

// loginIdentity follows the native Team consent contract: Team authorization
// strips user-only openid/email scopes and does not issue an ID token. Access
// claims are hints from the pinned token exchange, not an authenticated account
// assertion. The caller must verify the bearer against Grok's /user endpoint
// before persisting the login or committing refreshed credentials.
func (o GrokOAuth) loginIdentity(ctx context.Context, d grokDiscovery, tokens GrokTokens, nonce string) (grokIDClaims, error) {
	principal, err := grokAccessPrincipal(tokens.AccessToken)
	if err != nil {
		return grokIDClaims{}, err
	}
	if principal.PrincipalType == "Team" {
		return principal, nil
	}
	claims, err := o.validateIDToken(ctx, d, tokens.IDToken, nonce)
	if err != nil {
		return grokIDClaims{}, err
	}
	if principal.PrincipalType != "" {
		claims.PrincipalType = principal.PrincipalType
		claims.PrincipalID = principal.PrincipalID
		if claims.TeamID == "" {
			claims.TeamID = principal.TeamID
		}
	}
	return claims, nil
}

func grokAccessPrincipal(token string) (grokIDClaims, error) {
	invalid := errors.New("invalid Grok access-token principal")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return grokIDClaims{}, nil
	} // Opaque personal bearer.
	if len(token) > 64<<10 || parts[0] == "" || parts[2] == "" {
		return grokIDClaims{}, invalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return grokIDClaims{}, invalid
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(payload, &values) != nil || values == nil {
		return grokIDClaims{}, invalid
	}
	text := func(primary, alias string) (string, error) {
		raw, ok := values[primary]
		if !ok && alias != "" {
			raw, ok = values[alias]
		}
		if !ok {
			return "", nil
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return "", invalid
		}
		if alias != "" {
			if other, exists := values[alias]; exists {
				var repeated string
				if json.Unmarshal(other, &repeated) != nil || repeated != value {
					return "", invalid
				}
			}
		}
		return value, nil
	}
	kind, err := text("principal_type", "principalType")
	if err != nil {
		return grokIDClaims{}, err
	}
	id, err := text("principal_id", "principalId")
	if err != nil {
		return grokIDClaims{}, err
	}
	team, err := text("team_id", "")
	if err != nil {
		return grokIDClaims{}, err
	}
	if kind == "" && id == "" {
		return grokIDClaims{}, nil
	}
	if kind == "" || id == "" {
		return grokIDClaims{}, invalid
	}
	principal := grokIDClaims{PrincipalType: kind, PrincipalID: id, TeamID: team}
	if kind == "Team" {
		principal.Subject = id
		principal.TeamID = id
	}
	return principal, nil
}

func grokOAuthCallback(state string, codes chan<- string, failures chan<- error) http.Handler {
	var accepted sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || r.Method != http.MethodGet || r.URL.Path != "/callback" || len(q["state"]) != 1 || q.Get("state") != state {
			http.Error(w, "Invalid OAuth callback", http.StatusBadRequest)
			return
		}
		if q.Get("error") != "" {
			accepted.Do(func() { failures <- errors.New("Grok login was rejected") })
			http.Error(w, "Login rejected", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if len(q["code"]) != 1 || code == "" || len(code) > 16*1024 || !validOAuthCode.MatchString(code) {
			http.Error(w, "Invalid OAuth code", http.StatusBadRequest)
			return
		}
		accepted.Do(func() { codes <- code })
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Grok login completed. You can close this tab.")
	})
}

// Refresh performs exactly one token exchange. The credential owner, not this
// helper, decides whether the lineage may rotate or an outcome may be retried.
func (o GrokOAuth) Refresh(ctx context.Context, refreshToken string) (GrokTokens, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := o.discovery(ctx)
	if err != nil {
		return GrokTokens{}, err
	}
	tokens, err := o.exchange(ctx, d.Token, url.Values{"grant_type": {"refresh_token"}, "client_id": {GrokClientID}, "refresh_token": {refreshToken}})
	if err != nil {
		return GrokTokens{}, err
	}
	principal, err := grokAccessPrincipal(tokens.AccessToken)
	if err != nil {
		return GrokTokens{}, err
	}
	if principal.PrincipalType == "Team" {
		tokens.UserID = principal.Subject
		tokens.PrincipalType = principal.PrincipalType
		tokens.PrincipalID = principal.PrincipalID
		tokens.TeamID = principal.TeamID
	} else if tokens.IDToken != "" {
		claims, err := o.validateIDToken(ctx, d, tokens.IDToken, "")
		if err != nil {
			return GrokTokens{}, err
		}
		tokens.UserID = claims.Subject
		tokens.PrincipalType = claims.PrincipalType
		tokens.PrincipalID = claims.PrincipalID
		tokens.TeamID = claims.TeamID
	}
	return tokens, nil
}

type grokTokenError struct{ definitive bool }

func (e grokTokenError) Error() string           { return "Grok OAuth token exchange rejected" }
func (e grokTokenError) RefreshDefinitive() bool { return e.definitive }

func (o GrokOAuth) exchange(ctx context.Context, endpoint string, form url.Values) (GrokTokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return GrokTokens{}, errors.New("create Grok token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if o.Client == nil {
		return GrokTokens{}, errors.New("Grok HTTP client unavailable")
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return GrokTokens{}, errors.New("Grok token exchange failed")
	}
	defer resp.Body.Close()
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return GrokTokens{}, errors.New("read Grok token response")
	}
	if resp.StatusCode != http.StatusOK {
		var result struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &result)
		return GrokTokens{}, grokTokenError{definitive: resp.StatusCode == 400 && result.Error == "invalid_grant"}
	}
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
		Type    string `json:"token_type"`
		Expires int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &result) != nil || result.Access == "" || strings.ContainsAny(result.Access, "\r\n\x00") || result.Expires <= 0 || result.Expires > 365*86400 || !strings.EqualFold(result.Type, "Bearer") {
		return GrokTokens{}, errors.New("invalid Grok token response")
	}
	return GrokTokens{AccessToken: result.Access, RefreshToken: result.Refresh, IDToken: result.ID, ExpiresAt: time.Now().Add(time.Duration(result.Expires) * time.Second)}, nil
}

func (o GrokOAuth) readJSON(ctx context.Context, method, endpoint string, body io.Reader, out any) error {
	if o.Client == nil {
		return errors.New("Grok HTTP client unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("create Grok OAuth request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return errors.New("Grok OAuth request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("Grok OAuth endpoint rejected request")
	}
	data, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return errors.New("read Grok OAuth response")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("invalid Grok OAuth response")
	}
	return nil
}

type grokIDClaims struct {
	Issuer        string          `json:"iss"`
	Audience      json.RawMessage `json:"aud"`
	Subject       string          `json:"sub"`
	Email         string          `json:"email"`
	Nonce         string          `json:"nonce"`
	Expires       int64           `json:"exp"`
	NotBefore     int64           `json:"nbf"`
	PrincipalType string          `json:"principal_type"`
	PrincipalID   string          `json:"principal_id"`
	TeamID        string          `json:"team_id"`
}

func (o GrokOAuth) validateIDToken(ctx context.Context, d grokDiscovery, token, nonce string) (grokIDClaims, error) {
	invalid := errors.New("invalid Grok identity token")
	var claims grokIDClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, invalid
	}
	decode := base64.RawURLEncoding.Strict().DecodeString
	h, err := decode(parts[0])
	if err != nil {
		return claims, invalid
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if json.Unmarshal(h, &header) != nil || header.Algorithm != "ES256" || header.KeyID == "" {
		return claims, invalid
	}
	var jwks struct {
		Keys []struct {
			Type      string `json:"kty"`
			Curve     string `json:"crv"`
			ID        string `json:"kid"`
			Algorithm string `json:"alg"`
			Use       string `json:"use"`
			X         string `json:"x"`
			Y         string `json:"y"`
		} `json:"keys"`
	}
	if err = o.readJSON(ctx, http.MethodGet, d.JWKS, nil, &jwks); err != nil {
		return claims, err
	}
	signature, err := decode(parts[2])
	if err != nil || len(signature) != 64 {
		return claims, invalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	verified := false
	for _, k := range jwks.Keys {
		if k.ID != header.KeyID || k.Type != "EC" || k.Curve != "P-256" || k.Algorithm != "" && k.Algorithm != "ES256" || k.Use != "" && k.Use != "sig" {
			continue
		}
		x, e1 := decode(k.X)
		y, e2 := decode(k.Y)
		if e1 != nil || e2 != nil || len(x) != 32 || len(y) != 32 {
			continue
		}
		pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			continue
		}
		if ecdsa.Verify(&pub, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
			verified = true
			break
		}
	}
	if !verified {
		return claims, invalid
	}
	payload, err := decode(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return claims, invalid
	}
	var audience string
	var audiences []string
	audOK := json.Unmarshal(claims.Audience, &audience) == nil && audience == GrokClientID
	if !audOK && json.Unmarshal(claims.Audience, &audiences) == nil {
		audOK = slices.Contains(audiences, GrokClientID)
	}
	if claims.Issuer != GrokIssuer || !audOK || claims.Subject == "" || claims.Expires <= time.Now().Unix() || claims.NotBefore > time.Now().Unix()+30 || nonce != "" && claims.Nonce != nonce {
		return claims, invalid
	}
	return claims, nil
}
