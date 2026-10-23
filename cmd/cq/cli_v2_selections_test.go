package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/keyring"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func init() {
	registerV2Fixture("pin-ambiguous", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		var cfg proxy.Config
		if err := json.Unmarshal([]byte(`{"future_field":{"keep":true}}`), &cfg); err != nil {
			t.Fatal(err)
		}
		deps := v2SelectionDependencies{
			LoadConfig: func() (*proxy.Config, error) { f.Call("config-read"); return &cfg, nil },
			SaveConfig: func(*proxy.Config) error { f.Call("filesystem-write"); return nil },
			Codex: func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
				f.Call("inventory")
				return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
					{Key: "account-a", Identity: codexprov.AccountIdentity{Email: "alice@example.com"}},
					{Key: "account-b", Identity: codexprov.AccountIdentity{Email: "alice@example.com"}},
				}}, codexprov.AccountAliasIndex{}, nil
			},
		}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Selection(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2SelectionWithDependencies(ctx, inv, s, deps)
			}, ok
		}
		return f
	})
	registerV2Fixture("selection-configured", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		cfg := &proxy.Config{PinnedClaudeAccount: "known@example.com", CodexRoutingPinnedAccountKey: "account-a", CodexRoutingDefaultAccountKey: "account-b", CodexWindowPriming: proxy.CodexWindowPrimingConfig{ModelOverrides: map[string]string{"5h": "model-a"}}}
		deps := v2SelectionDependencies{
			LoadConfig: func() (*proxy.Config, error) { f.Call("config-read"); return cfg, nil },
			SaveConfig: func(*proxy.Config) error { f.Call("filesystem-write"); return nil },
			Codex: func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
				f.Call("inventory")
				return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "account-a", Identity: codexprov.AccountIdentity{Email: "alice@example.com"}}, {Key: "account-b", Identity: codexprov.AccountIdentity{Email: "bob@example.com"}}}}, codexprov.AccountAliasIndex{}, nil
			},
			Claude: func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
				f.Call("inventory")
				return []keyring.ClaudeAccountInspection{{Account: keyring.ClaudeOAuth{AccountUUID: "550e8400-e29b-41d4-a716-446655440000", Email: "known@example.com"}}}, nil
			},
		}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Selection(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2SelectionWithDependencies(ctx, inv, s, deps)
			}, ok
		}
		return f
	})
	registerV2Fixture("selection-no-config", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Selection(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2SelectionWithDependencies(ctx, inv, s, v2SelectionDependencies{
					LoadConfig: func() (*proxy.Config, error) { f.Call("config-read"); return nil, os.ErrNotExist },
					SaveConfig: func(*proxy.Config) error { f.Call("filesystem-write"); return nil },
				})
			}, ok
		}
		return f
	})
	registerV2Fixture("prime-enable", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { f.Call("prime-request") }))
		t.Cleanup(upstream.Close)
		cfg := &proxy.Config{CodexUpstream: upstream.URL, CodexWindowPriming: proxy.CodexWindowPrimingConfig{Enabled: false, ModelOverrides: map[string]string{"5h": "model-a"}}}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Selection(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2SelectionWithDependencies(ctx, inv, s, v2SelectionDependencies{
					LoadConfig: func() (*proxy.Config, error) { f.Call("config-read"); return cfg, nil },
					SaveConfig: func(*proxy.Config) error { f.Call("filesystem-write"); return nil },
				})
			}, ok
		}
		return f
	})
}

func TestCLIV2SelectionContract(t *testing.T) {
	runV2Case(t, v2Case{
		Name:     "ambiguous routing target does not write",
		Scenario: "pin-ambiguous",
		Args:     []string{"codex", "proxy", "pin", "set", "alice@example.com", "--json"},
		Exit:     6,
		Command:  "codex proxy pin set",
		Code:     "routing_account_ambiguous",
		Forbid:   []string{"filesystem-write", "credential-activate", "service", "consume"},
	})
}

func TestCLIV2SelectionAllLeaves(t *testing.T) {
	for _, tc := range []struct {
		path, account, data string
		writes, inventory   int
	}{
		{"claude proxy pin show", "", `{"provider":"claude","kind":"pin","configured":true,"account":"known@example.com","application":"configured_only","restart_required":false}`, 0, 0},
		{"claude proxy pin set", "known@example.com", `{"provider":"claude","kind":"pin","account":"known@example.com","application":"hot_reload","restart_required":false}`, 1, 1},
		{"claude proxy pin clear", "", `{"provider":"claude","kind":"pin","configured":false,"account":null,"application":"hot_reload","restart_required":false}`, 1, 0},
		{"codex proxy pin show", "", `{"provider":"codex","kind":"pin","account":"account-a","application":"configured_only","restart_required":false}`, 0, 0},
		{"codex proxy pin set", "alice@example.com", `{"provider":"codex","kind":"pin","account":"account-a","application":"configured_only","restart_required":true}`, 1, 1},
		{"codex proxy pin clear", "", `{"provider":"codex","kind":"pin","configured":false,"account":null,"application":"configured_only","restart_required":true}`, 1, 0},
		{"codex proxy fallback show", "", `{"provider":"codex","kind":"fallback","account":"account-b","application":"configured_only","restart_required":false}`, 0, 0},
		{"codex proxy fallback set", "alice@example.com", `{"provider":"codex","kind":"fallback","account":"account-a","application":"configured_only","restart_required":true}`, 1, 1},
		{"codex proxy fallback clear", "", `{"provider":"codex","kind":"fallback","configured":false,"account":null,"application":"configured_only","restart_required":true}`, 1, 0},
		{"codex proxy prime status", "", `{"enabled":false,"model_overrides":{"5h":"model-a"},"restart_required":false}`, 0, 0},
		{"codex proxy prime enable", "", `{"enabled":true,"model_overrides":{"5h":"model-a"},"restart_required":true}`, 1, 0},
		{"codex proxy prime disable", "", `{"enabled":false,"model_overrides":{"5h":"model-a"},"restart_required":true}`, 1, 0},
	} {
		args := strings.Fields(tc.path)
		if tc.account != "" {
			args = append(args, tc.account)
		}
		args = append(args, "--json")
		runV2Case(t, v2Case{Name: tc.path, Scenario: "selection-configured", Args: args, Command: tc.path, WantJSON: tc.data,
			Calls:  map[string]int{"config-read": 1, "filesystem-write": tc.writes, "inventory": tc.inventory},
			Forbid: []string{"credential-activate", "service", "consume", "prime-request"}})
	}
}

func TestCLIV2SelectionAliasesAndPreconditions(t *testing.T) {
	runV2Case(t, v2Case{Name: "enable does not prime immediately", Scenario: "prime-enable", Args: []string{"codex", "proxy", "prime", "enable", "--json"}, Command: "codex proxy prime enable", WantJSON: `{"enabled":true,"model_overrides":{"5h":"model-a"},"restart_required":true}`, Calls: map[string]int{"filesystem-write": 1}, Forbid: []string{"prime-request", "service", "consume"}})
	for _, tc := range []struct {
		name              string
		args              []string
		command, data     string
		writes, inventory int
	}{
		{"legacy Claude UUID", []string{"proxy", "pin", "claude", "550e8400-e29b-41d4-a716-446655440000", "--json"}, "claude proxy pin set", `{"account":"known@example.com"}`, 1, 1},
		{"legacy Codex pin", []string{"proxy", "pin", "codex", "account-a", "--json"}, "codex proxy pin set", `{"account":"account-a"}`, 1, 1},
		{"legacy fallback clear", []string{"proxy", "default", "codex", "--clear", "--json"}, "codex proxy fallback clear", `{"account":null}`, 1, 0},
		{"legacy prime status", []string{"proxy", "prime", "status", "--json"}, "codex proxy prime status", `{"enabled":false}`, 0, 0},
	} {
		runV2Case(t, v2Case{Name: tc.name, Scenario: "selection-configured", Args: tc.args, Command: tc.command, WantJSON: tc.data, Calls: map[string]int{"filesystem-write": tc.writes, "inventory": tc.inventory}})
	}
	for _, path := range []string{"claude proxy pin show", "claude proxy pin set", "claude proxy pin clear", "codex proxy pin show", "codex proxy pin set", "codex proxy pin clear", "codex proxy fallback show", "codex proxy fallback set", "codex proxy fallback clear", "codex proxy prime status", "codex proxy prime enable", "codex proxy prime disable"} {
		args := strings.Fields(path)
		if strings.HasSuffix(path, " set") {
			if strings.HasPrefix(path, "claude") {
				args = append(args, "known@example.com")
			} else {
				args = append(args, "account-a")
			}
		}
		args = append(args, "--json")
		runV2Case(t, v2Case{Name: "missing config " + path, Scenario: "selection-no-config", Args: args, Exit: 1, Command: path, Code: "routing_io_failed", Forbid: []string{"filesystem-write", "inventory", "service", "consume"}})
	}
	for _, path := range []string{"claude proxy pin set", "codex proxy pin set", "codex proxy fallback set"} {
		args := append(strings.Fields(path), "nobody@example.com", "--json")
		runV2Case(t, v2Case{Name: "missing account " + path, Scenario: "selection-configured", Args: args, Exit: 3, Command: path, Code: "routing_account_not_found", Forbid: []string{"filesystem-write"}})
	}
	runV2Case(t, v2Case{Name: "canonical Claude UUID rejected", Scenario: "no-access", Args: []string{"claude", "proxy", "pin", "set", "550e8400-e29b-41d4-a716-446655440000", "--json"}, Exit: 2, Command: "claude proxy pin set", Code: "routing_invalid_argument", Forbid: []string{"lookup"}})
	runV2Case(t, v2Case{Name: "legacy pin arity rejected", Scenario: "no-access", Args: []string{"proxy", "pin", "codex", "account-a", "extra", "--json"}, Exit: 2, Command: "codex proxy pin set", Code: "routing_invalid_argument", Forbid: []string{"lookup"}})
}

func TestCLIV2SelectionFutureFieldsAndFailures(t *testing.T) {
	var cfg proxy.Config
	if err := json.Unmarshal([]byte(`{"port":19280,"local_token":"synthetic-secret","future_field":{"keep":true},"codex_routing_default_account_key":"account-b"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	saves := 0
	deps := v2SelectionDependencies{LoadConfig: func() (*proxy.Config, error) { return &cfg, nil }, SaveConfig: func(*proxy.Config) error { saves++; return nil }}
	inv, err := cli.Parse([]string{"codex", "proxy", "fallback", "clear", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	out := handleV2SelectionWithDependencies(context.Background(), inv, nil, deps)
	if out.ExitCode != 0 || saves != 1 {
		t.Fatalf("clear outcome=%+v saves=%d", out, saves)
	}
	encoded, marshalErr := json.Marshal(&cfg)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !strings.Contains(string(encoded), `"future_field":{"keep":true}`) {
		t.Fatal("unknown config field lost")
	}
	deps.SaveConfig = func(*proxy.Config) error { return errors.New("private-path") }
	out = handleV2SelectionWithDependencies(context.Background(), inv, nil, deps)
	if out.ExitCode != 1 || out.Errors[0].Code != "routing_io_failed" || strings.Contains(out.Errors[0].Message, "private-path") {
		t.Fatalf("save failure=%+v", out)
	}
}

func TestCLIV2SelectionCapturedConfigPaths(t *testing.T) {
	base := t.TempDir()
	paths := proxy.DefaultPaths{ConfigFile: filepath.Join(base, "captured", "proxy.json"), RescueBootstrap: filepath.Join(base, "captured-state", "rescue.json")}
	if _, err := proxy.LoadExistingConfigAt(paths); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config error = %v", err)
	}
	if _, err := os.Stat(paths.ConfigFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("load created config")
	}
	if err := os.MkdirAll(filepath.Dir(paths.RescueBootstrap), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &proxy.Config{Port: proxy.DefaultPort, LocalToken: "synthetic-secret", ClaudeUpstream: proxy.DefaultUpstream, CodexUpstream: proxy.DefaultCodexUpstream}
	if err := proxy.SaveConfigAt(paths, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "changed-environment"))
	if _, err := proxy.LoadExistingConfigAt(paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "changed-environment", "cq", "proxy.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("captured save escaped to changed environment")
	}
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["future_field"] = json.RawMessage(`{"keep":true}`)
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := proxy.LoadExistingConfigAt(paths)
	if err != nil {
		t.Fatal(err)
	}
	loaded.PinnedClaudeAccount = "known@example.com"
	if err := proxy.SaveConfigAt(paths, loaded); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"future_field"`) || !strings.Contains(string(data), `"known@example.com"`) {
		t.Fatal("save lost existing or future configuration")
	}
}

func TestCLIV2SelectionSaveCanCommitBeforeBootstrapError(t *testing.T) {
	base := t.TempDir()
	paths := proxy.DefaultPaths{ConfigFile: filepath.Join(base, "config", "proxy.json"), RescueBootstrap: filepath.Join(base, "absent-state", "rescue.json")}
	if err := os.MkdirAll(paths.RescueBootstrap, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &proxy.Config{Port: proxy.DefaultPort, LocalToken: "synthetic-secret", ClaudeUpstream: proxy.DefaultUpstream, CodexUpstream: proxy.DefaultCodexUpstream, ProxyResilienceStateDir: filepath.Join(base, "resilience")}
	if err := proxy.SaveConfigAt(paths, cfg); err == nil {
		t.Fatal("expected bootstrap save failure")
	}
	if _, err := proxy.LoadExistingConfigAt(paths); err != nil {
		t.Fatalf("config should have committed before bootstrap failure: %v", err)
	}
}

func TestCLIV2SelectionExactHumanAndTimeout(t *testing.T) {
	inv, parseErr := cli.Parse([]string{"claude", "proxy", "pin", "clear"})
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	cfg := &proxy.Config{}
	out := handleV2SelectionWithDependencies(context.Background(), inv, nil, v2SelectionDependencies{
		LoadConfig: func() (*proxy.Config, error) { return cfg, nil }, SaveConfig: func(*proxy.Config) error { return nil },
	})
	if want := "claude proxy pin: not configured\nApplication: hot_reload\nRestart required: false\n"; out.Human != want {
		t.Fatalf("human=%q want=%q", out.Human, want)
	}
	inv, parseErr = cli.Parse([]string{"codex", "proxy", "prime", "enable"})
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	out = handleV2SelectionWithDependencies(context.Background(), inv, nil, v2SelectionDependencies{
		LoadConfig: func() (*proxy.Config, error) { return cfg, nil }, SaveConfig: func(*proxy.Config) error { return nil },
	})
	if want := "Codex window priming: enabled\nModel overrides: 0\nRestart required: true\n"; out.Human != want {
		t.Fatalf("human=%q want=%q", out.Human, want)
	}
	inv, parseErr = cli.Parse([]string{"codex", "proxy", "pin", "set", "account-a", "--timeout", "1ms"})
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	beforeSave := false
	out = handleV2SelectionWithDependencies(context.Background(), inv, nil, v2SelectionDependencies{
		LoadConfig: func() (*proxy.Config, error) { return cfg, nil },
		SaveConfig: func(*proxy.Config) error { beforeSave = true; return nil },
		Codex: func(ctx context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
			<-ctx.Done()
			return codexprov.Inventory{}, codexprov.AccountAliasIndex{}, ctx.Err()
		},
	})
	if out.ExitCode != 7 || out.Errors[0].Code != "routing_timeout" || beforeSave {
		t.Fatalf("timeout outcome=%+v saved=%t", out, beforeSave)
	}
}

func TestCLIV2SelectionPostSaveReceipt(t *testing.T) {
	for _, action := range []string{"set", "clear"} {
		for _, stop := range []string{"timeout", "interrupted"} {
			t.Run(action+"/"+stop, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				if stop == "timeout" {
					ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				}
				defer cancel()
				cfg := &proxy.Config{PinnedClaudeAccount: "old@example.test"}
				var saved string
				deps := v2SelectionDependencies{
					LoadConfig: func() (*proxy.Config, error) { return cfg, nil },
					SaveConfig: func(savedCfg *proxy.Config) error {
						saved = savedCfg.PinnedClaudeAccount
						if stop == "timeout" {
							<-ctx.Done()
						} else {
							cancel()
						}
						return nil
					},
					Claude: func(context.Context) ([]keyring.ClaudeAccountInspection, error) {
						return []keyring.ClaudeAccountInspection{{Account: keyring.ClaudeOAuth{Email: "alex@example.test"}}}, nil
					},
				}
				args := []string{"claude", "proxy", "pin", action, "--timeout", "1s"}
				wantAccount := "none"
				if action == "set" {
					args = append(args, "alex@example.test")
					wantAccount = "alex@example.test"
				}
				var stdout, stderr bytes.Buffer
				o := terminalFixtureOptions(100)
				var outcome cli.Outcome
				exit := cli.Run(ctx, args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, func(string) (cli.Handler, bool) {
					return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
						outcome = handleV2SelectionWithDependencies(ctx, inv, session, deps)
						return outcome
					}, true
				})
				wantExit := 7
				wantCode := "routing_timeout"
				wantMarker := "Warning · Routing operation timed out"
				if stop == "interrupted" {
					wantExit, wantCode, wantMarker = 130, "interrupted", "Warning · Operation interrupted"
				}
				if exit != wantExit || !outcome.ConfirmedReceipt || len(outcome.Errors) != 1 || outcome.Errors[0].Code != wantCode || outcome.Human != "" || len(outcome.Data) != 0 {
					t.Fatalf("exit=%d outcome=%+v", exit, outcome)
				}
				if saved != "" && action == "clear" || saved != "alex@example.test" && action == "set" {
					t.Fatalf("saved selector=%q", saved)
				}
				plainOut, plainErr := ansi.Strip(stdout.String()), ansi.Strip(stderr.String())
				if !strings.Contains(plainOut, "Account          "+wantAccount) || !strings.Contains(plainOut, "Live application unconfirmed") || !strings.Contains(plainErr, wantMarker) || !strings.Contains(plainErr, "Inspect saved state") {
					t.Fatalf("stdout=%q stderr=%q", plainOut, plainErr)
				}
				if stop == "interrupted" && !strings.Contains(plainErr, "Result: completion unknown") {
					t.Fatalf("interrupted completion status missing: %q", plainErr)
				}
			})
		}
	}
}

func TestCLIV2CodexSelectionPostSaveReceipt(t *testing.T) {
	for _, tc := range []struct {
		path, account, saved, state string
	}{
		{"codex proxy pin set", "account-a", "pin", "Codex pin · configured"},
		{"codex proxy pin clear", "", "pin", "Codex pin · not configured"},
		{"codex proxy fallback set", "account-a", "fallback", "Codex fallback · configured"},
		{"codex proxy fallback clear", "", "fallback", "Codex fallback · not configured"},
		{"codex proxy prime enable", "", "priming", "Codex priming · configured enabled"},
		{"codex proxy prime disable", "", "priming", "Codex priming · configured disabled"},
	} {
		for _, stop := range []string{"timeout", "interrupted"} {
			t.Run(tc.path+"/"+stop, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				if stop == "timeout" {
					ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
				}
				defer cancel()
				cfg := &proxy.Config{CodexRoutingPinnedAccountKey: "old-pin", CodexRoutingDefaultAccountKey: "old-fallback"}
				saves, inventoryCalls := 0, 0
				deps := v2SelectionDependencies{
					LoadConfig: func() (*proxy.Config, error) { return cfg, nil },
					SaveConfig: func(*proxy.Config) error {
						saves++
						if stop == "timeout" {
							<-ctx.Done()
						} else {
							cancel()
						}
						return nil
					},
					Codex: func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
						inventoryCalls++
						return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "account-a"}}}, codexprov.AccountAliasIndex{}, nil
					},
				}
				args := strings.Fields(tc.path)
				if tc.account != "" {
					args = append(args, tc.account)
				}
				var stdout, stderr bytes.Buffer
				o := terminalFixtureOptions(100)
				var outcome cli.Outcome
				exit := cli.Run(ctx, args, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, func(string) (cli.Handler, bool) {
					return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
						outcome = handleV2SelectionWithDependencies(ctx, inv, s, deps)
						return outcome
					}, true
				})
				wantExit, wantCode, wantWarning := 7, "routing_timeout", "Warning · Routing operation timed out"
				if stop == "interrupted" {
					wantExit, wantCode, wantWarning = 130, "interrupted", "Warning · Operation interrupted"
				}
				if exit != wantExit || !outcome.ConfirmedReceipt || len(outcome.Errors) != 1 || outcome.Errors[0].Code != wantCode || outcome.Human != "" || len(outcome.Data) != 0 || saves != 1 {
					t.Fatalf("exit=%d outcome=%+v saves=%d", exit, outcome, saves)
				}
				wantInventory := 0
				if tc.account != "" {
					wantInventory = 1
				}
				if inventoryCalls != wantInventory {
					t.Fatalf("inventory calls=%d want=%d", inventoryCalls, wantInventory)
				}
				plainOut, plainErr := ansi.Strip(stdout.String()), ansi.Strip(stderr.String())
				if !strings.Contains(plainOut, "Saved Codex "+tc.saved+" configuration.") || !strings.Contains(plainOut, tc.state) || !strings.Contains(plainOut, "Live application unconfirmed") || !strings.Contains(plainErr, wantWarning) {
					t.Fatalf("stdout=%q stderr=%q", plainOut, plainErr)
				}
				if !strings.Contains(stdout.String(), "\x1b[32mSaved Codex ") || !strings.Contains(stdout.String(), "\x1b[33munconfirmed\x1b[0m") || !strings.Contains(stderr.String(), "\x1b[33mWarning\x1b[0m") {
					t.Fatalf("receipt or warning colour role missing: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
				noColour := o
				noColour.Colour = false
				if rendered := outcome.Terminal(noColour); rendered != plainOut || strings.Contains(rendered, "\x1b[") {
					t.Fatalf("colour-free receipt differs: %q", rendered)
				}
				if stop == "interrupted" && !strings.Contains(plainErr, "Result: completion unknown") {
					t.Fatalf("interrupted completion status missing: %q", plainErr)
				}
				for _, line := range strings.Split(stdout.String()+stderr.String(), "\n") {
					if ansi.StringWidth(line) > 100 {
						t.Fatalf("line exceeds 100 cells: %q", line)
					}
				}
			})
		}
	}
}

func TestCLIV2CodexSelectionStoppedPlainAndJSON(t *testing.T) {
	for _, mode := range []string{"plain", "json"} {
		for _, stop := range []string{"timeout", "interrupted"} {
			t.Run(mode+"/"+stop, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				deps := v2SelectionDependencies{
					LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{}, nil },
					SaveConfig: func(*proxy.Config) error {
						if stop == "timeout" {
							<-ctx.Done()
						} else {
							cancel()
						}
						return nil
					},
				}
				args := []string{"codex", "proxy", "prime", "enable"}
				if mode == "json" {
					args = append(args, "--json")
				}
				var stdout, stderr bytes.Buffer
				exit := cli.Run(ctx, args, &cli.Session{Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) {
					return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
						return handleV2SelectionWithDependencies(ctx, inv, session, deps)
					}, true
				})
				wantExit, wantCode, wantMessage := 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation."
				if stop == "interrupted" {
					wantExit, wantCode, wantMessage = 130, "interrupted", "Operation interrupted; inspect state before retrying."
				}
				if exit != wantExit || strings.Contains(stdout.String(), "Saved Codex") || strings.Contains(stdout.String(), "Live application") {
					t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
				}
				if mode == "plain" {
					if stdout.Len() != 0 || stderr.String() != "cq: "+wantMessage+"\n" {
						t.Fatalf("plain stdout=%q stderr=%q", stdout.String(), stderr.String())
					}
				} else {
					var envelope map[string]json.RawMessage
					if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !bytes.Contains(stdout.Bytes(), []byte(`"`+wantCode+`"`)) || stderr.Len() != 0 {
						t.Fatalf("JSON stdout=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
					}
				}
			})
		}
	}
}

func TestCLIV2CodexSelectionNoUnconfirmedReceipt(t *testing.T) {
	for _, path := range []string{"codex proxy pin set", "codex proxy pin clear", "codex proxy fallback set", "codex proxy fallback clear", "codex proxy prime enable", "codex proxy prime disable"} {
		for _, phase := range []string{"before save timeout", "before save interruption", "failed save"} {
			t.Run(path+"/"+phase, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				saves := 0
				deps := v2SelectionDependencies{
					LoadConfig: func() (*proxy.Config, error) {
						if phase == "before save interruption" {
							cancel()
						} else if phase == "before save timeout" {
							<-ctx.Done()
						}
						return &proxy.Config{}, nil
					},
					SaveConfig: func(*proxy.Config) error { saves++; return errors.New("private write error") },
					Codex: func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
						return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "account-a"}}}, codexprov.AccountAliasIndex{}, nil
					},
				}
				args := strings.Fields(path)
				if strings.HasSuffix(path, " set") {
					args = append(args, "account-a")
				}
				inv, err := cli.Parse(args)
				if err != nil {
					t.Fatal(err)
				}
				out := handleV2SelectionWithDependencies(ctx, inv, nil, deps)
				wantExit, wantCode, wantSaves := 1, "routing_io_failed", 1
				if phase == "before save timeout" {
					wantExit, wantCode, wantSaves = 7, "routing_timeout", 0
				} else if phase == "before save interruption" {
					wantExit, wantCode, wantSaves = 130, "interrupted", 0
				}
				if out.ExitCode != wantExit || len(out.Errors) != 1 || out.Errors[0].Code != wantCode || out.ConfirmedReceipt || out.Terminal != nil || saves != wantSaves {
					t.Fatalf("outcome=%+v saves=%d", out, saves)
				}
			})
		}
	}
}

func TestCLIV2SelectionIdentityAndInventoryFailures(t *testing.T) {
	for _, tc := range []struct {
		name, reference, code string
		exit                  int
		inventory             codexprov.Inventory
		inventoryErr          error
	}{
		{"opaque key preserves case", "ACCOUNT-A", "routing_account_not_found", 3, codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "account-a"}}}, nil},
		{"inventory error", "account-a", "routing_inventory_unavailable", 4, codexprov.Inventory{}, errors.New("private inventory error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, parseErr := cli.Parse([]string{"codex", "proxy", "pin", "set", tc.reference})
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			wrote := false
			out := handleV2SelectionWithDependencies(context.Background(), inv, nil, v2SelectionDependencies{
				LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{}, nil },
				SaveConfig: func(*proxy.Config) error { wrote = true; return nil },
				Codex: func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
					return tc.inventory, codexprov.AccountAliasIndex{}, tc.inventoryErr
				},
			})
			if out.ExitCode != tc.exit || len(out.Errors) != 1 || out.Errors[0].Code != tc.code || wrote || strings.Contains(out.Errors[0].Message, "private inventory error") {
				t.Fatalf("outcome=%+v wrote=%t", out, wrote)
			}
		})
	}
	for _, tc := range []struct {
		rows []keyring.ClaudeAccountInspection
		code string
		exit int
	}{
		{nil, "routing_account_not_found", 3},
		{[]keyring.ClaudeAccountInspection{
			{Account: keyring.ClaudeOAuth{AccountUUID: "550e8400-e29b-41d4-a716-446655440000", Email: "first@example.com"}},
			{Account: keyring.ClaudeOAuth{AccountUUID: "550e8400-e29b-41d4-a716-446655440000", Email: "second@example.com"}},
		}, "routing_account_ambiguous", 6},
	} {
		inv, parseErr := cli.Parse([]string{"proxy", "pin", "claude", "550e8400-e29b-41d4-a716-446655440000"})
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		wrote := false
		out := handleV2SelectionWithDependencies(context.Background(), inv, nil, v2SelectionDependencies{
			LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{}, nil },
			SaveConfig: func(*proxy.Config) error { wrote = true; return nil },
			Claude:     func(context.Context) ([]keyring.ClaudeAccountInspection, error) { return tc.rows, nil },
		})
		if out.ExitCode != tc.exit || out.Errors[0].Code != tc.code || wrote {
			t.Fatalf("legacy UUID outcome=%+v wrote=%t", out, wrote)
		}
	}
}

func TestCLIV2SelectionHelpHasNoStateAccess(t *testing.T) {
	for _, path := range []string{"claude proxy pin show", "claude proxy pin set", "claude proxy pin clear", "codex proxy pin show", "codex proxy pin set", "codex proxy pin clear", "codex proxy fallback show", "codex proxy fallback set", "codex proxy fallback clear", "codex proxy prime status", "codex proxy prime enable", "codex proxy prime disable"} {
		t.Run(path, func(t *testing.T) {
			fixture := noAccessV2Fixture(t)
			var stdout, stderr bytes.Buffer
			args := append(strings.Fields(path), "--help")
			if exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr}, fixture.Lookup); exit != 0 {
				t.Fatalf("help exit=%d stderr=%q", exit, stderr.String())
			}
			if want, ok := cli.Help(path); !ok || stdout.String() != want {
				t.Fatalf("help differs from registered resource for %s", path)
			}
			if fixture.calls["lookup"] != 0 {
				t.Fatal("help performed lookup")
			}
		})
	}
}

func TestCLIV2SelectionPreparationBudget(t *testing.T) {
	inv, parseErr := cli.Parse([]string{"codex", "proxy", "pin", "show", "--timeout", "1ms"})
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	var reads int
	deps := v2SelectionDependencies{LoadConfig: func() (*proxy.Config, error) { reads++; return &proxy.Config{}, nil }}
	t.Run("pre-cancelled parent skips preparation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		prepares := 0
		out := handleV2SelectionWithPreparation(ctx, inv, nil, func(context.Context) (v2SelectionDependencies, error) {
			prepares++
			return deps, nil
		})
		if out.ExitCode != 130 || prepares != 0 || reads != 0 {
			t.Fatalf("outcome=%+v prepares=%d reads=%d", out, prepares, reads)
		}
	})
	t.Run("slow preparation exhausts one budget", func(t *testing.T) {
		out := handleV2SelectionWithPreparation(context.Background(), inv, nil, func(ctx context.Context) (v2SelectionDependencies, error) {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			return deps, nil
		})
		if out.ExitCode != 7 || out.Errors[0].Code != "routing_timeout" || reads != 0 {
			t.Fatalf("outcome=%+v reads=%d", out, reads)
		}
	})
	t.Run("deadline precedes preparation error", func(t *testing.T) {
		out := handleV2SelectionWithPreparation(context.Background(), inv, nil, func(ctx context.Context) (v2SelectionDependencies, error) {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			return v2SelectionDependencies{}, errors.New("private preparation error")
		})
		if out.ExitCode != 7 || out.Errors[0].Code != "routing_timeout" || reads != 0 {
			t.Fatalf("outcome=%+v reads=%d", out, reads)
		}
	})
	t.Run("cancellation precedes preparation error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := handleV2SelectionWithPreparation(ctx, inv, nil, func(context.Context) (v2SelectionDependencies, error) {
			cancel()
			return v2SelectionDependencies{}, errors.New("private preparation error")
		})
		if out.ExitCode != 130 || out.Errors[0].Code != "interrupted" || reads != 0 {
			t.Fatalf("outcome=%+v reads=%d", out, reads)
		}
	})
}

// Parent cancellation closes Done before its children must observe cancellation.
// Keep the work context live to model that propagation interval deterministically.
func TestCLIV2SelectionParentDeadlineBeforeWorkPropagation(t *testing.T) {
	for _, phase := range []string{"before-save", "after-save"} {
		t.Run(phase, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			parent := selectionDeadlineParent{base}
			defer cancel()
			if phase == "before-save" {
				cancel()
			}
			inv, err := cli.Parse([]string{"codex", "proxy", "fallback", "clear"})
			if err != nil {
				t.Fatal(err)
			}
			saves := 0
			out := executeV2Selection(parent, context.Background(), inv, v2SelectionDependencies{
				LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{}, nil },
				SaveConfig: func(*proxy.Config) error {
					saves++
					cancel()
					<-parent.Done()
					return nil
				},
			})
			if out.ExitCode != 7 || len(out.Errors) != 1 || out.Errors[0].Code != "routing_timeout" {
				t.Fatalf("parent deadline lost: %+v", out)
			}
			if phase == "before-save" {
				if saves != 0 || out.ConfirmedReceipt {
					t.Fatalf("expired parent dispatched save: saves=%d outcome=%+v", saves, out)
				}
			} else if saves != 1 || !out.ConfirmedReceipt || out.Terminal == nil || !strings.Contains(out.Terminal(terminalFixtureOptions(100)), "Saved Codex fallback configuration.") {
				t.Fatalf("saved receipt lost: saves=%d outcome=%+v", saves, out)
			}
		})
	}
}

// The test manually expires this parent so no timer or scheduler controls the
// interval between the parent's deadline and the work context observing it.
type selectionDeadlineParent struct{ context.Context }

func (c selectionDeadlineParent) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}
