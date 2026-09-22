package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type authTestAuthority struct {
	inventory codexprov.Inventory
	listErr   error
	refresh   func(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.RefreshResult, error)
}

func (a *authTestAuthority) List(context.Context) (codexprov.Inventory, error) {
	return a.inventory, a.listErr
}
func (a *authTestAuthority) Refresh(ctx context.Context, r codexprov.CandidateRef, v codexprov.Revision) (codexprov.RefreshResult, error) {
	return a.refresh(ctx, r, v)
}
func authCandidate(id string, expiry time.Time) codexprov.CredentialCandidate {
	return codexprov.CredentialCandidate{Ref: codexprov.CandidateRef{CandidateID: codexprov.CandidateID(id)}, Revision: "r1", Source: codexprov.SourceManaged, CQAuthored: true, RefreshEligible: true, AccessExpiresAt: expiry}
}
func init() {
	registerV2Fixture("auth-partial-write", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		now := time.Unix(1000, 0)
		a := &authTestAuthority{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "opaque-account", Identity: codexprov.AccountIdentity{Email: "test@example.com"}, Candidates: []codexprov.CredentialCandidate{authCandidate("a", now), authCandidate("b", now)}}}}}
		a.refresh = func(_ context.Context, r codexprov.CandidateRef, _ codexprov.Revision) (codexprov.RefreshResult, error) {
			f.Call("refresh")
			if r.CandidateID == "a" {
				return codexprov.RefreshResult{CredentialsChanged: true}, nil
			}
			return codexprov.RefreshResult{}, errors.New("secret-refresh-error")
		}
		deps := v2AuthDependencies{Resolve: func() error { return nil }, Now: func() time.Time { return now }, Codex: func(context.Context) (codexRefreshAuthority, func() error, error) {
			return a, func() error { return nil }, nil
		}, Invalidate: func(provider.ID) error { f.Call("invalidate"); return nil }}
		f.Secrets = []string{"secret-refresh-error"}
		f.Lookup = func(path string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2AuthWithDependencies(ctx, inv, s, deps)
			}, path == "auth refresh"
		}
		return f
	})
}
func TestCLIV2AuthContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "committed credential change stays visible", Scenario: "auth-partial-write", Args: []string{"auth", "refresh", "codex", "--json"}, Exit: 8, Command: "auth refresh", Code: "auth_refresh_partial", WantJSON: `{"credentials_changed":true,"changed_count":1}`, Forbid: []string{"service", "consume", "credential-activate"}, Calls: map[string]int{"refresh": 2, "invalidate": 1}})
}

func authTestDependencies(t *testing.T) v2AuthDependencies {
	t.Helper()
	return v2AuthDependencies{
		Resolve: func() error { return nil }, Now: func() time.Time { return time.Unix(1000, 0) },
		HTTP:           testDoer(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected HTTP access"); return nil, nil }),
		DiscoverClaude: func(context.Context) []keyring.ClaudeOAuth { return nil },
		PersistClaude: func(context.Context, *keyring.ClaudeOAuth) (bool, error) {
			t.Fatal("unexpected credential persistence")
			return false, nil
		},
		Codex: func(context.Context) (codexRefreshAuthority, func() error, error) {
			return &authTestAuthority{}, func() error { return nil }, nil
		},
		Login: func(context.Context) (authReauthResult, error) {
			t.Fatal("unexpected browser login")
			return authReauthResult{}, nil
		},
		Invalidate: func(provider.ID) error { return nil },
	}
}
func authTestRun(t *testing.T, ctx context.Context, args []string, deps v2AuthDependencies, input string, interactive bool) (int, map[string]any, string) {
	t.Helper()
	var out, diag bytes.Buffer
	exit := cli.Run(ctx, args, &cli.Session{In: strings.NewReader(input), Out: &out, Err: &diag, Interactive: interactive}, func(path string) (cli.Handler, bool) {
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2AuthWithDependencies(ctx, inv, s, deps)
		}, path == "auth refresh"
	})
	var envelope map[string]any
	if strings.Contains(out.String(), "secret-sentinel") {
		t.Fatal("credential material leaked")
	}
	if json.Unmarshal(out.Bytes(), &envelope) != nil {
		t.Fatalf("invalid JSON: %s", out.String())
	}
	return exit, envelope, diag.String()
}
func TestCLIV2AuthProviderSelection(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
		exit int
	}{
		{[]string{"auth", "refresh", "--json"}, []string{"claude", "codex"}, 0},
		{[]string{"auth", "refresh", "codex", "claude", "--json"}, []string{"codex", "claude"}, 0},
		{[]string{"auth", "refresh", "claude", "--json"}, []string{"claude"}, 0},
		{[]string{"auth", "refresh", "codex", "--json"}, []string{"codex"}, 0},
		{[]string{"auth", "refresh", "codex", "codex", "--json"}, nil, 2},
		{[]string{"auth", "refresh", "gemini", "--json"}, nil, 2},
		{[]string{"auth", "refresh", "--timeout", "1s", "--json"}, nil, 2},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			deps := authTestDependencies(t)
			var reads []string
			resolved := false
			deps.Resolve = func() error { resolved = true; return nil }
			deps.DiscoverClaude = func(context.Context) []keyring.ClaudeOAuth { reads = append(reads, "claude"); return nil }
			deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
				reads = append(reads, "codex")
				return &authTestAuthority{}, func() error { return nil }, nil
			}
			exit, env, _ := authTestRun(t, context.Background(), tc.args, deps, "", false)
			if exit != tc.exit || !reflect.DeepEqual(reads, tc.want) {
				t.Fatalf("exit=%d reads=%v", exit, reads)
			}
			if tc.exit == 2 && resolved {
				t.Fatal("invalid input resolved state")
			}
			if tc.exit == 0 {
				rows := env["data"].(map[string]any)["providers"].([]any)
				for i, r := range rows {
					if r.(map[string]any)["provider"] != tc.want[i] {
						t.Fatal("provider order lost")
					}
				}
			}
		})
	}
}
func TestCLIV2AuthBoundaryAndReadOnly(t *testing.T) {
	deps := authTestDependencies(t)
	now := deps.Now()
	a := &authTestAuthority{}
	for i, expiry := range []time.Time{now.Add(30 * time.Minute), now.Add(30*time.Minute + time.Nanosecond), {}, now.Add(-time.Minute), now, now, now} {
		c := authCandidate(fmt.Sprint(i), expiry)
		if i == 4 {
			c.Source = codexprov.SourceSystem
		}
		if i == 5 {
			c.Source = codexprov.SourceExternal
		}
		if i == 6 {
			c.RefreshEligible = false
		}
		a.inventory.Accounts = append(a.inventory.Accounts, codexprov.LogicalAccount{Key: codexprov.AccountKey(fmt.Sprint(i)), Identity: codexprov.AccountIdentity{Email: fmt.Sprint(i)}, Candidates: []codexprov.CredentialCandidate{c}})
	}
	var refreshed []string
	a.refresh = func(_ context.Context, r codexprov.CandidateRef, _ codexprov.Revision) (codexprov.RefreshResult, error) {
		refreshed = append(refreshed, string(r.CandidateID))
		return codexprov.RefreshResult{CredentialsChanged: true}, nil
	}
	deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
		return a, func() error { return nil }, nil
	}
	exit, env, _ := authTestRun(t, context.Background(), []string{"auth", "refresh", "codex", "--json"}, deps, "", false)
	if exit != 0 || !reflect.DeepEqual(refreshed, []string{"0", "3"}) {
		t.Fatalf("exit=%d refreshes=%v", exit, refreshed)
	}
	data := env["data"].(map[string]any)
	if data["changed_count"] != float64(2) {
		t.Fatalf("count=%v", data["changed_count"])
	}
	rows := data["providers"].([]any)[0].(map[string]any)
	if rows["refreshed"] != float64(2) || rows["unchanged"] != float64(5) {
		t.Fatal("account counts wrong")
	}
}
func TestCLIV2AuthClaudeReconciliationAndFailure(t *testing.T) {
	deps := authTestDependencies(t)
	now := deps.Now().UnixMilli()
	deps.DiscoverClaude = func(context.Context) []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{Email: "account@test.invalid", AccountUUID: "id", AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: now - 1}, {AccessToken: "secret-sentinel", RefreshToken: "new-refresh", ExpiresAt: now + 60_000}}
	}
	deps.HTTP = testDoer(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return profileJSON("account@test.invalid"), nil
		}
		return nil, errors.New("secret-sentinel")
	})
	var saved []keyring.ClaudeOAuth
	deps.PersistClaude = func(_ context.Context, a *keyring.ClaudeOAuth) (bool, error) {
		saved = append(saved, *a)
		return true, nil
	}
	exit, env, _ := authTestRun(t, context.Background(), []string{"auth", "refresh", "claude", "--json"}, deps, "\n", true)
	data := env["data"].(map[string]any)
	if exit != 8 || data["changed_count"] != float64(1) || len(saved) != 1 || saved[0].AccessToken != "secret-sentinel" {
		t.Fatal("reconciled commit lost after refresh failure")
	}
	rows := data["providers"].([]any)[0].(map[string]any)["accounts"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["account_key"] != nil {
		t.Fatal("Claude identity aggregation/null key wrong")
	}
}
func TestCLIV2AuthClaudeLoginModes(t *testing.T) {
	for _, tc := range []struct {
		name, input           string
		json, terminal, login bool
		exit                  int
	}{
		{"JSON never launches", "\n", true, true, false, 5}, {"nonterminal never launches", "\n", true, false, false, 5}, {"EOF unresolved", "", false, true, false, 5}, {"skip unresolved", "s\n", false, true, false, 5}, {"login success", "\n", false, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := authTestDependencies(t)
			deps.DiscoverClaude = func(context.Context) []keyring.ClaudeOAuth {
				return []keyring.ClaudeOAuth{{Email: "account@test.invalid", AccountUUID: "id", ExpiresAt: 1}}
			}
			logins := 0
			deps.Login = func(context.Context) (authReauthResult, error) {
				logins++
				return authReauthResult{AccountUUID: "id", Email: "account@test.invalid", CredentialsChanged: true}, nil
			}
			var stdout, stderr bytes.Buffer
			inv := cli.Invocation{Path: "auth refresh", Arguments: map[string][]string{"providers": {"claude"}}, JSON: tc.json}
			out := handleV2AuthWithDependencies(context.Background(), inv, &cli.Session{In: strings.NewReader(tc.input), Out: &stdout, Err: &stderr, Interactive: tc.terminal}, deps)
			if out.ExitCode != tc.exit || (logins == 1) != tc.login {
				t.Fatalf("exit=%d logins=%d", out.ExitCode, logins)
			}
			if tc.json && stderr.Len() != 0 {
				t.Fatal("JSON printed prompt")
			}
			if tc.login && out.Human != "claude: 1 refreshed, 0 unchanged, 0 need login, 0 failed\n" {
				t.Fatalf("human=%q", out.Human)
			}
		})
	}
}
func TestCLIV2AuthAuthorityAndStoreFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		exit int
		code string
	}{
		{"unavailable", codexprov.ErrCredentialAuthorityUnavailable, 4, "auth_authority_unavailable"},
		{"inventory", codexprov.ErrCredentialInventoryDegraded, 6, "auth_inventory_degraded"},
		{"broker authentication", errors.New("secret-sentinel"), 5, "auth_refresh_failed"},
		{"broker store", &codexprov.RefreshPersistenceError{Err: errors.New("secret-sentinel")}, 1, "auth_store_failed"},
		{"server interruption", context.Canceled, 130, "auth_interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := authTestDependencies(t)
			a := &authTestAuthority{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "opaque", Candidates: []codexprov.CredentialCandidate{authCandidate("a", deps.Now())}}}}}
			a.refresh = func(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.RefreshResult, error) {
				return codexprov.RefreshResult{}, tc.err
			}
			deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
				return a, func() error { return nil }, nil
			}
			exit, env, _ := authTestRun(t, context.Background(), []string{"auth", "refresh", "codex", "--json"}, deps, "", false)
			if exit != tc.exit {
				t.Fatalf("exit=%d want=%d", exit, tc.exit)
			}
			found := false
			for _, e := range env["errors"].([]any) {
				found = found || e.(map[string]any)["code"] == tc.code
			}
			if !found {
				t.Fatalf("missing %s", tc.code)
			}
		})
	}
}
func TestCLIV2AuthInventoryDegradedNoRefresh(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run(fmt.Sprint(optional), func(t *testing.T) {
			deps := authTestDependencies(t)
			a := &authTestAuthority{inventory: codexprov.Inventory{ExternalSources: []codexprov.ExternalSourceStatus{{ErrorCode: "unreadable", OptionalAbsent: optional}}, Accounts: []codexprov.LogicalAccount{{Key: "opaque", Candidates: []codexprov.CredentialCandidate{authCandidate("a", deps.Now())}}}}}
			calls := 0
			a.refresh = func(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.RefreshResult, error) {
				calls++
				return codexprov.RefreshResult{CredentialsChanged: true}, nil
			}
			deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
				return a, func() error { return nil }, nil
			}
			exit, _, _ := authTestRun(t, context.Background(), []string{"auth", "refresh", "codex", "--json"}, deps, "", false)
			if optional && (exit != 0 || calls != 1) || !optional && (exit != 6 || calls != 0) {
				t.Fatalf("exit=%d calls=%d", exit, calls)
			}
		})
	}
}

func TestCLIV2AuthProductionPendingRemoval(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint(remote), func(t *testing.T) {
			store, record, journal := resetProductionFixture(t)
			client := testDoer(func(*http.Request) (*http.Response, error) {
				t.Fatal("pending removal exchanged credentials")
				return nil, nil
			})
			var owner *codexprov.CredentialControl
			if remote {
				c, err := codexprov.NewCredentialCoordinator(store, journal.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				c.RefreshMutations = resetProductionRecorder{}
				c.CredentialOwner = resetProductionRecorder{}
				c.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
					return auth.RefreshCodexToken(ctx, client, token)
				}
				owner, err = codexprov.OpenCredentialControl(codexprov.DefaultCredentialControlPath(journal.StateDir), c)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
			}
			plan := codexprov.RemovalPlan{Version: 1, OperationID: "auth-pending", AccountKey: record.Metadata.AccountKey, Candidates: []codexprov.RemovalCandidate{{CandidateID: record.Metadata.CandidateID, Revision: record.Metadata.Revision}}}
			if err := journal.Save(plan); err != nil {
				t.Fatal(err)
			}
			deps := authTestDependencies(t)
			deps.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
			deps.Codex = func(ctx context.Context) (codexRefreshAuthority, func() error, error) {
				c, err := codexprov.OpenDefaultCanonicalCredentialRefreshControl(ctx, store.FS, client)
				if err != nil {
					return nil, nil, err
				}
				return c.CanonicalAdmin(), c.Close, nil
			}
			exit, _, _ := authTestRun(t, context.Background(), []string{"auth", "refresh", "codex", "--json"}, deps, "", false)
			if exit != 5 {
				t.Fatalf("exit=%d", exit)
			}
			current, err := store.Load(record.Path)
			if err != nil || current.Metadata.Revision != record.Metadata.Revision {
				t.Fatal("pending refresh changed/deleted credentials")
			}
			if _, pending, err := journal.Load(); err != nil || !pending {
				t.Fatal("canonical refresh recovered pending removal")
			}
		})
	}
}
func TestCLIV2AuthCancellationRetainsEarlierCommit(t *testing.T) {
	deps := authTestDependencies(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &authTestAuthority{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "a", Candidates: []codexprov.CredentialCandidate{authCandidate("a", deps.Now()), authCandidate("b", deps.Now())}}}}}
	a.refresh = func(_ context.Context, r codexprov.CandidateRef, _ codexprov.Revision) (codexprov.RefreshResult, error) {
		if r.CandidateID == "a" {
			return codexprov.RefreshResult{CredentialsChanged: true}, nil
		}
		cancel()
		return codexprov.RefreshResult{}, &codexprov.MutationOutcomeUnknown{Err: context.Canceled}
	}
	deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
		return a, func() error { return nil }, nil
	}
	exit, env, _ := authTestRun(t, ctx, []string{"auth", "refresh", "codex", "--json"}, deps, "", false)
	if exit != 130 || env["data"].(map[string]any)["changed_count"] != float64(1) {
		t.Fatal("interruption lost known commit")
	}
	warnings := env["warnings"].([]any)
	if len(warnings) != 1 || warnings[0].(map[string]any)["code"] != "auth_refresh_outcome_unknown" {
		t.Fatal("unknown receipt not disclosed")
	}
}
func TestCLIV2AuthReauthenticationCountsActualIdentity(t *testing.T) {
	deps := authTestDependencies(t)
	deps.DiscoverClaude = func(context.Context) []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{AccountUUID: "requested", Email: "requested@test.invalid", ExpiresAt: 1}, {AccountUUID: "returned", Email: "returned@test.invalid", ExpiresAt: 1}}
	}
	deps.Login = func(context.Context) (authReauthResult, error) {
		return authReauthResult{AccountUUID: "returned", Email: "returned@test.invalid", CredentialsChanged: true}, nil
	}
	out := handleV2AuthWithDependencies(context.Background(), cli.Invocation{Path: "auth refresh", Arguments: map[string][]string{"providers": {"claude"}}}, &cli.Session{In: strings.NewReader("\n"), Err: io.Discard, Interactive: true}, deps)
	var data struct {
		Providers    []AuthRefreshProviderResult `json:"providers"`
		ChangedCount int                         `json:"changed_count"`
	}
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 8 || data.ChangedCount != 1 || len(data.Providers[0].Accounts) != 2 {
		t.Fatalf("exit=%d data=%s", out.ExitCode, out.Data)
	}
	rows := data.Providers[0].Accounts
	if rows[0].CredentialsChanged || rows[0].Status != "reauth_required" || !rows[1].CredentialsChanged || rows[1].Status != "refreshed" {
		t.Fatal("browser result attributed to requested identity")
	}
}
func TestCLIV2AuthReauthenticationPersistence(t *testing.T) {
	now := time.Unix(1000, 0)
	tokens := &auth.TokenResponse{AccessToken: "secret-sentinel", RefreshToken: "refresh", Scope: "one two"}
	profile := &auth.Profile{AccountUUID: "actual", Email: "actual@test.invalid", OrgUUID: "org", Plan: "pro", RateLimitTier: "tier", RawJSON: json.RawMessage(`{"account":"profile"}`)}
	for _, tc := range []struct {
		name    string
		tokens  *auth.TokenResponse
		profile *auth.Profile
		changed bool
		err     error
	}{
		{"saved then failed", tokens, profile, true, errors.New("store failed")},
		{"saved", tokens, profile, true, nil},
		{"nil tokens", nil, profile, false, nil},
		{"nil profile", tokens, nil, false, nil},
		{"empty access", &auth.TokenResponse{}, profile, false, nil},
		{"empty identity", tokens, &auth.Profile{}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			result, err := refreshClaudeLogin(context.Background(), nil, func(ctx context.Context, _ httputil.Doer) (*auth.TokenResponse, *auth.Profile, error) {
				if _, bounded := ctx.Deadline(); bounded {
					t.Fatal("overall deadline imposed")
				}
				return tc.tokens, tc.profile, nil
			}, func(_ context.Context, a *keyring.ClaudeOAuth) (bool, error) {
				calls++
				if a.AccountUUID != "actual" || a.ExpiresAt != now.UnixMilli()+auth.DefaultExpiresInSec*1000 || !reflect.DeepEqual(a.Scopes, []string{"one", "two"}) || a.TokenAccount.OrganizationUUID != "org" || a.SubscriptionType != "pro" || a.RateLimitTier != "tier" || string(a.Profile) != `{"account":"profile"}` {
					t.Fatal("login metadata/defaults lost")
				}
				return tc.changed, tc.err
			}, func() time.Time { return now })
			if result.CredentialsChanged != tc.changed {
				t.Fatal("lost partial commit")
			}
			if tc.tokens == nil || tc.profile == nil || tc.tokens.AccessToken == "" || tc.profile.AccountUUID == "" {
				if calls != 0 || !errors.Is(err, app.ErrAccountAuthentication) {
					t.Fatal("invalid browser response persisted")
				}
			} else if result.AccountUUID != "actual" || result.Email != "actual@test.invalid" || calls != 1 || (err == nil) != (tc.err == nil) {
				t.Fatal("login outcome lost identity or persistence failure")
			}
		})
	}
}
func TestCLIV2AuthAuthorityFailureAfterOtherProviderCommit(t *testing.T) {
	deps := authTestDependencies(t)
	deps.DiscoverClaude = func(context.Context) []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{AccountUUID: "id", Email: "claude@test.invalid", RefreshToken: "refresh", ExpiresAt: 1}}
	}
	deps.HTTP = testDoer(func(*http.Request) (*http.Response, error) {
		return resetProductionResponse(200, `{"access_token":"secret-sentinel","expires_in":3600}`), nil
	})
	deps.PersistClaude = func(context.Context, *keyring.ClaudeOAuth) (bool, error) { return true, nil }
	deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
		return nil, nil, codexprov.ErrCredentialAuthorityUnavailable
	}
	exit, env, _ := authTestRun(t, context.Background(), []string{"auth", "refresh", "--json"}, deps, "", false)
	if exit != 8 || env["data"].(map[string]any)["changed_count"] != float64(1) {
		t.Fatal("other-provider commit hidden")
	}
	found := false
	for _, e := range env["errors"].([]any) {
		found = found || e.(map[string]any)["code"] == "auth_authority_unavailable"
	}
	if !found {
		t.Fatal("constituent authority error lost")
	}
}
func TestCLIV2AuthHelpHasNoAccess(t *testing.T) {
	var out, diag bytes.Buffer
	exit := cli.Run(context.Background(), []string{"auth", "refresh", "--help"}, &cli.Session{Out: &out, Err: &diag}, func(string) (cli.Handler, bool) { t.Fatal("help reached dependencies"); return nil, false })
	want, err := os.ReadFile("../../specs/cli-v2/help/auth-refresh.txt")
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 || !bytes.Equal(out.Bytes(), want) || diag.Len() != 0 {
		t.Fatal("exact auth help changed or accessed state")
	}
}
func TestCLIV2AuthAuthorityOpenErrorIsUnavailable(t *testing.T) {
	deps := authTestDependencies(t)
	deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
		return nil, nil, errors.New("secret-sentinel")
	}
	exit, env, _ := authTestRun(t, context.Background(), []string{"auth", "refresh", "codex", "--json"}, deps, "", false)
	if exit != 4 || env["errors"].([]any)[0].(map[string]any)["code"] != "auth_authority_unavailable" {
		t.Fatal("opener failure misclassified as authentication")
	}
}
func TestCLIV2AuthCacheFailureIsSafeWarning(t *testing.T) {
	deps := authTestDependencies(t)
	a := &authTestAuthority{inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "a", Candidates: []codexprov.CredentialCandidate{authCandidate("a", deps.Now())}}}}}
	a.refresh = func(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.RefreshResult, error) {
		return codexprov.RefreshResult{CredentialsChanged: true}, nil
	}
	deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
		return a, func() error { return nil }, nil
	}
	deps.Invalidate = func(provider.ID) error { return errors.New("secret-sentinel") }
	exit, env, stderr := authTestRun(t, context.Background(), []string{"auth", "refresh", "codex", "--json"}, deps, "", false)
	warnings := env["warnings"].([]any)
	wantStderr := "cq: warning: Changed credentials were saved, but the provider quota cache could not be invalidated.\n"
	if exit != 0 || stderr != wantStderr || len(warnings) != 1 || warnings[0].(map[string]any)["code"] != "auth_cache_invalidation_failed" || env["data"].(map[string]any)["changed_count"] != float64(1) {
		t.Fatal("cache warning lost credential success or was not safely rendered")
	}
}
func TestCLIV2AuthPromptInterruption(t *testing.T) {
	deps := authTestDependencies(t)
	deps.DiscoverClaude = func(context.Context) []keyring.ClaudeOAuth {
		return []keyring.ClaudeOAuth{{AccountUUID: "id", Email: "a@test.invalid", ExpiresAt: 1}}
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan cli.Outcome, 1)
	session := &cli.Session{In: reader, Err: authPromptWriter{entered: entered}, Interactive: true}
	go func() {
		defer func() {
			if recover() != nil {
				done <- cli.Outcome{ExitCode: 99}
			}
		}()
		done <- handleV2AuthWithDependencies(ctx, cli.Invocation{Path: "auth refresh", Arguments: map[string][]string{"providers": {"claude"}}}, session, deps)
	}()
	<-entered
	cancel()
	select {
	case out := <-done:
		if out.ExitCode != 130 {
			t.Fatalf("exit=%d", out.ExitCode)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt ignored interruption")
	}
}

type authPromptWriter struct{ entered chan struct{} }

func (w authPromptWriter) Write(p []byte) (int, error) { close(w.entered); return len(p), nil }
