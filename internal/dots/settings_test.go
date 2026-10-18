package dots

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func catalogueTestSettings(t *testing.T) (string, map[string]any) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "settings.json")
	value := map[string]any{
		"runtime_root": filepath.Join(root, "runtime"), "app_path": filepath.Join(root, "App.app"),
		"app_version": "1.2.3", "app_build": "456", "asar_sha256": strings.Repeat("a", 64), "module_asset": "app-shared-example.js",
		"system_auth": filepath.Join(root, "credentials", "auth.json"), "system_alias": "11111111",
		"secondary_alias": "22222222", "secondary_selector": "synthetic@example.invalid",
		"dots": map[string]any{
			"Primary":   map[string]any{"owner_alias": "11111111", "profile_id": "profile-primary", "root_thread_id": "root-primary"},
			"Secondary": map[string]any{"owner_alias": "22222222", "profile_id": "profile-secondary", "root_thread_id": "root-secondary"},
		},
	}
	catalogueTestWrite(t, path, value)
	return path, value
}

func TestSettingsPrivateLegacyRoundTripAndDefaults(t *testing.T) {
	path, _ := catalogueTestSettings(t)
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.DebugPort != 0 || s.IntegrationMode != "isolated" || s.Dots["Primary"].DisplayName != "Primary" || s.CQAccounts != filepath.Join(filepath.Dir(s.SystemAuth), "accounts") {
		t.Fatalf("incorrect defaults: %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("revalidation rejected legacy state: %v", err)
	}
	if err := SaveJSON(path, s); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "display_name") {
		t.Fatal("legacy schema acquired a partial stable identity")
	}
	if _, err := LoadSettings(path); err != nil {
		t.Fatalf("saved settings did not reload: %v", err)
	}
	t.Setenv("CQ_DOTS_CONFIG", path)
	if _, err := LoadSettings(""); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsRejectMalformedAndUnsupportedFields(t *testing.T) {
	path, original := catalogueTestSettings(t)
	for _, change := range []map[string]any{
		{"runtime_root": "relative"}, {"system_alias": "invalid"}, {"secondary_alias": "11111111"},
		{"secondary_selector": ""}, {"system_account_id": nil}, {"asar_sha256": "invalid"},
		{"module_asset": "../app-shared-example.js"}, {"debug_port": true}, {"debug_port": 65536},
		{"unknown": true}, {"dots": nil}, {"dots": []any{}}, {"integration_mode": ""}, {"integration_mode": "normal"},
		{"cq_accounts": ""}, {"cq_accounts": nil}, {"account_source": ""},
	} {
		value := map[string]any{}
		for key, item := range original {
			value[key] = item
		}
		for key, item := range change {
			value[key] = item
		}
		catalogueTestWrite(t, path, value)
		if _, err := LoadSettings(path); err == nil {
			t.Fatalf("accepted invalid settings: %v", change)
		}
	}
	for _, raw := range []string{"synthetic-private-value", `{} {}`, `null`, `[]`} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadSettings(path)
		if err == nil || strings.Contains(err.Error(), "synthetic-private-value") {
			t.Fatalf("unsafe JSON error: %v", err)
		}
	}
}

func TestSettingsRejectUnsafePermissionsAndRuntimeState(t *testing.T) {
	path, value := catalogueTestSettings(t)
	for _, mode := range []os.FileMode{0o644, 0o640, 0o400, 0o700} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettings(path); err == nil {
			t.Fatalf("accepted file mode %o", mode)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(path)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("accepted public parent")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(link); err == nil {
		t.Fatal("followed config symlink")
	}
	runtime := value["runtime_root"].(string)
	if err := os.Mkdir(runtime, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("accepted public runtime")
	}
	if err := os.Chmod(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(runtime, "user-data")
	if err := os.Symlink(root, state); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRuntimePaths(runtime); err == nil {
		t.Fatal("followed mutable state symlink")
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, "launch.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRuntimePaths(runtime); err == nil {
		t.Fatal("accepted public launch state")
	}
}

func TestSettingsStableIdentityAndProtectedPaths(t *testing.T) {
	path, value := catalogueTestSettings(t)
	owner, profile := "synthetic-owner", "synthetic-profile"
	handle := StableHandle(owner, profile)
	dot := map[string]any{"owner_alias": AccountAlias(owner), "owner_account_id": owner, "profile_id": profile, "root_thread_id": "root", "display_name": "Same"}
	value["dots"] = map[string]any{handle: dot}
	catalogueTestWrite(t, path, value)
	s, err := LoadSettings(path)
	if err != nil || s.Dots[handle].DisplayName != "Same" {
		t.Fatalf("stable identity rejected: %+v %v", s, err)
	}
	dot["display_name"], dot["root_thread_id"] = "Renamed", "new-root"
	catalogueTestWrite(t, path, value)
	if _, err := LoadSettings(path); err != nil {
		t.Fatal(err)
	}
	delete(dot, "display_name")
	catalogueTestWrite(t, path, value)
	if _, err := LoadSettings(path); err != nil {
		t.Fatalf("owner identity with default display name rejected: %v", err)
	}
	dot["owner_account_id"] = "foreign-owner"
	catalogueTestWrite(t, path, value)
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("accepted owner identity drift")
	}
	value["dots"] = map[string]any{}
	for _, runtime := range []string{filepath.Dir(value["system_auth"].(string)), filepath.Join(value["app_path"].(string), "runtime")} {
		value["runtime_root"] = runtime
		catalogueTestWrite(t, path, value)
		if _, err := LoadSettings(path); err == nil {
			t.Fatalf("accepted protected runtime %s", runtime)
		}
	}
}

func TestSettingsPythonCatalogueRevision(t *testing.T) {
	s := &Settings{Dots: map[string]Dot{"dot-one": {OwnerAlias: "11111111", ProfileID: "profile-1", RootThreadID: "root-1", DisplayName: "Café 😀<\u007f"}}}
	// Independently generated by Python's sorted compact json.dumps with ensure_ascii=True.
	const want = "fec2dd26a340d08665e14ece93ce28ab00f979b7642f0b9c5a529a97448dbc94"
	if got := s.CatalogueRevision(); got != want {
		t.Fatalf("revision=%s want=%s", got, want)
	}
	encoded, err := pythonJSON(map[string]any{"array": []any{"😀", true, nil}}, true)
	if err != nil || string(encoded) != `{"array":["\ud83d\ude00",true,null]}` {
		t.Fatalf("Python JSON=%s err=%v", encoded, err)
	}
}

func TestPrivateStateAtomicPermissionsAndSymlinkRejection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "value.json")
	if err := SaveJSON(path, map[string]any{"first": true}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := SaveJSON(path, make(chan bool)); err == nil {
		t.Fatal("accepted unencodable state")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed write changed existing state")
	}
	if err := SaveJSON(path, map[string]any{"second": true}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("state permissions are not private")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("temporary file was retained")
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(link, map[string]any{}); err == nil {
		t.Fatal("replaced state symlink")
	}
	var value map[string]any
	data, _ := os.ReadFile(path)
	if json.Unmarshal(data, &value) != nil || value["second"] != true {
		t.Fatal("symlink write changed target")
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDirectory(root); err == nil {
		t.Fatal("accepted public existing state directory")
	}
}
