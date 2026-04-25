package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func testModelsDeps() (*fsutil.MemFS, *bytes.Buffer, *bytes.Buffer, modelsDeps) {
	fsys := fsutil.NewMemFS()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	deps := modelsDeps{
		FS:       fsys,
		HomeDir:  "/home/test",
		Env:      func(string) string { return "" },
		Stdout:   stdout,
		Stderr:   stderr,
		Natives:  func() []modelregistry.Entry { return nil },
		Refresh:  func() error { return nil },
		UseProxy: func() bool { return false },
	}
	return fsys, stdout, stderr, deps
}

func TestRunModels_OverlayCommandsAutoRefresh(t *testing.T) {
	_, _, _, deps := testModelsDeps()
	refreshCalls := 0
	deps.Refresh = func() error {
		refreshCalls++
		return nil
	}

	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5", "--clone-from", "gpt-5.4"}, deps); err != nil {
		t.Fatalf("overlay add: %v", err)
	}
	if err := runModels([]string{"overlay", "remove", "--provider", "codex", "--id", "gpt-5.5"}, deps); err != nil {
		t.Fatalf("overlay remove: %v", err)
	}
	if err := runModels([]string{"overlay", "prune"}, deps); err != nil {
		t.Fatalf("overlay prune: %v", err)
	}
	if refreshCalls != 3 {
		t.Fatalf("refreshCalls = %d, want 3", refreshCalls)
	}
}

func TestRunModels_OverlayAddListRemoveJSON(t *testing.T) {
	_, stdout, _, deps := testModelsDeps()

	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5", "--clone-from", "gpt-5.4"}, deps); err != nil {
		t.Fatalf("overlay add: %v", err)
	}
	if err := runModels([]string{"list", "--json"}, deps); err != nil {
		t.Fatalf("list: %v", err)
	}

	var listed []modelregistry.Entry
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list: %v; output=%s", err, stdout.String())
	}
	if len(listed) != 1 {
		t.Fatalf("len(listed) = %d, want 1", len(listed))
	}
	if listed[0].Provider != modelregistry.ProviderCodex || listed[0].ID != "gpt-5.5" || listed[0].CloneFrom != "gpt-5.4" || listed[0].Source != modelregistry.SourceOverlay {
		t.Fatalf("listed[0] = %+v", listed[0])
	}

	stdout.Reset()
	if err := runModels([]string{"overlay", "remove", "--provider", "codex", "--id", "gpt-5.5"}, deps); err != nil {
		t.Fatalf("overlay remove: %v", err)
	}
	if err := runModels([]string{"list", "--json"}, deps); err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	listed = nil
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list after remove: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("len(listed) after remove = %d, want 0", len(listed))
	}
}

func TestRunModels_OverlayAddUpdatesExisting(t *testing.T) {
	_, stdout, _, deps := testModelsDeps()

	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5", "--clone-from", "gpt-5.4"}, deps); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5", "--clone-from", "gpt-5.3"}, deps); err != nil {
		t.Fatalf("second add: %v", err)
	}
	if err := runModels([]string{"list", "--json"}, deps); err != nil {
		t.Fatalf("list: %v", err)
	}

	var listed []modelregistry.Entry
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listed) != 1 || listed[0].CloneFrom != "gpt-5.3" {
		t.Fatalf("listed = %+v, want one updated entry", listed)
	}
}

func TestRunModels_OverlayPruneUsesCachedNativesByDefault(t *testing.T) {
	fsys, stdout, _, deps := testModelsDeps()
	deps.Natives = nil
	_ = fsys.WriteFile("/home/test/.codex/models_cache.json", []byte(`{
"client_version":"0.124.0",
"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5","context_window":272000}]
}`), 0o600)

	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5"}, deps); err != nil {
		t.Fatalf("add prunable: %v", err)
	}
	if err := runModels([]string{"overlay", "prune"}, deps); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(stdout.String(), "pruned 1 overlay models") {
		t.Fatalf("stdout = %q, want prune count", stdout.String())
	}
}

func TestRunModels_OverlayPruneUsesInjectedNatives(t *testing.T) {
	_, stdout, _, deps := testModelsDeps()
	deps.Natives = func() []modelregistry.Entry {
		return []modelregistry.Entry{{Provider: modelregistry.ProviderCodex, ID: "gpt-5.5", Source: modelregistry.SourceNative}}
	}

	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5"}, deps); err != nil {
		t.Fatalf("add prunable: %v", err)
	}
	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.6"}, deps); err != nil {
		t.Fatalf("add kept: %v", err)
	}
	if err := runModels([]string{"overlay", "prune"}, deps); err != nil {
		t.Fatalf("prune: %v", err)
	}
	stdout.Reset()
	if err := runModels([]string{"list", "--json"}, deps); err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed []modelregistry.Entry
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "gpt-5.6" {
		t.Fatalf("listed = %+v, want only gpt-5.6", listed)
	}
}

func TestRunModels_ListShowsCachedNatives(t *testing.T) {
	fsys, stdout, _, deps := testModelsDeps()
	_ = fsys.WriteFile("/home/test/.codex/models_cache.json", []byte(`{
"client_version":"0.124.0",
"models":[{"slug":"gpt-5.4","display_name":"GPT-5.4","context_window":1050000}]
}`), 0o600)
	_ = fsys.WriteFile("/home/test/.claude/cache/model-capabilities.json", []byte(`{
"timestamp":1700000000,
"models":[{"id":"claude-opus-4","max_input_tokens":200000,"max_tokens":32000}]
}`), 0o600)

	if err := runModels([]string{"list", "--json"}, deps); err != nil {
		t.Fatalf("list: %v", err)
	}

	var listed []modelregistry.Entry
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, stdout.String())
	}
	seen := map[string]modelregistry.Provider{}
	for _, e := range listed {
		seen[e.ID] = e.Provider
	}
	if seen["gpt-5.4"] != modelregistry.ProviderCodex {
		t.Errorf("gpt-5.4 missing or wrong provider: %+v", listed)
	}
	if seen["claude-opus-4"] != modelregistry.ProviderAnthropic {
		t.Errorf("claude-opus-4 missing or wrong provider: %+v", listed)
	}
}

func TestRunModels_ListRejectsUnknownProvider(t *testing.T) {
	_, _, _, deps := testModelsDeps()

	err := runModels([]string{"list", "--provider", "gemini"}, deps)
	if err == nil {
		t.Fatal("runModels list accepted unknown provider, want error")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("error = %v, want unknown provider", err)
	}
}

func TestRunModels_ListProviderFilterHuman(t *testing.T) {
	_, stdout, _, deps := testModelsDeps()
	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5"}, deps); err != nil {
		t.Fatalf("add codex: %v", err)
	}
	if err := runModels([]string{"overlay", "add", "--provider", "anthropic", "--id", "claude-test"}, deps); err != nil {
		t.Fatalf("add anthropic: %v", err)
	}
	stdout.Reset()
	if err := runModels([]string{"list", "--provider", "codex"}, deps); err != nil {
		t.Fatalf("list filter: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "gpt-5.5") || strings.Contains(out, "claude-test") {
		t.Fatalf("filtered output = %q", out)
	}
}

func TestRunModels_RefreshPrunesBeforeRefresh(t *testing.T) {
	fsys, _, _, deps := testModelsDeps()
	_ = fsys.WriteFile("/home/test/.codex/models_cache.json", []byte(`{
"client_version":"0.124.0",
"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5","context_window":272000}]
}`), 0o600)
	if err := runModels([]string{"overlay", "add", "--provider", "codex", "--id", "gpt-5.5"}, deps); err != nil {
		t.Fatalf("overlay add: %v", err)
	}

	if err := runModels([]string{"refresh"}, deps); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	deps.Stdout.(*bytes.Buffer).Reset()
	if err := runModels([]string{"list", "--json"}, deps); err != nil {
		t.Fatalf("list: %v", err)
	}

	var listed []modelregistry.Entry
	if err := json.Unmarshal(deps.Stdout.(*bytes.Buffer).Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	for _, e := range listed {
		if e.Provider == modelregistry.ProviderCodex && e.ID == "gpt-5.5" && e.Source == modelregistry.SourceOverlay {
			t.Fatal("refresh should prune overlay shadowed by native model")
		}
	}
}

func TestRunModels_RefreshCallsInjectedFallback(t *testing.T) {
	_, _, _, deps := testModelsDeps()
	called := false
	deps.Refresh = func() error {
		called = true
		return nil
	}
	if err := runModels([]string{"refresh"}, deps); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !called {
		t.Fatal("Refresh dependency was not called")
	}
}

func TestMainManualDispatchIncludesModels(t *testing.T) {
	file := parseGoFile(t, "main.go")
	body := findFuncBody(t, file, "main")
	if !hasStringLiteral(body, "models") {
		t.Fatal("main manual dispatch should include models before kong parsing")
	}
	if !hasIdentifier(body, "runModelsCommand") {
		t.Fatal("main manual dispatch should call runModelsCommand")
	}
}
