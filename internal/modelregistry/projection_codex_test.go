package modelregistry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// --- CodexModelsResponse projection tests ---

func TestCodexModelsResponse_Shape(t *testing.T) {
	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Description: "Flagship model",
		Source:      SourceNative,
	})

	resp := CodexModelsResponse(snap)

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Models) == 0 {
		t.Fatal("models array is empty")
	}
}

func TestCodexModelsResponse_NativeRoundTrips(t *testing.T) {
	// Native entry raw JSON must round-trip exactly.
	rawJSON := `{"slug":"gpt-5.4","display_name":"GPT-5.4","description":"Flagship","context_window":128000,"shell_type":"default","visibility":"public","priority":10,"supported_in_api":true}`

	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
		},
		CodexRawByID: map[string]json.RawMessage{
			"gpt-5.4": json.RawMessage(rawJSON),
		},
		FetchedAt: time.Now(),
	}

	resp := CodexModelsResponse(snap)

	data, _ := json.Marshal(resp)
	var decoded struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(decoded.Models))
	}

	// Re-parse the model object.
	var m map[string]any
	if err := json.Unmarshal(decoded.Models[0], &m); err != nil {
		t.Fatalf("unmarshal model: %v", err)
	}

	// Key fields from raw must be present.
	if m["slug"] != "gpt-5.4" {
		t.Errorf("slug = %v, want gpt-5.4", m["slug"])
	}
	if m["shell_type"] != "default" {
		t.Errorf("shell_type = %v, want default", m["shell_type"])
	}
	if m["supported_in_api"] != true {
		t.Errorf("supported_in_api = %v, want true", m["supported_in_api"])
	}
}

func TestCodexModelsResponse_SynthesisedOverlayRequiredFields(t *testing.T) {
	// An overlay/non-native entry with no raw JSON should still emit required fields.
	snap := Snapshot{
		Entries: []Entry{
			{
				Provider:      ProviderCodex,
				ID:            "my-overlay-model",
				DisplayName:   "My Overlay Model",
				Description:   "Custom overlay",
				ContextWindow: 128000,
				Visibility:    "public",
				Priority:      50,
				Source:        SourceOverlay,
			},
		},
		CodexRawByID: nil, // no raw for overlay
		FetchedAt:    time.Now(),
	}

	resp := CodexModelsResponse(snap)

	data, _ := json.Marshal(resp)
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(decoded.Models))
	}
	m := decoded.Models[0]

	requiredFields := []string{
		"slug", "display_name", "description", "visibility",
		"supported_in_api", "priority", "shell_type",
		"context_window", "truncation_policy",
	}
	for _, field := range requiredFields {
		if _, ok := m[field]; !ok {
			t.Errorf("required field %q missing from synthesised overlay entry", field)
		}
	}

	if m["slug"] != "my-overlay-model" {
		t.Errorf("slug = %v, want my-overlay-model", m["slug"])
	}
	if m["supported_in_api"] != true {
		t.Errorf("supported_in_api = %v, want true", m["supported_in_api"])
	}
}

func TestCodexModelsResponse_OverlayInheritsRawClone(t *testing.T) {
	// An overlay that has a CloneFrom pointing to a native entry with raw JSON
	// should clone that raw JSON and override slug/id.
	nativeRaw := `{"slug":"gpt-5.4","display_name":"GPT-5.4","description":"Flagship","context_window":128000,"shell_type":"default","visibility":"public","priority":10,"supported_in_api":true,"base_instructions":""}`

	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
			{
				Provider:    ProviderCodex,
				ID:          "my-gpt-5.4-clone",
				CloneFrom:   "gpt-5.4",
				DisplayName: "My GPT-5.4 Clone",
				Source:      SourceOverlay,
			},
		},
		CodexRawByID: map[string]json.RawMessage{
			"gpt-5.4": json.RawMessage(nativeRaw),
		},
		FetchedAt: time.Now(),
	}

	resp := CodexModelsResponse(snap)
	data, _ := json.Marshal(resp)

	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var clone map[string]any
	for _, m := range decoded.Models {
		if m["slug"] == "my-gpt-5.4-clone" {
			clone = m
			break
		}
	}
	if clone == nil {
		t.Fatal("clone model my-gpt-5.4-clone not found in response")
	}

	// Should have inherited shell_type from raw.
	if clone["shell_type"] != "default" {
		t.Errorf("shell_type = %v, want default (inherited from raw)", clone["shell_type"])
	}
	// slug must be overridden.
	if clone["slug"] != "my-gpt-5.4-clone" {
		t.Errorf("slug = %v, want my-gpt-5.4-clone", clone["slug"])
	}
}

// --- PublishCodexCache tests ---

func TestPublishCodexCache_EnvelopeFields(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/models_cache.json"

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Source:      SourceNative,
	})

	now := time.Unix(1700000000, 0)
	if err := PublishCodexCache(fsys, path, snap, now, "1.2.3"); err != nil {
		t.Fatalf("PublishCodexCache() error = %v", err)
	}

	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got struct {
		FetchedAt     string            `json:"fetched_at"`
		ClientVersion string            `json:"client_version"`
		Models        []map[string]any  `json:"models"`
		Etag          *string           `json:"etag"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.FetchedAt == "" {
		t.Error("fetched_at is empty")
	}
	if got.ClientVersion != "1.2.3" {
		t.Errorf("client_version = %q, want 1.2.3", got.ClientVersion)
	}
	if len(got.Models) == 0 {
		t.Error("models array is empty")
	}
}

func TestPublishCodexCache_FreshInstallNoCacheBestEffort(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/models_cache.json"

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Source:      SourceNative,
	})

	// No existing cache, no clientVersion — should succeed best-effort.
	if err := PublishCodexCache(fsys, path, snap, time.Now(), ""); err != nil {
		t.Fatalf("PublishCodexCache() with empty clientVersion should not error: %v", err)
	}

	data, _ := fsys.ReadFile(path)
	if len(data) == 0 {
		t.Fatal("file is empty after fresh install")
	}
}

func TestPublishCodexCache_PreservesExistingEnvelopeFields(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/models_cache.json"

	// Write existing cache with etag and client_version.
	existing := `{
  "fetched_at": "2024-01-01T00:00:00Z",
  "etag": "\"abc123\"",
  "client_version": "1.0.0",
  "models": []
}`
	_ = fsys.WriteFile(path, []byte(existing), 0o600)

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Source:      SourceNative,
	})

	now := time.Unix(1700000000, 0)
	if err := PublishCodexCache(fsys, path, snap, now, "2.0.0"); err != nil {
		t.Fatalf("PublishCodexCache() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		FetchedAt     string  `json:"fetched_at"`
		Etag          *string `json:"etag"`
		ClientVersion string  `json:"client_version"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// fetched_at must be updated to now.
	if got.FetchedAt == "2024-01-01T00:00:00Z" {
		t.Error("fetched_at was not updated")
	}
	// client_version must be updated.
	if got.ClientVersion != "2.0.0" {
		t.Errorf("client_version = %q, want 2.0.0", got.ClientVersion)
	}
	// etag preserved (we don't change it).
	if got.Etag == nil || *got.Etag != `"abc123"` {
		t.Errorf("etag = %v, want %q", got.Etag, `"abc123"`)
	}
}

func TestPublishCodexCache_AtomicNoTempFile(t *testing.T) {
	// Use MemFS: check that no .tmp key remains after a successful write.
	fsys := fsutil.NewMemFS()
	path := "/home/test/models_cache.json"

	snap := makeCodexSnapshot(Entry{
		Provider:    ProviderCodex,
		ID:          "gpt-5.4",
		DisplayName: "GPT-5.4",
		Source:      SourceNative,
	})

	if err := PublishCodexCache(fsys, path, snap, time.Now(), "1.0"); err != nil {
		t.Fatalf("PublishCodexCache() error = %v", err)
	}

	// The .tmp file should not exist.
	if _, err := fsys.ReadFile(path + ".tmp"); err == nil {
		t.Error("temp file still exists after successful publish")
	}
	// The real file must exist.
	if _, err := fsys.ReadFile(path); err != nil {
		t.Errorf("cache file missing after publish: %v", err)
	}
}

func TestPublishCodexCache_OnlyCodexEntries(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/models_cache.json"

	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-opus-4", Source: SourceNative},
			{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
		},
		FetchedAt: time.Now(),
	}

	if err := PublishCodexCache(fsys, path, snap, time.Now(), "1.0"); err != nil {
		t.Fatalf("PublishCodexCache() error = %v", err)
	}

	data, _ := fsys.ReadFile(path)
	var got struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, m := range got.Models {
		if slug, _ := m["slug"].(string); slug == "claude-opus-4" {
			t.Error("Anthropic entry claude-opus-4 must not appear in Codex cache")
		}
	}

	// gpt-5.4 must be present.
	var found bool
	for _, m := range got.Models {
		if slug, _ := m["slug"].(string); slug == "gpt-5.4" {
			found = true
		}
	}
	if !found {
		t.Error("gpt-5.4 not found in Codex cache")
	}
}

func TestCodexModelsResponse_SynthesisedFallbackContextWindow(t *testing.T) {
	// When ContextWindow is 0 for overlay, should use fallback 272000.
	snap := Snapshot{
		Entries: []Entry{
			{
				Provider:    ProviderCodex,
				ID:          "my-model",
				DisplayName: "My Model",
				Source:      SourceOverlay,
				// ContextWindow intentionally 0.
			},
		},
		FetchedAt: time.Now(),
	}

	resp := CodexModelsResponse(snap)
	data, _ := json.Marshal(resp)

	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(decoded.Models))
	}

	cw, ok := decoded.Models[0]["context_window"].(float64)
	if !ok {
		t.Fatalf("context_window is not a number: %T %v", decoded.Models[0]["context_window"], decoded.Models[0]["context_window"])
	}
	if int(cw) != 272000 {
		t.Errorf("context_window = %d, want 272000 (fallback)", int(cw))
	}
}

func TestCodexModelsResponse_TruncationPolicyPresent(t *testing.T) {
	// Synthesised overlay must include truncation_policy with bytes limit.
	snap := Snapshot{
		Entries: []Entry{
			{
				Provider:    ProviderCodex,
				ID:          "my-model",
				DisplayName: "My Model",
				Source:      SourceOverlay,
			},
		},
		FetchedAt: time.Now(),
	}

	resp := CodexModelsResponse(snap)
	data, _ := json.Marshal(resp)

	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	m := decoded.Models[0]
	tp, ok := m["truncation_policy"]
	if !ok {
		t.Fatal("truncation_policy missing from synthesised entry")
	}
	tpMap, ok := tp.(map[string]any)
	if !ok {
		t.Fatalf("truncation_policy is not an object: %T", tp)
	}
	if _, ok := tpMap["type"]; !ok {
		// some implementations may encode as {"type":"last_n_bytes","bytes":10000}
		// check for any bytes-related key
		found := false
		for k := range tpMap {
			if strings.Contains(k, "byte") || strings.Contains(k, "type") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("truncation_policy object has no expected keys: %v", tpMap)
		}
	}
}
