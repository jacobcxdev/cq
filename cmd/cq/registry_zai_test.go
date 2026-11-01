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
