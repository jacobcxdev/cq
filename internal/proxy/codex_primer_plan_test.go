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

func TestValidateCodexPrimerOverridesRequiresVisibleRegistryModels(t *testing.T) {
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
	if len(targets) != 0 || len(unresolved) != 1 {
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
