package modelregistry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// CodexRegistryCachePath is CQ-owned routing/listing metadata. It must never be
// used as the native client's account-scoped entitlement cache.
func CodexRegistryCachePath(roots userdirs.Roots) string {
	if roots.Cache == "" {
		return ""
	}
	return filepath.Join(roots.Cache, "model-registry.json")
}

// PublishCodexRegistryCache uses the existing atomic entry-file writer. Only
// native entries are cached; user overlays are read from configuration.
func PublishCodexRegistryCache(fsys fsutil.FileSystem, roots userdirs.Roots, snap Snapshot) error {
	path := CodexRegistryCachePath(roots)
	if path == "" {
		return nil
	}
	var entries []Entry
	for _, entry := range snap.Entries {
		if (entry.Provider == ProviderCodex || entry.Provider == ProviderZAI) && entry.Source == SourceNative {
			entries = append(entries, entry)
		}
	}
	return SaveOverlays(fsys, path, OverlayFile{Version: 1, Models: entries})
}

// LoadCodexRegistryCache reports whether a CQ-owned cache exists, including an
// intentionally empty one, so native cache rows cannot reintroduce old models.
func LoadCodexRegistryCache(fsys fsutil.FileSystem, roots userdirs.Roots) ([]Entry, bool, error) {
	path := CodexRegistryCachePath(roots)
	if path == "" {
		return nil, false, nil
	}
	if _, err := fsys.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read CQ model cache: %w", err)
	}
	cache, err := LoadOverlays(fsys, path)
	if err != nil {
		return nil, true, err
	}
	if cache.Version != 1 {
		return nil, true, fmt.Errorf("unsupported CQ model cache version")
	}
	for _, entry := range cache.Models {
		if entry.ID == "" || entry.Source != SourceNative || (entry.Provider != ProviderCodex && entry.Provider != ProviderZAI) {
			return nil, true, fmt.Errorf("invalid CQ model cache entry")
		}
	}
	return cache.Models, true, nil
}
