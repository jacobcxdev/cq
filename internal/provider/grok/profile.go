package grok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// ValidateLogin reads the subscription behind a verified OAuth identity before
// CQ saves a login. It never buys credit or changes billing settings.
func ValidateLogin(ctx context.Context, client httputil.Doer, login Login) (Login, error) {
	if client == nil || !validToken(login.AccessToken) || !validAccount(login.Account) {
		return Login{}, errors.New("invalid Grok subscription login")
	}
	profile, err := readSubscriptionProfile(ctx, client, login.AccessToken)
	if err != nil {
		return Login{}, err
	}
	if profile.UserID != login.Account.UserID {
		return Login{}, errors.New("Grok subscription identity mismatch")
	}
	if profile.PrincipalType != "" && profile.PrincipalType != login.Account.PrincipalType || profile.PrincipalID != "" && profile.PrincipalID != login.Account.PrincipalID || profile.TeamID != "" && profile.TeamID != login.Account.TeamID {
		return Login{}, errors.New("Grok subscription principal mismatch")
	}
	login.Account.Plan = profile.Plan
	if profile.Email != "" {
		login.Account.Email = profile.Email
	}
	if !validAccount(login.Account) {
		return Login{}, errors.New("invalid Grok subscription profile")
	}
	return login, nil
}

// SubscriptionIdentity verifies the current bearer against the subscription
// service. Refresh callers use this even when the token endpoint omits id_token.
func SubscriptionIdentity(ctx context.Context, client httputil.Doer, token string) (Account, error) {
	return readSubscriptionProfile(ctx, client, token)
}

// readSubscriptionProfile is shared by fresh OAuth validation and borrowed
// credential discovery. The native /user contract uses camelCase fields.
func readSubscriptionProfile(ctx context.Context, client httputil.Doer, token string) (Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if client == nil || !validToken(token) {
		return Account{}, errors.New("invalid Grok subscription credential")
	}
	if concrete, ok := client.(*http.Client); ok {
		copy := *concrete
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/user?include=subscription", nil)
	if err != nil {
		return Account{}, errors.New("create Grok subscription request")
	}
	setSubscriptionHeaders(req, token, "")
	response, err := client.Do(req)
	if err != nil {
		return Account{}, errors.New("read Grok subscription")
	}
	if response == nil || response.Body == nil {
		return Account{}, errors.New("invalid Grok subscription response")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return Account{}, ErrAuthExpired
	}
	if response.StatusCode != http.StatusOK {
		return Account{}, errors.New("Grok subscription login rejected")
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return Account{}, errors.New("read Grok subscription response")
	}
	var profile struct {
		UserID        string `json:"userId"`
		Email         string `json:"email"`
		Plan          string `json:"subscriptionTier"`
		PrincipalType string `json:"principalType"`
		PrincipalID   string `json:"principalId"`
		TeamID        string `json:"teamId"`
	}
	if json.Unmarshal(body, &profile) != nil {
		return Account{}, errors.New("invalid Grok subscription profile")
	}
	account := Account{UserID: profile.UserID, Email: profile.Email, Plan: profile.Plan, PrincipalType: profile.PrincipalType, PrincipalID: profile.PrincipalID, TeamID: profile.TeamID}
	if !validAccount(account) {
		return Account{}, errors.New("invalid Grok subscription profile")
	}
	if account.Plan == "" || strings.EqualFold(account.Plan, "free") {
		return Account{}, errors.New("Grok account has no active subscription")
	}
	return account, nil
}

func setSubscriptionHeaders(req *http.Request, token, user string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("X-Grok-Client-Version", "1.0.77")
	req.Header.Set("X-Grok-Client-Mode", "headless")
	req.Header.Set("X-Authenticateresponse", "authenticate-response")
	if user != "" {
		req.Header.Set("X-Userid", user)
		req.Header.Set("X-Grok-User-Id", user)
	}
}
