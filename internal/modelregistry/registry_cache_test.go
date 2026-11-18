package modelregistry

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestLoadCodexRegistryCacheRejectsInvalidEntries(t *testing.T) {
	for _, body := range []string{
		`null`, `{`, `{"version":2}`, `{"version":1,"models":[{}]}`,
		`{"version":1,"models":[{"id":"model","provider":"codex","source":"overlay"}]}`,
		`{"version":1,"models":[{"id":"model","provider":"anthropic","source":"native"}]}`,
		`{"version":1,"models":[{"id":"model","provider":"fake-subscription","source":"native","context_window":100}]}`,
		`{"version":1,"models":[{"id":"model","provider":"fake-subscription","source":"native","context_window":100,"shared_budget":{}}]}`,
		`{"version":1,"shared_budget_models":{"unknown":[{"id":"model","provider":"fake-subscription","source":"native","context_window":100,"shared_budget":{"provider_name":"Fake"}}]}}`,
		`{"version":1,"shared_budget_models":{"model":[]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			roots := userdirs.Roots{Cache: "/cq/cache"}
			_ = fsys.WriteFile(CodexRegistryCachePath(roots), []byte(body), 0o600)
			if _, _, err := LoadCodexRegistryCache(fsys, roots); err == nil {
				t.Fatal("invalid registry cache accepted")
			}
		})
	}
}

func TestCodexRegistryCacheSeedsSharedBudgetRoutes(t *testing.T) {
	fsys := fsutil.NewMemFS()
	roots := userdirs.Roots{Cache: "/cq/cache"}
	snap := Snapshot{Entries: []Entry{{
		Provider: "fake-subscription", ID: "Test", ContextWindow: 100, Source: SourceNative,
		SharedBudget: &SharedBudgetModel{ProviderName: "Test Provider", ReasoningEfforts: []string{"high"}},
	}}}
	if err := PublishCodexRegistryCache(fsys, roots, snap); err != nil {
		t.Fatal(err)
	}
	entries, present, err := LoadCodexRegistryCache(fsys, roots)
	if err != nil || !present {
		t.Fatalf("cache present = %v, error = %v", present, err)
	}
	catalog := NewCatalog(Snapshot{Entries: entries})
	routes := catalog.SharedBudgetModels("TEST")
	if !catalog.HasSharedBudgetModels() || len(routes) != 1 || routes[0].SharedBudget.ProviderName != "Test Provider" {
		t.Fatalf("seeded routes = %+v", routes)
	}
	entries[0].SharedBudget.ReasoningEfforts[0] = "invalid"
	routes[0].SharedBudget.ReasoningEfforts[0] = "invalid"
	catalog.Snapshot().SharedBudgetModels["test"][0].SharedBudget.ProviderName = "mutated"
	copied := catalog.SharedBudgetModels("test")[0].SharedBudget
	if copied.ProviderName != "Test Provider" || copied.ReasoningEfforts[0] != "high" ||
		catalog.Snapshot().Entries[0].SharedBudget.ReasoningEfforts[0] != "high" {
		t.Fatalf("mutation escaped a copy of the catalogue: %+v", copied)
	}
}

func TestCodexRegistryCacheExcludesOverlaysAndPreservesMissingState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	roots := userdirs.Roots{Cache: "/cq/cache"}
	if _, present, err := LoadCodexRegistryCache(fsys, roots); err != nil || present {
		t.Fatalf("missing cache: %t, %v", present, err)
	}
	snap := Snapshot{Entries: []Entry{
		{Provider: ProviderCodex, ID: "native", Source: SourceNative},
		{Provider: ProviderCodex, ID: "alias", Source: SourceOverlay},
	}}
	if err := PublishCodexRegistryCache(fsys, roots, snap); err != nil {
		t.Fatal(err)
	}
	entries, present, err := LoadCodexRegistryCache(fsys, roots)
	if err != nil || !present || len(entries) != 1 || entries[0].ID != "native" {
		t.Fatalf("cache = %+v, %t, %v", entries, present, err)
	}
	if _, err := fsys.ReadFile(CodexRegistryCachePath(roots) + ".tmp"); err == nil {
		t.Fatal("temporary file survived")
	}
}

func TestCodexRegistryCacheDoesNotOverwriteOverlaysWithSharedRoots(t *testing.T) {
	fsys := fsutil.NewMemFS()
	roots := userdirs.Roots{Config: "/cq", Cache: "/cq"}
	const overlay = `{"version":1,"models":[{"id":"alias","provider":"codex","source":"overlay"}]}`
	_ = fsys.WriteFile(OverlayPath(roots), []byte(overlay), 0o600)
	if err := PublishCodexRegistryCache(fsys, roots, Snapshot{}); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile(OverlayPath(roots))
	if err != nil || string(data) != overlay {
		t.Fatal("registry cache overwrote user overlays")
	}
}
