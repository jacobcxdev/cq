package grok

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
)

type AccountID string

// Account contains only public subscription identity, never credential material.
type Account struct {
	ID            AccountID `json:"id"`
	UserID        string    `json:"user_id"`
	PrincipalType string    `json:"principal_type,omitempty"`
	PrincipalID   string    `json:"principal_id,omitempty"`
	TeamID        string    `json:"team_id,omitempty"`
	Email         string    `json:"email,omitempty"`
	Label         string    `json:"label,omitempty"`
	Plan          string    `json:"plan,omitempty"`
	Active        bool      `json:"active"`
	Source        string    `json:"source,omitempty"`
	ReportingOnly bool      `json:"reporting_only,omitempty"`
}

type Authority interface {
	Accounts(context.Context) ([]Account, error)
	Do(context.Context, AccountID, *http.Request) (*http.Response, error)
}

// Login is accepted only from the verified OAuth and subscription identity path.
type Login struct {
	Account      Account
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	IDToken      string `json:"-"`
	ExpiresAt    time.Time
}

type TokenSet struct {
	AccessToken   string `json:"-"`
	RefreshToken  string `json:"-"`
	IDToken       string `json:"-"`
	ExpiresAt     time.Time
	UserID        string
	PrincipalType string
	PrincipalID   string
	TeamID        string
}

type RefreshExchange func(context.Context, string) (TokenSet, error)

var ErrAuthExpired = errors.New("Grok login expired; run cq grok login")

func accountID(a Account) AccountID {
	h := sha256.Sum256([]byte("grok\x00" + a.UserID + "\x00" + a.PrincipalType + "\x00" + a.PrincipalID + "\x00" + a.TeamID))
	return AccountID("grok_" + hex.EncodeToString(h[:16]))
}

// subscriptionAccounts excludes unverified sources and collapses aliases of
// one user/principal/seat. Keep the first account's public ID and preference.
func subscriptionAccounts(accounts []Account) []Account {
	unique := make([]Account, 0, len(accounts))
	seenIDs := make(map[AccountID]bool, len(accounts))
	seenIdentities := make(map[AccountID]bool, len(accounts))
	for _, account := range accounts {
		if account.ID == "" || account.ReportingOnly || account.Source == "codexbar" && (!validAccount(account) || account.Plan == "" || strings.EqualFold(account.Plan, "free")) {
			continue
		}
		identity := account.ID
		if account.UserID != "" {
			identity = accountID(account)
		}
		if seenIDs[account.ID] || seenIdentities[identity] {
			continue
		}
		seenIDs[account.ID], seenIdentities[identity] = true, true
		unique = append(unique, account)
	}
	return unique
}

func validText(s string, n int) bool {
	return len(s) <= n && !strings.ContainsFunc(s, unicode.IsControl)
}
func validAccount(a Account) bool {
	return a.UserID != "" && validText(a.UserID, 256) && validText(a.PrincipalType, 128) && validText(a.PrincipalID, 256) && validText(a.TeamID, 256) && validText(a.Email, 512) && validText(a.Label, 128) && validText(a.Plan, 256) && (a.PrincipalType != "Team" || a.PrincipalID != "")
}
func validToken(s string) bool {
	return s != "" && len(s) <= 64<<10 && !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r >= '\x7f' })
}
