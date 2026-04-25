package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- CodexSource tests ---

func codexModelListJSON(models ...map[string]any) []byte {
	list := map[string]any{"models": models}
	b, _ := json.Marshal(list)
	return b
}

func TestCodexSource_HappyPath(t *testing.T) {
	const clientVersion = "1.2.3"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %q, want /models", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got != clientVersion {
			t.Errorf("client_version = %q, want %q", got, clientVersion)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer codex-token" {
			t.Errorf("Authorization = %q, want Bearer codex-token", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(codexModelListJSON(
			map[string]any{
				"slug":           "gpt-5.4",
				"display_name":   "GPT-5.4",
				"description":    "Flagship model",
				"context_window": 128000,
				"priority":       10,
				"visibility":     "public",
			},
			map[string]any{
				"slug":               "gpt-4.1",
				"display_name":       "GPT-4.1",
				"context_window":     8192,
				"max_context_window": 16384,
				"visibility":         "private",
				"priority":           5,
			},
		))
	}))
	defer srv.Close()

	src := &CodexSource{
		Client:        srv.Client(),
		BaseURL:       srv.URL,
		Token:         func(_ context.Context) (string, error) { return "codex-token", nil },
		ClientVersion: clientVersion,
	}

	result, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(result.Entries))
	}
	if result.FetchedAt.IsZero() {
		t.Error("FetchedAt must not be zero")
	}

	e0 := result.Entries[0]
	if e0.Provider != ProviderCodex {
		t.Errorf("Entries[0].Provider = %q, want %q", e0.Provider, ProviderCodex)
	}
	if e0.ID != "gpt-5.4" {
		t.Errorf("Entries[0].ID = %q, want gpt-5.4", e0.ID)
	}
	if e0.DisplayName != "GPT-5.4" {
		t.Errorf("Entries[0].DisplayName = %q, want GPT-5.4", e0.DisplayName)
	}
	if e0.Description != "Flagship model" {
		t.Errorf("Entries[0].Description = %q, want Flagship model", e0.Description)
	}
	if e0.ContextWindow != 128000 {
		t.Errorf("Entries[0].ContextWindow = %d, want 128000", e0.ContextWindow)
	}
	if e0.Priority != 10 {
		t.Errorf("Entries[0].Priority = %d, want 10", e0.Priority)
	}
	if e0.Visibility != "public" {
		t.Errorf("Entries[0].Visibility = %q, want public", e0.Visibility)
	}
	if e0.Source != SourceNative {
		t.Errorf("Entries[0].Source = %q, want %q", e0.Source, SourceNative)
	}

	e1 := result.Entries[1]
	if e1.ID != "gpt-4.1" {
		t.Errorf("Entries[1].ID = %q, want gpt-4.1", e1.ID)
	}
	if e1.ContextWindow != 8192 {
		t.Errorf("Entries[1].ContextWindow = %d, want 8192", e1.ContextWindow)
	}
	if e1.MaxContextWindow != 16384 {
		t.Errorf("Entries[1].MaxContextWindow = %d, want 16384", e1.MaxContextWindow)
	}

	// CodexRawByID must be populated
	if result.CodexRawByID == nil {
		t.Fatal("CodexRawByID must not be nil")
	}
	if _, ok := result.CodexRawByID["gpt-5.4"]; !ok {
		t.Error("CodexRawByID missing gpt-5.4")
	}
	if _, ok := result.CodexRawByID["gpt-4.1"]; !ok {
		t.Error("CodexRawByID missing gpt-4.1")
	}
}

func TestCodexSource_Non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	src := &CodexSource{
		Client:        srv.Client(),
		BaseURL:       srv.URL,
		Token:         func(_ context.Context) (string, error) { return "bad-token", nil },
		ClientVersion: "1.0",
	}

	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() expected error for non-2xx response, got nil")
	}
}

func TestCodexSource_MalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	src := &CodexSource{
		Client:        srv.Client(),
		BaseURL:       srv.URL,
		Token:         func(_ context.Context) (string, error) { return "token", nil },
		ClientVersion: "1.0",
	}

	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() expected error for malformed JSON, got nil")
	}
}

func TestCodexSource_TokenFuncError(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tokenErr := errors.New("no token")
	src := &CodexSource{
		Client:        srv.Client(),
		BaseURL:       srv.URL,
		Token:         func(_ context.Context) (string, error) { return "", tokenErr },
		ClientVersion: "1.0",
	}

	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() expected error when token func fails, got nil")
	}
	if called {
		t.Error("HTTP server should not have been called when token func fails")
	}
}

func TestCodexSourceMalformedModelEntriesAreCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(codexModelListJSON(
			map[string]any{"slug": "gpt-5.5", "display_name": "GPT-5.5"},
			map[string]any{"display_name": "missing slug"},
		))
	}))
	defer srv.Close()

	src := &CodexSource{
		Client:        srv.Client(),
		BaseURL:       srv.URL,
		Token:         func(_ context.Context) (string, error) { return "tok", nil },
		ClientVersion: "0.124.0",
	}

	result, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(result.Entries))
	}
	if result.Entries[0].ID != "gpt-5.5" {
		t.Errorf("Entries[0].ID = %q, want gpt-5.5", result.Entries[0].ID)
	}
	if result.MalformedEntries != 1 {
		t.Errorf("MalformedEntries = %d, want 1", result.MalformedEntries)
	}
}

func TestCodexSource_PreservesRawJSON(t *testing.T) {
	// Raw JSON per model must preserve all upstream fields (not just mapped ones).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(codexModelListJSON(
			map[string]any{
				"slug":         "gpt-5.4",
				"display_name": "GPT-5.4",
				"extra_field":  "preserved",
				"shell_type":   "default",
			},
		))
	}))
	defer srv.Close()

	src := &CodexSource{
		Client:        srv.Client(),
		BaseURL:       srv.URL,
		Token:         func(_ context.Context) (string, error) { return "token", nil },
		ClientVersion: "1.0",
	}

	result, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	raw, ok := result.CodexRawByID["gpt-5.4"]
	if !ok {
		t.Fatal("CodexRawByID missing gpt-5.4")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if decoded["extra_field"] != "preserved" {
		t.Errorf("extra_field = %v, want preserved", decoded["extra_field"])
	}
	if decoded["shell_type"] != "default" {
		t.Errorf("shell_type = %v, want default", decoded["shell_type"])
	}
}
