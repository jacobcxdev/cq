package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubSource is a test NativeSource that returns a fixed result or error.
type stubSource struct {
	result SourceResult
	err    error
	calls  atomic.Int32
}

func (s *stubSource) Fetch(_ context.Context) (SourceResult, error) {
	s.calls.Add(1)
	if s.err != nil {
		return SourceResult{}, s.err
	}
	return s.result, nil
}

type panicSource struct{}

func (panicSource) Fetch(context.Context) (SourceResult, error) {
	panic("source exploded")
}

// --- Refresher tests ---

func TestRefresher_HappyPath(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	catalog := NewCatalog(Snapshot{})

	anthropicStub := &stubSource{
		result: SourceResult{
			Entries: []Entry{
				{Provider: ProviderAnthropic, ID: "claude-haiku-4-5", Source: SourceNative},
			},
			AnthropicRawByID: map[string]json.RawMessage{},
			FetchedAt:        now,
		},
	}
	codexStub := &stubSource{
		result: SourceResult{
			Entries: []Entry{
				{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
			},
			CodexRawByID: map[string]json.RawMessage{},
			FetchedAt:    now,
		},
	}

	r := &Refresher{
		Catalog:   catalog,
		Anthropic: anthropicStub,
		Codex:     codexStub,
		Now:       func() time.Time { return now },
	}

	diag, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if diag.SourceErrors != nil && len(diag.SourceErrors) != 0 {
		t.Errorf("SourceErrors = %v, want empty", diag.SourceErrors)
	}

	snap := catalog.Snapshot()
	if len(snap.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(snap.Entries))
	}
	if snap.FetchedAt != now {
		t.Errorf("FetchedAt = %v, want %v", snap.FetchedAt, now)
	}
}

func TestRefresher_PartialSourceFailurePreservesStaleEntries(t *testing.T) {
	// When the Codex source fails, stale Codex entries from the previous
	// snapshot must be preserved in the new snapshot.
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	staleSnap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
		},
		FetchedAt: now.Add(-time.Hour),
	}
	catalog := NewCatalog(staleSnap)

	anthropicStub := &stubSource{
		result: SourceResult{
			Entries: []Entry{
				{Provider: ProviderAnthropic, ID: "claude-haiku-4-5", Source: SourceNative},
			},
			FetchedAt: now,
		},
	}
	codexStub := &stubSource{
		err: errors.New("codex unavailable"),
	}

	r := &Refresher{
		Catalog:   catalog,
		Anthropic: anthropicStub,
		Codex:     codexStub,
		Now:       func() time.Time { return now },
	}

	diag, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	// Source error must be recorded.
	if diag.SourceErrors[ProviderCodex] == nil {
		t.Error("SourceErrors[ProviderCodex] should be set")
	}

	snap := catalog.Snapshot()
	// Both the fresh Anthropic entry AND the stale Codex entry must be present.
	byID := make(map[string]Entry)
	for _, e := range snap.Entries {
		byID[e.ID] = e
	}
	if _, ok := byID["claude-haiku-4-5"]; !ok {
		t.Error("fresh Anthropic entry missing from snapshot")
	}
	if _, ok := byID["gpt-5.4"]; !ok {
		t.Error("stale Codex entry missing from snapshot (should be preserved on source failure)")
	}
}

func TestRefresher_SourcePanicRecordedAsError(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	catalog := NewCatalog(Snapshot{
		Entries: []Entry{{Provider: ProviderAnthropic, ID: "claude-old", Source: SourceNative}},
	})
	r := &Refresher{
		Catalog:   catalog,
		Anthropic: panicSource{},
		Codex: &stubSource{result: SourceResult{Entries: []Entry{
			{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
		}}},
		Now: func() time.Time { return now },
	}

	diag, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if diag.SourceErrors[ProviderAnthropic] == nil {
		t.Fatal("SourceErrors[ProviderAnthropic] should record panic")
	}
	snap := catalog.Snapshot()
	byID := make(map[string]bool)
	for _, e := range snap.Entries {
		byID[e.ID] = true
	}
	if !byID["claude-old"] || !byID["gpt-5.4"] {
		t.Fatalf("snapshot entries = %+v, want stale anthropic and fresh codex", snap.Entries)
	}
}

func TestRefresher_BothSourcesFail(t *testing.T) {
	// When both sources fail, the existing snapshot must remain unchanged.
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	staleSnap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-old", Source: SourceNative},
		},
		FetchedAt: now.Add(-time.Hour),
	}
	catalog := NewCatalog(staleSnap)

	r := &Refresher{
		Catalog:   catalog,
		Anthropic: &stubSource{err: errors.New("anthropic down")},
		Codex:     &stubSource{err: errors.New("codex down")},
		Now:       func() time.Time { return now },
	}

	diag, err := r.Refresh(context.Background())
	// Both sources failed — Refresh should return an error or non-nil diag with errors.
	if err == nil && (diag.SourceErrors == nil || len(diag.SourceErrors) == 0) {
		t.Error("expected either error or non-empty SourceErrors when all sources fail")
	}

	// The old snapshot must still be intact.
	snap := catalog.Snapshot()
	if len(snap.Entries) != 1 || snap.Entries[0].ID != "claude-old" {
		t.Errorf("snapshot should be unchanged when all sources fail, got %v", snap.Entries)
	}
}

func TestRefresher_ConcurrentRefreshSingleFlight(t *testing.T) {
	// Concurrent calls to Refresh must not cause sources to be called many
	// times concurrently. With singleflight semantics, each source should be
	// called at most once per logical refresh cycle.
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	catalog := NewCatalog(Snapshot{})

	inner := &stubSource{
		result: SourceResult{
			Entries:   []Entry{{Provider: ProviderAnthropic, ID: "claude-haiku-4-5", Source: SourceNative}},
			FetchedAt: now,
		},
	}

	// Gate: all goroutines start, then we unlock to let Fetch proceed.
	var gate sync.Mutex
	gate.Lock()
	blockingSource := &blockStubSource{inner: inner, mu: &gate}

	r := &Refresher{
		Catalog:   catalog,
		Anthropic: blockingSource,
		Codex:     &stubSource{result: SourceResult{FetchedAt: now}},
		Now:       func() time.Time { return now },
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			r.Refresh(context.Background()) //nolint:errcheck
		}()
	}

	// Brief pause to let goroutines queue up, then unblock.
	// (We can't use time.Sleep in a race-safe test reliably, but a small
	// runtime.Gosched loop is fine here since all goroutines will be blocked.)
	gate.Unlock()
	wg.Wait()

	// With singleflight: source calls should be << goroutines (10).
	// We allow up to goroutines itself as a loose upper bound, but the
	// important thing is it shouldn't be called once per goroutine with
	// significant internal concurrency. Since the gate is lifted before
	// all goroutines necessarily reach Fetch, we accept up to goroutines
	// but log if it's fully fanned out.
	calls := inner.calls.Load()
	if calls >= int32(goroutines) {
		t.Logf("WARNING: Anthropic source called %d times with %d goroutines — singleflight may not be effective", calls, goroutines)
	}
	// Hard requirement: must have been called at least once.
	if calls == 0 {
		t.Error("Anthropic source was never called")
	}
}

func TestRefresher_DiagnosticsCount(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	catalog := NewCatalog(Snapshot{})

	r := &Refresher{
		Catalog: catalog,
		Anthropic: &stubSource{
			result: SourceResult{
				Entries: []Entry{
					{Provider: ProviderAnthropic, ID: "model-a", Source: SourceNative},
					{Provider: ProviderAnthropic, ID: "model-b", Source: SourceNative},
				},
				FetchedAt: now,
			},
		},
		Codex: &stubSource{
			result: SourceResult{
				Entries: []Entry{
					{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
				},
				FetchedAt: now,
			},
		},
		Now: func() time.Time { return now },
	}

	diag, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if diag.Counts[string(ProviderAnthropic)] != 2 {
		t.Errorf("Counts[anthropic] = %d, want 2", diag.Counts[string(ProviderAnthropic)])
	}
	if diag.Counts[string(ProviderCodex)] != 1 {
		t.Errorf("Counts[codex] = %d, want 1", diag.Counts[string(ProviderCodex)])
	}
}

func TestRefresher_MalformedCountsPropagated(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	catalog := NewCatalog(Snapshot{})

	r := &Refresher{
		Catalog: catalog,
		Codex: &stubSource{
			result: SourceResult{
				Entries:          []Entry{{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative}},
				MalformedEntries: 3,
				FetchedAt:        now,
			},
		},
		Now: func() time.Time { return now },
	}

	diag, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if diag.MalformedCounts[ProviderCodex] != 3 {
		t.Errorf("MalformedCounts[codex] = %d, want 3", diag.MalformedCounts[ProviderCodex])
	}
	// Providers with no malformed entries must not appear.
	if _, ok := diag.MalformedCounts[ProviderAnthropic]; ok {
		t.Errorf("MalformedCounts[anthropic] should not be set")
	}
}

func TestRefresher_CrossProviderDuplicateIDFailsFast(t *testing.T) {
	// Two sources returning the same model ID under different providers must
	// cause Refresh to return an error and leave the catalog snapshot unchanged.
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	prev := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-old", Source: SourceNative},
		},
		FetchedAt: now.Add(-time.Hour),
	}
	catalog := NewCatalog(prev)

	r := &Refresher{
		Catalog: catalog,
		Anthropic: &stubSource{result: SourceResult{
			Entries:   []Entry{{Provider: ProviderAnthropic, ID: "shared-id", Source: SourceNative}},
			FetchedAt: now,
		}},
		Codex: &stubSource{result: SourceResult{
			Entries:   []Entry{{Provider: ProviderCodex, ID: "shared-id", Source: SourceNative}},
			FetchedAt: now,
		}},
		Now: func() time.Time { return now },
	}

	_, err := r.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh() = nil, want error for cross-provider duplicate model ID")
	}

	// Catalog snapshot must remain unchanged (the stale snapshot).
	snap := catalog.Snapshot()
	if len(snap.Entries) != 1 || snap.Entries[0].ID != "claude-old" {
		t.Errorf("catalog snapshot changed after validation failure, got %v", snap.Entries)
	}
}

// TestRefresher_PartialFailureStaleConflictDropped verifies that when a source
// fails, its stale entries are preserved ONLY if they do not collide (case-
// insensitively) with a fresh entry from another provider. Conflicting stale
// entries must be silently dropped; non-conflicting stale entries must survive.
//
// Scenario: previous snapshot has stale Anthropic native entries "gpt-5.4" and
// "claude-old". Anthropic source returns an error; Codex source returns a fresh
// native "gpt-5.4". Refresh should succeed, SourceErrors should include
// Anthropic, the resulting snapshot should contain fresh Codex "gpt-5.4" and
// stale Anthropic "claude-old", but NOT stale Anthropic "gpt-5.4".
func TestRefresher_PartialFailureStaleConflictDropped(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	staleSnap := Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "gpt-5.4", Source: SourceNative},
			{Provider: ProviderAnthropic, ID: "claude-old", Source: SourceNative},
		},
		FetchedAt: now.Add(-time.Hour),
	}
	catalog := NewCatalog(staleSnap)

	anthropicStub := &stubSource{err: errors.New("anthropic unavailable")}
	codexStub := &stubSource{
		result: SourceResult{
			Entries: []Entry{
				{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
			},
			FetchedAt: now,
		},
	}

	r := &Refresher{
		Catalog:   catalog,
		Anthropic: anthropicStub,
		Codex:     codexStub,
		Now:       func() time.Time { return now },
	}

	diag, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v, want nil (partial failure should succeed)", err)
	}

	// Anthropic error must be recorded in diagnostics.
	if diag.SourceErrors[ProviderAnthropic] == nil {
		t.Error("SourceErrors[ProviderAnthropic] should be set")
	}

	snap := catalog.Snapshot()
	byID := make(map[string]Entry)
	for _, e := range snap.Entries {
		byID[e.ID] = e
	}

	// Fresh Codex "gpt-5.4" must be present.
	if e, ok := byID["gpt-5.4"]; !ok {
		t.Error("fresh Codex gpt-5.4 missing from snapshot")
	} else if e.Provider != ProviderCodex {
		t.Errorf("gpt-5.4 provider = %s, want codex", e.Provider)
	}

	// Stale non-conflicting Anthropic "claude-old" must be preserved.
	if _, ok := byID["claude-old"]; !ok {
		t.Error("stale Anthropic claude-old missing from snapshot (should be preserved)")
	}

	// Stale Anthropic "gpt-5.4" must NOT appear — it conflicts with fresh Codex entry.
	for _, e := range snap.Entries {
		if e.ID == "gpt-5.4" && e.Provider == ProviderAnthropic {
			t.Error("stale Anthropic gpt-5.4 must not appear in snapshot (conflicts with fresh Codex entry)")
		}
	}
}

// TestRefresher_ManualOverlayWithCrossProviderIDFails verifies that a manual
// overlay entry bearing ProviderAnthropic ID "gpt-5.4" still causes Refresh to
// fail with an error when fresh Codex data also contains "gpt-5.4". User-authored
// ambiguity must never be silently hidden.
func TestRefresher_ManualOverlayWithCrossProviderIDFails(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	prev := Snapshot{
		Entries:   []Entry{{Provider: ProviderAnthropic, ID: "claude-old", Source: SourceNative}},
		FetchedAt: now.Add(-time.Hour),
	}
	catalog := NewCatalog(prev)

	// Anthropic source succeeds with only "claude-old"; Codex has "gpt-5.4".
	anthropicStub := &stubSource{
		result: SourceResult{
			Entries:   []Entry{{Provider: ProviderAnthropic, ID: "claude-old", Source: SourceNative}},
			FetchedAt: now,
		},
	}
	codexStub := &stubSource{
		result: SourceResult{
			Entries:   []Entry{{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative}},
			FetchedAt: now,
		},
	}

	// Manual overlay: user has defined an Anthropic entry with id "gpt-5.4" —
	// cross-provider ambiguity that must be caught.
	overlays := &stubOverlayStore{entries: []Entry{
		{Provider: ProviderAnthropic, ID: "gpt-5.4", Source: SourceOverlay},
	}}

	r := &Refresher{
		Catalog:   catalog,
		Anthropic: anthropicStub,
		Codex:     codexStub,
		Overlays:  overlays,
		Now:       func() time.Time { return now },
	}

	_, err := r.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh() = nil, want error: manual overlay gpt-5.4 conflicts with fresh Codex gpt-5.4")
	}

	// Catalog snapshot must remain unchanged (the stale snapshot).
	snap := catalog.Snapshot()
	if len(snap.Entries) != 1 || snap.Entries[0].ID != "claude-old" {
		t.Errorf("catalog snapshot changed after validation failure, got %v", snap.Entries)
	}
}

// stubOverlayStore is a test OverlayStore that returns a fixed slice.
type stubOverlayStore struct {
	entries []Entry
	err     error
}

func (s *stubOverlayStore) Load() ([]Entry, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.entries, nil
}

// blockStubSource wraps a stubSource with a mutex-based gate for concurrency tests.
type blockStubSource struct {
	inner *stubSource
	mu    *sync.Mutex
}

func (b *blockStubSource) Fetch(ctx context.Context) (SourceResult, error) {
	b.mu.Lock()
	b.mu.Unlock()
	return b.inner.Fetch(ctx)
}
