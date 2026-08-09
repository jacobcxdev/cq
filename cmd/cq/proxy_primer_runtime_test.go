package main

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestBuildCodexPrimerRequiresEnabledOwner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/config")
	catalog := modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark", Visibility: "list"},
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.4", Visibility: "list"},
	}})
	fsys := fsutil.NewMemFS()
	cfg := &proxy.Config{CodexUpstream: "https://chatgpt.example/backend-api/codex"}
	primer, err := buildCodexPrimer(cfg, true, nil, catalog, fsys)
	if err != nil || primer != nil {
		t.Fatalf("disabled primer = %v, %v", primer, err)
	}
	cfg.CodexWindowPriming.Enabled = true
	primer, err = buildCodexPrimer(cfg, false, nil, catalog, fsys)
	if err != nil || primer != nil {
		t.Fatalf("delegate primer = %v, %v", primer, err)
	}
	primer, err = buildCodexPrimer(cfg, true, &proxy.CodexRequestRouter{}, catalog, fsys)
	if err != nil || primer == nil {
		t.Fatalf("owner primer = %v, %v", primer, err)
	}
}

func TestBuildCodexPrimerRejectsUnavailableOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/config")
	cfg := &proxy.Config{
		CodexUpstream: "https://chatgpt.example/backend-api/codex",
		CodexWindowPriming: proxy.CodexWindowPrimingConfig{
			Enabled: true, ModelOverrides: map[string]string{"spark": "missing"},
		},
	}
	catalog := modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{{
		Provider: modelregistry.ProviderCodex, ID: "gpt-5.3-codex-spark", Visibility: "list",
	}}})
	if _, err := buildCodexPrimer(cfg, true, &proxy.CodexRequestRouter{}, catalog, fsutil.NewMemFS()); err == nil {
		t.Fatal("unavailable override accepted")
	}
}
