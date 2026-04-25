package modelregistry

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestLoadCodexEntriesFromCache_MissingFile(t *testing.T) {
	entries, err := LoadCodexEntriesFromCache(fsutil.NewMemFS(), "/no/such/file.json")
	if err != nil {
		t.Fatalf("LoadCodexEntriesFromCache: %v", err)
	}
	if entries != nil {
		t.Errorf("entries = %v, want nil", entries)
	}
}

func TestLoadCodexEntriesFromCache_ParsesRichInfo(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.codex/models_cache.json"
	envelope := `{
  "fetched_at": "2026-04-24T00:00:00Z",
  "client_version": "0.124.0",
  "models": [
    {"slug":"gpt-5.4","display_name":"GPT-5.4","description":"Flagship","context_window":1050000,"priority":1,"visibility":"list"},
    {"slug":"gpt-5.4-mini","display_name":"GPT-5.4 Mini","context_window":400000,"priority":2,"visibility":"list"}
  ]
}`
	_ = fsys.WriteFile(path, []byte(envelope), 0o600)

	entries, err := LoadCodexEntriesFromCache(fsys, path)
	if err != nil {
		t.Fatalf("LoadCodexEntriesFromCache: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].ID != "gpt-5.4" || entries[0].Provider != ProviderCodex || entries[0].Source != SourceNative {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if entries[0].ContextWindow != 1050000 {
		t.Errorf("entries[0].ContextWindow = %d, want 1050000", entries[0].ContextWindow)
	}
	if entries[0].DisplayName != "GPT-5.4" {
		t.Errorf("entries[0].DisplayName = %q", entries[0].DisplayName)
	}
}

func TestLoadCodexEntriesFromCache_MalformedSkips(t *testing.T) {
	fsys := fsutil.NewMemFS()
	_ = fsys.WriteFile("/cache.json", []byte("not json"), 0o600)
	_, err := LoadCodexEntriesFromCache(fsys, "/cache.json")
	if err == nil {
		t.Error("LoadCodexEntriesFromCache malformed = nil err, want error")
	}
}

func TestLoadClaudeEntriesFromCapabilities_MissingFile(t *testing.T) {
	entries, err := LoadClaudeEntriesFromCapabilities(fsutil.NewMemFS(), "/missing")
	if err != nil {
		t.Fatalf("LoadClaudeEntriesFromCapabilities: %v", err)
	}
	if entries != nil {
		t.Errorf("entries = %v, want nil", entries)
	}
}

func TestLoadClaudeEntriesFromCapabilities_IgnoresStringTimestamp(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude/cache/model-capabilities.json"
	cache := `{
  "timestamp": "2026-04-08T20:00:00Z",
  "models": [
    {"id":"claude-opus-4","max_input_tokens":200000,"max_tokens":32000}
  ]
}`
	_ = fsys.WriteFile(path, []byte(cache), 0o600)

	entries, err := LoadClaudeEntriesFromCapabilities(fsys, path)
	if err != nil {
		t.Fatalf("LoadClaudeEntriesFromCapabilities: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "claude-opus-4" {
		t.Fatalf("entries = %+v, want cached Claude entry", entries)
	}
}

func TestLoadClaudeEntriesFromCapabilities_ParsesCache(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.claude/cache/model-capabilities.json"
	cache := `{
  "timestamp": 1700000000,
  "models": [
    {"id":"claude-opus-4","max_input_tokens":200000,"max_tokens":32000},
    {"id":"claude-haiku-4-5","max_input_tokens":100000,"max_tokens":8192}
  ]
}`
	_ = fsys.WriteFile(path, []byte(cache), 0o600)

	entries, err := LoadClaudeEntriesFromCapabilities(fsys, path)
	if err != nil {
		t.Fatalf("LoadClaudeEntriesFromCapabilities: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Provider != ProviderAnthropic || entries[0].Source != SourceNative {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if entries[0].ContextWindow != 200000 || entries[0].MaxOutputTokens != 32000 {
		t.Errorf("entries[0] tokens = %d/%d", entries[0].ContextWindow, entries[0].MaxOutputTokens)
	}
}
