package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type v2MutationAdmin struct{ fixture *v2Fixture }

func (a *v2MutationAdmin) SaveLogin(context.Context, codexprov.LoginCredential) (codexprov.CandidateRef, codexprov.Revision, error) {
	a.fixture.Call("save")
	return codexprov.CandidateRef{AccountKey: "exact-key", CandidateID: "candidate"}, "revision", nil
}
func (a *v2MutationAdmin) Activate(context.Context, codexprov.CandidateRef, codexprov.Revision) (codexprov.ActivationResult, error) {
	a.fixture.Call("activate")
	return codexprov.ActivationResult{}, errors.New("credential-secret")
}
func (a *v2MutationAdmin) Adopt(context.Context, codexprov.SystemSnapshot) (codexprov.CandidateRef, codexprov.Revision, error) {
	panic("unexpected adopt")
}
func (a *v2MutationAdmin) RemoveManaged(context.Context, codexprov.AccountKey, codexprov.RevisionSet, bool) (codexprov.RemovalResult, error) {
	panic("unexpected remove")
}
func init() {
	registerV2Fixture("login-activation-fails", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{Secrets: []string{"credential-secret"}}
		deps := v2AccountMutationDependencies{Login: func(ctx context.Context, id provider.ID, activate bool) (app.AccountLoginResult, error) {
			return app.LoginCodex(ctx, nil, activate, func(context.Context, httputil.Doer) (*auth.CodexTokenResponse, *auth.CodexClaims, error) {
				f.Call("oauth")
				return &auth.CodexTokenResponse{AccessToken: "credential-secret"}, &auth.CodexClaims{AccountID: "account", UserID: "user", Email: "user@example.com"}, nil
			}, &v2MutationAdmin{fixture: f})
		}}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2AccountMutation(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2AccountMutationWithDependencies(ctx, inv, s, deps)
			}, ok
		}
		return f
	})
}
func TestCLIV2AccountMutationContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "saved login reports partial activation", Scenario: "login-activation-fails", Args: []string{"codex", "account", "login", "--activate", "--json"}, Exit: 8, Command: "codex account login", Code: "account_login_partial", WantJSON: `{"credentials_saved":true,"activated":false}`, Forbid: []string{"service", "consume"}})
}

func TestCLIV2AccountMutationLoginWithoutActivation(t *testing.T) {
	for _, suffix := range [][]string{{"--json"}, {"--activate=false", "--json"}} {
		runV2Case(t, v2Case{Name: strings.Join(suffix, " "), Scenario: "login-activation-fails", Args: append([]string{"codex", "account", "login"}, suffix...), Command: "codex account login", WantJSON: `{"credentials_saved":true,"activated":false}`, Calls: map[string]int{"oauth": 1, "save": 1}, Forbid: []string{"activate", "service", "consume"}})
	}
}
func mutationInvocation(path string, options ...string) cli.Invocation {
	inv, err := cli.Parse(append(strings.Fields(path), options...))
	if err != nil {
		panic(err)
	}
	return inv
}
func TestCLIV2AccountMutationConsentAndPartial(t *testing.T) {
	for _, id := range []string{"claude", "codex"} {
		for _, mode := range []string{"json-no-consent", "nonterminal", "negative", "eof", "yes", "partial", "read-only", "vanished"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				var out, errOut bytes.Buffer
				calls, reads := 0, 0
				session := &cli.Session{In: strings.NewReader("yes\n"), Out: &out, Err: &errOut, Interactive: true}
				options := []string{"user@example.com"}
				if mode == "json-no-consent" {
					options = append(options, "--json")
				}
				if mode == "nonterminal" {
					session.Interactive = false
				}
				if mode == "negative" {
					session.In = strings.NewReader("\n")
				}
				if mode == "eof" {
					session.In = strings.NewReader("yes")
				}
				selected := v2AccountSelection{Account: v2AccountSummary(provider.ID(id), "uuid", "user@example.com", "", ""), Removable: mode != "read-only", Retained: []string{"external"}}
				selected.Account.AccountReference = v2AccountString("exact-reference")
				selected.Remove = func(context.Context) (v2AccountRemoval, error) {
					calls++
					switch mode {
					case "partial":
						return v2AccountRemoval{Changed: true, Pending: true}, errors.New("credential-secret")
					case "vanished":
						return v2AccountRemoval{}, v2MutationDiagnostic("account_not_found")
					}
					return v2AccountRemoval{Changed: true, ActiveRemoved: true}, nil
				}
				deps := v2AccountMutationDependencies{Select: func(context.Context, provider.ID, string) (v2AccountSelection, error) { reads++; return selected, nil }}
				result := handleV2AccountMutationWithDependencies(context.Background(), mutationInvocation(id+" account remove", options...), session, deps)
				want := 0
				switch mode {
				case "json-no-consent", "nonterminal", "read-only":
					want = 6
				case "partial":
					want = 8
				case "vanished":
					want = 3
				}
				if result.ExitCode != want {
					t.Fatalf("exit=%d want=%d errors=%v", result.ExitCode, want, result.Errors)
				}
				switch mode {
				case "json-no-consent", "nonterminal":
					if reads != 0 || calls != 0 {
						t.Fatal("noninteractive missing consent accessed inventory")
					}
				case "negative", "eof", "read-only":
					if calls != 0 {
						t.Fatal("unconfirmed deletion")
					}
				default:
					if calls != 1 {
						t.Fatal("mutation repeated or omitted")
					}
				}
				if mode == "partial" && !bytes.Contains(result.Data, []byte(`"removed":true`)) {
					t.Fatal("partial deletion evidence lost")
				}
				if mode == "negative" && !strings.Contains(errOut.String(), "exact-reference") {
					t.Fatal("consent preview omitted exact identity")
				}
				if strings.Contains(errOut.String(), "credential-secret") {
					t.Fatal("private error leaked")
				}
			})
		}
	}
}
func TestCLIV2AccountMutationActivationOutcomes(t *testing.T) {
	for _, id := range []string{"claude", "codex"} {
		for _, mode := range []string{"success", "already-active", "partial", "failed", "not-activatable"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				calls := 0
				selected := v2AccountSelection{Account: v2AccountSummary(provider.ID(id), "uuid", "user@example.com", "", ""), Activatable: mode != "not-activatable"}
				selected.Activate = func(context.Context) (v2AccountActivation, error) {
					calls++
					switch mode {
					case "failed":
						return v2AccountActivation{}, errors.New("secret")
					case "partial":
						return v2AccountActivation{Changed: true, Active: true}, errors.New("secret")
					case "already-active":
						return v2AccountActivation{Active: true}, nil
					}
					return v2AccountActivation{Changed: true, Active: true}, nil
				}
				result := handleV2AccountMutationWithDependencies(context.Background(), mutationInvocation(id+" account activate", "user@example.com"), &cli.Session{}, v2AccountMutationDependencies{Select: func(context.Context, provider.ID, string) (v2AccountSelection, error) { return selected, nil }})
				want := 0
				switch mode {
				case "failed":
					want = 1
				case "partial":
					want = 8
				case "not-activatable":
					want = 6
				}
				if result.ExitCode != want {
					t.Fatalf("exit=%d errors=%v", result.ExitCode, result.Errors)
				}
				if mode == "partial" && !bytes.Contains(result.Data, []byte(`"changed":true`)) {
					t.Fatal("partial activation evidence lost")
				}
				if mode == "already-active" && !bytes.Contains(result.Data, []byte(`"changed":false`)) {
					t.Fatal("no-op reported changed")
				}
				if calls > 1 {
					t.Fatal("replayed mutation")
				}
			})
		}
	}
}
func TestCLIV2AccountMutationHelpAndValidation(t *testing.T) {
	for _, id := range []string{"claude", "codex"} {
		for _, action := range []string{"login", "activate", "remove"} {
			path := id + " account " + action
			for _, args := range [][]string{{"--help"}, {"--unknown"}, {"--timeout", "0s"}, {"--timeout", "1s", "--timeout", "2s"}} {
				t.Run(path+strings.Join(args, " "), func(t *testing.T) {
					var out, errOut bytes.Buffer
					exit := cli.Run(context.Background(), append(strings.Fields(path), args...), &cli.Session{Out: &out, Err: &errOut}, func(string) (cli.Handler, bool) {
						t.Fatal("presentation/validation accessed dependencies")
						return nil, false
					})
					want := 2
					if args[0] == "--help" {
						want = 0
						help, _ := cli.Help(path)
						if out.String() != help {
							t.Fatal("help differs")
						}
					}
					if exit != want {
						t.Fatalf("exit=%d want=%d", exit, want)
					}
				})
			}
		}
	}
}
func TestCLIV2AccountMutationConsentPausesOnlyWorkBudget(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	selected := v2AccountSelection{Account: v2AccountSummary(provider.Claude, "id", "user@example.com", "", ""), Removable: true}
	selected.Account.AccountReference = v2AccountString("user@example.com")
	selected.Remove = func(ctx context.Context) (v2AccountRemoval, error) {
		if ctx.Err() != nil {
			t.Fatal("consent spent command budget")
		}
		return v2AccountRemoval{Changed: true}, nil
	}
	inv := mutationInvocation("claude account remove", "user@example.com")
	inv.Options["timeout"] = []string{"20ms"}
	done := make(chan struct{})
	go func() { defer close(done); time.Sleep(40 * time.Millisecond); _, _ = io.WriteString(writer, "yes\n") }()
	result := handleV2AccountMutationWithDependencies(context.Background(), inv, &cli.Session{In: reader, Err: io.Discard, Interactive: true}, v2AccountMutationDependencies{Select: func(context.Context, provider.ID, string) (v2AccountSelection, error) { return selected, nil }})
	<-done
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d errors=%v", result.ExitCode, result.Errors)
	}
}

func TestCLIV2AccountMutationParentDeadlineDuringConsent(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	row := v2AccountSummary(provider.Claude, "id", "user@example.com", "", "")
	row.AccountReference = v2AccountString("user@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(writer, "yes\n")
	}()
	started := time.Now()
	result := handleV2AccountMutationWithDependencies(ctx, mutationInvocation("claude account remove", "user@example.com"), &cli.Session{In: reader, Err: io.Discard, Interactive: true}, v2AccountMutationDependencies{Select: func(context.Context, provider.ID, string) (v2AccountSelection, error) {
		return v2AccountSelection{Account: row, Removable: true, Remove: func(context.Context) (v2AccountRemoval, error) {
			t.Error("expired consent mutated")
			return v2AccountRemoval{}, nil
		}}, nil
	}})
	elapsed := time.Since(started)
	reader.Close()
	<-released
	if result.ExitCode != 7 || elapsed > 50*time.Millisecond {
		t.Fatalf("parent deadline not bounded: exit=%d elapsed=%v", result.ExitCode, elapsed)
	}
}

type v2FailedMutationOutput struct{}

func (v2FailedMutationOutput) Write(p []byte) (int, error) { return 0, errors.New("output failed") }
func TestCLIV2AccountMutationOutputFailureNeverReplays(t *testing.T) {
	fixture := v2Fixtures["login-activation-fails"](t)
	exit := cli.Run(context.Background(), []string{"codex", "account", "login", "--json"}, &cli.Session{Out: v2FailedMutationOutput{}, Err: io.Discard}, fixture.Lookup)
	if exit != 1 || fixture.calls["save"] != 1 || fixture.calls["oauth"] != 1 {
		t.Fatal("output failure replayed account mutation")
	}
}

func (a *v2MutationAdmin) List(ctx context.Context) (codexprov.Inventory, error) {
	return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "exact-key", Identity: codexprov.AccountIdentity{AccountID: "account", UserID: "user", Email: "user@example.com"}, Candidates: []codexprov.CredentialCandidate{{Source: codexprov.SourceManaged}}}}}, ctx.Err()
}

func TestCLIV2AccountMutationLoginUnknownFactsRemainNull(t *testing.T) {
	for _, mode := range []string{"postcheck", "activated-postcheck", "unknown", "both"} {
		t.Run(mode, func(t *testing.T) {
			result := app.AccountLoginResult{CredentialsSaved: true, Activated: mode == "activated-postcheck", ActivationKnown: mode != "unknown"}
			failure := error(app.ErrAccountLoginPostcheck)
			wantExit, wantCode := 8, "account_login_postcheck_partial"
			if mode == "unknown" {
				failure = errors.Join(app.ErrAccountActivation, &codexprov.MutationOutcomeUnknown{Err: context.DeadlineExceeded})
				wantExit, wantCode = 7, "account_timeout"
			}
			if mode == "both" {
				failure = errors.Join(app.ErrAccountActivation, app.ErrAccountLoginPostcheck)
				wantCode = "account_login_partial"
			}
			outcome := handleV2AccountMutationWithDependencies(context.Background(), mutationInvocation("codex account login", "--activate"), &cli.Session{}, v2AccountMutationDependencies{Login: func(context.Context, provider.ID, bool) (app.AccountLoginResult, error) { return result, failure }})
			if outcome.ExitCode != wantExit || len(outcome.Errors) == 0 || outcome.Errors[0].Code != wantCode {
				t.Fatalf("outcome=%+v", outcome)
			}
			if !bytes.Contains(outcome.Data, []byte(`"account":null`)) || !bytes.Contains(outcome.Data, []byte(`"credentials_saved":true`)) {
				t.Fatal("unknown account state fabricated")
			}
			if mode == "unknown" && (!bytes.Contains(outcome.Data, []byte(`"activated":null`)) || !strings.Contains(outcome.Human, "Native client default: unknown.")) {
				t.Fatal("unknown activation became false")
			}
		})
	}
}

func TestCLIV2AccountMutationLoginStableAmbiguousClaude(t *testing.T) {
	account := keyring.ClaudeOAuth{AccountUUID: "uuid", Email: "user@example.com", AccessToken: "secret"}
	other := account
	other.AccountUUID = "other"
	accounts := &claudeprov.Accounts{Mutations: &claudeprov.AccountMutationOperations{
		Store: func(context.Context, *keyring.ClaudeOAuth) error { return nil },
		Inspect: func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
			return []keyring.ClaudeAccountInspection{{Account: account, Sources: []string{"cq_managed"}}, {Account: other}}, nil
		},
	}}
	outcome := handleV2AccountMutationWithDependencies(context.Background(), mutationInvocation("claude account login"), &cli.Session{}, v2AccountMutationDependencies{Login: func(ctx context.Context, _ provider.ID, activate bool) (app.AccountLoginResult, error) {
		return app.LoginClaude(ctx, nil, activate, func(context.Context, httputil.Doer) (*auth.TokenResponse, *auth.Profile, error) {
			return &auth.TokenResponse{AccessToken: "secret"}, &auth.Profile{AccountUUID: "uuid", Email: "user@example.com"}, nil
		}, accounts)
	}})
	if outcome.ExitCode != 0 || !bytes.Contains(outcome.Data, []byte(`"account_reference":null`)) || !bytes.Contains(outcome.Data, []byte(`"stable":true`)) {
		t.Fatalf("stable UUID lost with ambiguous selector: %s", outcome.Data)
	}
}

func TestCLIV2AccountMutationLoginAliases(t *testing.T) {
	fs := fsutil.NewMemFS()
	home, err := fs.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "accounts", "registry.json")
	for _, malformed := range []bool{false, true} {
		content := `{"schema_version":3,"accounts":[{"account_key":"exact-key","alias":"Work"},{"account_key":"exact-key","alias":"alpha"}]}`
		if malformed {
			content = "{"
		}
		if err := fs.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		result := app.AccountLoginResult{Reference: "exact-key", CredentialsSaved: true, AccountObserved: true, ActivationKnown: true, Activated: true}
		err := v2LoginAliases(context.Background(), &result, &codexprov.Accounts{FS: fs})
		if malformed {
			if !errors.Is(err, app.ErrAccountLoginPostcheck) || result.AccountObserved || !result.CredentialsSaved || !result.Activated {
				t.Fatal("alias failure lost saved result or fabricated complete account")
			}
		} else if err != nil || strings.Join(result.Aliases, ",") != "alpha,Work" {
			t.Fatalf("aliases=%v error=%v", result.Aliases, err)
		}
	}
}

func TestCLIV2AccountMutationAuthorityUnavailable(t *testing.T) {
	result := v2MutationError(codexprov.ErrCredentialAuthorityUnavailable, "account_io_failed")
	if result.ExitCode != 4 || result.Errors[0].Code != "account_inventory_unavailable" {
		t.Fatalf("result=%+v", result)
	}
}
