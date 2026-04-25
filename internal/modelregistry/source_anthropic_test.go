package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- AnthropicSource tests ---

// anthropicModelList builds a minimal Anthropic /v1/models response.
func anthropicModelList(models ...map[string]any) []byte {
	list := map[string]any{"data": models}
	b, _ := json.Marshal(list)
	return b
}

func TestAnthropicSource_HappyPath(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q, want /v1/models", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", auth)
		}
		if beta := r.Header.Get("anthropic-beta"); beta != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", beta)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicModelList(
			map[string]any{
				"id":           "claude-haiku-4-5",
				"display_name": "Claude Haiku",
				"created_at":   createdAt.Unix(),
			},
			map[string]any{
				"id":                "claude-sonnet-4-5",
				"display_name":      "Claude Sonnet",
				"created_at":        createdAt.Unix(),
				"context_window":    200000,
				"max_output_tokens": 8192,
			},
		))
	}))
	defer srv.Close()

	src := &AnthropicSource{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   func(_ context.Context) (string, error) { return "test-token", nil },
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

	// Check first entry
	e0 := result.Entries[0]
	if e0.Provider != ProviderAnthropic {
		t.Errorf("Entries[0].Provider = %q, want %q", e0.Provider, ProviderAnthropic)
	}
	if e0.ID != "claude-haiku-4-5" {
		t.Errorf("Entries[0].ID = %q, want claude-haiku-4-5", e0.ID)
	}
	if e0.DisplayName != "Claude Haiku" {
		t.Errorf("Entries[0].DisplayName = %q, want Claude Haiku", e0.DisplayName)
	}
	if e0.Source != SourceNative {
		t.Errorf("Entries[0].Source = %q, want %q", e0.Source, SourceNative)
	}

	// Check second entry has context_window / max_output_tokens preserved
	e1 := result.Entries[1]
	if e1.ID != "claude-sonnet-4-5" {
		t.Errorf("Entries[1].ID = %q, want claude-sonnet-4-5", e1.ID)
	}
	if e1.ContextWindow != 200000 {
		t.Errorf("Entries[1].ContextWindow = %d, want 200000", e1.ContextWindow)
	}
	if e1.MaxOutputTokens != 8192 {
		t.Errorf("Entries[1].MaxOutputTokens = %d, want 8192", e1.MaxOutputTokens)
	}

	// AnthropicRawByID must be populated
	if result.AnthropicRawByID == nil {
		t.Fatal("AnthropicRawByID must not be nil")
	}
	if _, ok := result.AnthropicRawByID["claude-haiku-4-5"]; !ok {
		t.Error("AnthropicRawByID missing claude-haiku-4-5")
	}
	if _, ok := result.AnthropicRawByID["claude-sonnet-4-5"]; !ok {
		t.Error("AnthropicRawByID missing claude-sonnet-4-5")
	}
}

func TestAnthropicSource_RawByIDUsesModelIDNotArrayPosition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicModelList(
			map[string]any{"display_name": "missing id"},
			map[string]any{"id": "claude-haiku-4-5", "display_name": "Claude Haiku"},
		))
	}))
	defer srv.Close()

	src := &AnthropicSource{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   func(_ context.Context) (string, error) { return "test-token", nil },
	}

	result, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].ID != "claude-haiku-4-5" {
		t.Fatalf("Entries = %+v, want only claude-haiku-4-5", result.Entries)
	}
	var raw struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(result.AnthropicRawByID["claude-haiku-4-5"], &raw); err != nil {
		t.Fatalf("raw unmarshal: %v", err)
	}
	if raw.ID != "claude-haiku-4-5" || raw.DisplayName != "Claude Haiku" {
		t.Fatalf("raw = %+v, want matching claude-haiku entry", raw)
	}
}

func TestAnthropicSource_Non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	src := &AnthropicSource{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   func(_ context.Context) (string, error) { return "bad-token", nil },
	}

	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() expected error for non-2xx response, got nil")
	}
}

func TestAnthropicSource_MalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not valid json`))
	}))
	defer srv.Close()

	src := &AnthropicSource{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   func(_ context.Context) (string, error) { return "token", nil },
	}

	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() expected error for malformed JSON, got nil")
	}
}

func TestAnthropicSource_TokenFuncError(t *testing.T) {
	// If the token func returns an error, Fetch must propagate it without making HTTP calls.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tokenErr := errors.New("token func failed")
	src := &AnthropicSource{
		Client:  srv.Client(),
		BaseURL: srv.URL,
		Token:   func(_ context.Context) (string, error) { return "", tokenErr },
	}

	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() expected error when token func fails, got nil")
	}
	if called {
		t.Error("HTTP server should not have been called when token func fails")
	}
}
