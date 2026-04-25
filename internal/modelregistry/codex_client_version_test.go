package modelregistry

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestDiscoverCodexClientVersion_ReadsFromEnvelope(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.codex/models_cache.json"
	envelope := `{"fetched_at":"2026-04-24T00:00:00Z","client_version":"0.123.4","models":[]}`
	if err := fsys.WriteFile(path, []byte(envelope), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := DiscoverCodexClientVersion(fsys, path)
	if got != "0.123.4" {
		t.Errorf("DiscoverCodexClientVersion = %q, want %q", got, "0.123.4")
	}
}

func TestDiscoverCodexClientVersion_MissingFile(t *testing.T) {
	fsys := fsutil.NewMemFS()
	got := DiscoverCodexClientVersion(fsys, "/nope/models_cache.json")
	if got != "" {
		t.Errorf("DiscoverCodexClientVersion(missing) = %q, want empty string", got)
	}
}

func TestDiscoverCodexClientVersion_MalformedJSON(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.codex/models_cache.json"
	if err := fsys.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := DiscoverCodexClientVersion(fsys, path)
	if got != "" {
		t.Errorf("DiscoverCodexClientVersion(malformed) = %q, want empty string", got)
	}
}

func TestDiscoverCodexClientVersion_EmptyClientVersion(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/home/test/.codex/models_cache.json"
	if err := fsys.WriteFile(path, []byte(`{"fetched_at":"x","models":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := DiscoverCodexClientVersion(fsys, path)
	if got != "" {
		t.Errorf("DiscoverCodexClientVersion(missing field) = %q, want empty string", got)
	}
}
