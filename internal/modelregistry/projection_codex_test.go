package modelregistry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestCodexModelsResponse_EmptySnapshotUsesArray(t *testing.T) {
	data, err := json.Marshal(CodexModelsResponse(Snapshot{}))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"models":[]}` {
		t.Fatalf("empty models response = %s", data)
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

// --- Native cache ownership tests ---

func TestAugmentCodexModelsUsesNativeIDsAndCurrentAdditions(t *testing.T) {
	const upstream = `{"models":[{"slug":"native","available_access_programs":{"cyber":["daybreak_blue"]}}]}`
	snap := Snapshot{Entries: []Entry{
		{Provider: ProviderCodex, ID: "pooled-only", Source: SourceNative},
		{Provider: ProviderCodex, ID: "native", Source: SourceOverlay},
		{Provider: ProviderCodex, ID: "alias", Source: SourceOverlay, CloneFrom: "native"},
		{Provider: ProviderZAI, ID: "glm", Source: SourceNative, Raw: json.RawMessage(`{"slug":"glm"}`)},
	}}
	body, err := AugmentCodexModels([]byte(upstream), snap)
	if err != nil {
		t.Fatal(err)
	}
	var got codexModelsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 3 || !bytes.Contains(got.Models[0], []byte("daybreak_blue")) || !bytes.Contains(got.Models[1], []byte("daybreak_blue")) {
		t.Fatalf("native response replaced or wrong additions: %s", body)
	}
	body, err = AugmentCodexModels([]byte(upstream), Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("removed additions remained: %s", body)
	}
}

func TestAugmentCodexModelsRejectsInvalidNativeResponse(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"models":null}`, `{"models":[{}]}`, `{"models":[{"slug":"duplicate"},{"slug":"duplicate"}]}`} {
		if _, err := AugmentCodexModels([]byte(body), Snapshot{}); err == nil {
			t.Errorf("accepted %s", body)
		}
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
