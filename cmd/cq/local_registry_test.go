package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type registryModelsDoer struct {
	wantToken string
}

func (d registryModelsDoer) Do(req *http.Request) (*http.Response, error) {
	status := http.StatusServiceUnavailable
	body := `{"error":"unavailable"}`
	if req.URL.Path == "/models" || strings.HasSuffix(req.URL.Path, "/codex/models") {
		if req.Header.Get("Authorization") == "Bearer "+d.wantToken {
			status = http.StatusOK
			body = `{"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"public","priority":1}]}`
		} else {
			status = http.StatusUnauthorized
			body = `{"error":"unauthorised"}`
		}
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func externalRegistryAuthority(now time.Time, token string) *fakeCodexRegistryAuthority {
	identity := codexprov.AccountIdentity{AccountID: "account-external", UserID: "user-external", Email: "external@example.test", PlanType: "plus"}
	ref := codexprov.CandidateRef{AccountKey: "logical-external", CandidateID: "candidate-external"}
	return &fakeCodexRegistryAuthority{
		inventories: []codexprov.Inventory{registryInventory(identity, codexprov.CredentialCandidate{
			Ref: ref, Revision: "revision-external", Source: codexprov.SourceExternal,
			AccessExpiresAt: now.Add(time.Hour), Routable: true,
		})},
		resolve: func(_ int, planned codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			return registryCredentialMaterial(planned.Identity, token), nil
		},
	}
}

func assertCodexRegistryModel(t *testing.T, pipeline *registryPipeline, token string) {
	t.Helper()
	diagnostics, err := pipeline.Refresher.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if diagnostics.Counts[string(modelregistry.ProviderCodex)] != 1 {
		t.Fatalf("Codex count = %d, want 1", diagnostics.Counts[string(modelregistry.ProviderCodex)])
	}
	snapshot := pipeline.Catalog.Snapshot()
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Provider != modelregistry.ProviderCodex || snapshot.Entries[0].ID != "gpt-5.5" {
		t.Fatalf("catalogue = %+v, want external-authenticated Codex model", snapshot.Entries)
	}
	if strings.Contains(strings.Join([]string{snapshot.Entries[0].ID, snapshot.Entries[0].DisplayName, snapshot.Entries[0].Description}, " "), token) {
		t.Fatal("catalogue leaked access token")
	}
}

func TestBuildLocalRegistryUsesFederatedCredentialAuthority(t *testing.T) {
	now := time.Now()
	const token = "external-only-local-token"
	authority := externalRegistryAuthority(now, token)
	fsys := fsutil.NewMemFS()
	cfg := &proxy.Config{ClaudeUpstream: "https://claude.example", CodexUpstream: "https://codex.example"}

	registry, err := buildLocalRegistryFromAuthority(cfg, localRegistryDependencies{
		FS:                  fsys,
		HomeDir:             "/home/test",
		HTTPClient:          registryModelsDoer{wantToken: token},
		CodexClientVersion:  "test-client",
		ClaudeToken:         func() (string, error) { return "", errors.New("Claude unavailable") },
		CredentialAuthority: authority,
		Env:                 func(string) string { return "" },
		Stderr:              io.Discard,
	})
	if err != nil {
		t.Fatalf("buildLocalRegistryFromAuthority() error = %v", err)
	}
	defer registry.Close()

	assertCodexRegistryModel(t, &registryPipeline{Catalog: registry.Catalog, Refresher: registry.Refresher}, token)
	listCalls, plans, refreshCalls := authority.snapshot()
	if listCalls != 1 || len(plans) != 1 || refreshCalls != 0 {
		t.Fatalf("calls = list:%d resolve:%d refresh:%d, want 1/1/0", listCalls, len(plans), refreshCalls)
	}
}
