package modelregistry

import (
	"testing"
)

// --- Merge tests ---

func TestMerge_NativeWinsOverOverlay(t *testing.T) {
	// When native and overlay share (provider, id), native wins.
	// Overlay is returned in Prunable.
	natives := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4 Native", Source: SourceNative},
	}
	overlays := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4 Overlay", Source: SourceOverlay},
	}

	result := Merge(natives, overlays)

	if len(result.Active) != 1 {
		t.Fatalf("len(Active) = %d, want 1", len(result.Active))
	}
	if result.Active[0].DisplayName != "GPT-5.4 Native" {
		t.Errorf("Active[0].DisplayName = %q, want native display name", result.Active[0].DisplayName)
	}
	if result.Active[0].Source != SourceNative {
		t.Errorf("Active[0].Source = %q, want %q", result.Active[0].Source, SourceNative)
	}
	if len(result.Prunable) != 1 {
		t.Fatalf("len(Prunable) = %d, want 1", len(result.Prunable))
	}
	if result.Prunable[0].ID != "gpt-5.4" {
		t.Errorf("Prunable[0].ID = %q, want gpt-5.4", result.Prunable[0].ID)
	}
}

func TestMerge_OverlayWithNoNativeRemains(t *testing.T) {
	// Overlay entry with no native counterpart remains active.
	natives := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Source: SourceNative},
	}
	overlays := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.5", DisplayName: "GPT-5.5 Preview", Source: SourceOverlay},
	}

	result := Merge(natives, overlays)

	if len(result.Prunable) != 0 {
		t.Errorf("len(Prunable) = %d, want 0 (overlay-only model should remain)", len(result.Prunable))
	}
	if len(result.Active) != 2 {
		t.Fatalf("len(Active) = %d, want 2 (one native + one overlay)", len(result.Active))
	}

	// Verify the overlay entry is present and unchanged
	var overlayEntry *Entry
	for i := range result.Active {
		if result.Active[i].ID == "gpt-5.5" {
			overlayEntry = &result.Active[i]
			break
		}
	}
	if overlayEntry == nil {
		t.Fatal("overlay entry gpt-5.5 not found in Active")
	}
	if overlayEntry.Source != SourceOverlay {
		t.Errorf("overlay entry Source = %q, want %q", overlayEntry.Source, SourceOverlay)
	}
	if overlayEntry.Provider != ProviderCodex {
		t.Errorf("overlay entry Provider = %q, want %q", overlayEntry.Provider, ProviderCodex)
	}
	if overlayEntry.ID != "gpt-5.5" {
		t.Errorf("overlay entry ID = %q, want gpt-5.5", overlayEntry.ID)
	}
}

func TestMerge_OverlayInferredFromNative(t *testing.T) {
	// An overlay entry with missing metadata gets inferred from its native clone.
	natives := []Entry{
		{
			Provider:         ProviderCodex,
			ID:               "gpt-5.4",
			DisplayName:      "GPT-5.4",
			Description:      "Flagship model",
			ContextWindow:    128000,
			MaxContextWindow: 1050000,
			MaxOutputTokens:  32000,
			Priority:         10,
			Visibility:       "public",
			Source:           SourceNative,
		},
	}
	overlays := []Entry{
		// Minimal overlay — only provider/id/source set; clone_from points at native.
		{
			Provider:  ProviderCodex,
			ID:        "gpt-5.5",
			CloneFrom: "gpt-5.4",
			Source:    SourceOverlay,
		},
	}

	result := Merge(natives, overlays)

	var overlayEntry *Entry
	for i := range result.Active {
		if result.Active[i].ID == "gpt-5.5" {
			overlayEntry = &result.Active[i]
			break
		}
	}
	if overlayEntry == nil {
		t.Fatal("overlay entry gpt-5.5 not found in Active")
	}
	// ID must be preserved (not changed to clone's ID).
	if overlayEntry.ID != "gpt-5.5" {
		t.Errorf("ID = %q, want gpt-5.5 (must not be overwritten)", overlayEntry.ID)
	}
	// Provider must be preserved.
	if overlayEntry.Provider != ProviderCodex {
		t.Errorf("Provider = %q, want %q", overlayEntry.Provider, ProviderCodex)
	}
	// Source must remain overlay.
	if overlayEntry.Source != SourceOverlay {
		t.Errorf("Source = %q, want %q", overlayEntry.Source, SourceOverlay)
	}
	// Metadata should be inferred from native clone.
	if overlayEntry.DisplayName != "GPT-5.4" {
		t.Errorf("DisplayName = %q, want GPT-5.4", overlayEntry.DisplayName)
	}
	if overlayEntry.Description != "Flagship model" {
		t.Errorf("Description = %q, want Flagship model", overlayEntry.Description)
	}
	if overlayEntry.ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d, want 128000", overlayEntry.ContextWindow)
	}
	if overlayEntry.MaxContextWindow != 1050000 {
		t.Errorf("MaxContextWindow = %d, want 1050000", overlayEntry.MaxContextWindow)
	}
	if overlayEntry.MaxOutputTokens != 32000 {
		t.Errorf("MaxOutputTokens = %d, want 32000", overlayEntry.MaxOutputTokens)
	}
	if overlayEntry.Priority != 10 {
		t.Errorf("Priority = %d, want 10", overlayEntry.Priority)
	}
	if overlayEntry.Visibility != "public" {
		t.Errorf("Visibility = %q, want public", overlayEntry.Visibility)
	}
	// InferredFrom should be set.
	if overlayEntry.InferredFrom == "" {
		t.Error("InferredFrom should be set after inference")
	}
}

func TestMerge_SameIDDifferentProviderNoShadow(t *testing.T) {
	// Same ID on different providers must coexist without shadowing.
	natives := []Entry{
		{Provider: ProviderAnthropic, ID: "model-x", Source: SourceNative},
	}
	overlays := []Entry{
		{Provider: ProviderCodex, ID: "model-x", Source: SourceOverlay},
	}

	result := Merge(natives, overlays)

	if len(result.Active) != 2 {
		t.Fatalf("len(Active) = %d, want 2 (different providers must coexist)", len(result.Active))
	}
	if len(result.Prunable) != 0 {
		t.Errorf("len(Prunable) = %d, want 0 (different provider must not conflict)", len(result.Prunable))
	}
}

func TestMerge_EmptyInputs(t *testing.T) {
	result := Merge(nil, nil)
	if result.Active != nil && len(result.Active) != 0 {
		t.Errorf("Active = %v, want empty/nil", result.Active)
	}
	if result.Prunable != nil && len(result.Prunable) != 0 {
		t.Errorf("Prunable = %v, want empty/nil", result.Prunable)
	}
}

func TestMerge_AllNatives(t *testing.T) {
	natives := []Entry{
		{Provider: ProviderAnthropic, ID: "claude-haiku-4-5", Source: SourceNative},
		{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
	}

	result := Merge(natives, nil)

	if len(result.Active) != 2 {
		t.Fatalf("len(Active) = %d, want 2", len(result.Active))
	}
	if len(result.Prunable) != 0 {
		t.Errorf("len(Prunable) = %d, want 0", len(result.Prunable))
	}
}
