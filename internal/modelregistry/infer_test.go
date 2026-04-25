package modelregistry

import (
	"testing"
)

// helpers to build test entries concisely.
func codexEntry(id string) Entry {
	return Entry{Provider: ProviderCodex, ID: id, Source: SourceNative}
}

func anthropicEntry(id string) Entry {
	return Entry{Provider: ProviderAnthropic, ID: id, Source: SourceNative}
}

// TestInferClone_SuccorSiblingModel verifies that a new Codex model infers its
// clone_from as the most similar existing native Codex model.
func TestInferClone_SuccessorSiblingModel(t *testing.T) {
	overlay := Entry{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceOverlay}
	natives := []Entry{
		codexEntry("gpt-5.4"),
		codexEntry("gpt-4.1"),
		codexEntry("gpt-4o"),
	}

	got, ok := InferClone(overlay, natives)
	if !ok {
		t.Fatal("InferClone() returned ok=false, want true")
	}
	if got.ID != "gpt-5.4" {
		t.Errorf("InferClone().ID = %q, want %q", got.ID, "gpt-5.4")
	}
}

// TestInferClone_MiniVariantPrefersExactFamilyMatch ensures that a mini-suffixed
// overlay prefers a mini native over the base model.
func TestInferClone_MiniVariantPrefersExactFamilyMatch(t *testing.T) {
	overlay := Entry{Provider: ProviderCodex, ID: "gpt-5.5-mini", Source: SourceOverlay}
	natives := []Entry{
		codexEntry("gpt-5.4"),
		codexEntry("gpt-5.4-mini"),
		codexEntry("gpt-4o-mini"),
	}

	got, ok := InferClone(overlay, natives)
	if !ok {
		t.Fatal("InferClone() returned ok=false, want true")
	}
	if got.ID != "gpt-5.4-mini" {
		t.Errorf("InferClone().ID = %q, want %q", got.ID, "gpt-5.4-mini")
	}
}

// TestInferClone_AnthropicFamilyIsolation ensures that a new haiku overlay
// does not clone a sonnet or opus native; it must pick within its family.
func TestInferClone_AnthropicFamilyIsolation(t *testing.T) {
	overlay := Entry{Provider: ProviderAnthropic, ID: "claude-haiku-4-6-20260424", Source: SourceOverlay}
	natives := []Entry{
		anthropicEntry("claude-haiku-4-5-20250714"),
		anthropicEntry("claude-sonnet-4-5-20250715"),
		anthropicEntry("claude-opus-4-5-20250801"),
	}

	got, ok := InferClone(overlay, natives)
	if !ok {
		t.Fatal("InferClone() returned ok=false, want true")
	}
	if got.ID != "claude-haiku-4-5-20250714" {
		t.Errorf("InferClone().ID = %q, want %q", got.ID, "claude-haiku-4-5-20250714")
	}
}

// TestInferClone_NoCrossProviderClone verifies that a Codex overlay does not
// pick an Anthropic native as its clone source.
func TestInferClone_NoCrossProviderClone(t *testing.T) {
	overlay := Entry{Provider: ProviderCodex, ID: "gpt-o3", Source: SourceOverlay}
	natives := []Entry{
		anthropicEntry("gpt-5.4"), // wrong provider — should never match
		codexEntry("o1"),
		codexEntry("o1-mini"),
	}

	// "gpt-o3" shares no non-numeric token overlap with "o1" or "o1-mini"
	// beyond what the confidence threshold allows.
	_, ok := InferClone(overlay, natives)
	if ok {
		t.Error("InferClone() returned ok=true for low-confidence match, want false")
	}
}

// TestInferClone_TieBreakDeterministic confirms that calling InferClone
// multiple times with the same input always returns the same result.
func TestInferClone_LeadingTokenOverlapBreaksTie(t *testing.T) {
	overlay := Entry{Provider: ProviderCodex, ID: "gpt-alpha-beta", Source: SourceOverlay}
	natives := []Entry{
		codexEntry("alpha-gpt-beta"),
		codexEntry("gpt-beta-alpha"),
	}

	got, ok := InferClone(overlay, natives)
	if !ok {
		t.Fatal("InferClone() returned ok=false, want true")
	}
	if got.ID != "gpt-beta-alpha" {
		t.Errorf("InferClone().ID = %q, want %q", got.ID, "gpt-beta-alpha")
	}
}

func TestInferClone_LatestNumericVersionBreaksTie(t *testing.T) {
	overlay := Entry{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceOverlay}
	natives := []Entry{
		codexEntry("gpt-5.3"),
		codexEntry("gpt-5.4"),
	}

	got, ok := InferClone(overlay, natives)
	if !ok {
		t.Fatal("InferClone() returned ok=false, want true")
	}
	if got.ID != "gpt-5.4" {
		t.Errorf("InferClone().ID = %q, want %q", got.ID, "gpt-5.4")
	}
}

func TestInferClone_TieBreakDeterministic(t *testing.T) {
	overlay := Entry{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceOverlay}
	natives := []Entry{
		codexEntry("gpt-5.4a"),
		codexEntry("gpt-5.4b"),
	}

	first, ok1 := InferClone(overlay, natives)
	if !ok1 {
		t.Fatal("first call: InferClone() returned ok=false")
	}
	for i := 0; i < 20; i++ {
		got, ok := InferClone(overlay, natives)
		if !ok {
			t.Fatalf("iteration %d: InferClone() returned ok=false", i)
		}
		if got.ID != first.ID {
			t.Errorf("iteration %d: InferClone().ID = %q, want %q (non-deterministic)", i, got.ID, first.ID)
		}
	}
}

// TestInferClone_ExplicitCloneFromBypassesHeuristic verifies that when the
// overlay already has CloneFrom set, InferClone respects that value and returns
// the matching native (or the explicit entry) without running scoring.
func TestInferClone_ExplicitCloneFromBypassesHeuristic(t *testing.T) {
	overlay := Entry{
		Provider:  ProviderCodex,
		ID:        "gpt-5.5",
		Source:    SourceOverlay,
		CloneFrom: "gpt-5.4",
	}
	natives := []Entry{
		codexEntry("gpt-5.4"),
		codexEntry("gpt-4.1"), // would also be a reasonable heuristic pick
	}

	got, ok := InferClone(overlay, natives)
	if !ok {
		t.Fatal("InferClone() returned ok=false for explicit clone_from, want true")
	}
	if got.ID != "gpt-5.4" {
		t.Errorf("InferClone().ID = %q, want %q (explicit clone_from not honoured)", got.ID, "gpt-5.4")
	}
}

// TestInferClone_ExplicitCloneFromNotFoundReturnsFalse verifies that when
// clone_from names a model that is not in natives, InferClone returns false.
func TestInferClone_ExplicitCloneFromNotFoundReturnsFalse(t *testing.T) {
	overlay := Entry{
		Provider:  ProviderCodex,
		ID:        "gpt-5.5",
		Source:    SourceOverlay,
		CloneFrom: "gpt-nonexistent",
	}
	natives := []Entry{
		codexEntry("gpt-5.4"),
	}

	_, ok := InferClone(overlay, natives)
	if ok {
		t.Error("InferClone() returned ok=true when explicit clone_from is not in natives, want false")
	}
}
