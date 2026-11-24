//go:build windows

package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeInventoryWindowsAppManagedConfig(t *testing.T) {
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := privateWindowsNativeTestHome(t)
	config := "[computer_use.windows]\nalways_allowed_app_ids = [\"com.example.fixture\"]\n"
	writePrivateWindowsNativeConfig(t, home, config)
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (NativeInventory{Executable: executable}).Snapshot(context.Background(), cwd); err != nil {
		t.Fatalf("app-managed Windows setting blocked native inventory: %v (%s)", err, nativeWindowsPreflightCategory(executable, cwd))
	}
	if after, err := os.ReadFile(filepath.Join(home, "config.toml")); err != nil || string(after) != config {
		t.Fatalf("native inventory changed Windows computer-use config: %v", err)
	}
}

func TestNativePluginInventoryWindowsAppManagedConfig(t *testing.T) {
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := privateWindowsNativeTestHome(t)
	writePrivateWindowsNativeConfig(t, home, "[computer_use.windows]\nalways_allowed_app_ids = []\n")
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (PluginCLI{Executable: executable}).Installed(context.Background(), cwd); err != nil {
		t.Fatalf("app-managed Windows setting blocked native plugin inventory: %v (%s)", err, nativeWindowsPreflightCategory(executable, cwd))
	}
}

func TestNativeInventoryWindowsRejectsOtherUnknownConfig(t *testing.T) {
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := privateWindowsNativeTestHome(t)
	config := "[computer_use.windows]\nalways_allowed_app_ids = []\n[unrecognized_test_field]\nvalue = 1\n"
	writePrivateWindowsNativeConfig(t, home, config)
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (NativeInventory{Executable: executable}).Snapshot(context.Background(), cwd); err == nil {
		t.Fatal("unrelated unknown configuration field was accepted")
	}
}

func TestNativeInventoryWindowsRejectsMalformedConfig(t *testing.T) {
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := privateWindowsNativeTestHome(t)
	writePrivateWindowsNativeConfig(t, home, "[computer_use.windows\n")
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (NativeInventory{Executable: executable}).Snapshot(context.Background(), cwd); err == nil {
		t.Fatal("malformed configuration was accepted")
	}
}

func privateWindowsNativeTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	setWindowsHarnessACL(t, home, false)
	return home
}

func writePrivateWindowsNativeConfig(t *testing.T, home, config string) {
	t.Helper()
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	setWindowsHarnessACL(t, path, false)
}

func nativeWindowsPreflightCategory(executable, cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	allowed, err := nativeAppServerConfigMode(ctx, executable, cwd)
	if err != nil {
		return "strict configuration preflight failed"
	}
	if allowed {
		return "known app-managed field accepted by preflight"
	}
	return "strict configuration accepted by preflight"
}
