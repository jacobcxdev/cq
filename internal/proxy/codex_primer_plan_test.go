package proxy

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func primerRegistryEntries() []modelregistry.Entry {
	return []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark", DisplayName: "GPT-5.3-Codex-Spark", Visibility: "list", Priority: 26},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4", DisplayName: "GPT-5.4", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "hidden-spark", DisplayName: "Hidden Spark", Visibility: "hidden", Priority: 0},
	}
}

func TestResolveCodexPrimerModelExactDisplayName(t *testing.T) {
	entry, err := ResolveCodexPrimerModel("GPT-5.3-Codex-Spark", nil, primerRegistryEntries())
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "gpt-5.3-codex-spark" {
		t.Fatalf("model = %q", entry.ID)
	}
}

func TestResolveCodexPrimerModelRejectsAmbiguousFamily(t *testing.T) {
	entries := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5-pro", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "o3-pro", Visibility: "list", Priority: 2},
	}
	if _, err := ResolveCodexPrimerModel("Pro", nil, entries); err == nil {
		t.Fatal("ambiguous family resolved")
	}
}

func TestResolveCodexPrimerModelUsesExactOverride(t *testing.T) {
	entry, err := ResolveCodexPrimerModel("future-backend-name", map[string]string{"future-backend-name": "gpt-5.4"}, primerRegistryEntries())
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "gpt-5.4" {
		t.Fatalf("override model = %q", entry.ID)
	}
}

func TestCodexPrimerModelOverrideValidationRequiresVisibleRegistryModels(t *testing.T) {
	if err := ValidateCodexPrimerOverrides(map[string]string{"codex_spark": "gpt-5.3-codex-spark"}, primerRegistryEntries()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCodexPrimerOverrides(map[string]string{"codex_spark": "missing"}, primerRegistryEntries()); err == nil {
		t.Fatal("missing override model accepted")
	}
}

func TestPlanCodexPrimerTargetsSeparatesScopedAndSharedCapabilities(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	descriptors := []codex.WindowDescriptor{
		{RawLimitName: "primary_window", WindowName: quota.Window5Hour, Period: 5 * time.Hour, ScopeKind: codex.WindowScopeShared, ResetAt: reset},
		{RawLimitName: "secondary_window", WindowName: quota.Window7Day, Period: 7 * 24 * time.Hour, ScopeKind: codex.WindowScopeShared, ResetAt: reset},
		{RawLimitName: "GPT-5.3-Codex-Spark", WindowName: "7d:GPT-5.3-Codex-Spark", Period: 7 * 24 * time.Hour, ScopeKind: codex.WindowScopeModelFamily, Scope: "GPT-5.3-Codex-Spark", ResetAt: reset},
	}

	targets, unresolved := PlanCodexPrimerTargets(descriptors, nil, primerRegistryEntries())
	if len(unresolved) != 0 || len(targets) != 2 {
		t.Fatalf("targets/unresolved = %+v/%+v", targets, unresolved)
	}
	byModel := make(map[string]CodexPrimerTarget)
	for _, target := range targets {
		byModel[target.ModelID] = target
	}
	if len(byModel["gpt-5.3-codex-spark"].Windows) != 1 || len(byModel["gpt-5.4"].Windows) != 2 {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestPlanCodexPrimerSharedWindowFailsClosedWithOnlySparkModel(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	descriptors := []codex.WindowDescriptor{{
		RawLimitName: "primary_window", WindowName: quota.Window7Day,
		Period: 7 * 24 * time.Hour, ScopeKind: codex.WindowScopeShared, ResetAt: reset,
	}}
	entries := []modelregistry.Entry{{
		Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark",
		DisplayName: "GPT-5.3-Codex-Spark", Visibility: "list", Priority: 1,
	}}

	targets, unresolved := PlanCodexPrimerTargets(descriptors, nil, entries)
	if len(targets) != 0 || len(unresolved) != 1 || unresolved[0].Code != "model_unresolved" {
		t.Fatalf("targets/unresolved = %+v/%+v", targets, unresolved)
	}
}

func TestPlanCodexPrimerSharedOverrideRequiresNonSparkCapability(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	descriptors := []codex.WindowDescriptor{{
		RawLimitName: "primary_window", WindowName: quota.Window7Day,
		Period: 7 * 24 * time.Hour, ScopeKind: codex.WindowScopeShared, ResetAt: reset,
	}}
	entries := append(primerRegistryEntries(), modelregistry.Entry{
		Provider: modelregistry.ProviderCodex, ID: "gpt-5.4-mini", Visibility: "list", Priority: 2,
	})

	targets, unresolved := PlanCodexPrimerTargets(descriptors, map[string]string{"primary_window": "gpt-5.4-mini"}, entries)
	if len(unresolved) != 0 || len(targets) != 1 || targets[0].ModelID != "gpt-5.4-mini" {
		t.Fatalf("general override = %+v/%+v", targets, unresolved)
	}
	targets, unresolved = PlanCodexPrimerTargets(descriptors, map[string]string{"primary_window": "gpt-5.3-codex-spark"}, entries)
	if len(targets) != 0 || len(unresolved) != 1 || unresolved[0].Code != "override_incompatible" {
		t.Fatalf("Spark shared override = %+v/%+v", targets, unresolved)
	}
}

func TestPlanCodexPrimerTargetsKeepsResetEpochsSeparate(t *testing.T) {
	first := time.Unix(1774569600, 0)
	second := first.Add(time.Hour)
	descriptors := []codex.WindowDescriptor{
		{RawLimitName: "primary_window", WindowName: quota.Window7Day, ScopeKind: codex.WindowScopeShared, ResetAt: first},
		{RawLimitName: "secondary_window", WindowName: quota.Window7Day, ScopeKind: codex.WindowScopeShared, ResetAt: second},
	}

	targets, unresolved := PlanCodexPrimerTargets(descriptors, nil, primerRegistryEntries())
	if len(unresolved) != 0 || len(targets) != 2 {
		t.Fatalf("targets/unresolved = %+v/%+v", targets, unresolved)
	}
	if targets[0].ResetAt.Equal(targets[1].ResetAt) {
		t.Fatalf("epochs coalesced: %+v", targets)
	}
}

func TestPlanCodexPrimerTargetsCoalescesCompatibleScopedModels(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	descriptors := []codex.WindowDescriptor{
		{RawLimitName: "gpt-5.3-codex", WindowName: "7d:gpt-5.3-codex", ScopeKind: codex.WindowScopeModelFamily, Scope: "gpt-5.3-codex", ResetAt: reset},
		{RawLimitName: "gpt-5.4-codex", WindowName: "7d:gpt-5.4-codex", ScopeKind: codex.WindowScopeModelFamily, Scope: "gpt-5.4-codex", ResetAt: reset},
	}
	entries := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex", Visibility: "list", Priority: 2},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4-codex", Visibility: "list", Priority: 1},
	}

	targets, unresolved := PlanCodexPrimerTargets(descriptors, nil, entries)
	if len(unresolved) != 0 || len(targets) != 1 {
		t.Fatalf("targets/unresolved = %+v/%+v", targets, unresolved)
	}
	if len(targets[0].Windows) != 2 {
		t.Fatalf("windows = %+v", targets[0].Windows)
	}
	if targets[0].ScopeKind != codex.WindowScopeModelFamily || targets[0].CapabilityFamily != "gpt-codex" {
		t.Fatalf("capability = %+v", targets[0])
	}
	wantCompatible := []string{"gpt-5.3-codex", "gpt-5.4-codex"}
	if !equalPrimerStrings(targets[0].CompatibleModelIDs, wantCompatible) {
		t.Fatalf("compatible IDs = %v, want %v", targets[0].CompatibleModelIDs, wantCompatible)
	}
	if targets[0].ModelID != "gpt-5.4-codex" || targets[0].PolicyRevision == "" || targets[0].CapabilityHash == "" {
		t.Fatalf("target = %+v", targets[0])
	}
}

func TestCodexPrimerPolicyIgnoresRegistryPreferenceAndUnrelatedFacts(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	descriptors := []codex.WindowDescriptor{{
		RawLimitName: "codex", WindowName: "7d:codex", ScopeKind: codex.WindowScopeModelFamily, Scope: "codex", ResetAt: reset,
	}}
	base := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex", Aliases: []string{"codex-5.3"}, DisplayName: "GPT 5.3 Codex", Description: "base", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4-codex", Aliases: []string{"codex-5.4"}, DisplayName: "GPT 5.4 Codex", Description: "base", Visibility: "list", Priority: 2},
	}
	changed := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "o3-pro", DisplayName: "Unrelated", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4-codex", Aliases: []string{"codex-5.4", "codex-5.4"}, DisplayName: "GPT 5.4 Codex", Description: "changed", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex", Aliases: []string{"codex-5.3"}, DisplayName: "GPT 5.3 Codex", Description: "changed", Visibility: "list", Priority: 2},
	}

	baseTargets, baseUnresolved := PlanCodexPrimerTargets(descriptors, nil, base)
	changedTargets, changedUnresolved := PlanCodexPrimerTargets(descriptors, nil, changed)
	if len(baseUnresolved) != 0 || len(changedUnresolved) != 0 || len(baseTargets) != 1 || len(changedTargets) != 1 {
		t.Fatalf("plans = %+v/%+v; %+v/%+v", baseTargets, baseUnresolved, changedTargets, changedUnresolved)
	}
	if baseTargets[0].ModelID == changedTargets[0].ModelID {
		t.Fatalf("selection did not follow priority: %+v / %+v", baseTargets[0], changedTargets[0])
	}
	if baseTargets[0].PolicyRevision != changedTargets[0].PolicyRevision {
		t.Fatalf("policy revision changed for preference-only facts: %q / %q", baseTargets[0].PolicyRevision, changedTargets[0].PolicyRevision)
	}
	if baseTargets[0].CapabilityHash != changedTargets[0].CapabilityHash {
		t.Fatalf("capability hash changed for preference-only facts: %q / %q", baseTargets[0].CapabilityHash, changedTargets[0].CapabilityHash)
	}
}

func TestCodexPrimerPolicyTracksCompatibilityFacts(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	descriptors := []codex.WindowDescriptor{{
		RawLimitName: "codex", WindowName: "7d:codex", ScopeKind: codex.WindowScopeModelFamily, Scope: "codex", ResetAt: reset,
	}}
	base := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex", Aliases: []string{"codex-5.3"}, DisplayName: "GPT 5.3 Codex", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4-codex", Aliases: []string{"codex-5.4"}, DisplayName: "GPT 5.4 Codex", Visibility: "list", Priority: 2},
	}
	removed := base[:1]
	aliased := append([]modelregistry.Entry(nil), base...)
	aliased[1].Aliases = []string{"codex-next"}

	baseTarget := mustPlanOnePrimerTarget(t, descriptors, nil, base)
	removedTarget := mustPlanOnePrimerTarget(t, descriptors, nil, removed)
	aliasedTarget := mustPlanOnePrimerTarget(t, descriptors, nil, aliased)
	if baseTarget.PolicyRevision == removedTarget.PolicyRevision || baseTarget.CapabilityHash == removedTarget.CapabilityHash {
		t.Fatalf("compatible-set removal did not fence policy: %+v / %+v", baseTarget, removedTarget)
	}
	if baseTarget.PolicyRevision == aliasedTarget.PolicyRevision {
		t.Fatalf("alias membership did not change policy revision: %+v / %+v", baseTarget, aliasedTarget)
	}
	if baseTarget.CapabilityHash != aliasedTarget.CapabilityHash {
		t.Fatalf("alias-only change altered semantic capability: %+v / %+v", baseTarget, aliasedTarget)
	}
}

func TestCodexPrimerPolicyTracksExactOverrideAndProviderAdapter(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	entries := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4-codex", Visibility: "list", Priority: 2},
		{Provider: modelregistry.ProviderCodex, ID: "o3-pro", Visibility: "list", Priority: 3},
	}
	descriptors := []codex.WindowDescriptor{{
		RawLimitName: "backend-special", WindowName: "7d:backend-special", ScopeKind: codex.WindowScopeModelFamily, Scope: "backend-special", ResetAt: reset,
	}}
	firstOverride := mustPlanOnePrimerTarget(t, descriptors, map[string]string{"backend-special": "gpt-5.3-codex"}, entries)
	secondOverride := mustPlanOnePrimerTarget(t, descriptors, map[string]string{"backend-special": "gpt-5.4-codex"}, entries)
	if firstOverride.PolicyRevision == secondOverride.PolicyRevision || firstOverride.CapabilityHash == secondOverride.CapabilityHash {
		t.Fatalf("override change did not fence capability: %+v / %+v", firstOverride, secondOverride)
	}

	adapterV1 := testCodexPrimerAdapter{revision: "adapter-v1", families: map[string][]string{"backend-special": {"gpt-codex"}}}
	adapterV2 := testCodexPrimerAdapter{revision: "adapter-v2", families: map[string][]string{"backend-special": {"gpt-codex"}}}
	adapterOther := testCodexPrimerAdapter{revision: "adapter-v3", families: map[string][]string{"backend-special": {"o3-pro"}}}
	v1 := mustPlanOnePrimerTargetWithPolicy(t, descriptors, nil, entries, adapterV1, codexPrimerSharedRuleRevision)
	v2 := mustPlanOnePrimerTargetWithPolicy(t, descriptors, nil, entries, adapterV2, codexPrimerSharedRuleRevision)
	other := mustPlanOnePrimerTargetWithPolicy(t, descriptors, nil, entries, adapterOther, codexPrimerSharedRuleRevision)
	if v1.PolicyRevision == v2.PolicyRevision || v1.CapabilityHash != v2.CapabilityHash {
		t.Fatalf("adapter revision fence = %+v / %+v", v1, v2)
	}
	if v1.PolicyRevision == other.PolicyRevision || v1.CapabilityHash == other.CapabilityHash {
		t.Fatalf("adapter mapping fence = %+v / %+v", v1, other)
	}
}

func TestCodexPrimerPolicyTracksSharedRuleRevision(t *testing.T) {
	reset := time.Unix(1774569600, 0)
	descriptors := []codex.WindowDescriptor{{RawLimitName: "primary_window", ScopeKind: codex.WindowScopeShared, ResetAt: reset}}
	first := mustPlanOnePrimerTargetWithPolicy(t, descriptors, nil, primerRegistryEntries(), defaultCodexPrimerProviderAdapter{}, "shared-non-spark-v1")
	second := mustPlanOnePrimerTargetWithPolicy(t, descriptors, nil, primerRegistryEntries(), defaultCodexPrimerProviderAdapter{}, "shared-non-spark-v2")
	if first.PolicyRevision == second.PolicyRevision {
		t.Fatalf("shared rule revision did not change policy: %+v / %+v", first, second)
	}
	if first.CapabilityHash != second.CapabilityHash {
		t.Fatalf("rule revision alone changed semantic capability: %+v / %+v", first, second)
	}
}

func mustPlanOnePrimerTarget(t *testing.T, descriptors []codex.WindowDescriptor, overrides map[string]string, entries []modelregistry.Entry) CodexPrimerTarget {
	t.Helper()
	return mustPlanOnePrimerTargetWithPolicy(t, descriptors, overrides, entries, defaultCodexPrimerProviderAdapter{}, codexPrimerSharedRuleRevision)
}

func mustPlanOnePrimerTargetWithPolicy(t *testing.T, descriptors []codex.WindowDescriptor, overrides map[string]string, entries []modelregistry.Entry, adapter codexPrimerProviderAdapter, sharedRuleRevision string) CodexPrimerTarget {
	t.Helper()
	targets, unresolved := planCodexPrimerTargetsWithPolicy(descriptors, overrides, entries, adapter, sharedRuleRevision)
	if len(unresolved) != 0 || len(targets) != 1 {
		t.Fatalf("targets/unresolved = %+v/%+v", targets, unresolved)
	}
	return targets[0]
}

func equalPrimerStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
