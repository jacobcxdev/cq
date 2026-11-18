package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestRegistryPipelineRechecksOpenCodePermissions(t *testing.T) {
	fsys := fsutil.NewMemFS()
	goKey := "fake-key"
	goOutage := false
	allowKimi := true
	opts := registryPipelineOptions{
		FS: fsys, HomeDir: "/home/test", Roots: testCQRoots(),
		GrokAuthority: &grokRegistryAuthority{},
		ClaudeToken:   func() (string, error) { return "", errors.New("unavailable") },
		CodexToken:    func() (string, error) { return "", errors.New("unavailable") },
		Env: func(name string) string {
			switch name {
			case "ZAI_API_KEY":
				return "fake-zai"
			case "OPENCODE_API_KEY":
				return goKey
			}
			return ""
		},
		HTTPClient: zaiRegistryDoer(func(r *http.Request) (*http.Response, error) {
			var body string
			switch r.URL.String() {
			case "https://api.z.ai/api/v1/models":
				body = `{"models":[{"slug":"glm-5.3","context_window":250000}]}`
			case modelregistry.OpenCodeGoBaseURL + "/v1/models":
				if r.Header.Get("Authorization") != "Bearer fake-key" || r.Header.Get("X-OpenCode-Org-Id") != "" {
					t.Error("registry did not use the workspace-bound API key")
				}
				if goOutage {
					return nil, errors.New("shared catalogue unavailable")
				}
				body = `{"data":[{"id":"GLM-5.3","type":"chat","limit":{"context":200000},"capabilities":{"tools":{"supported":true}},"go":{"monthly_usage":{"go":100}}}`
				if allowKimi {
					body += `,{"id":"kimi-k3","type":"chat","limit":{"context":100000},"capabilities":{"tools":{"supported":true}},"go":{"monthly_usage":{"go":15}}}`
				}
				body += `]}`
			default:
				t.Errorf("unexpected request to %s", r.URL)
				return nil, errors.New("unexpected request")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		}),
	}
	newPipeline := func() *registryPipeline {
		t.Helper()
		pipeline, err := newRegistryPipeline(opts)
		if err != nil {
			t.Fatal(err)
		}
		return pipeline
	}
	assertCatalogue := func(pipeline *registryPipeline, wantFallback bool) {
		t.Helper()
		entries := pipeline.Catalog.Snapshot().Entries
		if len(entries) != 1 || entries[0].Provider != modelregistry.ProviderZAI {
			t.Fatalf("wanted one first-party picker entry, got %+v", entries)
		}
		routes := pipeline.Catalog.SharedBudgetModels("glm-5.3")
		if wantFallback && (len(routes) != 1 || routes[0].ID != "GLM-5.3" || routes[0].Provider != modelregistry.ProviderOpenCodeGo) ||
			!wantFallback && len(routes) != 0 {
			t.Fatalf("fallback = %+v, want present = %v", routes, wantFallback)
		}
	}

	initial := newPipeline()
	if _, err := initial.Refresher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(initial.Catalog.SharedBudgetModels("kimi-k3")) != 1 {
		t.Fatal("enabled model did not become routable")
	}
	initial.Publish()

	allowKimi = false
	restarted := newPipeline()
	assertCatalogue(restarted, false)
	if _, err := restarted.Refresher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertCatalogue(restarted, true)
	if len(restarted.Catalog.SharedBudgetModels("kimi-k3")) != 0 {
		t.Fatal("disabled model survived the workspace refresh")
	}

	goOutage = true
	diagnostics, err := restarted.Refresher.Refresh(t.Context())
	if err != nil || diagnostics.SourceErrors[modelregistry.ProviderOpenCodeGo] == nil {
		t.Fatalf("partial refresh = %+v, %v", diagnostics, err)
	}
	assertCatalogue(restarted, false)

	goKey = ""
	unconfigured := newPipeline()
	assertCatalogue(unconfigured, false)
	if _, err := unconfigured.Refresher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertCatalogue(unconfigured, false)
}
