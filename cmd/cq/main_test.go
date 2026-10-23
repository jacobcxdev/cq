package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
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
		{"plus", "+30", 30 * time.Second},
		{"plus nondefault", "+31", 31 * time.Second},
		{"whitespace", " 30", 30 * time.Second},
		{"suffix", "30s", 30 * time.Second},
		{"overflow", "9999999999999999999999999", 30 * time.Second},
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
	wrapper := findFuncBody(t, file, "runProxyStart")
	if !hasIdentifier(wrapper, "runProxyStartWithContext") {
		t.Fatal("legacy startup must delegate to the shared implementation")
	}
	body := findFuncBody(t, file, "runProxyStartWithContext")

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
	wrapper := findFuncBody(t, file, "runProxyStart")
	if !hasIdentifier(wrapper, "runProxyStartWithContext") {
		t.Fatal("legacy startup must delegate to the shared implementation")
	}
	body := findFuncBody(t, file, "runProxyStartWithContext")

	for _, selector := range codexCredentialMutationSelectors(body) {
		t.Errorf("runProxyStart must not reference Codex credential mutation selector %s", selector)
	}
	if !hasIdentifier(body, "listProxyCodexStartupInventory") {
		t.Fatal("runProxyStart should discover Codex candidates through the read-only startup inventory boundary")
	}
}

func TestRunProxyStartDoesNotLogLocalToken(t *testing.T) {
	file := parseGoFile(t, "proxy.go")
	wrapper := findFuncBody(t, file, "runProxyStart")
	if !hasIdentifier(wrapper, "runProxyStartWithContext") {
		t.Fatal("legacy startup must delegate to the shared implementation")
	}
	body := findFuncBody(t, file, "runProxyStartWithContext")

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
