package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func init() {
	registerV2Fixture("accounts-unavailable", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		deps := v2AccountDependencies{Codex: func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
			f.Call("codex-inventory")
			return codexprov.Inventory{}, codexprov.AccountAliasIndex{}, errors.New("unavailable")
		}}
		f.Lookup = func(path string) (cli.Handler, bool) {
			if path != "codex account list" {
				return nil, false
			}
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2AccountInspectionWithDependencies(ctx, inv, s, deps)
			}, true
		}
		return f
	})
}

func TestCLIV2AccountInspectionContract(t *testing.T) {
	runV2Case(t, v2Case{
		Name: "inventory failure remains unavailable", Scenario: "accounts-unavailable",
		Args: []string{"codex", "account", "list", "--json"}, Exit: 4,
		Command: "codex account list", Code: "account_inventory_unavailable",
		Forbid: []string{"filesystem-write", "service", "consume"},
	})
}

type v2InspectionInventory struct {
	inventory codexprov.Inventory
	fixture   *v2Fixture
}

func (s v2InspectionInventory) List(context.Context) (codexprov.Inventory, error) {
	s.fixture.Call("codex-inventory")
	return s.inventory, nil
}

func init() {
	for _, scenario := range []string{"accounts-readonly", "accounts-empty"} {
		scenario := scenario
		registerV2Fixture(scenario, func(t *testing.T) *v2Fixture {
			f := &v2Fixture{Secrets: []string{"credential-secret-never-output"}}
			empty := scenario == "accounts-empty"
			inventory := codexprov.Inventory{}
			if !empty {
				inventory.Accounts = []codexprov.LogicalAccount{
					{Key: "key-z", Identity: codexprov.AccountIdentity{AccountID: "id-z", Email: "same@example.com", PlanType: "pro"}, Candidates: []codexprov.CredentialCandidate{{Source: codexprov.SourceExternal}}},
					{Key: "key-a", Identity: codexprov.AccountIdentity{AccountID: "id-a", Email: "same@example.com"}, Active: true, Candidates: []codexprov.CredentialCandidate{{Source: codexprov.SourceSystem, Credential: codexprov.CodexAccount{AccessToken: f.Secrets[0]}}, {Source: codexprov.SourceManaged}}},
					{Key: "unstable", Unstable: true},
				}
			}
			accounts := &codexprov.Accounts{Inventory: v2InspectionInventory{inventory: inventory, fixture: f}}
			deps := v2AccountDependencies{
				Codex: func(ctx context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
					f.Call("codex-state-read")
					inv, err := accounts.Inspect(ctx)
					return inv, codexprov.AccountAliasIndex{}, err
				},
				Claude: func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
					f.Call("claude-inventory")
					if empty {
						return nil, nil
					}
					return []keyring.ClaudeAccountInspection{
						{Account: keyring.ClaudeOAuth{AccountUUID: "b", Email: "same@example.com", AccessToken: f.Secrets[0]}, Sources: []string{"cq_managed"}},
						{Account: keyring.ClaudeOAuth{AccountUUID: "a", Email: " SAME@example.com "}, Sources: []string{"native_client"}, Active: true},
						{Account: keyring.ClaudeOAuth{Email: "unique@example.com"}, Sources: []string{"platform_keychain"}},
					}, nil
				},
				Gemini: func(context.Context) ([]provider.Account, error) {
					f.Call("gemini-inventory")
					if empty {
						return nil, nil
					}
					return []provider.Account{{AccountID: "antigravity", Label: "Antigravity CLI", Active: true}}, nil
				},
			}
			f.Lookup = func(path string) (cli.Handler, bool) {
				_, ok := lookupV2AccountInspection(path)
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					return handleV2AccountInspectionWithDependencies(ctx, inv, s, deps)
				}, ok
			}
			return f
		})
	}
}

func TestCLIV2AccountInspectionLeaves(t *testing.T) {
	for _, id := range []string{"claude", "codex", "gemini"} {
		leaf := "list"
		if id == "gemini" {
			leaf = "show"
		}
		command := id + " account " + leaf
		forbidden := []string{"filesystem-write", "network", "service", "refresh", "consume", "recovery"}
		if id != "codex" {
			forbidden = append(forbidden, "codex-state-read")
		}
		for _, other := range []string{"claude", "codex", "gemini"} {
			if other != id {
				forbidden = append(forbidden, other+"-inventory")
			}
		}
		want := `{"accounts":[]}`
		if id == "gemini" {
			want = `{"configured":false,"account":null}`
		}
		runV2Case(t, v2Case{Name: id + " empty", Scenario: "accounts-empty", Args: []string{id, "account", leaf, "--json"}, Command: command, WantJSON: want, Forbid: forbidden, Calls: map[string]int{id + "-inventory": 1}})
		switch id {
		case "codex":
			want = `{"accounts":[{"account_reference":"key-a","account_id":"id-a","email":"same@example.com","label":null,"rate_limit_tier":null,"active":true,"sources":["cq_managed","native_client"],"aliases":[],"stable":true},{"account_reference":"key-z","active":false,"sources":["external"],"stable":true},{"account_reference":null,"account_id":null,"email":null,"label":null,"rate_limit_tier":null,"display_name":"Unknown account","active":false,"sources":[],"aliases":[],"stable":false}]}`
		case "claude":
			want = `{"accounts":[{"account_reference":"unique@example.com","sources":["platform_keychain"],"stable":true},{"account_reference":null,"account_id":"a","active":true},{"account_reference":null,"account_id":"b","active":false}]}`
		case "gemini":
			want = `{"configured":true,"account":{"provider":"gemini","account_reference":null,"account_id":"antigravity","email":null,"display_name":"Antigravity CLI","label":null,"rate_limit_tier":null,"active":true,"sources":["external"],"aliases":[],"stable":true}}`
		}
		runV2Case(t, v2Case{Name: id + " readonly", Scenario: "accounts-readonly", Args: []string{id, "account", leaf, "--json"}, Command: command, WantJSON: want, Forbid: forbidden, Calls: map[string]int{id + "-inventory": 1}})
	}
}

func TestCLIV2AccountInspectionParserAndHelp(t *testing.T) {
	for _, path := range []string{"claude account list", "codex account list", "gemini account show"} {
		for _, suffix := range [][]string{{"--help"}, {"--json", "--help"}, {"--unknown"}, {"--timeout", "1s", "--timeout", "2s"}, {"extra"}, {"--timeout", "999ms"}, {"--timeout", "10m1s"}} {
			t.Run(path+strings.Join(suffix, " "), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				calls := 0
				exit := cli.Run(context.Background(), append(strings.Fields(path), suffix...), &cli.Session{Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) { calls++; t.Fatal("parser accessed inventory"); return nil, false })
				want := 2
				if slices.Contains(suffix, "--help") {
					want = 0
					help, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "help", strings.ReplaceAll(path, " ", "-")+".txt"))
					if err != nil {
						t.Fatal(err)
					}
					if stdout.String() != string(help) {
						t.Fatal("help differs from exact contract")
					}
				}
				if exit != want || calls != 0 {
					t.Fatalf("exit=%d want=%d calls=%d", exit, want, calls)
				}
			})
		}
	}
}

func TestCLIV2AccountInspectionHumanAndSchema(t *testing.T) {
	for _, path := range []string{"claude account list", "codex account list", "gemini account show"} {
		t.Run(path, func(t *testing.T) {
			fixture := v2Fixtures["accounts-empty"](t)
			var out, errOut bytes.Buffer
			exit := cli.Run(context.Background(), strings.Fields(path), &cli.Session{Out: &out, Err: &errOut}, fixture.Lookup)
			want := "Accounts: 0\n"
			if path == "gemini account show" {
				want = "Gemini configured: false.\nAccount: none.\nManaged by: Antigravity.\n"
			}
			if exit != 0 || out.String() != want || errOut.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, out.String(), errOut.String())
			}
		})
	}
	row := v2AccountSummary(provider.Claude, "", "evil\x1b[31m\nname", "", "")
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 11 {
		t.Fatalf("DTO fields=%d want=11", len(fields))
	}
	inv := cli.Invocation{Path: "claude account list", Options: map[string][]string{"timeout": {"1s"}}}
	result := handleV2AccountInspectionWithDependencies(context.Background(), inv, nil, v2AccountDependencies{Claude: func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
		return []keyring.ClaudeAccountInspection{{Account: keyring.ClaudeOAuth{Email: *row.Email}}}, nil
	}})
	if strings.ContainsRune(result.Human, '\x1b') || !strings.Contains(result.Human, `evil\u001b[31m\u000aname`) {
		t.Fatalf("human controls not escaped: %q", result.Human)
	}
}

func TestCLIV2AccountInspectionDeadlineAndErrors(t *testing.T) {
	for _, path := range []string{"claude account list", "codex account list", "gemini account show"} {
		for _, mode := range []string{"timeout", "cancel", "error", "panic"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "cancel" {
					cancel()
				}
				read := func(ctx context.Context) error {
					switch mode {
					case "timeout":
						<-ctx.Done()
						return ctx.Err()
					case "panic":
						panic("private details")
					default:
						return errors.New("private details")
					}
				}
				deps := v2AccountDependencies{
					Codex: func(ctx context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
						return codexprov.Inventory{}, codexprov.AccountAliasIndex{}, read(ctx)
					},
					Claude: func(ctx context.Context) ([]keyring.ClaudeAccountInspection, error) { return nil, read(ctx) },
					Gemini: func(ctx context.Context) ([]provider.Account, error) { return nil, read(ctx) },
				}
				inv := cli.Invocation{Path: path, Options: map[string][]string{"timeout": {"1ms"}}}
				result := handleV2AccountInspectionWithDependencies(ctx, inv, nil, deps)
				want := 4
				code := "account_inventory_unavailable"
				if path == "gemini account show" {
					code = "gemini_account_unavailable"
				}
				if mode == "timeout" {
					want = 7
					code = "account_timeout"
					if path == "gemini account show" {
						code = "gemini_account_timeout"
					}
				}
				if mode == "cancel" {
					want = 130
					code = "interrupted"
				}
				if result.ExitCode != want || len(result.Errors) != 1 || result.Errors[0].Code != code {
					t.Fatalf("result=%+v", result)
				}
				if strings.Contains(result.Errors[0].Message, "private") {
					t.Fatal("source error leaked")
				}
			})
		}
	}
}

func TestCLIV2AccountInspectionExactHumanAndAliases(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{"codex account list", "Accounts: 3\nkey-a\tsame@example.com\tactive=true\tcq_managed,native_client\nkey-z\tsame@example.com\tactive=false\texternal\n—\tUnknown account\tactive=false\t\n"},
		{"claude account list", "Accounts: 3\nunique@example.com\tunique@example.com\tactive=false\tplatform_keychain\n—\t SAME@example.com \tactive=true\tnative_client\n—\tsame@example.com\tactive=false\tcq_managed\n"},
		{"gemini account show", "Gemini configured: true.\nAccount: Antigravity CLI.\nManaged by: Antigravity.\n"},
	} {
		t.Run(test.path, func(t *testing.T) {
			f := v2Fixtures["accounts-readonly"](t)
			var stdout, stderr bytes.Buffer
			exit := cli.Run(context.Background(), strings.Fields(test.path), &cli.Session{Out: &stdout, Err: &stderr}, f.Lookup)
			for _, secret := range f.Secrets {
				if v2OutputsContainSecret(stdout.Bytes(), stderr.Bytes(), secret) {
					t.Fatal("credential leaked")
				}
			}
			if exit != 0 || stdout.String() != test.want || stderr.Len() != 0 {
				t.Fatalf("exit=%d output=%q stderr=%q", exit, stdout.String(), stderr.String())
			}
			id := strings.Fields(test.path)[0]
			runV2Case(t, v2Case{Name: "legacy canonical envelope", Scenario: "accounts-empty", Args: []string{id, "accounts", "--json"}, Command: test.path})
		})
	}
}

func TestCLIV2AccountInspectionBoundsUninterruptibleReader(t *testing.T) {
	release, finished := make(chan struct{}), make(chan struct{})
	deps := v2AccountDependencies{Gemini: func(context.Context) ([]provider.Account, error) { defer close(finished); <-release; return nil, nil }}
	result := handleV2AccountInspectionWithDependencies(context.Background(), cli.Invocation{Path: "gemini account show", Options: map[string][]string{"timeout": {"1ms"}}}, nil, deps)
	close(release)
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d wanted timeout", result.ExitCode)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("reader did not finish after release")
	}
}

func TestCLIV2AccountInspectionMergedClaudeReference(t *testing.T) {
	// This is the domain result covered by the three-source refreshed-identity
	// bridge regression in keyring. Its sole logical identity must stay selectable.
	result := handleV2AccountInspectionWithDependencies(context.Background(), cli.Invocation{
		Path: "claude account list", Options: map[string][]string{"timeout": {"1s"}},
	}, nil, v2AccountDependencies{Claude: func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
		return []keyring.ClaudeAccountInspection{{
			Account: keyring.ClaudeOAuth{AccountUUID: "a", Email: "user@example.com", SubscriptionType: "fresh-plan", RateLimitTier: "fresh-tier"},
			Sources: []string{"cq_managed", "native_client", "platform_keychain"}, Active: true,
		}}, nil
	}})
	var data struct {
		Accounts []AccountSummary `json:"accounts"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || len(data.Accounts) != 1 {
		t.Fatalf("exit=%d rows=%d", result.ExitCode, len(data.Accounts))
	}
	row := data.Accounts[0]
	if row.AccountReference == nil || *row.AccountReference != "user@example.com" || !row.Stable || !row.Active {
		t.Fatal("unique merged Claude identity lost its valid selector")
	}
	if row.Label == nil || *row.Label != "fresh-plan" || row.RateLimitTier == nil || *row.RateLimitTier != "fresh-tier" || len(row.Sources) != 3 {
		t.Fatal("fresh metadata or merged provenance lost in CLI projection")
	}
}
