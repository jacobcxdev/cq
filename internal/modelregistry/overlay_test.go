package modelregistry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// --- OverlayPath tests ---

func TestOverlayPath_XDGConfigHome(t *testing.T) {
	env := func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return "/custom/config"
		}
		return ""
	}
	got := OverlayPath(env, "/home/user")
	want := "/custom/config/cq/models.json"
	if got != want {
		t.Errorf("OverlayPath() = %q, want %q", got, want)
	}
}

func TestOverlayPath_FallbackHome(t *testing.T) {
	env := func(key string) string { return "" }
	got := OverlayPath(env, "/home/user")
	want := "/home/user/.config/cq/models.json"
	if got != want {
		t.Errorf("OverlayPath() = %q, want %q", got, want)
	}
}

// --- LoadOverlays tests ---

func TestLoadOverlays_MissingFile(t *testing.T) {
	fsys := fsutil.NewMemFS()
	got, err := LoadOverlays(fsys, "/no/such/file.json")
	if err != nil {
		t.Fatalf("LoadOverlays() unexpected error for missing file: %v", err)
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if len(got.Models) != 0 {
		t.Errorf("Models = %v, want empty", got.Models)
	}
}

func TestLoadOverlays_MalformedJSON(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.WriteFile("/config/cq/models.json", []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOverlays(fsys, "/config/cq/models.json")
	if err == nil {
		t.Fatal("LoadOverlays() expected error for malformed JSON, got nil")
	}
}

func TestLoadOverlays_UnknownTopLevelField(t *testing.T) {
	fsys := fsutil.NewMemFS()
	data := `{"version":1,"models":[],"future_field":"ignored"}`
	if err := fsys.WriteFile("/config/cq/models.json", []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOverlays(fsys, "/config/cq/models.json")
	if err != nil {
		t.Fatalf("LoadOverlays() unexpected error: %v", err)
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
}

func TestLoadOverlays_SourceOverlayApplied(t *testing.T) {
	fsys := fsutil.NewMemFS()
	// Entry with source omitted — should be set to SourceOverlay on load.
	data := `{"version":1,"models":[{"provider":"codex","id":"gpt-5.5","display_name":"GPT-5.5"}]}`
	if err := fsys.WriteFile("/config/cq/models.json", []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOverlays(fsys, "/config/cq/models.json")
	if err != nil {
		t.Fatalf("LoadOverlays() unexpected error: %v", err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("len(Models) = %d, want 1", len(got.Models))
	}
	if got.Models[0].Source != SourceOverlay {
		t.Errorf("Source = %q, want %q", got.Models[0].Source, SourceOverlay)
	}
}

func TestLoadOverlays_ExplicitSourcePreserved(t *testing.T) {
	fsys := fsutil.NewMemFS()
	// Entry with explicit source set — should not be overwritten.
	data := `{"version":1,"models":[{"provider":"codex","id":"gpt-5.5","source":"native"}]}`
	if err := fsys.WriteFile("/config/cq/models.json", []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOverlays(fsys, "/config/cq/models.json")
	if err != nil {
		t.Fatalf("LoadOverlays() unexpected error: %v", err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("len(Models) = %d, want 1", len(got.Models))
	}
	if got.Models[0].Source != SourceNative {
		t.Errorf("Source = %q, want %q (explicit wins)", got.Models[0].Source, SourceNative)
	}
}

// --- SaveOverlays tests ---

func TestSaveOverlays_AtomicWriteMemFS(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/config/cq/models.json"
	overlays := OverlayFile{
		Version: 1,
		Models: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.5", DisplayName: "GPT-5.5", Source: SourceOverlay},
		},
	}

	if err := SaveOverlays(fsys, path, overlays); err != nil {
		t.Fatalf("SaveOverlays() unexpected error: %v", err)
	}

	// The final file must exist.
	if _, err := fsys.Stat(path); err != nil {
		t.Errorf("target file missing after save: %v", err)
	}

	// The .tmp file must NOT exist after a successful save.
	tmp := path + ".tmp"
	if _, err := fsys.Stat(tmp); err == nil {
		t.Error(".tmp file still exists after successful save")
	}

	// The saved content must round-trip.
	loaded, err := LoadOverlays(fsys, path)
	if err != nil {
		t.Fatalf("LoadOverlays after save: %v", err)
	}
	if len(loaded.Models) != 1 || loaded.Models[0].ID != "gpt-5.5" {
		t.Errorf("round-trip mismatch: %+v", loaded.Models)
	}
}

func TestSaveOverlays_PermissionsRealFS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "models.json")
	fsys := fsutil.OSFileSystem{}

	overlays := OverlayFile{Version: 1, Models: []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceOverlay},
	}}

	if err := SaveOverlays(fsys, path, overlays); err != nil {
		t.Fatalf("SaveOverlays() unexpected error: %v", err)
	}

	// Verify directory was created with 0o700.
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %04o, want 0700", perm)
	}

	// Verify file has 0o600.
	finfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := finfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %04o, want 0600", perm)
	}

	// No .tmp file should remain.
	tmp := path + ".tmp"
	if _, err := os.Stat(tmp); err == nil {
		t.Error(".tmp file still exists after successful save")
	}
}

// --- PruneOverlays tests ---

func TestPruneOverlays_RemovesNativeConflicts(t *testing.T) {
	overlays := OverlayFile{
		Version: 1,
		Models: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceOverlay},
			{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceOverlay},
		},
	}
	natives := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
	}

	remaining, pruned := PruneOverlays(overlays, natives)

	if len(remaining.Models) != 1 {
		t.Fatalf("remaining = %d, want 1", len(remaining.Models))
	}
	if remaining.Models[0].ID != "gpt-5.5" {
		t.Errorf("remaining model = %q, want gpt-5.5", remaining.Models[0].ID)
	}
	if len(pruned) != 1 {
		t.Fatalf("pruned = %d, want 1", len(pruned))
	}
	if pruned[0].ID != "gpt-5.4" {
		t.Errorf("pruned model = %q, want gpt-5.4", pruned[0].ID)
	}
}

func TestPruneOverlays_DifferentProviderNotPruned(t *testing.T) {
	overlays := OverlayFile{
		Version: 1,
		Models: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceOverlay},
		},
	}
	// Same ID but different provider — must not prune.
	natives := []Entry{
		{Provider: ProviderAnthropic, ID: "gpt-5.5", Source: SourceNative},
	}

	remaining, pruned := PruneOverlays(overlays, natives)

	if len(remaining.Models) != 1 {
		t.Fatalf("remaining = %d, want 1 (different provider must not prune)", len(remaining.Models))
	}
	if len(pruned) != 0 {
		t.Errorf("pruned = %d, want 0", len(pruned))
	}
}

func TestPruneOverlays_EmptyOverlays(t *testing.T) {
	overlays := OverlayFile{Version: 1}
	natives := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
	}
	remaining, pruned := PruneOverlays(overlays, natives)
	if len(remaining.Models) != 0 {
		t.Errorf("remaining = %d, want 0", len(remaining.Models))
	}
	if len(pruned) != 0 {
		t.Errorf("pruned = %d, want 0", len(pruned))
	}
}

func TestPruneOverlays_EmptyNatives(t *testing.T) {
	overlays := OverlayFile{
		Version: 1,
		Models: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.5", Source: SourceOverlay},
		},
	}
	remaining, pruned := PruneOverlays(overlays, nil)
	if len(remaining.Models) != 1 {
		t.Errorf("remaining = %d, want 1", len(remaining.Models))
	}
	if len(pruned) != 0 {
		t.Errorf("pruned = %d, want 0", len(pruned))
	}
}

// --- error wrapping ---

func TestLoadOverlays_MalformedJSONWrapped(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.WriteFile("/models.json", []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOverlays(fsys, "/models.json")
	if err == nil {
		t.Fatal("expected error")
	}
	// Must reference the path in the error message (wrapped with context).
	if !strings.Contains(err.Error(), "models.json") {
		t.Errorf("error message %q does not reference the file path", err.Error())
	}
}
