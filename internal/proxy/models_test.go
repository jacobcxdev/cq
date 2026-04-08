package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSyntheticModelCatalogRoutesViaCodex(t *testing.T) {
	for _, model := range SyntheticModelCatalog() {
		if got := RouteModel(model.ID); got != ProviderCodex {
			t.Fatalf("RouteModel(%q) = %v, want %v", model.ID, got, ProviderCodex)
		}
	}
}

func TestWriteClaudeModelCapabilitiesCache_CreatesExpectedShape(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache", "model-capabilities.json")

	if err := writeClaudeModelCapabilitiesCache(cachePath, []ModelMetadata{{
		ID:             "claude-opus-4-6",
		MaxInputTokens: 200000,
		MaxTokens:      32000,
	}}); err != nil {
		t.Fatalf("writeClaudeModelCapabilitiesCache() error = %v", err)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got struct {
		Models []struct {
			ID             string `json:"id"`
			MaxInputTokens int    `json:"max_input_tokens"`
			MaxTokens      int    `json:"max_tokens"`
		} `json:"models"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.Timestamp == "" {
		t.Fatal("timestamp is empty")
	}
	if _, err := time.Parse(time.RFC3339, got.Timestamp); err != nil {
		t.Fatalf("timestamp parse error = %v", err)
	}

	seen := map[string]struct {
		maxInput int
		maxOut   int
	}{}
	for _, model := range got.Models {
		seen[model.ID] = struct {
			maxInput int
			maxOut   int
		}{model.MaxInputTokens, model.MaxTokens}
	}

	gpt, ok := seen["gpt-5.4"]
	if !ok {
		t.Fatal("missing gpt-5.4 entry")
	}
	if gpt.maxInput != 1050000 || gpt.maxOut != 128000 {
		t.Fatalf("gpt-5.4 = %+v, want 1050000/128000", gpt)
	}
	claude, ok := seen["claude-opus-4-6"]
	if !ok {
		t.Fatal("missing preserved claude entry")
	}
	if claude.maxInput != 200000 || claude.maxOut != 32000 {
		t.Fatalf("claude-opus-4-6 = %+v, want 200000/32000", claude)
	}
}

func TestWriteClaudeModelCapabilitiesCache_ReplacesAtomicallyWithoutTempFile(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	cachePath := filepath.Join(cacheDir, "model-capabilities.json")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(cachePath, []byte(`{"models":[{"id":"claude-opus-4-6","max_input_tokens":200000,"max_tokens":32000}],"timestamp":"2026-04-08T20:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := writeClaudeModelCapabilitiesCache(cachePath, nil); err != nil {
		t.Fatalf("writeClaudeModelCapabilitiesCache() error = %v", err)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("unexpected temp file left behind: %s", entry.Name())
		}
	}
}
