package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		if err := runProxyPin([]string{"--clear"}); err != nil {
			t.Fatalf("runProxyPin(--clear) returned error: %v", err)
		}
		if got := loadPin(t); got != "" {
			t.Errorf("pin after --clear = %q, want empty", got)
		}
	})

	t.Run("clear (bare word) returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"clear"})
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
		err := runProxyPin([]string{"remove"})
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
		err := runProxyPin([]string{"CLEAR"})
		if err == nil {
			t.Fatal("runProxyPin(CLEAR) expected error, got nil")
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("REMOVE (case-insensitive) returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"REMOVE"})
		if err == nil {
			t.Fatal("runProxyPin(REMOVE) expected error, got nil")
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("unknown flag returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"--help"})
		if err == nil {
			t.Fatal("runProxyPin(--help) expected error, got nil")
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("other flag-like arg returns error and leaves pin unchanged", func(t *testing.T) {
		setupPinTest(t, "user@example.com")
		err := runProxyPin([]string{"--unknown"})
		if err == nil {
			t.Fatal("runProxyPin(--unknown) expected error, got nil")
		}
		if got := loadPin(t); got != "user@example.com" {
			t.Errorf("pin changed to %q, want %q", got, "user@example.com")
		}
	})

	t.Run("valid email sets pin", func(t *testing.T) {
		setupPinTest(t, "")
		if err := runProxyPin([]string{"new@example.com"}); err != nil {
			t.Fatalf("runProxyPin(email) returned error: %v", err)
		}
		if got := loadPin(t); got != "new@example.com" {
			t.Errorf("pin = %q, want %q", got, "new@example.com")
		}
	})

	t.Run("UUID-like value sets pin", func(t *testing.T) {
		setupPinTest(t, "")
		uuid := "550e8400-e29b-41d4-a716-446655440000"
		if err := runProxyPin([]string{uuid}); err != nil {
			t.Fatalf("runProxyPin(uuid) returned error: %v", err)
		}
		if got := loadPin(t); got != uuid {
			t.Errorf("pin = %q, want %q", got, uuid)
		}
	})

	t.Run("multiple args returns usage error", func(t *testing.T) {
		setupPinTest(t, "")
		err := runProxyPin([]string{"one@example.com", "two@example.com"})
		if err == nil {
			t.Fatal("runProxyPin with multiple args expected error, got nil")
		}
	})
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
