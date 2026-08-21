package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const proxyCodexDefaultUsage = "usage: cq proxy default codex [--clear | <account-reference>]"

type proxyCodexDefaultHarness struct {
	inventory    codexprov.Inventory
	inventoryErr error
	aliases      codexprov.AccountAliasIndex
	aliasErr     error
	config       *proxy.Config
	loadErr      error
	saveErr      error
	stdout       bytes.Buffer
	calls        []string
	saved        *proxy.Config
}

func newProxyCodexDefaultHarness() *proxyCodexDefaultHarness {
	return &proxyCodexDefaultHarness{config: &proxy.Config{}}
}

func (h *proxyCodexDefaultHarness) dependencies() proxyCodexDefaultDependencies {
	return proxyCodexDefaultDependencies{
		ListInventory: func(context.Context) (codexprov.Inventory, error) {
			h.calls = append(h.calls, "inventory")
			return h.inventory, h.inventoryErr
		},
		LoadAliasIndex: func() (codexprov.AccountAliasIndex, error) {
			h.calls = append(h.calls, "aliases")
			return h.aliases, h.aliasErr
		},
		LoadConfig: func() (*proxy.Config, error) {
			h.calls = append(h.calls, "config")
			return h.config, h.loadErr
		},
		SaveConfig: func(cfg *proxy.Config) error {
			h.calls = append(h.calls, "save")
			h.saved = cfg
			return h.saveErr
		},
		Stdout: &h.stdout,
	}
}

func TestRunProxyCodexDefaultStatusUnconfiguredLoadsOnlyConfig(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.inventoryErr = errors.New("inventory for private@example.test contained token-secret")
	h.aliasErr = errors.New("registry alias-secret contained account-id-secret")

	err := runProxyCodexDefaultWithDependencies(context.Background(), nil, h.dependencies())
	if err != nil {
		t.Fatalf("runProxyCodexDefaultWithDependencies() error = %v", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "config")
	if h.saved != nil {
		t.Fatal("status saved proxy config")
	}
	if got, want := h.stdout.String(), "Codex routing default: not configured.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertProxyCodexDefaultPrivateAbsent(t, h.stdout.String(),
		"private@example.test", "token-secret", "alias-secret", "account-id-secret")
}

func TestRunProxyCodexDefaultStatusConfiguredPrintsOnlyOpaqueKey(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.config.CodexRoutingDefaultAccountKey = "opaque-configured-key"
	h.inventory = privateProxyCodexDefaultInventory("unused-key", "private@example.test", false, true)

	err := runProxyCodexDefaultWithDependencies(context.Background(), nil, h.dependencies())
	if err != nil {
		t.Fatalf("runProxyCodexDefaultWithDependencies() error = %v", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "config")
	if got, want := h.stdout.String(), "Codex routing default: \"opaque-configured-key\"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertProxyCodexDefaultPrivateAbsent(t, h.stdout.String(),
		"private@example.test", "provider-account-secret", "candidate-id-secret", "access-token-secret")
}

func TestRunProxyCodexDefaultClearMutatesOnlyRoutingDefault(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.config = proxyCodexDefaultConfigWithFutureField(t)
	beforePort := h.config.Port
	beforePin := h.config.PinnedClaudeAccount
	beforeHTTPMode := h.config.CodexTurnRouting
	beforeWSMode := h.config.CodexWSTurnRouting

	err := runProxyCodexDefaultWithDependencies(context.Background(), []string{"--clear"}, h.dependencies())
	if err != nil {
		t.Fatalf("runProxyCodexDefaultWithDependencies() error = %v", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "config", "save")
	if h.saved != h.config {
		t.Fatal("SaveConfig did not receive the loaded config pointer")
	}
	if h.saved.CodexRoutingDefaultAccountKey != "" {
		t.Fatalf("routing default = %q, want empty", h.saved.CodexRoutingDefaultAccountKey)
	}
	if h.saved.Port != beforePort || h.saved.PinnedClaudeAccount != beforePin ||
		h.saved.CodexTurnRouting != beforeHTTPMode || h.saved.CodexWSTurnRouting != beforeWSMode {
		t.Fatalf("clear changed unrelated fields: %+v", h.saved)
	}
	assertProxyCodexDefaultFutureField(t, h.saved)
	if got, want := h.stdout.String(), "Codex routing default cleared.\nRestart proxy to apply change.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertProxyCodexDefaultPrivateAbsent(t, h.stdout.String(),
		"opaque-old-key", "private-pin@example.test", "future-token-secret")
}

func TestRunProxyCodexDefaultRejectsMalformedArgumentsBeforeDependencies(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "clear plus reference", args: []string{"--clear", "private@example.test"}},
		{name: "two references", args: []string{"private@example.test", "alias-secret"}},
		{name: "three arguments", args: []string{"one", "two", "token-secret"}},
		{name: "unknown long option", args: []string{"--bogus"}},
		{name: "unknown short option", args: []string{"-x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyCodexDefaultHarness()
			err := runProxyCodexDefaultWithDependencies(context.Background(), tt.args, h.dependencies())
			if err == nil || err.Error() != proxyCodexDefaultUsage {
				t.Fatalf("error = %v, want %q", err, proxyCodexDefaultUsage)
			}
			assertProxyCodexDefaultCalls(t, h.calls)
			if h.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", h.stdout.String())
			}
			assertProxyCodexDefaultPrivateAbsent(t, err.Error(),
				"private@example.test", "alias-secret", "token-secret")
		})
	}
}

func TestRunProxyCodexDefaultResolvesBeforeLoadingAndSavingConfig(t *testing.T) {
	const (
		privateEmail = "private.person@example.test"
		privateAlias = "private-alias-secret"
	)
	tests := []struct {
		name      string
		reference string
		inventory codexprov.Inventory
		aliases   []proxyCodexDefaultAlias
		wantKey   codexprov.AccountKey
	}{
		{
			name:      "exact opaque key takes precedence",
			reference: "opaque-exact-key",
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				privateProxyCodexDefaultAccount("opaque-exact-key", "first@example.test", false, true),
				privateProxyCodexDefaultAccount("metadata-match-key", "opaque-exact-key", false, true),
			}},
			aliases: []proxyCodexDefaultAlias{{alias: "opaque-exact-key", key: "metadata-match-key"}},
			wantKey: "opaque-exact-key",
		},
		{
			name:      "unique email",
			reference: privateEmail,
			inventory: privateProxyCodexDefaultInventory("opaque-email-key", privateEmail, false, true),
			wantKey:   "opaque-email-key",
		},
		{
			name:      "unique registry alias",
			reference: privateAlias,
			inventory: privateProxyCodexDefaultInventory("opaque-alias-key", "alias-owner@example.test", false, true),
			aliases:   []proxyCodexDefaultAlias{{alias: privateAlias, key: "opaque-alias-key"}},
			wantKey:   "opaque-alias-key",
		},
		{
			name:      "stable unroutable account",
			reference: "unroutable@example.test",
			inventory: privateProxyCodexDefaultInventory("opaque-unroutable-key", "unroutable@example.test", false, false),
			wantKey:   "opaque-unroutable-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyCodexDefaultHarness()
			h.inventory = tt.inventory
			h.aliases = proxyCodexDefaultAliasIndex(t, tt.aliases...)
			h.config = proxyCodexDefaultConfigWithFutureField(t)

			err := runProxyCodexDefaultWithDependencies(
				context.Background(), []string{tt.reference}, h.dependencies(),
			)
			if err != nil {
				t.Fatalf("runProxyCodexDefaultWithDependencies() error = %v", err)
			}
			assertProxyCodexDefaultCalls(t, h.calls, "inventory", "aliases", "config", "save")
			if h.saved != h.config {
				t.Fatal("SaveConfig did not receive the loaded config pointer")
			}
			if h.saved.CodexRoutingDefaultAccountKey != tt.wantKey {
				t.Fatalf("routing default = %q, want %q", h.saved.CodexRoutingDefaultAccountKey, tt.wantKey)
			}
			if h.saved.PinnedClaudeAccount != "private-pin@example.test" || h.saved.Port != 19443 {
				t.Fatalf("set changed unrelated fields: %+v", h.saved)
			}
			assertProxyCodexDefaultFutureField(t, h.saved)
			wantOutput := "Codex routing default: \"" + string(tt.wantKey) + "\"\nRestart proxy to apply change.\n"
			if got := h.stdout.String(); got != wantOutput {
				t.Fatalf("stdout = %q, want %q", got, wantOutput)
			}
			forbidden := []string{
				"provider-account-secret", "candidate-id-secret", "access-token-secret",
				"refresh-token-secret", "raw-error-token-secret",
			}
			if tt.reference != string(tt.wantKey) {
				forbidden = append(forbidden, tt.reference)
			}
			assertProxyCodexDefaultPrivateAbsent(t, h.stdout.String(), forbidden...)
		})
	}
}

func TestRunProxyCodexDefaultEscapesConfiguredKeyForTerminal(t *testing.T) {
	key := codexprov.AccountKey("opaque\n\x1b[31mspoof\t\"quoted\"")
	h := newProxyCodexDefaultHarness()
	h.config.CodexRoutingDefaultAccountKey = key

	err := runProxyCodexDefaultWithDependencies(context.Background(), nil, h.dependencies())
	if err != nil {
		t.Fatalf("runProxyCodexDefaultWithDependencies() error = %v", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "config")
	want := `Codex routing default: "opaque\n\x1b[31mspoof\t\"quoted\""` + "\n"
	if got := h.stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertProxyCodexDefaultTerminalSafe(t, h.stdout.String(), 1)
}

func TestRunProxyCodexDefaultEscapesResolvedKeyButPersistsExactValue(t *testing.T) {
	key := codexprov.AccountKey("opaque\n\x1b[31mspoof\t\"quoted\"")
	h := newProxyCodexDefaultHarness()
	h.inventory = privateProxyCodexDefaultInventory(key, "private@example.test", false, true)
	h.aliases = proxyCodexDefaultAliasIndex(t, proxyCodexDefaultAlias{alias: "safe-alias", key: key})

	err := runProxyCodexDefaultWithDependencies(
		context.Background(), []string{"safe-alias"}, h.dependencies(),
	)
	if err != nil {
		t.Fatalf("runProxyCodexDefaultWithDependencies() error = %v", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "inventory", "aliases", "config", "save")
	if h.saved == nil || h.saved.CodexRoutingDefaultAccountKey != key {
		t.Fatalf("saved routing default = %q, want exact original key", h.saved.CodexRoutingDefaultAccountKey)
	}
	want := `Codex routing default: "opaque\n\x1b[31mspoof\t\"quoted\""` + "\nRestart proxy to apply change.\n"
	if got := h.stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertProxyCodexDefaultTerminalSafe(t, h.stdout.String(), 2)
}

func TestRunProxyCodexDefaultResolutionFailuresDoNotLoadOrSaveConfig(t *testing.T) {
	const privateReference = "private.person@example.test"
	tests := []struct {
		name      string
		inventory codexprov.Inventory
		aliases   []proxyCodexDefaultAlias
		wantCode  codexprov.AccountReferenceErrorCode
	}{
		{
			name:      "missing",
			inventory: privateProxyCodexDefaultInventory("unrelated-key", "other@example.test", false, true),
			wantCode:  codexprov.AccountReferenceMissing,
		},
		{
			name: "ambiguous",
			inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
				privateProxyCodexDefaultAccount("ambiguous-a", privateReference, false, true),
				privateProxyCodexDefaultAccount("ambiguous-b", privateReference, false, true),
			}},
			wantCode: codexprov.AccountReferenceAmbiguous,
		},
		{
			name:      "unstable",
			inventory: privateProxyCodexDefaultInventory("unstable-key", privateReference, true, true),
			wantCode:  codexprov.AccountReferenceUnstable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyCodexDefaultHarness()
			h.inventory = tt.inventory
			h.aliases = proxyCodexDefaultAliasIndex(t, tt.aliases...)
			err := runProxyCodexDefaultWithDependencies(
				context.Background(), []string{privateReference}, h.dependencies(),
			)
			var referenceErr *codexprov.AccountReferenceError
			if !errors.As(err, &referenceErr) || referenceErr.Code != tt.wantCode {
				t.Fatalf("error = %v, want AccountReferenceError code %q", err, tt.wantCode)
			}
			assertProxyCodexDefaultCalls(t, h.calls, "inventory", "aliases")
			if h.saved != nil {
				t.Fatal("resolution failure saved proxy config")
			}
			if h.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", h.stdout.String())
			}
			assertProxyCodexDefaultPrivateAbsent(t, err.Error(),
				privateReference, "provider-account-secret", "candidate-id-secret", "access-token-secret")
		})
	}
}

func TestRunProxyCodexDefaultInventoryFailureIsFixedAndPrivate(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.inventoryErr = errors.New("open /private/managed-home for private@example.test: token-secret")

	err := runProxyCodexDefaultWithDependencies(
		context.Background(), []string{"private@example.test"}, h.dependencies(),
	)
	if err == nil || err.Error() != "list Codex account inventory: unavailable" {
		t.Fatalf("error = %v, want fixed inventory error", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "inventory")
	if h.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", h.stdout.String())
	}
	assertProxyCodexDefaultPrivateAbsent(t, err.Error(),
		"/private/managed-home", "private@example.test", "token-secret")
}

func TestRunProxyCodexDefaultRejectsPartialInventorySourceFailure(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.inventory = privateProxyCodexDefaultInventory("opaque-key", "private@example.test", false, true)
	h.inventory.ExternalSources = []codexprov.ExternalSourceStatus{{
		Name:      "codexbar",
		ErrorCode: "open /private/managed-home for private@example.test: token-secret",
	}}

	err := runProxyCodexDefaultWithDependencies(
		context.Background(), []string{"private@example.test"}, h.dependencies(),
	)
	if err == nil || err.Error() != "list Codex account inventory: unavailable" {
		t.Fatalf("error = %v, want fixed inventory error", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "inventory")
	if h.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", h.stdout.String())
	}
	assertProxyCodexDefaultPrivateAbsent(t, err.Error(),
		"/private/managed-home", "private@example.test", "token-secret")
}

func TestRunProxyCodexDefaultAllowsOptionalAbsentExternalSource(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.inventory = privateProxyCodexDefaultInventory("opaque-key", "private@example.test", false, true)
	h.inventory.ExternalSources = []codexprov.ExternalSourceStatus{{
		Name: "codexbar", ErrorCode: "unavailable", OptionalAbsent: true,
	}}
	h.config = proxyCodexDefaultConfigWithFutureField(t)

	err := runProxyCodexDefaultWithDependencies(
		context.Background(), []string{"private@example.test"}, h.dependencies(),
	)
	if err != nil {
		t.Fatalf("runProxyCodexDefaultWithDependencies() error = %v", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "inventory", "aliases", "config", "save")
	if h.saved == nil || h.saved.CodexRoutingDefaultAccountKey != "opaque-key" {
		t.Fatalf("saved routing default = %q, want opaque-key", h.saved.CodexRoutingDefaultAccountKey)
	}
}

func TestListProxyCodexDefaultInventoryFailsClosedOnUnreadableManagedAuthority(t *testing.T) {
	_, fsys, path, before := newReadableManagedInventoryCoordinator(t)
	fsys.setFailing(true)

	inventory, home, err := listProxyCodexDefaultInventory(context.Background(), fsys)
	if !errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		t.Fatalf("inventory/home/error = %+v/%q/%v, want typed authority unavailable", inventory, home, err)
	}
	if len(inventory.Accounts) != 0 {
		t.Fatalf("inventory = %+v, want no permissive partial fallback", inventory)
	}
	after, readErr := fsys.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("authoritative inventory changed managed credential: %v", readErr)
	}
}

func TestRunProxyCodexDefaultAliasFailureIsFixedAndPrivate(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.inventory = privateProxyCodexDefaultInventory("opaque-key", "private@example.test", false, true)
	h.aliasErr = errors.New("parse alias-secret for provider-account-secret with token-secret")

	err := runProxyCodexDefaultWithDependencies(
		context.Background(), []string{"private@example.test"}, h.dependencies(),
	)
	if err == nil || err.Error() != "load Codex account aliases: unavailable" {
		t.Fatalf("error = %v, want fixed alias error", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "inventory", "aliases")
	if h.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", h.stdout.String())
	}
	assertProxyCodexDefaultPrivateAbsent(t, err.Error(),
		"private@example.test", "alias-secret", "provider-account-secret", "token-secret")
}

func TestRunProxyCodexDefaultConfigFailuresAreContextWrapped(t *testing.T) {
	loadErr := errors.New("load failure")
	saveErr := errors.New("save failure")
	tests := []struct {
		name       string
		loadErr    error
		saveErr    error
		wantCalls  []string
		wantErr    error
		wantPrefix string
	}{
		{
			name: "load", loadErr: loadErr,
			wantCalls: []string{"inventory", "aliases", "config"},
			wantErr:   loadErr, wantPrefix: "load config: ",
		},
		{
			name: "save", saveErr: saveErr,
			wantCalls: []string{"inventory", "aliases", "config", "save"},
			wantErr:   saveErr, wantPrefix: "save config: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyCodexDefaultHarness()
			h.inventory = privateProxyCodexDefaultInventory("opaque-key", "private@example.test", false, true)
			h.loadErr = tt.loadErr
			h.saveErr = tt.saveErr
			err := runProxyCodexDefaultWithDependencies(
				context.Background(), []string{"private@example.test"}, h.dependencies(),
			)
			if !errors.Is(err, tt.wantErr) || !strings.HasPrefix(err.Error(), tt.wantPrefix) {
				t.Fatalf("error = %v, want prefix %q wrapping %v", err, tt.wantPrefix, tt.wantErr)
			}
			assertProxyCodexDefaultCalls(t, h.calls, tt.wantCalls...)
			if h.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", h.stdout.String())
			}
		})
	}
}

func TestRunProxyCodexDefaultSourceHasReadOnlyAccountBoundary(t *testing.T) {
	data, err := os.ReadFile("proxy_codex_default.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "proxy_codex_default.go", data, 0)
	if err != nil {
		t.Fatal(err)
	}

	forbiddenExact := map[string]bool{
		"Adopt": true, "Activate": true, "ProjectActive": true, "Switch": true,
		"PersistCodexAccount": true, "StoreCQAccount": true, "UpsertAccount": true,
		"RemoveManaged": true, "SaveManaged": true, "WriteManaged": true,
		"WriteFile": true, "Rename": true, "Remove": true, "MkdirAll": true,
	}
	forbiddenFragments := []string{"CredentialControl", "RefreshControl", "SystemActivator"}
	ast.Inspect(file, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		if forbiddenExact[ident.Name] {
			t.Errorf("production source references forbidden identifier %q", ident.Name)
		}
		for _, fragment := range forbiddenFragments {
			if strings.Contains(ident.Name, fragment) {
				t.Errorf("production source references forbidden identifier %q", ident.Name)
			}
		}
		return true
	})

	source := string(data)
	for _, required := range []string{
		"fsutil.OSFileSystem", "codexprov.NewManagedStore", "codexprov.NewCredentialCoordinator",
		"coordinator.List", "codexprov.Registry",
		"AccountAliasIndex", "proxy.LoadConfig", "proxy.SaveConfig", "os.Stdout",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("production source does not reference required read-only boundary %q", required)
		}
	}
}

type proxyCodexDefaultAlias struct {
	alias string
	key   codexprov.AccountKey
}

func proxyCodexDefaultAliasIndex(t *testing.T, aliases ...proxyCodexDefaultAlias) codexprov.AccountAliasIndex {
	t.Helper()
	accounts := make([]any, 0, len(aliases))
	for _, alias := range aliases {
		accounts = append(accounts, map[string]any{
			"alias": alias.alias, "account_key": string(alias.key),
		})
	}
	data, err := json.Marshal(map[string]any{"schema_version": 3, "accounts": accounts})
	if err != nil {
		t.Fatal(err)
	}
	fsys := fsutil.NewMemFS()
	home := "/home/test"
	path := filepath.Join(home, ".codex", "accounts", "registry.json")
	if err := fsys.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := (codexprov.Registry{FS: fsys, Home: home}).AccountAliasIndex()
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func privateProxyCodexDefaultInventory(
	key codexprov.AccountKey,
	email string,
	unstable bool,
	routable bool,
) codexprov.Inventory {
	return codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
		privateProxyCodexDefaultAccount(key, email, unstable, routable),
	}}
}

func privateProxyCodexDefaultAccount(
	key codexprov.AccountKey,
	email string,
	unstable bool,
	routable bool,
) codexprov.LogicalAccount {
	account := codexprov.LogicalAccount{
		Key: key,
		Identity: codexprov.AccountIdentity{
			AccountID: "provider-account-secret",
			UserID:    "workspace-account-secret",
			Email:     email,
			PlanType:  "raw-error-token-secret",
			RecordKey: "record-key-secret",
		},
		Routable: routable,
		Unstable: unstable,
	}
	if routable {
		account.Candidates = []codexprov.CredentialCandidate{{
			Ref: codexprov.CandidateRef{AccountKey: key, CandidateID: "candidate-id-secret"},
			Credential: codexprov.CodexAccount{
				AccessToken: "access-token-secret", RefreshToken: "refresh-token-secret",
				IDToken: "id-token-secret", AccountID: "provider-account-secret",
			},
			Routable: true,
		}}
	}
	return account
}

func proxyCodexDefaultConfigWithFutureField(t *testing.T) *proxy.Config {
	t.Helper()
	var cfg proxy.Config
	if err := json.Unmarshal([]byte(`{
		"port":19443,
		"local_token":"local-token-secret",
		"pinned_claude_account":"private-pin@example.test",
		"codex_turn_routing":"observe",
		"codex_ws_turn_routing":"enforce",
		"codex_routing_default_account_key":"opaque-old-key",
		"future":{"nested":"future-token-secret"}
	}`), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func assertProxyCodexDefaultFutureField(t *testing.T, cfg *proxy.Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw["future"]), `{"nested":"future-token-secret"}`; got != want {
		t.Fatalf("future field = %s, want %s", got, want)
	}
}

func assertProxyCodexDefaultCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("dependency calls = %v, want %v", got, want)
	}
}

func assertProxyCodexDefaultPrivateAbsent(t *testing.T, output string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if value != "" && strings.Contains(output, value) {
			t.Fatalf("output exposed private fixture %q: %s", value, output)
		}
	}
}

func assertProxyCodexDefaultTerminalSafe(t *testing.T, output string, wantNewlines int) {
	t.Helper()
	if strings.Count(output, "\n") != wantNewlines {
		t.Fatalf("stdout newline count = %d, want %d: %q", strings.Count(output, "\n"), wantNewlines, output)
	}
	for _, value := range []byte(output) {
		if value < 0x20 && value != '\n' {
			t.Fatalf("stdout contains unsafe control byte %#x: %q", value, output)
		}
	}
}
