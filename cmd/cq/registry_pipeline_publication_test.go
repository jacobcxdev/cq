package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestRegistryPublicationRetainsCapturedPathsAndSnapshotVersion(t *testing.T) {
	fsys := fsutil.NewMemFS()
	home, cwd := t.TempDir(), t.TempDir()
	codexHome := "codex-home"
	cachePath := filepath.Join(cwd, codexHome, "models_cache.json")
	if err := fsys.WriteFile(cachePath, []byte(`{"client_version":"0.158.0","models":[{"slug":"gpt-6-sol","supports_parallel_tool_calls":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS: fsys, HomeDir: home, CWD: cwd, Roots: testCQRoots(), HTTPClient: http.DefaultClient,
		CodexClientVersion: "0.124.0",
		ClaudeToken:        func() (string, error) { return "", errors.New("not configured") },
		CodexToken:         func() (string, error) { return "", errors.New("not configured") },
		Env: func(name string) string {
			if name == "CODEX_HOME" {
				return codexHome
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := pipeline.Catalog.Snapshot()
	if snap.CodexClientVersion != "0.158.0" || !bytes.Contains(snap.CodexRawByID["gpt-6-sol"], []byte(`"supports_parallel_tool_calls":true`)) {
		t.Fatalf("seed lost cached version or raw fields: %+v", snap)
	}
	codexHome = "moved-codex-home"
	snap.CodexClientVersion = "0.159.0"
	targets := pipeline.PublishReport(snap)
	if len(targets) != 3 || targets[0].Target != "codex_cache" || targets[0].Path != cachePath || targets[0].Status != "written" {
		t.Fatalf("publication targets = %+v", targets)
	}
	for _, target := range targets[1:] {
		if target.Status != "skipped" || target.Reason != "optional_client_absent" {
			t.Fatalf("optional client target = %+v", target)
		}
	}
	data, err := fsys.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var cache struct {
		ClientVersion string `json:"client_version"`
		Models        []struct {
			SupportsParallelToolCalls bool `json:"supports_parallel_tool_calls"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatal(err)
	}
	if cache.ClientVersion != "0.159.0" || len(cache.Models) != 1 || !cache.Models[0].SupportsParallelToolCalls {
		t.Fatalf("published cache lost snapshot version or raw fields: %s", data)
	}
	if _, err := fsys.Stat(filepath.Join(cwd, codexHome, "models_cache.json")); err == nil {
		t.Fatal("publication followed an environment change after paths were captured")
	}
}

func TestRegistryReconcilerClosesCompletionAfterPublicationPanic(t *testing.T) {
	var stderr bytes.Buffer
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS: fsutil.NewMemFS(), HomeDir: t.TempDir(), CWD: t.TempDir(), Roots: testCQRoots(), HTTPClient: http.DefaultClient,
		ClaudeToken:     func() (string, error) { return "", errors.New("not configured") },
		CodexToken:      func() (string, error) { return "", errors.New("not configured") },
		RefreshInterval: time.Millisecond,
		Stderr:          &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline.Refresher.Anthropic = nil
	pipeline.Refresher.Codex = modelregistry.SourceFunc(func(context.Context) (modelregistry.SourceResult, error) {
		return modelregistry.SourceResult{Entries: []modelregistry.Entry{{ID: "gpt-6-sol", Provider: modelregistry.ProviderCodex}}}, nil
	})
	pipeline.Publish = func() { panic("publication failed") }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-pipeline.StartReconciler(ctx):
	case <-time.After(5 * time.Second):
		t.Fatal("reconciler did not close completion after publication panic")
	}
	if !strings.Contains(stderr.String(), "background refresh stopped after panic") {
		t.Fatalf("panic diagnostic = %q", stderr.String())
	}
}
