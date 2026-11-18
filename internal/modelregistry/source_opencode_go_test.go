package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenCodeGoRegistryPrefersFirstParty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer configured" || r.Header.Get("X-OpenCode-Org-Id") != "wrk_test" {
			t.Error("workspace discovery did not receive the selected login")
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"glm-5.3","name":"GLM-5.3","type":"chat","limit":{"context":1000000,"output":65536},"capabilities":{"tools":{"supported":true}},"go":{"monthly_usage":{"go":500}}},
			{"id":"kimi-k3","name":"Kimi K3","type":"chat","limit":{"context":1048576,"output":32768},"capabilities":{"tools":{"supported":true}},"go":{"monthly_usage":{"go":15}}},
			{"id":"free-model","type":"chat","limit":{"context":1000000},"capabilities":{"tools":{"supported":true}},"go":null}
		]}`))
	}))
	defer server.Close()
	key := "configured"
	source := &OpenCodeGoSource{Client: server.Client(), URL: server.URL, Credential: func(context.Context) (string, string, error) { return key, "wrk_test", nil }}
	firstParty := Entry{Provider: ProviderZAI, ID: "glm-5.3", ContextWindow: 200000, Source: SourceNative}
	refresher := &Refresher{
		Catalog:       NewCatalog(Snapshot{}),
		SharedBudgets: []SharedBudgetSource{{Provider: ProviderOpenCodeGo, Source: source}},
		ZAI: SourceFunc(func(context.Context) (SourceResult, error) {
			return SourceResult{Entries: []Entry{firstParty}}, nil
		}),
	}
	if _, err := refresher.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := refresher.Catalog.Snapshot()
	if len(snap.Entries) != 2 || snap.Entries[0].Provider != ProviderZAI || snap.Entries[0].ContextWindow != 200000 {
		t.Fatalf("public entries = %+v", snap.Entries)
	}
	if routes := snap.SharedBudgetModels["glm-5.3"]; len(routes) != 1 || routes[0].ID != "glm-5.3" {
		t.Fatal("missing GLM fallback")
	}
	body, err := AugmentCodexModels([]byte(`{"models":[]}`), snap)
	if err != nil {
		t.Fatal(err)
	}
	var picker struct {
		Models []struct {
			Slug          string `json:"slug"`
			ContextWindow int    `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &picker); err != nil {
		t.Fatal(err)
	}
	if len(picker.Models) != 2 || picker.Models[1].Slug != "kimi-k3" || picker.Models[1].ContextWindow != 1048576 {
		t.Fatalf("picker = %+v", picker)
	}
	key = ""
	if _, err := refresher.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap = refresher.Catalog.Snapshot()
	if len(snap.Entries) != 1 || len(snap.SharedBudgetModels) != 0 {
		t.Fatalf("Go entries retained without a key: %+v", snap)
	}
}

func TestOpenCodeGoCatalogueDoesNotRedirectWorkspaceCredentials(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("workspace discovery followed a redirect")
	}))
	defer target.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()
	source := &OpenCodeGoSource{
		Client: endpoint.Client(), URL: endpoint.URL,
		Credential: func(context.Context) (string, string, error) { return "secret", "wrk_test", nil },
	}
	if _, err := source.Fetch(t.Context()); err == nil {
		t.Fatal("redirect accepted as a workspace catalogue")
	}
}

func TestWorkspaceCatalogueDropsStaleRoutesWhenAllSourcesFail(t *testing.T) {
	firstParty := Entry{Provider: ProviderZAI, ID: "glm-5.3", ContextWindow: 200000, Source: SourceNative}
	shared := Entry{
		Provider: ProviderOpenCodeGo, ID: "kimi-k3", ContextWindow: 200000, Source: SourceNative,
		SharedBudget: &SharedBudgetModel{ProviderName: "OpenCode Go"},
	}
	outage := SourceFunc(func(context.Context) (SourceResult, error) {
		return SourceResult{}, errors.New("catalogue unavailable")
	})
	catalogue := NewCatalog(Snapshot{Entries: []Entry{firstParty, shared}})
	refresher := Refresher{
		Catalog: catalogue, ZAI: outage,
		SharedBudgets: []SharedBudgetSource{{Provider: ProviderOpenCodeGo, Source: outage, RequireFresh: true}},
	}
	diagnostics, err := refresher.Refresh(t.Context())
	if err == nil || !diagnostics.Replaced {
		t.Fatalf("expected published removal and reported failures, got %+v, %v", diagnostics, err)
	}
	snapshot := catalogue.Snapshot()
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Provider != ProviderZAI || catalogue.HasSharedBudgetModels() {
		t.Fatalf("expected first-party row only after failure, got %+v", snapshot.Entries)
	}
}
