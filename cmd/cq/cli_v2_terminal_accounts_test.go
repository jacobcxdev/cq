package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/provider"
	"os"
	"strings"
	"testing"
	"time"
)

var terminalAccountsFixtures = []terminalFixtureRegistration{
	{"cq auth refresh", "Success", "stdout", "text/cq-auth-refresh/02-success.txt"},
	{"cq auth refresh", "No-op", "stdout", "text/cq-auth-refresh/03-no-op.txt"},
	{"cq auth refresh", "Partial", "stdout", "text/cq-auth-refresh/04-partial.txt"},
	{"cq auth refresh", "Partial", "stderr", "text/cq-auth-refresh/05-partial.txt"},
	{"cq auth refresh", "Failure", "stderr", "text/cq-auth-refresh/06-failure.txt"},
	{"cq auth refresh", "Interrupted after a confirmed write", "stdout", "text/cq-auth-refresh/07-interrupted-after-a-confirmed-write.txt"},
	{"cq auth refresh", "Interrupted after a confirmed write", "stderr", "text/cq-auth-refresh/08-interrupted-after-a-confirmed-write.txt"},
	{"cq claude account activate", "Success", "stdout", "text/cq-claude-account-activate/02-success.txt"},
	{"cq claude account activate", "No-op", "stdout", "text/cq-claude-account-activate/03-no-op.txt"},
	{"cq claude account activate", "Failure", "stderr", "text/cq-claude-account-activate/04-failure.txt"},
	{"cq claude account activate", "Partial", "stdout", "text/cq-claude-account-activate/05-partial.txt"},
	{"cq claude account activate", "Partial", "stderr", "text/cq-claude-account-activate/06-partial.txt"},
	{"cq claude account list", "Success", "stdout", "text/cq-claude-account-list/02-success.txt"},
	{"cq claude account list", "Empty", "stdout", "text/cq-claude-account-list/03-empty.txt"},
	{"cq claude account list", "Failure", "stderr", "text/cq-claude-account-list/04-failure.txt"},
	{"cq claude account login", "Success", "stdout", "text/cq-claude-account-login/02-success.txt"},
	{"cq claude account login", "Success with activation", "stdout", "text/cq-claude-account-login/03-success-with-activation.txt"},
	{"cq claude account login", "Failure", "stderr", "text/cq-claude-account-login/04-failure.txt"},
	{"cq claude account login", "Partial activation", "stdout", "text/cq-claude-account-login/05-partial-activation.txt"},
	{"cq claude account login", "Partial activation", "stderr", "text/cq-claude-account-login/06-partial-activation.txt"},
	{"cq claude account login", "Timeout before credentials saved", "stderr", "text/cq-claude-account-login/07-timeout-before-credentials-saved.txt"},
	{"cq claude account remove", "Success", "stdout", "text/cq-claude-account-remove/02-success.txt"},
	{"cq claude account remove", "Confirmation declined", "stdout", "text/cq-claude-account-remove/03-confirmation-declined.txt"},
	{"cq claude account remove", "Confirmation declined", "stderr", "text/cq-claude-account-remove/04-confirmation-declined.txt"},
	{"cq claude account remove", "Failure with redirected input", "stderr", "text/cq-claude-account-remove/05-failure-with-redirected-input.txt"},
	{"cq claude account remove", "Partial removal", "stderr", "text/cq-claude-account-remove/06-partial-removal.txt"},
	{"cq codex account activate", "Success", "stdout", "text/cq-codex-account-activate/02-success.txt"},
	{"cq codex account activate", "No-op", "stdout", "text/cq-codex-account-activate/03-no-op.txt"},
	{"cq codex account activate", "Failure", "stderr", "text/cq-codex-account-activate/04-failure.txt"},
	{"cq codex account activate", "Partial", "stdout", "text/cq-codex-account-activate/05-partial.txt"},
	{"cq codex account activate", "Partial", "stderr", "text/cq-codex-account-activate/06-partial.txt"},
	{"cq codex account list", "Success", "stdout", "text/cq-codex-account-list/02-success.txt"},
	{"cq codex account list", "Empty", "stdout", "text/cq-codex-account-list/03-empty.txt"},
	{"cq codex account list", "Failure", "stderr", "text/cq-codex-account-list/04-failure.txt"},
	{"cq codex account login", "Success", "stdout", "text/cq-codex-account-login/02-success.txt"},
	{"cq codex account login", "Success with activation", "stdout", "text/cq-codex-account-login/03-success-with-activation.txt"},
	{"cq codex account login", "Failure", "stderr", "text/cq-codex-account-login/04-failure.txt"},
	{"cq codex account login", "Partial activation", "stdout", "text/cq-codex-account-login/05-partial-activation.txt"},
	{"cq codex account login", "Partial activation", "stderr", "text/cq-codex-account-login/06-partial-activation.txt"},
	{"cq codex account login", "Timeout before credentials saved", "stderr", "text/cq-codex-account-login/07-timeout-before-credentials-saved.txt"},
	{"cq codex account remove", "Success", "stdout", "text/cq-codex-account-remove/02-success.txt"},
	{"cq codex account remove", "Confirmation declined", "stdout", "text/cq-codex-account-remove/03-confirmation-declined.txt"},
	{"cq codex account remove", "Confirmation declined", "stderr", "text/cq-codex-account-remove/04-confirmation-declined.txt"},
	{"cq codex account remove", "Failure with redirected input", "stderr", "text/cq-codex-account-remove/05-failure-with-redirected-input.txt"},
	{"cq codex account remove", "Partial removal", "stderr", "text/cq-codex-account-remove/06-partial-removal.txt"},
	{"cq gemini account show", "Success", "stdout", "text/cq-gemini-account-show/02-success.txt"},
	{"cq gemini account show", "Not configured", "stdout", "text/cq-gemini-account-show/03-not-configured.txt"},
	{"cq gemini account show", "Failure", "stderr", "text/cq-gemini-account-show/04-failure.txt"},
}

func terminalFixtureAccount(id string) AccountSummary {
	reference := "account-alex"
	if id == "claude" {
		reference = "alex@example.test"
	}
	a := v2AccountSummary(provider.ID(id), "acct_alex_01", "alex@example.test", "", id+"_pro_20x")
	if id == "claude" {
		a.RateLimitTier = v2AccountString("claude_max_20x")
	}
	a.AccountReference = &reference
	a.Active = true
	a.Stable = true
	a.Sources = []string{"cq_managed"}
	return a
}
func terminalFixtureOptions(width int) cli.TerminalOptions {
	location, _ := time.LoadLocation("Europe/London")
	return cli.TerminalOptions{Width: width, Colour: true, Now: time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC), Location: location}
}
func TestTerminalAccountsFixtures(t *testing.T) {
	for _, f := range terminalAccountsFixtures {
		for _, width := range []int{20, 40, 80, 100, 120} {
			t.Run(fmt.Sprintf("%s/%d", f.File, width), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				s := &cli.Session{In: strings.NewReader("n\n"), Out: &stdout, Err: &stderr, Interactive: true, OutTerminal: terminalFixtureOptions(width), ErrTerminal: terminalFixtureOptions(width)}
				args := strings.Fields(strings.TrimPrefix(f.Command, "cq "))
				if strings.HasSuffix(f.Command, "activate") || strings.HasSuffix(f.Command, "remove") {
					args = append(args, "alex@example.test")
				}
				if f.Scenario == "Success with activation" || f.Scenario == "Partial activation" {
					args = append(args, "--activate")
				}
				if strings.HasSuffix(f.Command, "remove") && f.Scenario != "Confirmation declined" && f.Scenario != "Failure with redirected input" {
					args = append(args, "--yes")
				}
				if f.Scenario == "Failure with redirected input" {
					s.Interactive = false
				}
				exit := cli.Run(context.Background(), args, s, func(path string) (cli.Handler, bool) {
					return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
						if path == "auth refresh" {
							return terminalAuthFixtureOutcome(f.Scenario)
						}
						id := strings.Fields(path)[0]
						title := terminalProviderName(id)
						a := terminalFixtureAccount(id)
						if strings.HasSuffix(path, "list") {
							if f.Scenario == "Failure" {
								return v2AccountFailure(path, false)
							}
							accounts := []AccountSummary{a}
							if f.Scenario == "Empty" {
								accounts = nil
							}
							return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalAccounts(title, accounts, o) }}
						}
						if id == "gemini" {
							if f.Scenario == "Failure" {
								return v2AccountFailure(path, false)
							}
							a.AccountID = v2AccountString("gemini_alex_01")
							a.AccountReference = nil
							a.RateLimitTier = nil
							a.Sources = []string{"external"}
							var account *AccountSummary = &a
							if f.Scenario == "Not configured" {
								account = nil
							}
							return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalGeminiAccount(account, true, o) }}
						}
						deps := v2AccountMutationDependencies{
							Login: func(context.Context, provider.ID, bool) (app.AccountLoginResult, error) {
								if f.Scenario == "Failure" {
									return app.AccountLoginResult{}, app.ErrAccountAuthentication
								}
								if f.Scenario == "Timeout before credentials saved" {
									return app.AccountLoginResult{}, context.DeadlineExceeded
								}
								active := f.Scenario == "Success with activation"
								result := app.AccountLoginResult{CredentialsSaved: true, AccountObserved: true, ActivationKnown: true, Activated: active, Reference: *a.AccountReference, Sources: a.Sources, Account: provider.Account{AccountID: *a.AccountID, Email: *a.Email, RateLimitTier: *a.RateLimitTier, Active: active}}
								if f.Scenario == "Partial activation" {
									result.ActivationKnown = false
									return result, app.ErrAccountActivation
								}
								return result, nil
							},
							Select: func(context.Context, provider.ID, string) (v2AccountSelection, error) {
								return v2AccountSelection{Account: a, Activatable: f.Scenario != "Failure", Removable: true, Activate: func(context.Context) (v2AccountActivation, error) {
									r := v2AccountActivation{Changed: f.Scenario != "No-op", Active: true}
									if f.Scenario == "Partial" {
										return r, errors.New("safe fixture failure")
									}
									return r, nil
								}, Remove: func(context.Context) (v2AccountRemoval, error) {
									if f.Scenario == "Partial removal" {
										return v2AccountRemoval{Pending: true}, errors.New("safe fixture failure")
									}
									return v2AccountRemoval{Changed: true, ActiveRemoved: true}, nil
								}}, nil
							},
						}
						return handleV2AccountMutationWithDependencies(ctx, inv, s, deps)
					}, true
				})
				wantExit := 0
				switch f.Scenario {
				case "Partial", "Partial activation", "Partial removal":
					wantExit = 8
				case "Timeout before credentials saved":
					wantExit = 7
				case "Failure with redirected input":
					wantExit = 6
				case "Interrupted after a confirmed write":
					wantExit = 130
				case "Failure":
					wantExit = 4
					if strings.HasSuffix(f.Command, "activate") {
						wantExit = 6
					}
					if strings.HasSuffix(f.Command, "login") {
						wantExit = 5
					}
				}
				if exit != wantExit {
					t.Fatalf("exit %d want %d", exit, wantExit)
				}
				got := stdout.String()
				if f.Stream == "stderr" {
					got = stderr.String()
				}
				if width >= 100 {
					assertTerminalFixture(t, f.File, got)
				}
				for _, line := range strings.Split(got, "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Fatalf("overflow at %d: %q", width, line)
					}
				}
			})
		}
	}
}
func terminalAuthFixtureOutcome(scenario string) cli.Outcome {
	alex := AuthRefreshAccountResult{AccountLabel: "alex@example.test", AccountKey: v2AccountString("account-alex"), CredentialsChanged: true, Status: "refreshed", Reason: "refreshed"}
	sam := AuthRefreshAccountResult{AccountLabel: "sam@example.test", AccountKey: v2AccountString("account-sam"), Status: "unchanged", Reason: "not_expiring"}
	r := AuthRefreshProviderResult{Provider: "codex", Refreshed: 1, Unchanged: 1, Accounts: []AuthRefreshAccountResult{alex, sam}}
	total := 1
	out := cli.Outcome{}
	switch scenario {
	case "Failure":
		return authFailure("auth_authority_unavailable", "")
	case "No-op":
		alex.CredentialsChanged = false
		alex.Status = "unchanged"
		alex.Reason = "unknown_expiry"
		r.Refreshed = 0
		r.Accounts = []AuthRefreshAccountResult{alex}
		total = 0
	case "Partial":
		sam.Status = "reauth_required"
		r.Unchanged = 0
		r.ReauthRequired = 1
		r.Accounts = []AuthRefreshAccountResult{alex, sam}
		out = authFailure("auth_refresh_partial", "")
	case "Interrupted after a confirmed write":
		r.Accounts = []AuthRefreshAccountResult{alex}
		out = authFailure("auth_interrupted", "")
	}
	out.Terminal = func(o cli.TerminalOptions) string {
		return terminalAuthRefresh([]authRefreshWork{{result: r}}, total, out.ExitCode, o)
	}
	return out
}
func TestTerminalAccountsRegistration(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct{ Command, Scenario, Stream, File string }
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	registered := map[string]terminalFixtureRegistration{}
	for _, f := range terminalAccountsFixtures {
		if _, ok := registered[f.File]; ok {
			t.Fatal("duplicate", f.File)
		}
		registered[f.File] = f
	}
	for _, f := range index.Fixtures {
		if f.Scenario == "Help" {
			continue
		}
		if f.Command != "cq auth refresh" && !(strings.Contains(f.Command, " account ") && len(strings.Fields(f.Command)) == 4) {
			continue
		}
		r, ok := registered[f.File]
		if !ok || r.Command != f.Command || r.Scenario != f.Scenario || r.Stream != f.Stream {
			t.Fatal("missing or mismatched fixture", f.File)
		}
		delete(registered, f.File)
	}
	if len(registered) != 0 {
		t.Fatal("unrecognised fixture registrations", registered)
	}
}
func TestTerminalAccountsRolesAndWidths(t *testing.T) {
	a := terminalFixtureAccount("codex")
	a.DisplayName = "failed@example.test\x1b[31m界界界"
	a.AccountReference = v2AccountString(strings.Repeat("long-id-", 20))
	a.Aliases = []string{"failed-alias"}
	for _, width := range []int{20, 40, 80, 120} {
		o := terminalFixtureOptions(width)
		got := terminalAccounts("Codex", []AccountSummary{a}, o)
		if !strings.Contains(got, "\x1b[36mfailed@example") || strings.Contains(got, "\x1b[31m") {
			t.Fatalf("untrusted identity affected role: %q", got)
		}
		if !strings.Contains(got, "\x1b[32mActive") || !strings.Contains(got, "\x1b[1;34m1") {
			t.Fatal("missing typed roles")
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d: %q", width, line)
			}
		}
		o.Colour = false
		if plain := terminalAccounts("Codex", []AccountSummary{a}, o); plain != ansi.Strip(got) {
			t.Fatal("NO_COLOR changed content")
		}
	}
}

func TestTerminalAccountsInspectionAdapter(t *testing.T) {
	for _, id := range []string{"codex", "claude", "gemini"} {
		t.Run(id, func(t *testing.T) {
			scenario := v2Fixtures["accounts-readonly"](t)
			var stdout, stderr bytes.Buffer
			action := "list"
			if id == "gemini" {
				action = "show"
			}
			exit := cli.Run(context.Background(), []string{id, "account", action}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(100), ErrTerminal: terminalFixtureOptions(100)}, scenario.Lookup)
			if exit != 0 {
				t.Fatalf("exit %d: %s", exit, stderr.String())
			}
			got := ansi.Strip(stdout.String())
			if !strings.Contains(got, terminalProviderName(id)+" account") {
				t.Fatalf("adapter did not attach terminal callback: %s", got)
			}
			if id == "codex" && (!strings.Contains(got, "Tier            unknown") || !strings.Contains(got, "Label           pro")) {
				t.Fatalf("plan label fabricated tier: %s", got)
			}
			if id == "claude" && !strings.Contains(got, "Reference       unknown") {
				t.Fatalf("duplicate email became selector: %s", got)
			}
		})
	}
}

func TestTerminalAuthAdapterPartialWrite(t *testing.T) {
	f := v2Fixtures["auth-partial-write"](t)
	var stdout, stderr bytes.Buffer
	exit := cli.Run(context.Background(), []string{"auth", "refresh", "codex"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(100), ErrTerminal: terminalFixtureOptions(100)}, f.Lookup)
	if exit != 8 {
		t.Fatal(exit)
	}
	text := ansi.Strip(stdout.String())
	if !strings.Contains(text, "Authentication refresh · Partial") || !strings.Contains(text, "Credentials: Saved") || !strings.Contains(text, "Credentials saved for 1 account") {
		t.Fatal(text)
	}
	if strings.Contains(stdout.String()+stderr.String(), "secret-refresh-error") {
		t.Fatal("leaked upstream detail")
	}
	if f.calls["refresh"] != 2 || f.calls["invalidate"] != 1 {
		t.Fatal(f.calls)
	}
}

func TestTerminalAccountsCancelledLoginAndFailedRemoval(t *testing.T) {
	for _, action := range []string{"login", "remove"} {
		var stdout, stderr bytes.Buffer
		args := []string{"codex", "account", action}
		if action == "remove" {
			args = append(args, "alex@example.test", "--yes")
		}
		deps := v2AccountMutationDependencies{Login: func(context.Context, provider.ID, bool) (app.AccountLoginResult, error) {
			return app.AccountLoginResult{}, context.Canceled
		}, Select: func(context.Context, provider.ID, string) (v2AccountSelection, error) {
			return v2AccountSelection{Account: terminalFixtureAccount("codex"), Removable: true, Remove: func(context.Context) (v2AccountRemoval, error) {
				return v2AccountRemoval{}, errors.New("private removal error")
			}}, nil
		}}
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(100), ErrTerminal: terminalFixtureOptions(100)}, func(string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2AccountMutationWithDependencies(ctx, inv, s, deps)
			}, true
		})
		want := 130
		if action == "remove" {
			want = 1
		}
		if exit != want || stdout.Len() != 0 || strings.Contains(stderr.String(), "private removal error") {
			t.Fatalf("%s: exit %d stdout %q stderr %q", action, exit, stdout.String(), stderr.String())
		}
	}
}

func TestTerminalAuthFailedDoesNotClaimPartialOrNoWork(t *testing.T) {
	got := ansi.Strip(terminalAuthRefresh([]authRefreshWork{{result: AuthRefreshProviderResult{Provider: "codex", Failed: 1, Accounts: []AuthRefreshAccountResult{{AccountLabel: "failed@example.test", Status: "failed"}}}}}, 0, 5, terminalFixtureOptions(100)))
	if !strings.HasPrefix(got, "Authentication refresh · Failed\n") || strings.Contains(got, "needed refreshing") {
		t.Fatal(got)
	}
}

func TestTerminalAuthUnavailableInventoryIsNotZero(t *testing.T) {
	deps := authTestDependencies(t)
	deps.Codex = func(context.Context) (codexRefreshAuthority, func() error, error) {
		return nil, nil, errors.New("unavailable")
	}
	inv := cli.Invocation{Path: "auth refresh", Arguments: map[string][]string{"providers": {"codex"}}}
	out := handleV2AuthWithDependencies(context.Background(), inv, &cli.Session{}, deps)
	if got := out.Terminal(terminalFixtureOptions(100)); got != "" {
		t.Fatalf("unobserved inventory displayed as counts: %s", got)
	}
	inv.Arguments["providers"] = []string{"claude", "codex"}
	out = handleV2AuthWithDependencies(context.Background(), inv, &cli.Session{}, deps)
	got := ansi.Strip(out.Terminal(terminalFixtureOptions(100)))
	if !strings.Contains(got, "Provider: Codex\n") || !strings.Contains(got, "Refreshed: unknown") {
		t.Fatalf("failed inventory not unknown: %s", got)
	}
}
