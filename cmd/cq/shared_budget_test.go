package main

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestSharedBudgetSettingsAndCheckSelection(t *testing.T) {
	fsys, dir := privateSharedBudgetConfig(t)
	settingsPath := filepath.Join(dir, "opencode-go.json")
	if err := fsys.WriteFile(settingsPath, []byte(`{"email":"registered@example.com","extra_usage":false,"allow_fallback":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	env := func(string) string { return "" }
	id := modelregistry.ProviderOpenCodeGo
	routes := sharedBudgetProxyRoutes(fsys, env, dir)
	if !routes[id].AllowFallback {
		t.Fatal("explicit fallback permission missing")
	}
	if !routes[id].OmitStreamOptions {
		t.Fatal("Go route must omit native Responses stream options")
	}
	if !routes[id].OmitEmptyCallIDOutputs {
		t.Fatal("Go route must omit tool outputs without call IDs")
	}
	_, checks := sharedBudgetChecks(fsys, env, dir, nil)
	if len(checks) != 0 {
		t.Fatal("default check included an unconfigured subscription")
	}
	providers, checks := sharedBudgetChecks(fsys, env, dir, []string{"zai", string(id)})
	if !slices.Equal(providers, []string{"zai"}) || len(checks) != 1 {
		t.Fatalf("selection: providers %v, checks %d", providers, len(checks))
	}
	report := checks[0].Report
	if report.Email != "registered@example.com" || report.ProviderName != "OpenCode" ||
		report.Plan != "Go" || report.Icon != "\U000F0C15" || report.ExtraUsage == nil || report.ExtraUsage.Enabled {
		t.Fatalf("display metadata = %+v", report)
	}
	if got := checks[0].Fetch(context.Background()); len(got) != 0 {
		t.Fatalf("windows with no key = %+v", got)
	}
	if err := fsys.WriteFile(settingsPath, []byte(`{"auth":"api_key","allow_fallback":true,"extra_usgae":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	route := sharedBudgetProxyRoutes(fsys, func(string) string { return "headless-key" }, dir)[id]
	if route.AllowFallback {
		t.Fatal("invalid settings allowed fallback")
	}
	if credential, err := route.Credential(context.Background()); err == nil || credential.Bearer != "" {
		t.Fatal("invalid settings changed auth selection")
	}
}

func TestSharedBudgetKeyPrefersEnvironmentAndRequiresPrivateFile(t *testing.T) {
	fsys, dir := privateSharedBudgetConfig(t)
	path := filepath.Join(dir, "opencode-go.key")
	if err := fsys.WriteFile(path, []byte("file-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := func(string) string { return "" }
	id := modelregistry.ProviderOpenCodeGo
	get := func(env func(string) string) (string, error) {
		return sharedBudgetKey(fsys, env, dir, id, "OPENCODE_API_KEY")
	}
	if key, err := get(env); err != nil || key != "file-key" {
		t.Fatal("private key file not selected")
	}
	if key, err := get(func(string) string { return "env-key" }); err != nil || key != "env-key" {
		t.Fatal("environment override not selected")
	}
	if err := fsys.WriteFile(filepath.Join(dir, "other.key"), []byte("file-key"), 0644); err != nil {
		t.Fatal(err)
	}
	if key, err := sharedBudgetKey(fsys, env, dir, "other", "OTHER_KEY"); key != "" || err == nil {
		t.Fatal("non-private key accepted")
	}
}

func TestSharedBudgetSettingsAbsentInInheritedConfigDirectory(t *testing.T) {
	fsys, dir := privateSharedBudgetConfig(t)
	if err := fsys.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	id := modelregistry.ProviderOpenCodeGo
	if got := loadSharedBudgetSettings(fsys, dir, id); got.invalid {
		t.Fatal("missing optional settings blocked credential refresh")
	}
	if err := fsys.WriteFile(filepath.Join(dir, string(id)+".json"), []byte(`{"allow_fallback":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadSharedBudgetSettings(fsys, dir, id); !got.invalid || got.AllowFallback {
		t.Fatal("settings under non-private directory were trusted")
	}
}

func privateSharedBudgetConfig(t *testing.T) (*fsutil.MemFS, string) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	dir := filepath.Join(string(filepath.Separator), "config")
	if runtime.GOOS == "windows" {
		dir = filepath.Join("C:\\", dir)
	}
	if err := fsys.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return fsys, dir
}
