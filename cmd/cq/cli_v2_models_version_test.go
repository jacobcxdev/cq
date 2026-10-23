package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestCLIV2ModelsLocalRefreshUsesCurrentClientBuild(t *testing.T) {
	for _, desktopInstalled := range []bool{true, false} {
		name := "cli"
		if desktopInstalled {
			name = "desktop"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			codexHome := filepath.Join(home, "codex")
			t.Setenv("CODEX_HOME", codexHome)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
			if err := os.MkdirAll(codexHome, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{"client_version":"0.158.0","models":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			oldDesktop, oldBinary := probeCodexDesktopVersionFn, probeCodexBinaryVersionFn
			probeCodexDesktopVersionFn = func() (string, bool) { return "0.160.0", desktopInstalled }
			probeCodexBinaryVersionFn = func() (string, bool) { return "0.159.0", true }
			t.Cleanup(func() { probeCodexDesktopVersionFn, probeCodexBinaryVersionFn = oldDesktop, oldBinary })
			wantVersion := "0.159.0"
			if desktopInstalled {
				wantVersion = "0.160.0"
			}
			reg, err := buildCanonicalLocalRegistryWithControl(context.Background(), &proxy.Config{CodexUpstream: "https://codex.invalid"}, modelsDeps{
				FS: fsutil.OSFileSystem{}, HomeDir: home, CWD: home,
				Roots: userdirs.Roots{Config: filepath.Join(home, "config")}, Env: os.Getenv,
			}, func(context.Context, fsutil.DurableFileSystem, httputil.Doer) (*codexprov.CredentialControl, error) {
				return &codexprov.CredentialControl{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			source := reg.Refresher.Codex.(*modelregistry.CodexSource)
			source.AuthenticatedDo = func(_ context.Context, req *http.Request) (*http.Response, error) {
				body := `{"models":[]}`
				if req.URL.Query().Get("client_version") == wantVersion {
					body = `{"models":[{"slug":"gpt-6.1-sol"}]}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			result, err := source.Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.CodexClientVersion != wantVersion || len(result.Entries) != 1 || result.Entries[0].ID != "gpt-6.1-sol" {
				t.Fatalf("local refresh used version %q and returned %+v; want upgraded client %q and its model", result.CodexClientVersion, result.Entries, wantVersion)
			}
		})
	}
}
