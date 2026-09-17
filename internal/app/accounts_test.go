package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

type appStaticCodexInventory struct {
	inventory codexprov.Inventory
}

func (s appStaticCodexInventory) List(context.Context) (codexprov.Inventory, error) {
	return s.inventory, nil
}

func appCodexJWT(email, accountID, userID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"chatgpt_plan_type":  "plus",
		},
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func fakeAppCodexLogin(tokens auth.CodexTokenResponse, claims auth.CodexClaims) codexLoginFunc {
	return func(context.Context, httputil.Doer) (*auth.CodexTokenResponse, *auth.CodexClaims, error) {
		return &tokens, &claims, nil
	}
}

func TestRunCodexLoginWithoutActivatePreservesSystemAndActiveProjection(t *testing.T) {
	fs := fsutil.NewMemFS()
	systemPath := "/home/test/.codex/auth.json"
	registryPath := "/home/test/.codex/accounts/registry.json"
	systemBefore := []byte(`{"system":"untouched"}`)
	registryBefore := []byte(`{"schema_version":3,"active_account_key":"existing::active","accounts":[]}`)
	_ = fs.WriteFile(systemPath, systemBefore, 0o600)
	_ = fs.WriteFile(registryPath, registryBefore, 0o600)
	claims := auth.CodexClaims{Email: "new@test.com", AccountID: "acct-new", UserID: "user-new", PlanType: "plus"}
	tokens := auth.CodexTokenResponse{IDToken: appCodexJWT(claims.Email, claims.AccountID, claims.UserID), AccessToken: "new-access", RefreshToken: "new-refresh"}

	var output bytes.Buffer
	err := runCodexLogin(context.Background(), nil, false, fs, "/cq/state", fakeAppCodexLogin(tokens, claims), func() time.Time { return time.Unix(100, 0) }, &output)
	if err != nil {
		t.Fatalf("runCodexLogin: %v", err)
	}
	if got, _ := fs.ReadFile(systemPath); string(got) != string(systemBefore) {
		t.Fatalf("system auth changed: %s", got)
	}
	var registry map[string]any
	data, _ := fs.ReadFile(registryPath)
	_ = json.Unmarshal(data, &registry)
	if registry["active_account_key"] != "existing::active" {
		t.Fatalf("active projection changed: %#v", registry["active_account_key"])
	}
}

func TestRunCodexLoginActivateUsesExactSavedCandidate(t *testing.T) {
	fs := fsutil.NewMemFS()
	otherJWT := appCodexJWT("same@test.com", "acct-other", "user-other")
	other := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"wrong","id_token":"` + otherJWT + `","account_id":"acct-other"}}`)
	_ = fs.WriteFile("/home/test/.codex/accounts/user-other::acct-other.auth.json", other, 0o600)
	claims := auth.CodexClaims{Email: "same@test.com", AccountID: "acct-new", UserID: "user-new", PlanType: "plus"}
	tokens := auth.CodexTokenResponse{IDToken: appCodexJWT(claims.Email, claims.AccountID, claims.UserID), AccessToken: "exact", RefreshToken: "new-refresh"}

	var output bytes.Buffer
	err := runCodexLogin(context.Background(), nil, true, fs, "/cq/state", fakeAppCodexLogin(tokens, claims), func() time.Time { return time.Unix(100, 0) }, &output)
	if err != nil {
		t.Fatalf("runCodexLogin: %v", err)
	}
	data, _ := fs.ReadFile("/home/test/.codex/auth.json")
	var system map[string]any
	_ = json.Unmarshal(data, &system)
	if got := system["tokens"].(map[string]any)["access_token"]; got != "exact" {
		t.Fatalf("active access token = %#v, want exact saved candidate", got)
	}
}

func TestAccountListManagerUsesSharedCodexInventory(t *testing.T) {
	inventory := appStaticCodexInventory{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
		Key:      "account-a",
		Identity: codexprov.AccountIdentity{AccountID: "acct-a", UserID: "user-a", Email: "a@example.com", PlanType: "pro"},
		Active:   true,
		Routable: true,
		Candidates: []codexprov.CredentialCandidate{{
			Ref:      codexprov.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"},
			Revision: "revision-a", Source: codexprov.SourceSystem, Routable: true,
		}},
	}}}}
	mgr := accountListManager(provider.Codex, nil, inventory)
	accounts, err := mgr.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Email != "a@example.com" || !accounts[0].Active {
		t.Fatalf("accounts = %+v", accounts)
	}
}

type canonicalLoginAdmin struct {
	active          bool
	activationCalls int
	observeErr      error
	activationErr   error
	committed       bool
}

func (a *canonicalLoginAdmin) SaveLogin(context.Context, codexprov.LoginCredential) (codexprov.CandidateRef, codexprov.Revision, error) {
	return codexprov.CandidateRef{AccountKey: "exact", CandidateID: "candidate"}, "revision", nil
}
func (a *canonicalLoginAdmin) Activate(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.ActivationResult, error) {
	a.activationCalls++
	if a.committed {
		a.active = true
	}
	return codexprov.ActivationResult{SystemCommitted: a.committed}, a.activationErr
}
func (a *canonicalLoginAdmin) Adopt(context.Context, codexprov.SystemSnapshot) (codexprov.CandidateRef, codexprov.Revision, error) {
	panic("unexpected adoption")
}
func (a *canonicalLoginAdmin) RemoveManaged(context.Context, codexprov.AccountKey, codexprov.RevisionSet, bool) (codexprov.RemovalResult, error) {
	panic("unexpected removal")
}
func (a *canonicalLoginAdmin) List(ctx context.Context) (codexprov.Inventory, error) {
	if a.observeErr != nil {
		return codexprov.Inventory{}, a.observeErr
	}
	return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "exact", Identity: codexprov.AccountIdentity{AccountID: "account", UserID: "user"}, Active: a.active}}}, ctx.Err()
}
func TestSaveLoginCodexObservedDefaultAndUnknownOutcome(t *testing.T) {
	for _, mode := range []string{"relogin-active", "observe-failed", "activated-observe-failed", "activation-unknown", "activation-failed"} {
		t.Run(mode, func(t *testing.T) {
			admin := &canonicalLoginAdmin{active: mode == "relogin-active", committed: mode == "activated-observe-failed"}
			activate := strings.HasPrefix(mode, "activat")
			if strings.Contains(mode, "observe-failed") {
				admin.observeErr = errors.New("private observation")
			}
			if mode == "activation-unknown" {
				admin.activationErr = &codexprov.MutationOutcomeUnknown{Err: context.DeadlineExceeded}
			}
			if mode == "activation-failed" {
				admin.activationErr = errors.New("private activation")
			}
			result, err := LoginCodex(context.Background(), nil, activate, func(context.Context, httputil.Doer) (*auth.CodexTokenResponse, *auth.CodexClaims, error) {
				return &auth.CodexTokenResponse{AccessToken: "secret"}, &auth.CodexClaims{AccountID: "account", UserID: "user"}, nil
			}, admin)
			if !result.CredentialsSaved {
				t.Fatal("save receipt lost")
			}
			switch mode {
			case "relogin-active":
				if err != nil || !result.Account.Active || result.Activated || !result.AccountObserved || admin.activationCalls != 0 {
					t.Fatalf("relogin=%+v err=%v", result, err)
				}
			case "observe-failed", "activated-observe-failed":
				if !errors.Is(err, ErrAccountLoginPostcheck) || result.AccountObserved || result.Activated != admin.committed {
					t.Fatalf("postcheck=%+v err=%v", result, err)
				}
			case "activation-unknown":
				if result.ActivationKnown || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("unknown activation became false")
				}
			case "activation-failed":
				if !result.ActivationKnown || result.Activated || !errors.Is(err, ErrAccountActivation) {
					t.Fatal("definite rejection became uncertain")
				}
			}
		})
	}
}
func TestSaveLoginClaudeObservedDefaultAndAmbiguousEmail(t *testing.T) {
	for _, mode := range []string{"relogin-active", "duplicate-email", "observe-failed", "activated-observe-failed"} {
		t.Run(mode, func(t *testing.T) {
			writes, stores := 0, 0
			active := mode == "relogin-active"
			inspections := 0
			account := keyring.ClaudeOAuth{AccountUUID: "uuid", Email: "user@example.com", AccessToken: "secret"}
			accounts := &claudeprov.Accounts{Mutations: &claudeprov.AccountMutationOperations{
				Store: func(context.Context, *keyring.ClaudeOAuth) error { stores++; return nil },
				Inspect: func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
					inspections++
					if mode == "observe-failed" || mode == "activated-observe-failed" && writes > 0 {
						return nil, errors.New("private observation")
					}
					rows := []keyring.ClaudeAccountInspection{{Account: account, Active: active, Sources: []string{"cq_managed"}}}
					if mode == "duplicate-email" {
						other := account
						other.AccountUUID = "other"
						rows = append(rows, keyring.ClaudeAccountInspection{Account: other})
					}
					return rows, nil
				},
				Write: func(context.Context, *keyring.ClaudeCredentials) error { writes++; active = true; return nil }, Update: func(context.Context, string, *keyring.ClaudeCredentials) error { return nil },
			}}
			result, err := LoginClaude(context.Background(), nil, mode == "activated-observe-failed", func(context.Context, httputil.Doer) (*auth.TokenResponse, *auth.Profile, error) {
				return &auth.TokenResponse{AccessToken: "secret"}, &auth.Profile{AccountUUID: "uuid", Email: "user@example.com"}, nil
			}, accounts)
			if !result.CredentialsSaved || stores == 0 {
				t.Fatal("save omitted")
			}
			switch mode {
			case "relogin-active":
				if err != nil || !result.Account.Active || result.Activated || writes != 0 {
					t.Fatal("default login changed native projection")
				}
			case "duplicate-email":
				if err != nil || !result.AccountObserved || result.Reference != "" {
					t.Fatal("ambiguous email invented selector")
				}
			default:
				if !errors.Is(err, ErrAccountLoginPostcheck) || result.AccountObserved {
					t.Fatal("unobserved account fabricated")
				}
			}
		})
	}
}
