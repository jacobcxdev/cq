package modelregistry

import (
	"strings"
	"testing"
)

func TestValidateSnapshotAllowsUniqueIDs(t *testing.T) {
	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-sonnet-4-5", Source: SourceNative},
			{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceNative},
		},
	}
	if err := ValidateSnapshot(snap); err != nil {
		t.Errorf("ValidateSnapshot() = %v, want nil", err)
	}
}

func TestValidateSnapshotRejectsSameIDAcrossProviders(t *testing.T) {
	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "shared-id", Source: SourceNative},
			{Provider: ProviderCodex, ID: "shared-id", Source: SourceNative},
		},
	}
	err := ValidateSnapshot(snap)
	if err == nil {
		t.Fatal("ValidateSnapshot() = nil, want non-nil error for duplicate ID across providers")
	}
	if !strings.Contains(err.Error(), "shared-id") {
		t.Errorf("error %q should contain model ID %q", err.Error(), "shared-id")
	}
}

func TestValidateSnapshotAllowsDuplicateIDSameProvider(t *testing.T) {
	// Same ID under the same provider is allowed (e.g. aliases / stale entries).
	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-sonnet-4-5", Source: SourceNative},
			{Provider: ProviderAnthropic, ID: "claude-sonnet-4-5", Source: SourceOverlay},
		},
	}
	if err := ValidateSnapshot(snap); err != nil {
		t.Errorf("ValidateSnapshot() = %v, want nil for same-provider duplicates", err)
	}
}

// TestValidateSnapshotDeterministicOrdering verifies that ValidateSnapshot
// produces a deterministic error string regardless of the order entries appear
// in the snapshot. Two conflicting IDs are supplied with providers in reverse
// alphabetical order to confirm the output is always sorted by ID and then by
// provider name.
func TestValidateSnapshotDeterministicOrdering(t *testing.T) {
	// "zebra-model" sorts after "alpha-model"; codex sorts before anthropic
	// alphabetically — the input order is intentionally "wrong" to prove the
	// output is normalised.
	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderCodex, ID: "zebra-model", Source: SourceNative},
			{Provider: ProviderAnthropic, ID: "zebra-model", Source: SourceNative},
			{Provider: ProviderCodex, ID: "alpha-model", Source: SourceNative},
			{Provider: ProviderAnthropic, ID: "alpha-model", Source: SourceNative},
		},
	}
	err := ValidateSnapshot(snap)
	if err == nil {
		t.Fatal("ValidateSnapshot() = nil, want error for duplicate IDs across providers")
	}
	const want = "model registry contains duplicate model IDs across providers: alpha-model (anthropic, codex); zebra-model (anthropic, codex)"
	if err.Error() != want {
		t.Errorf("ValidateSnapshot() error =\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// TestValidateSnapshotCaseInsensitiveDuplicates verifies that model IDs that
// differ only in case (e.g. "Foo" vs "foo") are treated as duplicates when they
// appear under different providers.
func TestValidateSnapshotCaseInsensitiveDuplicates(t *testing.T) {
	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "Claude-Sonnet", Source: SourceNative},
			{Provider: ProviderCodex, ID: "claude-sonnet", Source: SourceNative},
		},
	}
	err := ValidateSnapshot(snap)
	if err == nil {
		t.Fatal("ValidateSnapshot() = nil, want error for case-insensitive cross-provider duplicate")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "claude-sonnet") {
		t.Errorf("error %q should reference the duplicate model ID", err.Error())
	}
}

// TestValidateSnapshotAllowsSameIDSameProviderDifferentCase documents that the
// same ID in different cases under the same provider is treated as same-provider
// and is allowed (same-provider duplicates are always permitted).
func TestValidateSnapshotAllowsSameIDSameProviderDifferentCase(t *testing.T) {
	snap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "Claude-Sonnet", Source: SourceNative},
			{Provider: ProviderAnthropic, ID: "claude-sonnet", Source: SourceOverlay},
		},
	}
	if err := ValidateSnapshot(snap); err != nil {
		t.Errorf("ValidateSnapshot() = %v, want nil for same-provider case variants", err)
	}
}

// TestValidateSnapshotRejectsMergeSameIDDifferentProvider documents that Merge
// itself may produce a snapshot containing the same ID across different providers
// (it does not validate cross-provider uniqueness), and that ValidateSnapshot is
// the layer that rejects such outputs.
func TestValidateSnapshotRejectsMergeSameIDDifferentProvider(t *testing.T) {
	// Merge produces a result where native Anthropic and overlay Codex share "model-x".
	// This is legal from Merge's perspective (it only deduplicates within the same
	// provider), but ValidateSnapshot must reject it.
	natives := []Entry{
		{Provider: ProviderAnthropic, ID: "model-x", Source: SourceNative},
	}
	overlays := []Entry{
		{Provider: ProviderCodex, ID: "model-x", Source: SourceOverlay},
	}
	merged := Merge(natives, overlays)

	// Merge must produce both entries (no conflict at merge level).
	if len(merged.Active) != 2 {
		t.Fatalf("Merge produced %d active entries, want 2", len(merged.Active))
	}

	snap := Snapshot{Entries: merged.Active}
	err := ValidateSnapshot(snap)
	if err == nil {
		t.Fatal("ValidateSnapshot() = nil, want error for same ID across providers after Merge")
	}
	if !strings.Contains(err.Error(), "model-x") {
		t.Errorf("error %q should contain model ID %q", err.Error(), "model-x")
	}
}
