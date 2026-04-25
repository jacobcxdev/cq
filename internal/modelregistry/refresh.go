package modelregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// RefreshDiagnostics summarises what happened during a Refresh call.
type RefreshDiagnostics struct {
	// Counts is the number of native entries fetched per provider.
	Counts map[string]int
	// Prunable contains overlay entries that conflicted with native entries.
	Prunable []Entry
	// SourceErrors records fetch errors for providers that failed. Only failing
	// providers are stored; successful providers are absent from the map (nil
	// errors are never inserted).
	SourceErrors map[Provider]error
	// MalformedCounts records the number of skipped malformed model entries per
	// provider. Only providers with at least one malformed entry appear here.
	MalformedCounts map[Provider]int
}

// OverlayStore is the read-only interface Refresher uses to load overlay entries.
// Implementations may load from disk or return a fixed slice for tests.
type OverlayStore interface {
	Load() ([]Entry, error)
}

// Refresher fetches fresh model data from all configured sources and updates
// the Catalog atomically. It is safe for concurrent use; concurrent Refresh
// calls are coalesced via an internal mutex so sources are not called in
// parallel by multiple callers.
type Refresher struct {
	// Catalog is the target store updated after each successful refresh.
	Catalog *Catalog
	// Overlays is an optional overlay loader. May be nil.
	Overlays OverlayStore
	// Anthropic fetches the Anthropic model catalogue. May be nil to skip.
	Anthropic NativeSource
	// Codex fetches the Codex model catalogue. May be nil to skip.
	Codex NativeSource
	// Now returns the current time. Defaults to time.Now when nil.
	Now func() time.Time

	// mu serialises concurrent Refresh calls (single-flight behaviour).
	mu sync.Mutex
}

// Refresh fetches fresh data from all sources, merges the results with any
// overlay entries, and atomically replaces the Catalog snapshot.
//
// Partial failures are tolerated: if a source fails, the previous snapshot's
// entries for that provider are preserved in the new snapshot.
// If all sources fail, the existing snapshot is left unchanged and the errors
// are returned in RefreshDiagnostics.
func (r *Refresher) Refresh(ctx context.Context) (RefreshDiagnostics, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}

	diag := RefreshDiagnostics{
		Counts:          make(map[string]int),
		SourceErrors:    make(map[Provider]error),
		MalformedCounts: make(map[Provider]int),
	}

	// Fetch from all sources concurrently.
	type fetchResult struct {
		provider Provider
		result   SourceResult
		err      error
	}

	sources := []struct {
		provider Provider
		source   NativeSource
	}{
		{ProviderAnthropic, r.Anthropic},
		{ProviderCodex, r.Codex},
	}

	configuredSources := 0
	for _, s := range sources {
		if s.source != nil {
			configuredSources++
		}
	}
	if configuredSources == 0 {
		return diag, nil
	}

	results := make([]fetchResult, 0, configuredSources)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, s := range sources {
		if s.source == nil {
			continue
		}
		wg.Add(1)
		go func(provider Provider, src NativeSource) {
			// wg.Done is deferred first so it always fires, even when the
			// recover defer below catches a panic and returns early.
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					mu.Lock()
					results = append(results, fetchResult{provider: provider, err: fmt.Errorf("source panic: %v", recovered)})
					mu.Unlock()
				}
			}()
			res, err := src.Fetch(ctx)
			mu.Lock()
			results = append(results, fetchResult{provider: provider, result: res, err: err})
			mu.Unlock()
		}(s.provider, s.source)
	}
	wg.Wait()

	// Categorise results.
	freshByProvider := make(map[Provider][]Entry)
	freshCodexRaw := make(map[string]json.RawMessage)
	freshAnthropicRaw := make(map[string]json.RawMessage)
	allFailed := true

	for _, fr := range results {
		if fr.err != nil {
			diag.SourceErrors[fr.provider] = fr.err
			continue
		}
		allFailed = false
		freshByProvider[fr.provider] = fr.result.Entries
		diag.Counts[string(fr.provider)] = len(fr.result.Entries)
		if fr.result.MalformedEntries > 0 {
			diag.MalformedCounts[fr.provider] = fr.result.MalformedEntries
		}
		for k, v := range fr.result.CodexRawByID {
			freshCodexRaw[k] = v
		}
		for k, v := range fr.result.AnthropicRawByID {
			freshAnthropicRaw[k] = v
		}
	}

	// If every configured source failed, leave the existing snapshot intact.
	// Build the error list using the stable sources slice order so the message
	// is deterministic regardless of map iteration order.
	if allFailed && len(results) > 0 {
		var errs []error
		for _, s := range sources {
			if e, ok := diag.SourceErrors[s.provider]; ok {
				errs = append(errs, e)
			}
		}
		return diag, fmt.Errorf("all model sources failed: %v", errs)
	}

	// For providers that failed, fall back to stale entries from the current snapshot.
	prev := r.Catalog.Snapshot()
	prevByProvider := make(map[Provider][]Entry)
	for _, e := range prev.Entries {
		prevByProvider[e.Provider] = append(prevByProvider[e.Provider], e)
	}

	// Build a case-insensitive set of IDs that are present in fresh data from
	// each provider. Used below to filter stale entries when a source fails.
	freshIDs := make(map[string]Provider) // lower-cased ID → provider that owns it freshly
	for prov, entries := range freshByProvider {
		for _, e := range entries {
			freshIDs[strings.ToLower(e.ID)] = prov
		}
	}

	var natives []Entry
	for _, s := range sources {
		if fresh, ok := freshByProvider[s.provider]; ok {
			natives = append(natives, fresh...)
		} else if stale, ok := prevByProvider[s.provider]; ok {
			// Source failed — preserve stale entries, but skip any whose ID
			// collides (case-insensitively) with a fresh entry from another
			// provider. Keeping such an entry would create cross-provider
			// ambiguity that ValidateSnapshot would reject anyway, but we want
			// to silently discard it here so a partial success can still publish.
			for _, e := range stale {
				key := strings.ToLower(e.ID)
				if freshOwner, conflict := freshIDs[key]; conflict && freshOwner != s.provider {
					continue // drop stale entry — fresh owner has this ID
				}
				natives = append(natives, e)
			}
		}
	}

	// Load and merge overlays.
	var overlays []Entry
	if r.Overlays != nil {
		loaded, err := r.Overlays.Load()
		if err != nil {
			return diag, fmt.Errorf("load model overlays: %w", err)
		}
		overlays = loaded
	}

	merged := Merge(natives, overlays)
	diag.Prunable = merged.Prunable

	// Merge raw maps: start from previous snapshot, overwrite with fresh data.
	codexRaw := make(map[string]json.RawMessage)
	for k, v := range prev.CodexRawByID {
		codexRaw[k] = v
	}
	for k, v := range freshCodexRaw {
		codexRaw[k] = v
	}

	anthropicRaw := make(map[string]json.RawMessage)
	for k, v := range prev.AnthropicRawByID {
		anthropicRaw[k] = v
	}
	for k, v := range freshAnthropicRaw {
		anthropicRaw[k] = v
	}

	snap := Snapshot{
		Entries:          merged.Active,
		CodexRawByID:     codexRaw,
		AnthropicRawByID: anthropicRaw,
		FetchedAt:        now,
	}

	if err := ValidateSnapshot(snap); err != nil {
		return diag, err
	}

	r.Catalog.Replace(snap)

	return diag, nil
}
