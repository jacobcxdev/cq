package proxy

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

type testCodexPrimerAdapter struct {
	revision string
	families map[string][]string
}

func (a testCodexPrimerAdapter) Revision() string { return a.revision }

func (a testCodexPrimerAdapter) ResolveFamilies(scope string) []string {
	return append([]string(nil), a.families[scope]...)
}

func TestCodexPrimerModelResolutionPrecedence(t *testing.T) {
	entries := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark", Aliases: []string{"spark-alias"}, DisplayName: "Codex Spark", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "o3-pro", Aliases: []string{"reasoning-pro"}, DisplayName: "O3 Pro", Visibility: "list", Priority: 2},
	}
	adapter := testCodexPrimerAdapter{
		revision: "test-adapter-v1",
		families: map[string][]string{
			"backend-special": {"gpt-codex-spark"},
			"spark-alias":     {"o3-pro"},
			"Codex Spark":     {"o3-pro"},
			"spark codex":     {"o3-pro"},
		},
	}
	tests := []struct {
		name      string
		scope     string
		overrides map[string]string
		wantID    string
		wantStage codexPrimerResolutionStage
	}{
		{name: "override before every inferred stage", scope: "backend-special", overrides: map[string]string{"backend-special": "o3-pro"}, wantID: "o3-pro", wantStage: codexPrimerResolutionOverride},
		{name: "exact id", scope: "gpt-5.3-codex-spark", wantID: "gpt-5.3-codex-spark", wantStage: codexPrimerResolutionExact},
		{name: "exact alias before adapter", scope: "spark-alias", wantID: "gpt-5.3-codex-spark", wantStage: codexPrimerResolutionExact},
		{name: "exact display before adapter", scope: "Codex Spark", wantID: "gpt-5.3-codex-spark", wantStage: codexPrimerResolutionExact},
		{name: "unique token family before adapter", scope: "spark codex", wantID: "gpt-5.3-codex-spark", wantStage: codexPrimerResolutionTokenFamily},
		{name: "provider adapter", scope: "backend-special", wantID: "gpt-5.3-codex-spark", wantStage: codexPrimerResolutionProviderAdapter},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := resolveCodexPrimerModelWithAdapter(test.scope, test.overrides, entries, adapter)
			if err != nil {
				t.Fatal(err)
			}
			if resolution.Selected.ID != test.wantID || resolution.Stage != test.wantStage {
				t.Fatalf("resolution = %+v", resolution)
			}
		})
	}
}

func TestCodexPrimerModelResolutionFailsClosed(t *testing.T) {
	entries := []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5-pro", Visibility: "list", Priority: 1},
		{Provider: modelregistry.ProviderCodex, ID: "o3-pro", Visibility: "list", Priority: 2},
	}
	tests := []struct {
		name    string
		scope   string
		adapter testCodexPrimerAdapter
	}{
		{name: "ambiguous token family", scope: "pro"},
		{name: "arbitrary substring", scope: "ro"},
		{name: "ambiguous adapter", scope: "backend-special", adapter: testCodexPrimerAdapter{revision: "v1", families: map[string][]string{"backend-special": {"gpt-pro", "o3-pro"}}}},
		{name: "unknown adapter family", scope: "backend-special", adapter: testCodexPrimerAdapter{revision: "v1", families: map[string][]string{"backend-special": {"missing"}}}},
		{name: "unversioned adapter", scope: "backend-special", adapter: testCodexPrimerAdapter{families: map[string][]string{"backend-special": {"gpt-pro"}}}},
		{name: "unresolved", scope: "backend-special"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolveCodexPrimerModelWithAdapter(test.scope, nil, entries, test.adapter); err == nil {
				t.Fatal("scope resolved")
			}
		})
	}
}
