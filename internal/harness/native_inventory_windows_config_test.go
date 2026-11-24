package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNativeAppManagedConfigWarningRequiresOnlyKnownField(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		message string
		wantErr bool
	}{
		{"known", "Codex is ignoring 1 unrecognized configuration setting.\n  user (config.toml): `computer_use.windows.always_allowed_app_ids` is ignored.", false},
		{"other", "Codex is ignoring 1 unrecognized configuration setting.\n  user (config.toml): `bogus` is ignored.", true},
		{"both", "Codex is ignoring 2 unrecognized configuration settings.\n  user (config.toml): `bogus` is ignored.\n  user (config.toml): `computer_use.windows.always_allowed_app_ids` is ignored.", true},
		{"missing", "", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			output := &nativeConfigOutput{}
			line, err := json.Marshal(map[string]any{"fields": map[string]string{"message": scenario.message}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := output.Write(append(line, '\n')); err != nil {
				t.Fatal(err)
			}
			if gotErr := output.validateAppManagedWarning(); (gotErr != nil) != scenario.wantErr {
				t.Fatalf("warning validation error = %v, want error %v", gotErr, scenario.wantErr)
			}
		})
	}
}

func TestNativeInventoryWindowsAppManagedConfig(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Codex configuration")
	}
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := t.TempDir()
	config := "[computer_use.windows]\nalways_allowed_app_ids = [\"com.example.fixture\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (NativeInventory{Executable: executable}).Snapshot(context.Background(), cwd); err != nil {
		t.Fatalf("app-managed Windows setting blocked native inventory: %v", err)
	}
	if after, err := os.ReadFile(filepath.Join(home, "config.toml")); err != nil || string(after) != config {
		t.Fatalf("native inventory changed Windows computer-use config: %v", err)
	}
}

func TestNativePluginInventoryWindowsAppManagedConfig(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Codex configuration")
	}
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[computer_use.windows]\nalways_allowed_app_ids = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (PluginCLI{Executable: executable}).Installed(context.Background(), cwd); err != nil {
		t.Fatalf("app-managed Windows setting blocked native plugin inventory: %v", err)
	}
}

func TestNativeInventoryWindowsRejectsOtherUnknownConfig(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Codex configuration")
	}
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := t.TempDir()
	config := "[computer_use.windows]\nalways_allowed_app_ids = []\n[unrecognized_test_field]\nvalue = 1\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if runtime.GOOS != "windows" {
		t.Skip("Windows Codex configuration")
	}
	executable := os.Getenv("CQ_TEST_NATIVE_INVENTORY_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_NATIVE_INVENTORY_CLI to an installed Codex CLI")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[computer_use.windows\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (NativeInventory{Executable: executable}).Snapshot(context.Background(), cwd); err == nil {
		t.Fatal("malformed configuration was accepted")
	}
}
