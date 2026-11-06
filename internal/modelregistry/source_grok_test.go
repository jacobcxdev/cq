package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/grok"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type grokSourceAuthority struct {
	accounts []grok.Account
	do       func(grok.AccountID, *http.Request) (*http.Response, error)
	err      error
}

func (a grokSourceAuthority) Accounts(context.Context) ([]grok.Account, error) {
	return a.accounts, a.err
}
func (a grokSourceAuthority) Do(_ context.Context, id grok.AccountID, r *http.Request) (*http.Response, error) {
	return a.do(id, r)
}

func TestGrokSourceUsesSubscriptionCatalogueAndExistingProjection(t *testing.T) {
	const body = `{"data":[{"id":"grok-4.7","name":"Grok 4.7","apiBackend":"responses","contextWindow":500000,"maxCompletionTokens":128000,"reasoningEfforts":["low","high"],"defaultReasoningEffort":"high"},{"id":"grok-chat","contextWindow":256000},{"id":"grok-messages","apiBackend":"messages","contextWindow":256000},{"id":"unknown-context","apiBackend":"responses"}]}`
	a := grokSourceAuthority{accounts: []grok.Account{{ID: "a"}}, do: func(id grok.AccountID, r *http.Request) (*http.Response, error) {
		if id != "a" || r.URL.String() != grok.BaseURL+"/models" {
			t.Errorf("wrong catalogue request: %s %s", id, r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
	result, err := (&GrokSource{Authority: a}).Fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("unsupported models advertised: %#v", result.Entries)
	}
	e := result.Entries[0]
	if e.Provider != ProviderGrok || e.ID != "grok-4.7" || e.ContextWindow != 500000 || e.MaxOutputTokens != 128000 || e.Validate() != nil {
		t.Fatalf("entry=%#v", e)
	}
	projected := CodexModelsResponse(Snapshot{Entries: result.Entries, CodexRawByID: result.CodexRawByID})
	if len(projected.Models) != 1 {
		t.Fatal("Grok missing from existing Codex projection")
	}
	var model struct {
		Slug    string `json:"slug"`
		Context int    `json:"context_window"`
		Default string `json:"default_reasoning_level"`
		Levels  []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
	}
	if json.Unmarshal(projected.Models[0], &model) != nil || model.Slug != "grok-4.7" || model.Context != 500000 || model.Default != "high" || len(model.Levels) != 2 {
		t.Fatalf("projection=%s", projected.Models[0])
	}
	var wire map[string]json.RawMessage
	if json.Unmarshal(projected.Models[0], &wire) != nil || string(wire["apply_patch_tool_type"]) != "null" || string(wire["default_verbosity"]) != "null" || string(wire["default_reasoning_summary"]) != `"none"` || string(wire["web_search_tool_type"]) != `"text"` || string(wire["truncation_policy"]) != `{"limit":10000,"mode":"bytes"}` {
		t.Fatalf("invalid Codex metadata field types: %s", projected.Models[0])
	}
	fs := fsutil.NewMemFS()
	roots := userdirs.Roots{Cache: "/cq/cache"}
	if err := PublishCodexRegistryCache(fs, roots, Snapshot{Entries: result.Entries}); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := LoadCodexRegistryCache(fs, roots)
	if err != nil || !exists || len(loaded) != 1 || loaded[0].Provider != ProviderGrok {
		t.Fatalf("cached=%#v %v", loaded, err)
	}
	roundtrip := CodexModelsResponse(Snapshot{Entries: loaded})
	if len(roundtrip.Models) != 1 || string(roundtrip.Models[0]) != string(projected.Models[0]) {
		t.Fatal("projection changed after cache round trip")
	}
	augmented, err := AugmentCodexModels([]byte(`{"models":[{"slug":"gpt-own"}],"extra":true}`), Snapshot{Entries: result.Entries})
	if err != nil || !strings.Contains(string(augmented), `"slug":"grok-4.7"`) || !strings.Contains(string(augmented), `"extra":true`) {
		t.Fatalf("augment=%s %v", augmented, err)
	}
}

func TestGrokSourceDeduplicatesDisplayWithoutGrantingEntitlement(t *testing.T) {
	a := grokSourceAuthority{accounts: []grok.Account{{ID: "preferred"}, {ID: "other"}}, do: func(id grok.AccountID, r *http.Request) (*http.Response, error) {
		body := `{"data":[{"id":"shared","apiBackend":"responses","contextWindow":500000}]}`
		if id == "other" {
			body = `{"data":[{"id":"shared","apiBackend":"responses","contextWindow":1000000},{"id":"other-only","apiBackend":"responses","contextWindow":500000}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
	result, err := (&GrokSource{Authority: a}).Fetch(t.Context())
	if err != nil || len(result.Entries) != 2 || result.Entries[0].ContextWindow != 500000 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	preferred, err := grok.FetchModels(t.Context(), a, "preferred")
	if err != nil || len(preferred) != 1 || preferred[0].ID != "shared" {
		t.Fatalf("pooled entitlement leaked: %#v %v", preferred, err)
	}
}

func TestGrokSourceEmptyAccountsClearsStaleDisplay(t *testing.T) {
	cat := NewCatalog(Snapshot{Entries: []Entry{{Provider: ProviderGrok, ID: "removed", Source: SourceNative}}})
	r := Refresher{Catalog: cat, Grok: &GrokSource{Authority: grokSourceAuthority{}}}
	if _, err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(cat.Snapshot().Entries) != 0 {
		t.Fatal("removed accounts retained models")
	}
}

func TestGrokSourceSanitizesFailureAndRecoversPanics(t *testing.T) {
	for _, a := range []grokSourceAuthority{{err: errors.New("secret-value")}, {accounts: []grok.Account{{ID: "a"}}, do: func(grok.AccountID, *http.Request) (*http.Response, error) { return nil, errors.New("secret-value") }}, {accounts: []grok.Account{{ID: "a"}}, do: func(grok.AccountID, *http.Request) (*http.Response, error) { panic("secret-value") }}} {
		_, err := (&GrokSource{Authority: a}).Fetch(t.Context())
		if err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("unsafe error=%v", err)
		}
	}
}

func TestGrokSourceDoesNotAdvertiseReportingOnlySource(t *testing.T) {
	a := grokSourceAuthority{accounts: []grok.Account{{ID: "unverified", ReportingOnly: true}}, do: func(grok.AccountID, *http.Request) (*http.Response, error) {
		t.Fatal("unverified source fetched models")
		return nil, nil
	}}
	result, err := (&GrokSource{Authority: a}).Fetch(t.Context())
	if err != nil || len(result.Entries) > 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestUnconfiguredGrokClearsOnlyItsModelsWithoutMaskingSourceFailure(t *testing.T) {
	wantErr := errors.New("typed credential authority failure")
	cat := NewCatalog(Snapshot{Entries: []Entry{
		{Provider: ProviderCodex, ID: "keep-codex", Source: SourceNative},
		{Provider: ProviderGrok, ID: "removed-grok", Source: SourceNative},
	}})
	r := Refresher{Catalog: cat,
		Codex: SourceFunc(func(context.Context) (SourceResult, error) { return SourceResult{}, wantErr }),
		Grok:  &GrokSource{Authority: grokSourceAuthority{}},
	}
	diagnostics, err := r.Refresh(t.Context())
	if !errors.Is(err, wantErr) || !errors.Is(diagnostics.SourceErrors[ProviderCodex], wantErr) {
		t.Fatalf("unconfigured source masked failure: %v %#v", err, diagnostics)
	}
	entries := cat.Snapshot().Entries
	if len(entries) != 1 || entries[0].Provider != ProviderCodex || entries[0].ID != "keep-codex" {
		t.Fatalf("wrong stale-state update: %#v", entries)
	}
}

func TestConfiguredEmptyCatalogueStillCountsAsSuccessfulFetch(t *testing.T) {
	cat := NewCatalog(Snapshot{})
	r := Refresher{Catalog: cat,
		Codex:     SourceFunc(func(context.Context) (SourceResult, error) { return SourceResult{Entries: []Entry{}}, nil }),
		Anthropic: SourceFunc(func(context.Context) (SourceResult, error) { return SourceResult{}, errors.New("failed") }),
		Grok:      &GrokSource{Authority: grokSourceAuthority{}},
	}
	if _, err := r.Refresh(t.Context()); err != nil {
		t.Fatalf("real empty fetch lost success: %v", err)
	}
}
