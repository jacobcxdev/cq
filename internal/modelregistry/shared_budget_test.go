package modelregistry

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestSharedBudgetCatalogueUsesRegisteredPriority(t *testing.T) {
	const first, second Provider = "first-subscription", "second-subscription"
	sharedModel := func(provider Provider, id string, context int) Entry {
		return Entry{
			Provider: provider, ID: id, Source: SourceNative, ContextWindow: context,
			SharedBudget: &SharedBudgetModel{ProviderName: string(provider), ReasoningEfforts: []string{"medium"}},
		}
	}
	firstSource := &stubSource{result: SourceResult{Entries: []Entry{
		sharedModel(first, "GLM", 10),
		sharedModel(first, "Claude", 20),
		sharedModel(first, "Kimi", 30),
	}}}
	secondSource := &stubSource{result: SourceResult{Entries: []Entry{
		sharedModel(second, "glm", 40),
		sharedModel(second, "claude", 50),
		sharedModel(second, "kimi", 60),
		sharedModel(second, "deepseek", 70),
		sharedModel(first, "wrong-provider", 80),
	}}}
	r := &Refresher{
		Catalog: NewCatalog(Snapshot{}),
		Anthropic: &stubSource{result: SourceResult{Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude", Source: SourceNative},
		}}},
		ZAI: &stubSource{result: SourceResult{Entries: []Entry{
			{Provider: ProviderZAI, ID: "glm", Source: SourceNative, ContextWindow: 100},
		}}},
		SharedBudgets: []SharedBudgetSource{{Provider: first, Source: firstSource}, {Provider: second, Source: secondSource}},
	}

	diag, err := r.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if diag.MalformedCounts[second] != 1 || diag.Counts[string(second)] != 4 {
		t.Fatalf("unexpected diagnostics: %+v", diag)
	}
	verifyPriorities := func(want ...Provider) {
		t.Helper()
		for _, id := range []string{"GLM", "claude", "kimi"} {
			var providers []Provider
			for _, route := range r.Catalog.SharedBudgetModels(id) {
				providers = append(providers, route.Provider)
			}
			if !slices.Equal(providers, want) {
				t.Errorf("routes for %s = %v, want %v", id, providers, want)
			}
		}
	}
	verifyPriorities(first, second)
	snap := r.Catalog.Snapshot()
	if len(snap.Entries) != 4 || snap.Entries[0].Provider != ProviderAnthropic ||
		snap.Entries[1].Provider != ProviderZAI || snap.Entries[2].Provider != first || snap.Entries[3].Provider != second {
		t.Fatalf("public entries = %+v", snap.Entries)
	}
	models := CodexModelsResponse(snap).Models
	if len(models) != 4 {
		t.Fatalf("picker contains %d models, want four", len(models))
	}
	var projected struct {
		Slug      string `json:"slug"`
		Context   int    `json:"context_window"`
		Reasoning string `json:"default_reasoning_level"`
	}
	if err := json.Unmarshal(models[0], &projected); err != nil || projected.Slug != "Claude" ||
		projected.Context != 20 || projected.Reasoning != "medium" {
		t.Fatalf("Anthropic overlap did not use shared Responses metadata: %+v, %v", projected, err)
	}
	var augmented codexModelsResponseOut
	body, err := AugmentCodexModels([]byte(`{"models":[{"slug":"KIMI","account_entitled":true}]}`), snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &augmented); err != nil || len(augmented.Models) != 4 ||
		string(augmented.Models[0]) != `{"slug":"KIMI","account_entitled":true}` {
		t.Fatalf("native Codex entry did not take precedence: %s, %v", body, err)
	}

	// Both the selected and hidden routes survive a partial outage exactly once.
	outage := errors.New("first source unavailable")
	firstSource.err = outage
	diag, err = r.Refresh(t.Context())
	if err != nil || !errors.Is(diag.SourceErrors[first], outage) {
		t.Fatalf("partial refresh = %+v, %v", diag, err)
	}
	verifyPriorities(first, second)
	firstSource.err = nil
	firstSource.result = SourceResult{NotConfigured: true}
	if _, err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	verifyPriorities(second)
}
