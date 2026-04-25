package modelregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// OverlayFile is the on-disk representation of the user overlay file.
type OverlayFile struct {
	Version int     `json:"version"`
	Models  []Entry `json:"models"`
}

// OverlayPath returns the path to the user overlay file.
// env is used to look up environment variables; homeDir is the fallback home directory.
// Resolves to $XDG_CONFIG_HOME/cq/models.json, else $homeDir/.config/cq/models.json.
func OverlayPath(env func(string) string, homeDir string) string {
	if xdg := env("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "cq", "models.json")
	}
	return filepath.Join(homeDir, ".config", "cq", "models.json")
}

// LoadOverlays reads the overlay file at path.
// A missing file returns an empty OverlayFile (version 1) and nil error.
// A malformed JSON file returns a wrapped error.
// Entries whose Source is empty have SourceOverlay applied.
func LoadOverlays(fsys fsutil.FileSystem, path string) (OverlayFile, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OverlayFile{Version: 1}, nil
		}
		return OverlayFile{}, fmt.Errorf("load overlays %s: %w", path, err)
	}

	var f OverlayFile
	if err := json.Unmarshal(data, &f); err != nil {
		return OverlayFile{}, fmt.Errorf("load overlays %s: %w", path, err)
	}

	// Apply SourceOverlay to entries that omit source.
	for i := range f.Models {
		if f.Models[i].Source == "" {
			f.Models[i].Source = SourceOverlay
		}
	}

	return f, nil
}

// SaveOverlays writes overlays to path atomically using tmp+rename.
// Parent directories are created with 0o700; the file is written with 0o600.
// The .tmp file is removed on rename failure so no partial file is left behind.
func SaveOverlays(fsys fsutil.FileSystem, path string, overlays OverlayFile) error {
	overlays.Models = copyEntries(overlays.Models)
	data, err := json.MarshalIndent(overlays, "", "  ")
	if err != nil {
		return fmt.Errorf("save overlays: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save overlays: create dir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	if err := fsys.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save overlays: write tmp: %w", err)
	}

	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp)
		return fmt.Errorf("save overlays: rename: %w", err)
	}

	return nil
}

// PruneOverlays removes overlay entries whose (provider, id) pair exists in natives.
// It returns the pruned OverlayFile (same version, only non-conflicting entries) and
// a slice of the removed entries.
func PruneOverlays(overlays OverlayFile, natives []Entry) (OverlayFile, []Entry) {
	type key struct {
		provider Provider
		id       string
	}
	nativeSet := make(map[key]struct{}, len(natives))
	for _, n := range natives {
		nativeSet[key{n.Provider, n.ID}] = struct{}{}
	}

	var kept, pruned []Entry
	for _, m := range overlays.Models {
		if _, conflict := nativeSet[key{m.Provider, m.ID}]; conflict {
			pruned = append(pruned, m)
		} else {
			kept = append(kept, m)
		}
	}

	return OverlayFile{Version: overlays.Version, Models: kept}, pruned
}
