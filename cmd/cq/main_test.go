package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCacheTTL(t *testing.T) {
	tests := []struct {
		name  string
		value string // empty string means unset
		want  time.Duration
	}{
		{"empty string", "", 30 * time.Second},
		{"valid 60", "60", 60 * time.Second},
		{"valid 0", "0", 0},
		{"negative clamped to 0", "-5", 0},
		{"above max clamped to 3600", "3601", 3600 * time.Second},
		{"exactly 3600", "3600", 3600 * time.Second},
		{"non-numeric falls back", "abc", 30 * time.Second},
		{"float non-numeric falls back", "1.5", 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value == "" {
				t.Setenv("CQ_TTL", "")
			} else {
				t.Setenv("CQ_TTL", tt.value)
			}
			got := cacheTTL()
			if got != tt.want {
				t.Errorf("cacheTTL() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- AccountManager ---

func TestAccountManager(t *testing.T) {
	t.Run("Claude returns non-nil", func(t *testing.T) {
		if got := app.AccountManager(provider.Claude, nil); got == nil {
			t.Error("AccountManager(Claude) = nil, want non-nil")
		}
	})

	t.Run("Codex returns non-nil", func(t *testing.T) {
		if got := app.AccountManager(provider.Codex, nil); got == nil {
			t.Error("AccountManager(Codex) = nil, want non-nil")
		}
	})

	t.Run("Gemini returns nil", func(t *testing.T) {
		if got := app.AccountManager(provider.Gemini, nil); got != nil {
			t.Errorf("AccountManager(Gemini) = %v, want nil", got)
		}
	})
}

// --- GetActiveCredentials ---

func TestGetActiveCredentials(t *testing.T) {
	writeCredentials := func(t *testing.T, dir string, token, email string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		creds := keyring.ClaudeCredentials{
			ClaudeAiOauth: &keyring.ClaudeOAuth{
				AccessToken: token,
				Email:       email,
			},
		}
		data, err := json.MarshalIndent(creds, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		path := filepath.Join(dir, ".claude", ".credentials.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	t.Run("valid credentials file returns token and email", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		writeCredentials(t, dir, "mytoken123", "user@example.com")

		tok, email := app.GetActiveCredentials()
		if tok != "mytoken123" {
			t.Errorf("token = %q, want mytoken123", tok)
		}
		if email != "user@example.com" {
			t.Errorf("email = %q, want user@example.com", email)
		}
	})

	t.Run("missing file returns empty strings", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		tok, email := app.GetActiveCredentials()
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
		if email != "" {
			t.Errorf("email = %q, want empty", email)
		}
	})

	t.Run("invalid JSON returns empty strings", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, ".claude", ".credentials.json")
		if err := os.WriteFile(path, []byte("not valid json {{{"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		tok, email := app.GetActiveCredentials()
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
		if email != "" {
			t.Errorf("email = %q, want empty", email)
		}
	})
}

// --- isTerminal ---

func TestIsTerminal(t *testing.T) {
	// isTerminal simply inspects os.Stdout; it must not panic.
	// In a test environment stdout is not a char device, so it returns false.
	got := isTerminal()
	if got {
		t.Error("expected false in test environment (stdout is not a char device)")
	}
}

// --- dispatch ---

func TestDispatchUnknownCommandReturnsError(t *testing.T) {
	// We need a kong.Context whose Command() returns something not in the switch.
	// Define a minimal CLI type with a single command that dispatch doesn't handle.
	type unknownCLI struct {
		Bogus struct{} `cmd:""`
	}
	var cli unknownCLI
	// Parse "bogus" against our stub CLI to get a real *kong.Context.
	kctx, err := kong.New(&cli,
		kong.Writers(io.Discard, io.Discard),
		kong.Exit(func(int) {}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	parsed, err := kctx.Parse([]string{"bogus"})
	if err != nil {
		t.Fatalf("kctx.Parse: %v", err)
	}

	// dispatch expects a *kong.Context; pass our real CLI as well (unused for
	// the default branch).
	var mainCLI CLI
	dispatchErr := dispatch(parsed, &mainCLI)
	if dispatchErr == nil {
		t.Fatal("dispatch returned nil error for unknown command, want non-nil")
	}
}

func TestDispatchCodexAccountsJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tempRoot := os.TempDir()
	if runtime.GOOS == "darwin" {
		tempRoot = "/private/tmp"
	}
	shortConfigDir, err := os.MkdirTemp(tempRoot, "cq-accounts-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortConfigDir) })
	t.Setenv("XDG_CONFIG_HOME", shortConfigDir)

	for _, args := range [][]string{
		{"codex", "accounts", "--json"},
		{"--json", "codex", "accounts"},
	} {
		var cli CLI
		kctx, err := kong.New(&cli,
			kong.Writers(io.Discard, io.Discard),
			kong.Exit(func(int) {}),
		)
		if err != nil {
			t.Fatalf("kong.New: %v", err)
		}
		parsed, err := kctx.Parse(args)
		if err != nil {
			t.Fatalf("Parse(%v): %v", args, err)
		}

		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		originalStdout := os.Stdout
		os.Stdout = writer
		dispatchErr := dispatch(parsed, &cli)
		_ = writer.Close()
		os.Stdout = originalStdout
		output, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if dispatchErr != nil {
			t.Fatalf("dispatch(%v): %v", args, dispatchErr)
		}
		if readErr != nil {
			t.Fatal(readErr)
		}

		var accounts []provider.Account
		if err := json.Unmarshal(output, &accounts); err != nil {
			t.Fatalf("dispatch(%v) output is not JSON: %v", args, err)
		}
		if accounts == nil || len(accounts) != 0 {
			t.Fatalf("dispatch(%v) accounts = %#v, want []", args, accounts)
		}
	}
}

func TestCLIParsesRemoveCommands(t *testing.T) {
	var cli CLI
	kctx, err := kong.New(&cli,
		kong.Writers(io.Discard, io.Discard),
		kong.Exit(func(int) {}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "claude remove", args: []string{"claude", "remove", "user@example.com"}, want: "claude remove <email>"},
		{name: "codex remove", args: []string{"codex", "remove", "user@example.com"}, want: "codex remove <email>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := kctx.Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%v): %v", tt.args, err)
			}
			if got := parsed.Command(); got != tt.want {
				t.Fatalf("Command() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCLIParsesProxyCodexDefault(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantClear     bool
		wantReference string
	}{
		{name: "status", args: []string{"proxy", "default", "codex"}},
		{name: "clear", args: []string{"proxy", "default", "codex", "--clear"}, wantClear: true},
		{name: "reference", args: []string{"proxy", "default", "codex", "person@example.test"}, wantReference: "person@example.test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cli CLI
			kctx, err := kong.New(&cli,
				kong.Writers(io.Discard, io.Discard),
				kong.Exit(func(int) {}),
			)
			if err != nil {
				t.Fatalf("kong.New: %v", err)
			}

			parsed, err := kctx.Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%v): %v", tt.args, err)
			}
			if got := parsed.Command(); !strings.HasPrefix(got, "proxy default codex") ||
				(tt.wantReference != "" && !strings.Contains(got, "<account-reference>")) {
				t.Fatalf("Command() = %q, want proxy default codex command for %q", got, tt.wantReference)
			}
			if cli.Proxy.Default.Codex.Clear != tt.wantClear {
				t.Fatalf("Clear = %t, want %t", cli.Proxy.Default.Codex.Clear, tt.wantClear)
			}
			if cli.Proxy.Default.Codex.Reference != tt.wantReference {
				t.Fatalf("Reference = %q, want %q", cli.Proxy.Default.Codex.Reference, tt.wantReference)
			}
		})
	}

	var cli CLI
	kctx, err := kong.New(&cli,
		kong.Writers(io.Discard, io.Discard),
		kong.Exit(func(int) {}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	if _, err := kctx.Parse([]string{"proxy", "codex-default"}); err == nil {
		t.Fatal("Parse(proxy codex-default) error = nil, want unknown command")
	}
}

func TestParseProxyCommandOptionsPort(t *testing.T) {
	opts, err := parseProxyCommandOptions([]string{"--port", "19281", "--migrate-legacy-managed"})
	if err != nil {
		t.Fatalf("parseProxyCommandOptions() error = %v", err)
	}
	if opts.Port != 19281 {
		t.Fatalf("Port = %d, want 19281", opts.Port)
	}
	if !opts.MigrateLegacyManaged {
		t.Fatal("MigrateLegacyManaged = false")
	}
}

func TestRunProxyStartAvoidsDirectClaudeStorageCalls(t *testing.T) {
	file := parseGoFile(t, "proxy.go")
	body := findFuncBody(t, file, "runProxyStart")

	if hasQualifiedSelector(body, "keyring", "DiscoverClaudeAccounts") {
		t.Fatal("runProxyStart should not call keyring.DiscoverClaudeAccounts directly")
	}
	if !hasIdentifier(body, "discoverClaudeAccountsFn") {
		t.Fatal("runProxyStart should use discoverClaudeAccountsFn")
	}
	if hasQualifiedSelector(body, "keyring", "ActiveClaudeEmail") {
		t.Fatal("runProxyStart should not reference keyring.ActiveClaudeEmail directly")
	}
	if !hasIdentifier(body, "activeClaudeEmailFn") {
		t.Fatal("runProxyStart should use activeClaudeEmailFn")
	}
}

func TestListProxyCodexStartupInventoryPreservesCandidates(t *testing.T) {
	want := proxyCodexStartupInventoryFixture()
	source := &staticProxyCodexStartupInventory{inventory: proxyCodexStartupInventoryFixture()}

	got, err := listProxyCodexStartupInventory(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 {
		t.Fatalf("inventory List calls = %d, want 1", source.calls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startup inventory = %+v, want distinct system and managed candidates %+v", got, want)
	}
}

func TestRunProxyStartDoesNotReachCodexCredentialMutators(t *testing.T) {
	file := parseGoFile(t, "proxy.go")
	body := findFuncBody(t, file, "runProxyStart")

	for _, selector := range codexCredentialMutationSelectors(body) {
		t.Errorf("runProxyStart must not reference Codex credential mutation selector %s", selector)
	}
	if !hasIdentifier(body, "listProxyCodexStartupInventory") {
		t.Fatal("runProxyStart should discover Codex candidates through the read-only startup inventory boundary")
	}
}

func TestRunProxyStartDoesNotLogLocalToken(t *testing.T) {
	file := parseGoFile(t, "proxy.go")
	body := findFuncBody(t, file, "runProxyStart")

	if hasQualifiedSelector(body, "cfg", "LocalToken") || hasStringLiteral(body, "cq: proxy token: %s\n") {
		t.Fatal("runProxyStart must not print the local proxy token")
	}
}

func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(%q): %v", path, err)
	}
	return file
}

func findFuncBody(t *testing.T, file *ast.File, name string) *ast.BlockStmt {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn.Body
		}
	}
	t.Fatalf("function %q not found", name)
	return nil
}

func hasQualifiedSelector(body *ast.BlockStmt, pkg, sel string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		selector, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name == pkg && selector.Sel.Name == sel {
			found = true
			return false
		}
		return true
	})
	return found
}

func hasIdentifier(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if ok && ident.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

func codexCredentialMutationSelectors(body *ast.BlockStmt) []string {
	var found []string
	ast.Inspect(body, func(n ast.Node) bool {
		selector, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "IntentAdopt", "NewFileSystemActivator", "SaveLogin", "Adopt", "Activate", "RemoveManaged", "RefreshReference":
			found = append(found, selector.Sel.Name)
		case "Refresh":
			receiver, ok := selector.X.(*ast.Ident)
			if !ok || receiver.Name != "registryRefresher" {
				found = append(found, selector.Sel.Name)
			}
		}
		return true
	})
	return found
}

type staticProxyCodexStartupInventory struct {
	inventory codexprov.Inventory
	calls     int
}

func (source *staticProxyCodexStartupInventory) List(context.Context) (codexprov.Inventory, error) {
	source.calls++
	return source.inventory, nil
}

func proxyCodexStartupInventoryFixture() codexprov.Inventory {
	const accountKey = codexprov.AccountKey("account-key")
	return codexprov.Inventory{
		Accounts: []codexprov.LogicalAccount{{
			Key: accountKey,
			Candidates: []codexprov.CredentialCandidate{
				{
					Ref: codexprov.CandidateRef{
						AccountKey: accountKey, CandidateID: "system-candidate",
					},
					Revision: "system-revision", Source: codexprov.SourceSystem, Routable: true,
				},
				{
					Ref: codexprov.CandidateRef{
						AccountKey: accountKey, CandidateID: "managed-candidate",
					},
					Revision: "managed-revision", Source: codexprov.SourceManaged, Routable: true,
				},
			},
		}},
		Intents: []codexprov.InventoryIntent{{
			Kind: codexprov.IntentAdopt, AccountKey: accountKey,
			Candidates: []codexprov.CandidateID{"system-candidate"},
		}},
	}
}

func hasStringLiteral(body *ast.BlockStmt, value string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && lit.Value == strconv.Quote(value) {
			found = true
			return false
		}
		return true
	})
	return found
}
