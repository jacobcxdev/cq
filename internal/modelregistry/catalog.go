package modelregistry

import (
	"encoding/json"
	"sync/atomic"
	"time"
)

// Snapshot is an immutable point-in-time view of the registry.
type Snapshot struct {
	Entries          []Entry
	CodexRawByID     map[string]json.RawMessage
	AnthropicRawByID map[string]json.RawMessage
	FetchedAt        time.Time
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

// deepCopySnapshot returns a fresh Snapshot with independently allocated
// slices, maps, and json.RawMessage byte slices.
func deepCopySnapshot(s Snapshot) Snapshot {
	return Snapshot{
		Entries:          copyEntries(s.Entries),
		CodexRawByID:     copyRawMap(s.CodexRawByID),
		AnthropicRawByID: copyRawMap(s.AnthropicRawByID),
		FetchedAt:        s.FetchedAt,
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
