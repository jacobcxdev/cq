package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// setupPinTest isolates proxy config to a temp dir and optionally seeds an
// existing pin value. Returns the config dir path for inspection.
func setupPinTest(t *testing.T, existingPin string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if existingPin != "" {
		// Seed a config with the given pin so tests that need a pre-existing
		// value can verify it remains unchanged.
		cfg, err := proxy.LoadConfig()
		if err != nil {
			t.Fatalf("seed LoadConfig: %v", err)
		}
		cfg.PinnedClaudeAccount = existingPin
		if err := proxy.SaveConfig(cfg); err != nil {
			t.Fatalf("seed SaveConfig: %v", err)
		}
	}
	return filepath.Join(dir, "cq")
}

// loadPin reads the persisted pin from the proxy config under XDG_CONFIG_HOME.
func loadPin(t *testing.T) string {
	t.Helper()
	cfg, err := proxy.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg.PinnedClaudeAccount
}

func TestProxyPin(t *testing.T) {
	t.Run("no args no pin configured prints message", func(t *testing.T) {
		setupPinTest(t, "")
		// No pin is set; runProxyPin(nil) should return nil and print no-pin message.
		if err := runProxyPin(nil); err != nil {
			t.Fatalf("runProxyPin(nil) returned error: %v", err)
		}
	})

	t.Run("no args with pin configured prints pin", func(t *testing.T) {
		setupPinTest(t, "pinned@example.com")
		if err := runProxyPin(nil); err != nil {
			t.Fatalf("runProxyPin(nil) returned error: %v", err)
		}
		// Pin should remain unchanged.
		if got := loadPin(t); got != "pinned@example.com" {
			t.Errorf("pin = %q, want %q", got, "pinned@example.com")
		}
	})

	t.Run("--clear clears existing pin", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		if err := runProxyPin([]string{"claude", "--clear"}); err != nil {
			t.Fatalf("runProxyPin(--clear) returned error: %v", err)
		}
		if got := loadPin(t); got != "" {
			t.Errorf("pin after --clear = %q, want empty", got)
		}
	})

	t.Run("clear (bare word) returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"claude", "clear"})
		if err == nil {
			t.Fatal("runProxyPin(clear) expected error, got nil")
		}
		if !strings.Contains(err.Error(), "clear") {
			t.Errorf("error %q does not mention 'clear'", err.Error())
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("remove (bare word) returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"claude", "remove"})
		if err == nil {
			t.Fatal("runProxyPin(remove) expected error, got nil")
		}
		if !strings.Contains(err.Error(), "remove") {
			t.Errorf("error %q does not mention 'remove'", err.Error())
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("CLEAR (case-insensitive) returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"claude", "CLEAR"})
		if err == nil {
			t.Fatal("runProxyPin(CLEAR) expected error, got nil")
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("REMOVE (case-insensitive) returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"claude", "REMOVE"})
		if err == nil {
			t.Fatal("runProxyPin(REMOVE) expected error, got nil")
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("--help returns help and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"--help"})
		if err != nil {
			t.Fatalf("runProxyPin(--help) returned error: %v", err)
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("other flag-like arg returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"claude", "--unknown"})
		if err == nil {
			t.Fatal("runProxyPin(--unknown) expected error, got nil")
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("valid email sets pin", func(t *testing.T) {
		setupPinTest(t, "")
		if err := runProxyPin([]string{"claude", "new@example.com"}); err != nil {
			t.Fatalf("runProxyPin(email) returned error: %v", err)
		}
		if got := loadPin(t); got != "new@example.com" {
			t.Errorf("pin = %q, want %q", got, "new@example.com")
		}
	})

	t.Run("UUID-like value sets pin", func(t *testing.T) {
		setupPinTest(t, "")
		uuid := "550e8400-e29b-41d4-a716-446655440000"
		if err := runProxyPin([]string{"claude", uuid}); err != nil {
			t.Fatalf("runProxyPin(uuid) returned error: %v", err)
		}
		if got := loadPin(t); got != uuid {
			t.Errorf("pin = %q, want %q", got, uuid)
		}
	})

	t.Run("multiple args returns usage error", func(t *testing.T) {
		setupPinTest(t, "")
		err := runProxyPin([]string{"claude", "one@example.com", "two@example.com"})
		if err == nil {
			t.Fatal("runProxyPin with multiple args expected error, got nil")
		}
	})
}

func TestProxyPinClaudeProviderSetsClaudePin(t *testing.T) {
	setupPinTest(t, "")

	if err := runProxyPin([]string{"claude", "new@example.com"}); err != nil {
		t.Fatalf("runProxyPin(claude email) returned error: %v", err)
	}
	if got := loadPin(t); got != "new@example.com" {
		t.Fatalf("Claude pin = %q, want new@example.com", got)
	}
}

func TestProxyPinClaudeUsesInjectedConfigAuthority(t *testing.T) {
	h := newProxyCodexDefaultHarness()

	err := runProxyPinWithDependencies(
		context.Background(),
		[]string{"claude", "new@example.test"},
		h.dependencies(),
	)

	if err != nil {
		t.Fatal(err)
	}
	if h.saved == nil || h.saved.PinnedClaudeAccount != "new@example.test" {
		t.Fatalf("saved Claude pin = %#v", h.saved)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "config", "save")
}

func TestProxyPinRejectsAmbiguousAccountWithoutProvider(t *testing.T) {
	setupPinTest(t, "existing@example.com")

	err := runProxyPin([]string{"new@example.com"})

	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("runProxyPin(ambiguous account) error = %v, want provider guidance", err)
	}
	if got := loadPin(t); got != "existing@example.com" {
		t.Fatalf("Claude pin changed to %q", got)
	}
}

func TestProxyPinStatusReportsBothProviders(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.config.PinnedClaudeAccount = "claude@example.test"
	h.config.CodexRoutingPinnedAccountKey = "account-c"

	if err := runProxyPinWithDependencies(context.Background(), nil, h.dependencies()); err != nil {
		t.Fatal(err)
	}
	assertProxyCodexDefaultCalls(t, h.calls, "config")
	want := "Claude proxy pin: \"claude@example.test\"\nCodex proxy pin: \"account-c\"\n"
	if got := h.stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestProxyPinRejectsProviderlessClearWithoutMutation(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.config.PinnedClaudeAccount = "claude@example.test"
	h.config.CodexRoutingPinnedAccountKey = "account-c"

	err := runProxyPinWithDependencies(context.Background(), []string{"--clear"}, h.dependencies())

	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("error = %v, want provider guidance", err)
	}
	assertProxyCodexDefaultCalls(t, h.calls)
	if h.saved != nil {
		t.Fatalf("providerless clear saved config: %#v", h.saved)
	}
}

func TestProxyPinCodexResolvesReferenceAndPreservesRoutingPolicy(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.inventory = privateProxyCodexDefaultInventory("account-c", "jacobclayden@example.test", false, true)
	h.config = proxyCodexDefaultConfigWithFutureField(t)
	h.config.PinnedClaudeAccount = "claude@example.test"
	h.config.CodexRoutingDefaultAccountKey = "account-a"
	h.config.CodexRoutingAccountKeys = []codexprov.AccountKey{"account-a", "account-b"}

	err := runProxyPinWithDependencies(
		context.Background(),
		[]string{"codex", "jacobclayden@example.test"},
		h.dependencies(),
	)

	if err != nil {
		t.Fatal(err)
	}
	if h.saved == nil || h.saved.CodexRoutingPinnedAccountKey != "account-c" {
		t.Fatalf("saved Codex pin = %#v, want account-c", h.saved)
	}
	if h.saved.PinnedClaudeAccount != "claude@example.test" ||
		h.saved.CodexRoutingDefaultAccountKey != "account-a" ||
		!reflect.DeepEqual(h.saved.CodexRoutingAccountKeys, []codexprov.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("unrelated routing policy changed: %#v", h.saved)
	}
	assertProxyCodexDefaultFutureField(t, h.saved)
	assertProxyCodexDefaultCalls(t, h.calls, "inventory", "aliases", "config", "save")
	if !strings.Contains(h.stdout.String(), "Restart proxy") {
		t.Fatalf("output = %q, want restart requirement", h.stdout.String())
	}
}

func TestProxyPinCodexResolvesAliasAndOpaqueKey(t *testing.T) {
	for _, tt := range []struct {
		name      string
		reference string
		aliases   []proxyCodexDefaultAlias
	}{
		{name: "alias", reference: "work", aliases: []proxyCodexDefaultAlias{{alias: "work", key: "account-c"}}},
		{name: "opaque key", reference: "account-c"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyCodexDefaultHarness()
			h.inventory = privateProxyCodexDefaultInventory("account-c", "private@example.test", false, true)
			h.aliases = proxyCodexDefaultAliasIndex(t, tt.aliases...)

			if err := runProxyPinWithDependencies(context.Background(), []string{"codex", tt.reference}, h.dependencies()); err != nil {
				t.Fatal(err)
			}
			if h.saved == nil || h.saved.CodexRoutingPinnedAccountKey != "account-c" {
				t.Fatalf("saved Codex pin = %#v", h.saved)
			}
		})
	}
}

func TestProxyPinCodexStatusAndClearUseOnlyConfig(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		h := newProxyCodexDefaultHarness()
		h.config.CodexRoutingPinnedAccountKey = "account-c"
		h.inventoryErr = errors.New("private inventory error")

		if err := runProxyPinWithDependencies(context.Background(), []string{"codex"}, h.dependencies()); err != nil {
			t.Fatal(err)
		}
		assertProxyCodexDefaultCalls(t, h.calls, "config")
		if got, want := h.stdout.String(), "Codex proxy pin: \"account-c\"\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("clear", func(t *testing.T) {
		h := newProxyCodexDefaultHarness()
		h.config.CodexRoutingPinnedAccountKey = "account-c"
		h.config.CodexRoutingDefaultAccountKey = "account-a"
		h.config.CodexRoutingAccountKeys = []codexprov.AccountKey{"account-a", "account-b"}

		if err := runProxyPinWithDependencies(context.Background(), []string{"codex", "--clear"}, h.dependencies()); err != nil {
			t.Fatal(err)
		}
		assertProxyCodexDefaultCalls(t, h.calls, "config", "save")
		if h.saved == nil || h.saved.CodexRoutingPinnedAccountKey != "" || h.saved.CodexRoutingDefaultAccountKey != "account-a" || !reflect.DeepEqual(h.saved.CodexRoutingAccountKeys, []codexprov.AccountKey{"account-a", "account-b"}) {
			t.Fatalf("cleared config = %#v", h.saved)
		}
	})
}

func TestProxyPinCodexRejectsMalformedArgumentsBeforeDependencies(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "--clear", "account-c"},
		{"codex", "account-a", "account-c"},
		{"codex", "--unknown"},
	} {
		h := newProxyCodexDefaultHarness()
		err := runProxyPinWithDependencies(context.Background(), args, h.dependencies())
		if err == nil || !strings.Contains(err.Error(), "usage: cq proxy pin codex") {
			t.Fatalf("args %q error = %v", args, err)
		}
		assertProxyCodexDefaultCalls(t, h.calls)
	}
}

func TestProxyPinCodexInventoryFailureIsPrivate(t *testing.T) {
	h := newProxyCodexDefaultHarness()
	h.inventoryErr = errors.New("private@example.test token-secret")

	err := runProxyPinWithDependencies(context.Background(), []string{"codex", "private@example.test"}, h.dependencies())

	if err == nil || err.Error() != "list Codex account inventory: unavailable" {
		t.Fatalf("error = %v", err)
	}
	assertProxyCodexDefaultPrivateAbsent(t, err.Error(), "private@example.test", "token-secret")
	assertProxyCodexDefaultCalls(t, h.calls, "inventory")
}

// TestProxyPinNoConfigDirCreation verifies that read-only operations (show
// current pin) do not fail when XDG_CONFIG_HOME is set to a non-existent path.
// The LoadConfig path will create the directory on first run, so this test
// just verifies no crash occurs on a fresh temp dir with no prior config.
func TestProxyPinFreshConfig(t *testing.T) {
	dir := t.TempDir()
	// Point at a sub-directory that doesn't exist yet.
	configHome := filepath.Join(dir, "new-config")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	// LoadConfig will create the dir and generate a default config.
	if err := runProxyPin(nil); err != nil {
		t.Fatalf("runProxyPin(nil) on fresh config: %v", err)
	}

	// Verify the config file was created.
	if _, err := os.Stat(filepath.Join(configHome, "cq", "proxy.json")); err != nil {
		t.Errorf("proxy.json not created: %v", err)
	}
}

func TestProxyPinPreservesFutureConfigFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	configDir := filepath.Join(dir, "cq")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "proxy.json")
	if err := os.WriteFile(path, []byte(`{
		"local_token":"token",
		"codex_turn_routing":"observe",
		"codex_ws_turn_routing":"enforce",
		"future":{"nested":true}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runProxyPin([]string{"claude", "person@example.test"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var future struct {
		Nested bool `json:"nested"`
	}
	if err := json.Unmarshal(raw["future"], &future); err != nil || !future.Nested {
		t.Fatalf("future field = %s, want preserved: %v", raw["future"], err)
	}
	if string(raw["codex_turn_routing"]) != `"observe"` || string(raw["codex_ws_turn_routing"]) != `"enforce"` {
		t.Fatalf("routing modes changed: %s", data)
	}
}

func TestProxyConfigReloadAppliesPinButNotRoutingModes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	configDir := filepath.Join(dir, "cq")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "proxy.json")
	if err := os.WriteFile(path, []byte(`{
		"local_token":"token",
		"pinned_claude_account":"person@example.test",
		"codex_turn_routing":"observe",
		"codex_ws_turn_routing":"enforce",
		"future":{"keep":true}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	selector := proxy.NewPinnedClaudeSelector(nil, nil, "", nil)
	runtime := &proxy.CodexRoutingRuntime{
		HTTP:      proxy.CodexModeStatus{Configured: proxy.CodexRoutingOff, Effective: proxy.CodexRoutingOff},
		WebSocket: proxy.CodexModeStatus{Configured: proxy.CodexRoutingOff, Effective: proxy.CodexRoutingOff},
	}
	reloadProxyConfig(selector, runtime)
	if selector.Pin() != "person@example.test" {
		t.Fatalf("pin = %q, want reloaded pin", selector.Pin())
	}
	if runtime.HTTP.Configured != proxy.CodexRoutingOff || runtime.WebSocket.Configured != proxy.CodexRoutingOff {
		t.Fatalf("routing modes hot-reloaded: %+v", runtime)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"future":{"keep":true}`) {
		t.Fatalf("reload changed future config: %s", data)
	}
}
