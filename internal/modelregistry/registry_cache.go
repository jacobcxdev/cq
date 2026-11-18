package modelregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type codexRegistryCache struct {
	Version            int                `json:"version"`
	Models             []Entry            `json:"models"`
	SharedBudgetModels map[string][]Entry `json:"shared_budget_models,omitempty"`
}

// CodexRegistryCachePath is CQ-owned routing/listing metadata. It must never be
// used as the native client's account-scoped entitlement cache.
func CodexRegistryCachePath(roots userdirs.Roots) string {
	if roots.Cache == "" {
		return ""
	}
	return filepath.Join(roots.Cache, "model-registry.json")
}

// PublishCodexRegistryCache atomically stores native entries and shared-budget
// routes, including routes hidden by a first-party entry in the picker.
// User overlays are read from configuration.
func PublishCodexRegistryCache(fsys fsutil.FileSystem, roots userdirs.Roots, snap Snapshot) error {
	path := CodexRegistryCachePath(roots)
	if path == "" {
		return nil
	}
	var entries []Entry
	for _, entry := range snap.Entries {
		if isCodexProjectionEntry(entry) && entry.Source == SourceNative {
			entries = append(entries, entry)
		}
	}
	return saveModelFile(fsys, path, codexRegistryCache{
		Version: 1, Models: entries, SharedBudgetModels: snap.SharedBudgetModels,
	})
}

// LoadCodexRegistryCache reports whether a CQ-owned cache exists, including an
// intentionally empty one, so native cache rows cannot reintroduce old models.
func LoadCodexRegistryCache(fsys fsutil.FileSystem, roots userdirs.Roots) ([]Entry, bool, error) {
	snap, present, err := LoadCodexRegistrySnapshot(fsys, roots)
	return snap.Entries, present, err
}

// LoadCodexRegistrySnapshot reads the public catalogue and its saved routes.
// Existing registry files may omit the route index.
func LoadCodexRegistrySnapshot(fsys fsutil.FileSystem, roots userdirs.Roots) (Snapshot, bool, error) {
	path := CodexRegistryCachePath(roots)
	if path == "" {
		return Snapshot{}, false, nil
	}
	if _, err := fsys.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, false, nil
		}
		return Snapshot{}, false, fmt.Errorf("read CQ model cache: %w", err)
	}
	data, err := fsys.ReadFile(path)
	if err != nil {
		return Snapshot{}, true, fmt.Errorf("read CQ model cache: %w", err)
	}
	var cache codexRegistryCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return Snapshot{}, true, fmt.Errorf("parse CQ model cache: %w", err)
	}
	if cache.Version != 1 {
		return Snapshot{}, true, fmt.Errorf("unsupported CQ model cache version")
	}
	for _, entry := range cache.Models {
		if entry.Validate() != nil || entry.Source != SourceNative || !isCodexProjectionEntry(entry) {
			return Snapshot{}, true, fmt.Errorf("invalid CQ model cache entry")
		}
	}
	for id, entries := range cache.SharedBudgetModels {
		if len(entries) == 0 {
			return Snapshot{}, true, fmt.Errorf("empty CQ shared-budget route list")
		}
		providers := make(map[Provider]bool, len(entries))
		for _, entry := range entries {
			if entry.SharedBudget == nil || entry.Validate() != nil || strings.ToLower(entry.ID) != id || providers[entry.Provider] {
				return Snapshot{}, true, fmt.Errorf("invalid CQ shared-budget route")
			}
			providers[entry.Provider] = true
		}
	}
	return Snapshot{Entries: cache.Models, SharedBudgetModels: cache.SharedBudgetModels}, true, nil
}
