package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func parseZAILoginDefaultsCLI(t *testing.T, args ...string) (ZAICmd, string) {
	t.Helper()
	var cli CLI
	parser, err := kong.New(&cli, kong.Writers(io.Discard, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.Parse(append([]string{"zai"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return cli.ZAI, strings.TrimPrefix(parsed.Command(), "zai ")
}

func TestZAILoginCLIUsedBrowserAndPreferredEmailLabelByDefault(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := zaiprov.StoreAccount(fsys, "/config", "legacy-private", "legacy", true); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	deps := zaiTestDependencies(fsys, &out)
	deps.deviceMid = func() (string, error) { return "device", nil }
	deps.promptKey = func() ([]byte, error) { t.Fatal("default browser login prompted for key"); return nil, nil }
	deps.client = zaiCommandDoer(func(*http.Request) (*http.Response, error) { t.Fatal("injected OAuth test sent HTTP"); return nil, nil })
	user := "first"
	deps.oauthLogin = func(context.Context, zcode.OAuthClient) (zcode.LoginSession, error) {
		return zcode.LoginSession{UserID: user, Email: user + "@example.com", ZCodeJWT: "private-zcode", RawOAuth: "private-oauth", BusinessJWT: "private-business"}, nil
	}
	options, command := parseZAILoginDefaultsCLI(t, "login")
	if !options.Login.shouldActivate() || options.Login.Activate != nil {
		t.Fatal("plain parsed login was not preferred by default")
	}
	if err := runZAICommand(t.Context(), command, options, true, deps); err != nil {
		t.Fatal(err)
	}
	accounts, err := zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].OAuth == nil || accounts[0].Label != "first@example.com" || accounts[1].APIKey != "legacy-private" {
		t.Fatal("default login lost email label, preference or legacy account")
	}
	firstID := accounts[0].ID
	user = "second"
	options, command = parseZAILoginDefaultsCLI(t, "login", "--no-activate", "--label", "Custom subscription")
	if options.Login.shouldActivate() {
		t.Fatal("--no-activate ignored")
	}
	if err := runZAICommand(t.Context(), command, options, true, deps); err != nil {
		t.Fatal(err)
	}
	accounts, err = zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
	if err != nil || len(accounts) != 3 || accounts[0].ID != firstID || accounts[2].Label != "Custom subscription" {
		t.Fatal("opt-out changed preference or custom label")
	}
	options, command = parseZAILoginDefaultsCLI(t, "login", "--zcode", "--activate")
	if err := runZAICommand(t.Context(), command, options, true, deps); err != nil {
		t.Fatal(err)
	}
	accounts, err = zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
	if err != nil || len(accounts) != 3 || accounts[0].OAuth == nil || accounts[0].OAuth.UserID != "second" || accounts[0].Label != "Custom subscription" {
		t.Fatal("legacy flags or custom label preservation regressed")
	}
	if strings.Contains(out.String(), "private-") {
		t.Fatal("browser login exposed credentials")
	}
}

func TestZAILoginCLIKeptExplicitAPIKeyMethods(t *testing.T) {
	for _, flags := range [][]string{{"--api-key"}, {"--api-key-stdin"}, {"--api-key", "--api-key-stdin"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if _, err := zaiprov.StoreAccount(fsys, "/config", "old-private", "old", true); err != nil {
				t.Fatal(err)
			}
			deps := zaiTestDependencies(fsys, io.Discard)
			deps.deviceMid = func() (string, error) { t.Fatal("key login discovered browser device"); return "", nil }
			deps.oauthLogin = func(context.Context, zcode.OAuthClient) (zcode.LoginSession, error) {
				t.Fatal("key login started browser auth")
				return zcode.LoginSession{}, nil
			}
			prompted := false
			deps.promptKey = func() ([]byte, error) { prompted = true; return []byte("new-private"), nil }
			deps.input = strings.NewReader("new-private\n")
			options, command := parseZAILoginDefaultsCLI(t, append([]string{"login"}, flags...)...)
			if err := runZAICommand(t.Context(), command, options, true, deps); err != nil {
				t.Fatal(err)
			}
			accounts, err := zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
			if err != nil || len(accounts) != 2 || accounts[0].APIKey != "old-private" || accounts[1].APIKey != "new-private" || prompted != (!options.Login.APIKeyStdin) {
				t.Fatal("explicit key login changed the existing script preference")
			}
			deps.input = strings.NewReader("new-private\n")
			options, command = parseZAILoginDefaultsCLI(t, append(append([]string{"login"}, flags...), "--activate")...)
			if err := runZAICommand(t.Context(), command, options, true, deps); err != nil {
				t.Fatal(err)
			}
			accounts, err = zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
			if err != nil || len(accounts) != 2 || accounts[0].APIKey != "new-private" {
				t.Fatal("explicit activation did not select the key account")
			}
		})
	}
}

func TestZAILoginCLIMethodConflictsAndRemoveConflictsRejected(t *testing.T) {
	for _, args := range [][]string{{"login", "--zcode", "--api-key"}, {"login", "--zcode", "--api-key-stdin"}, {"remove", "account", "--api-keys"}, {"remove"}} {
		var options ZAICmd
		parser, err := kong.New(&options, kong.Writers(io.Discard, io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parser.Parse(args); err == nil {
			t.Fatalf("conflicting/incomplete command accepted: %v", args)
		}
	}
}

func TestZAILoginCLIActivationFlagsKeptCompatibility(t *testing.T) {
	for _, test := range []struct {
		flags []string
		want  bool
	}{{nil, true}, {[]string{"--activate"}, true}, {[]string{"--no-activate"}, false}, {[]string{"--activate=false"}, false}, {[]string{"--api-key-stdin"}, false}, {[]string{"--api-key-stdin", "--activate"}, true}, {[]string{"--api-key-stdin", "--activate=false"}, false}} {
		options, _ := parseZAILoginDefaultsCLI(t, append([]string{"login"}, test.flags...)...)
		if options.Login.shouldActivate() != test.want {
			t.Fatalf("activation flags %v gave %t", test.flags, options.Login.shouldActivate())
		}
	}
}

func TestZAIRemoveCLIAPIKeysWasExplicitLocalAndIdempotent(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := zaiprov.StoreAccount(fsys, "/config", "old-private", "old", true); err != nil {
		t.Fatal(err)
	}
	oauth, err := zaiprov.StoreZCodeAccount(fsys, "/config", zcode.LoginSession{UserID: "saved", Email: "saved@example.com", ZCodeJWT: "private-zcode", RawOAuth: "private-oauth", BusinessJWT: "private-business", DeviceMid: "device"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	deps := zaiTestDependencies(fsys, &out)
	deps.getenv = func(string) string { return "environment-private" }
	deps.client = zaiCommandDoer(func(*http.Request) (*http.Response, error) {
		t.Fatal("bulk cleanup made network call")
		return nil, nil
	})
	options, command := parseZAILoginDefaultsCLI(t, "remove", "--api-keys")
	if command != "remove" {
		t.Fatalf("bulk remove dispatch path changed: %q", command)
	}
	for range 2 {
		if err := runZAICommand(t.Context(), command, options, true, deps); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := zaiprov.ReadAccounts(fsys, func(string) string { return "" }, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].ID != oauth.ID || accounts[0].OAuth == nil || strings.Contains(out.String(), "private-") {
		t.Fatal("bulk cleanup removed browser login, used env key or leaked")
	}
}

func TestZAIRemoveCLIReferenceStillSelectedOneAccount(t *testing.T) {
	fsys := fsutil.NewMemFS()
	for _, label := range []string{"first", "second"} {
		if _, err := zaiprov.StoreAccount(fsys, "/config", label+"-private", label, false); err != nil {
			t.Fatal(err)
		}
	}
	options, command := parseZAILoginDefaultsCLI(t, "remove", "second")
	if command != "remove <reference>" || options.Remove.APIKeys {
		t.Fatal("per-account remove dispatch changed")
	}
	deps := zaiTestDependencies(fsys, io.Discard)
	if err := runZAICommand(t.Context(), command, options, false, deps); err != nil {
		t.Fatal(err)
	}
	accounts, err := zaiprov.ReadAccounts(fsys, deps.getenv, "/config")
	if err != nil || len(accounts) != 1 || accounts[0].Label != "first" {
		t.Fatal("per-account remove lost unrelated account")
	}
}
