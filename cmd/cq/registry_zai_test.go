package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
)

type zaiRegistryDoer func(*http.Request) (*http.Response, error)

func (f zaiRegistryDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestRegistryPipelineDiscoversZAIAfterConfiguration(t *testing.T) {
	fsys := fsutil.NewMemFS()
	options := registryPipelineOptions{
		FS: fsys, HomeDir: "/home/test", Roots: testCQRoots(),
		ClaudeToken: func() (string, error) { return "", errors.New("unavailable") },
		CodexToken:  func() (string, error) { return "", errors.New("unavailable") },
		HTTPClient: zaiRegistryDoer(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://api.z.ai/api/v1/models" || r.Header.Get("Authorization") != "Bearer fake-key" {
				t.Fatalf("unexpected request: %s", r.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"glm-5.3","context_window":1048576}]}`))}, nil
		}),
		Env: func(name string) string {
			if name == "ZAI_API_KEY" {
				return "fake-key"
			}
			return ""
		},
	}
	pipeline, err := newRegistryPipeline(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Refresher.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries := pipeline.Catalog.Snapshot().Entries
	if len(entries) != 1 || entries[0].Provider != modelregistry.ProviderZAI {
		t.Fatalf("entries = %+v", entries)
	}
	pipeline.Publish()
	loaded, _, err := modelregistry.LoadCodexRegistryCache(fsys, testCQRoots())
	if err != nil || len(loaded) != 1 || loaded[0].Provider != modelregistry.ProviderZAI {
		t.Fatalf("cache: %+v, %v", loaded, err)
	}
	options.Env = func(string) string { return "" }
	unconfigured, err := newRegistryPipeline(options)
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured.Refresher.ZAI != nil || len(unconfigured.Catalog.Snapshot().Entries) != 0 {
		t.Fatal("advertised unavailable Z.ai models")
	}
}

func TestModelsListZAIProvider(t *testing.T) {
	fsys, stdout, _, deps := testModelsDeps()
	deps.Roots.Cache = "/cq/cache"
	snap := modelregistry.Snapshot{Entries: []modelregistry.Entry{{Provider: modelregistry.ProviderZAI, ID: "glm-5.3", Source: modelregistry.SourceNative, Raw: json.RawMessage(`{"slug":"glm-5.3"}`)}}}
	if err := modelregistry.PublishCodexRegistryCache(fsys, deps.Roots, snap); err != nil {
		t.Fatal(err)
	}
	if err := runModelsList([]string{"--provider", "zai"}, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "glm-5.3") || !strings.Contains(stdout.String(), "Z.ai API") {
		t.Fatalf("list output: %s", stdout)
	}
}

func TestRegistryPipelineDiscoversZAIWithRejectedPreferredAccount(t *testing.T) {
	fsys := fsutil.NewMemFS()
	roots := testCQRoots()
	for _, key := range []string{"rejected-key", "working-key"} {
		if _, err := zaiprov.StoreAccount(fsys, roots.Config, key, "", false); err != nil {
			t.Fatal(err)
		}
	}
	var attempts []string
	pipeline, err := newRegistryPipeline(registryPipelineOptions{
		FS: fsys, HomeDir: "/home/test", Roots: roots,
		ClaudeToken: func() (string, error) { return "", errors.New("unavailable") },
		CodexToken:  func() (string, error) { return "", errors.New("unavailable") },
		HTTPClient: zaiRegistryDoer(func(r *http.Request) (*http.Response, error) {
			key := r.Header.Get("Authorization")
			attempts = append(attempts, key)
			if key == "Bearer rejected-key" {
				return &http.Response{StatusCode: 401, Body: http.NoBody}, nil
			}
			if key != "Bearer working-key" {
				t.Error("unexpected model credential")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"glm-5.3","context_window":1048576}]}`))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Refresher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries := pipeline.Catalog.Snapshot().Entries
	if len(attempts) != 2 || len(entries) != 1 || entries[0].Provider != modelregistry.ProviderZAI {
		t.Fatalf("catalogue fallback: attempts=%d entries=%d", len(attempts), len(entries))
	}
	pipeline.Publish()
	loaded, _, err := modelregistry.LoadCodexRegistryCache(fsys, roots)
	if err != nil || len(loaded) != 1 || loaded[0].Provider != modelregistry.ProviderZAI {
		t.Fatal("fallback models not published to cache")
	}
}
