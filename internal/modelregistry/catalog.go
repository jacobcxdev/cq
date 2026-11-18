package modelregistry

import (
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Snapshot is an immutable point-in-time view of the registry.
type Snapshot struct {
	Entries []Entry
	// SharedBudgetModels holds every shared-budget route per lower-case model ID
	// in source registration order, including models selectable from Entries.
	SharedBudgetModels map[string][]Entry
	CodexRawByID       map[string]json.RawMessage
	AnthropicRawByID   map[string]json.RawMessage
	FetchedAt          time.Time
	CodexClientVersion string
}

// Catalog is a concurrency-safe store for the current registry Snapshot.
// Callers exchange the entire snapshot atomically; there is no partial update.
type Catalog struct {
	ptr atomic.Pointer[Snapshot]
}

// NewCatalog returns a new Catalog pre-loaded with initial.
// The initial snapshot is deep-copied on ingress.
func NewCatalog(initial Snapshot) *Catalog {
	c := &Catalog{}
	copied := deepCopySnapshot(initial)
	c.ptr.Store(&copied)
	return c
}

// Replace atomically swaps the stored snapshot with s.
// s is deep-copied on ingress so the caller may reuse or mutate it freely.
func (c *Catalog) Replace(s Snapshot) {
	copied := deepCopySnapshot(s)
	c.ptr.Store(&copied)
}

// Snapshot returns a deep copy of the current snapshot so the caller cannot
// inadvertently alias internal state.
func (c *Catalog) Snapshot() Snapshot {
	stored := c.ptr.Load()
	if stored == nil {
		return Snapshot{}
	}
	return deepCopySnapshot(*stored)
}

// SharedBudgetModels returns a copy of the registered routes for model.
func (c *Catalog) SharedBudgetModels(model string) []Entry {
	if stored := c.ptr.Load(); stored != nil {
		return copyEntries(stored.SharedBudgetModels[strings.ToLower(model)])
	}
	return nil
}

// HasSharedBudgetModels reports whether any shared-budget models are known.
func (c *Catalog) HasSharedBudgetModels() bool {
	stored := c.ptr.Load()
	return stored != nil && len(stored.SharedBudgetModels) > 0
}

// deepCopySnapshot returns a fresh Snapshot with independently allocated
// slices, maps, and json.RawMessage byte slices.
func deepCopySnapshot(s Snapshot) Snapshot {
	var sharedBudgetModels map[string][]Entry
	if s.SharedBudgetModels != nil {
		sharedBudgetModels = make(map[string][]Entry, len(s.SharedBudgetModels))
		for id, entries := range s.SharedBudgetModels {
			sharedBudgetModels[id] = copyEntries(entries)
		}
	} else {
		// The persisted public catalogue can seed selectable routes at startup.
		for _, e := range s.Entries {
			if e.SharedBudget == nil {
				continue
			}
			if sharedBudgetModels == nil {
				sharedBudgetModels = make(map[string][]Entry)
			}
			id := strings.ToLower(e.ID)
			sharedBudgetModels[id] = append(sharedBudgetModels[id], copyEntry(e))
		}
	}
	return Snapshot{
		SharedBudgetModels: sharedBudgetModels,
		Entries:            copyEntries(s.Entries),
		CodexRawByID:       copyRawMap(s.CodexRawByID),
		AnthropicRawByID:   copyRawMap(s.AnthropicRawByID),
		FetchedAt:          s.FetchedAt,
		CodexClientVersion: s.CodexClientVersion,
	}
}

func copyEntries(src []Entry) []Entry {
	if src == nil {
		return nil
	}
	dst := make([]Entry, len(src))
	for i, e := range src {
		dst[i] = copyEntry(e)
	}
	return dst
}

func copyEntry(e Entry) Entry {
	out := e
	if e.SharedBudget != nil {
		meta := *e.SharedBudget
		meta.ReasoningEfforts = slices.Clone(meta.ReasoningEfforts)
		out.SharedBudget = &meta
	}
	if e.Aliases != nil {
		out.Aliases = make([]string, len(e.Aliases))
		copy(out.Aliases, e.Aliases)
	}
	if e.Raw != nil {
		out.Raw = make(json.RawMessage, len(e.Raw))
		copy(out.Raw, e.Raw)
	}
	return out
}

func copyRawMap(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for k, v := range src {
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		dst[k] = cp
	}
	return dst
}
