package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestCLIV2EnvironmentIsLazy(t *testing.T) {
	t.Setenv("HOME", "relative-home")
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	got, ok := cli.Help("codex account list")
	if !ok || got == "" {
		t.Fatal("help required valid environment")
	}
}
func TestCLIV2EnvironmentCodexVersionWithoutHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("HOME", "")
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"client_version":"9.8.7","models":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := cachedCodexClientVersion(); got != "9.8.7" {
		t.Fatalf("version = %q, want explicit cache version", got)
	}
}

type modelNoHomeFS struct {
	fsutil.FileSystem
	t     *testing.T
	reads []string
}

func (f *modelNoHomeFS) UserHomeDir() (string, error) {
	f.t.Fatal("read unused native home")
	return "", errors.New("home unavailable")
}
func (f *modelNoHomeFS) ReadFile(path string) ([]byte, error) {
	f.reads = append(f.reads, path)
	return f.FileSystem.ReadFile(path)
}
func TestClientPathsModelReaderSelectsOnlyProvider(t *testing.T) {
	for _, provider := range []modelregistry.Provider{modelregistry.ProviderCodex, modelregistry.ProviderAnthropic} {
		fs := &modelNoHomeFS{FileSystem: fsutil.NewMemFS(), t: t}
		cwd := t.TempDir()
		key := "CODEX_HOME"
		if provider == modelregistry.ProviderAnthropic {
			key = "CLAUDE_CONFIG_DIR"
		}
		deps := modelsDeps{FS: fs, CWD: cwd, Env: func(name string) string {
			if name != key {
				t.Fatalf("read other provider env %s", name)
			}
			return "client"
		}}
		if _, err := loadCachedNativeEntries(deps, provider); err != nil {
			t.Fatal(err)
		}
		if len(fs.reads) != 1 || !strings.HasPrefix(fs.reads[0], filepath.Join(cwd, "client")) {
			t.Fatalf("read paths: %v", fs.reads)
		}
	}
}
func TestClientPathsReaderPublisherAgreeAndKeepNativeCredentials(t *testing.T) {
	fs := fsutil.NewMemFS()
	base := t.TempDir()
	home, cwd := filepath.Join(base, "native"), filepath.Join(base, "start")
	credentialPaths := []string{filepath.Join(home, ".codex", "auth.json"), filepath.Join(home, ".codex", "accounts", "registry.json"), filepath.Join(home, ".claude", ".credentials.json")}
	for _, path := range credentialPaths {
		if err := fs.WriteFile(path, []byte("native-owned"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cacheDir := "relative cache"
	env := func(name string) string {
		switch name {
		case "CODEX_HOME", "CLAUDE_CONFIG_DIR":
			return cacheDir
		}
		return ""
	}
	pipeline, err := newRegistryPipeline(registryPipelineOptions{FS: fs, HomeDir: home, CWD: cwd, Roots: userdirs.Roots{Config: filepath.Join(base, "config")}, HTTPClient: http.DefaultClient, ClaudeToken: func() (string, error) { t.Fatal("called credential provider"); return "", nil }, CodexToken: func() (string, error) { t.Fatal("called credential provider"); return "", nil }, Env: env, Stderr: io.Discard, CodexClientVersion: "9.8.7"})
	if err != nil {
		t.Fatal(err)
	}
	pipeline.Catalog.Replace(modelregistry.Snapshot{Entries: []modelregistry.Entry{{Provider: modelregistry.ProviderAnthropic, ID: "claude-test", ContextWindow: 200000, Source: modelregistry.SourceNative}, {Provider: modelregistry.ProviderCodex, ID: "codex-test", ContextWindow: 200000, Source: modelregistry.SourceNative}}})
	// Publication retains the paths captured at construction, even if the environment changes.
	cacheDir = "retargeted"
	t.Chdir(t.TempDir())
	pipeline.Publish()
	cacheDir = "relative cache"
	entries, err := loadCachedNativeEntries(modelsDeps{FS: fs, HomeDir: home, CWD: cwd, Env: env})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries %v error %v", entries, err)
	}
	for _, path := range credentialPaths {
		data, err := fs.ReadFile(path)
		if err != nil || string(data) != "native-owned" {
			t.Fatalf("credential altered %q", path)
		}
	}
	if _, err := fs.ReadFile(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal("global picker left native home")
	}
	if _, err := fs.ReadFile(filepath.Join(home, ".claude", "cache", "model-capabilities.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("publisher ignored cache override")
	}
	if _, err := fs.ReadFile(filepath.Join(cwd, "retargeted", "models_cache.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("publisher retargeted after construction")
	}
}
func TestCacheTTLUnset(t *testing.T) {
	t.Setenv("CQ_TTL", "")
	if err := os.Unsetenv("CQ_TTL"); err != nil {
		t.Fatal(err)
	}
	if got := cacheTTL(); got != 30*time.Second {
		t.Fatalf("TTL %s", got)
	}
}

type modelInventoryFS struct {
	fsutil.FileSystem
	t     *testing.T
	home  string
	reads []string
}

func (f *modelInventoryFS) UserHomeDir() (string, error) { return f.home, nil }
func (f *modelInventoryFS) ReadFile(path string) ([]byte, error) {
	f.reads = append(f.reads, path)
	return f.FileSystem.ReadFile(path)
}
func (f *modelInventoryFS) WriteFile(string, []byte, os.FileMode) error {
	f.t.Fatal("inventory wrote a file")
	return nil
}
func (f *modelInventoryFS) MkdirAll(string, os.FileMode) error {
	f.t.Fatal("inventory created directories")
	return nil
}
func TestClientPathsAccountInventoryStaysAtNativeHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "native")
	cache := filepath.Join(t.TempDir(), "model")
	t.Setenv("CODEX_HOME", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", cache)
	fs := &modelInventoryFS{FileSystem: fsutil.NewMemFS(), t: t, home: home}
	inventory := codexprov.DiscoverInventory(fs)
	_ = inventory
	found := false
	for _, path := range fs.reads {
		if path == filepath.Join(home, ".codex", "auth.json") {
			found = true
		}
		if strings.HasPrefix(path, cache) {
			t.Fatal("inventory read model cache home")
		}
	}
	if !found {
		t.Fatalf("native system path not read: %v", fs.reads)
	}
	paths, err := userdirs.ResolveClientPaths("codex", "", home, os.Getenv)
	if err != nil || paths.CodexModels != filepath.Join(cache, "models_cache.json") {
		t.Fatalf("cache override %v %v", paths, err)
	}
}
