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
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

type grokRegistryAuthority struct {
	accounts []grok.Account
	err      error
}

func (a *grokRegistryAuthority) Accounts(context.Context) ([]grok.Account, error) {
	return a.accounts, a.err
}
func (a *grokRegistryAuthority) Do(_ context.Context, id grok.AccountID, r *http.Request) (*http.Response, error) {
	if id != "subscription" || r.URL.String() != grok.BaseURL+"/models" {
		return nil, errors.New("unexpected model request")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"grok-4.7","apiBackend":"responses","contextWindow":500000}]}`))}, nil
}

func TestRegistryPipelineDiscoversGrokSubscriptionAndRemovesAbsentAccounts(t *testing.T) {
	fs := fsutil.NewMemFS()
	roots := testCQRoots()
	authority := &grokRegistryAuthority{accounts: []grok.Account{{ID: "subscription"}}}
	opts := registryPipelineOptions{FS: fs, HomeDir: "/home/test", Roots: roots, GrokAuthority: authority, ClaudeToken: func() (string, error) { return "", errors.New("unavailable") }, CodexToken: func() (string, error) { return "", errors.New("unavailable") }, HTTPClient: zaiRegistryDoer(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no non-subscription network expected")
	})}
	pipeline, err := newRegistryPipeline(opts)
	if err != nil {
		t.Fatal(err)
	}
	if pipeline.Refresher.Grok == nil {
		t.Fatal("Grok source not wired")
	}
	if _, err := pipeline.Refresher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries := pipeline.Catalog.Snapshot().Entries
	if len(entries) != 1 || entries[0].Provider != modelregistry.ProviderGrok {
		t.Fatalf("entries=%#v", entries)
	}
	pipeline.Publish()
	for _, readErr := range []error{nil, errors.New("unavailable")} {
		authority.accounts = nil
		authority.err = readErr
		absent, err := newRegistryPipeline(opts)
		if err != nil {
			t.Fatal(err)
		}
		if snapshotHasProvider(absent.Catalog.Snapshot(), modelregistry.ProviderGrok) {
			t.Fatal("unavailable account retained Grok cache seed")
		}
	}
	authority.err = nil
	diagnostics, err := pipeline.Refresher.Refresh(t.Context())
	if err == nil || diagnostics.SourceErrors[modelregistry.ProviderCodex] == nil || diagnostics.SourceErrors[modelregistry.ProviderAnthropic] == nil {
		t.Fatalf("removing the last Grok account masked configured source failures: %v %#v", err, diagnostics)
	}
	if snapshotHasProvider(pipeline.Catalog.Snapshot(), modelregistry.ProviderGrok) {
		t.Fatal("removed account retained models on refresh")
	}
}

func TestModelsListGrokSubscriptionProvider(t *testing.T) {
	fs, stdout, _, deps := testModelsDeps()
	deps.Roots.Cache = "/cq/cache"
	snap := modelregistry.Snapshot{Entries: []modelregistry.Entry{{Provider: modelregistry.ProviderGrok, ID: "grok-4.7", Source: modelregistry.SourceNative, ContextWindow: 500000, Raw: json.RawMessage(`{"id":"grok-4.7","apiBackend":"responses","contextWindow":500000}`)}}}
	if err := modelregistry.PublishCodexRegistryCache(fs, deps.Roots, snap); err != nil {
		t.Fatal(err)
	}
	if err := runModelsList([]string{"--provider", "grok"}, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "grok-4.7") || !strings.Contains(stdout.String(), "Grok subscription") || strings.Contains(stdout.String(), "Grok API") {
		t.Fatalf("list=%s", stdout)
	}
}
