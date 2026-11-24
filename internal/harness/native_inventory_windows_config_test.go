//go:build windows

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	var runErr error
	client := NativeInventory{Executable: executable, Run: func(ctx context.Context, executable, cwd string, output io.Writer) error {
		runErr = runNativeCurationInventory(ctx, executable, cwd, output)
		return runErr
	}}
	if _, err := client.Snapshot(context.Background(), cwd); err != nil {
		t.Fatalf("app-managed Windows setting blocked native inventory: %v (preflight: %s; runner: %s)", err, nativeWindowsPreflightCategory(executable, cwd), nativeWindowsRunFailureCategory(runErr))
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
	var runErr error
	client := PluginCLI{Executable: executable, Run: func(ctx context.Context, executable, cwd string, output io.Writer) error {
		runErr = runNativePluginInventory(ctx, executable, cwd, output)
		return runErr
	}}
	if _, err := client.Installed(context.Background(), cwd); err != nil {
		t.Fatalf("app-managed Windows setting blocked native plugin inventory: %v (preflight: %s; runner: %s)", err, nativeWindowsPreflightCategory(executable, cwd), nativeWindowsRunFailureCategory(runErr))
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

func nativeWindowsRunFailureCategory(err error) string {
	if err == nil {
		return "runner completed; response decoding failed"
	}
	for _, category := range []string{
		"native app-server Windows configuration warning unavailable",
		"native app-server ignored unrelated configuration",
		"native app-server strict configuration unavailable",
		"native inventory request rejected",
		"invalid native inventory response",
		"invalid native inventory protocol JSON",
		"incomplete native inventory protocol",
	} {
		if strings.Contains(err.Error(), category) {
			return category
		}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("native process exited with status %d", exit.ExitCode())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "native process timed out"
	}
	return fmt.Sprintf("other runner error type %T", err)
}
