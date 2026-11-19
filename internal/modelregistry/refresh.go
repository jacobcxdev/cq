package modelregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// RefreshDiagnostics summarises what happened during a Refresh call.
type RefreshDiagnostics struct {
	// Replaced reports whether refresh published a new in-memory snapshot.
	Replaced bool
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

// SharedBudgetSource registers a catalogue for a shared-budget subscription.
// The source's entries must carry SharedBudgetModel metadata and the same Provider.
type SharedBudgetSource struct {
	Provider Provider
	Source   NativeSource
	// RequireFresh suppresses stale routes when workspace permission cannot be verified.
	RequireFresh bool
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
	// ZAI fetches the Z.ai Responses model catalogue. May be nil to skip.
	ZAI NativeSource
	// Grok fetches account-scoped subscription model metadata.
	Grok NativeSource
	// SharedBudgets fetches third-party subscription catalogues in priority order.
	SharedBudgets []SharedBudgetSource
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
// Workspace catalogues marked RequireFresh drop cached routes on error.
// RefreshDiagnostics reports source errors.
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

	type registrySource struct {
		provider Provider
		source   NativeSource
	}
	sources := []registrySource{
		{ProviderAnthropic, r.Anthropic},
		{ProviderCodex, r.Codex},
		{ProviderZAI, r.ZAI},
		{ProviderGrok, r.Grok},
	}
	sharedProviders := make(map[Provider]bool, len(r.SharedBudgets))
	requireFresh := make(map[Provider]bool, len(r.SharedBudgets))
	for _, s := range r.SharedBudgets {
		if s.Source == nil {
			continue
		}
		if !s.Provider.isSharedBudgetID() || sharedProviders[s.Provider] {
			return diag, fmt.Errorf("invalid or duplicate shared-budget source provider %q", s.Provider)
		}
		sharedProviders[s.Provider] = true
		requireFresh[s.Provider] = s.RequireFresh
		sources = append(sources, registrySource{s.Provider, s.Source})
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
	prev := r.Catalog.Snapshot()
	codexClientVersion := prev.CodexClientVersion
	freshCodexRaw := make(map[string]json.RawMessage)
	freshAnthropicRaw := make(map[string]json.RawMessage)
	allFailed := true
	publishRemovals := false

	for _, fr := range results {
		if fr.err != nil {
			diag.SourceErrors[fr.provider] = fr.err
			if requireFresh[fr.provider] {
				freshByProvider[fr.provider] = nil
				diag.Counts[string(fr.provider)] = 0
				publishRemovals = true
			}
			continue
		}
		if fr.provider == ProviderCodex {
			codexClientVersion = fr.result.CodexClientVersion
		}
		if fr.result.NotConfigured {
			freshByProvider[fr.provider] = []Entry{}
			diag.Counts[string(fr.provider)] = 0
			publishRemovals = true
			continue
		}
		allFailed = false
		if sharedProviders[fr.provider] {
			entries := make([]Entry, 0, len(fr.result.Entries))
			for _, e := range fr.result.Entries {
				if e.Provider != fr.provider || e.SharedBudget == nil || e.Validate() != nil {
					fr.result.MalformedEntries++
					continue
				}
				entries = append(entries, e)
			}
			fr.result.Entries = entries
		}
		freshByProvider[fr.provider] = fr.result.Entries
		diag.Counts[string(fr.provider)] = len(fr.result.Entries)
		if fr.result.MalformedEntries > 0 {
			diag.MalformedCounts[fr.provider] = fr.result.MalformedEntries
		}
		if sharedProviders[fr.provider] {
			continue
		}
		for k, v := range fr.result.CodexRawByID {
			freshCodexRaw[k] = v
		}
		for k, v := range fr.result.AnthropicRawByID {
			freshAnthropicRaw[k] = v
		}
	}

	// If every configured source failed, preserve their previous entries.
	// Unconfigured sources and workspace catalogues with unverified permissions
	// still need obsolete entries removed.
	// Build the error list using the stable sources slice order so the message
	// is deterministic regardless of map iteration order.
	var allSourcesErr error
	if allFailed && len(diag.SourceErrors) > 0 {
		var errs []error
		for _, s := range sources {
			if e, ok := diag.SourceErrors[s.provider]; ok {
				errs = append(errs, fmt.Errorf("%s model source: %w", s.provider, e))
			}
		}
		allSourcesErr = fmt.Errorf("all model sources failed: %w", errors.Join(errs...))
		if !publishRemovals {
			return diag, allSourcesErr
		}
	}

	// For providers that failed, fall back to stale entries from the current snapshot.
	prevByProvider := make(map[Provider][]Entry)
	for _, e := range prev.Entries {
		if e.SharedBudget == nil {
			prevByProvider[e.Provider] = append(prevByProvider[e.Provider], e)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(prev.SharedBudgetModels)) {
		for _, e := range prev.SharedBudgetModels[id] {
			prevByProvider[e.Provider] = append(prevByProvider[e.Provider], e)
		}
	}

	// Build a case-insensitive set of IDs that are present in fresh data from
	// each provider. Used below to filter stale entries when a source fails.
	freshIDs := make(map[string]Provider) // lower-cased ID → provider that owns it freshly
	for prov, entries := range freshByProvider {
		if sharedProviders[prov] {
			continue
		}
		for _, e := range entries {
			freshIDs[strings.ToLower(e.ID)] = prov
		}
	}

	var natives []Entry
	var shared []Entry
	for _, s := range sources {
		if sharedProviders[s.provider] {
			entries, fresh := freshByProvider[s.provider]
			if !fresh {
				entries = prevByProvider[s.provider]
			}
			shared = append(shared, entries...)
			continue
		}
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

	active, sharedModels := mergeSharedBudgetModels(merged.Active, shared)
	snap := Snapshot{
		Entries:            active,
		SharedBudgetModels: sharedModels,
		CodexRawByID:       codexRaw,
		AnthropicRawByID:   anthropicRaw,
		FetchedAt:          now,
		CodexClientVersion: codexClientVersion,
	}

	if err := ValidateSnapshot(snap); err != nil {
		return diag, err
	}

	r.Catalog.Replace(snap)
	diag.Replaced = true

	return diag, allSourcesErr
}

// mergeSharedBudgetModels keeps one picker row per ID and indexes every route.
// Callers supply the first-party catalogue followed by shared models in priority order.
func mergeSharedBudgetModels(active, shared []Entry) ([]Entry, map[string][]Entry) {
	if len(shared) == 0 {
		return active, nil
	}
	claimed := make(map[string]bool, len(active))
	for _, e := range active {
		claimed[strings.ToLower(e.ID)] = true
	}
	routes := make(map[string][]Entry)
	for _, e := range shared {
		id := strings.ToLower(e.ID)
		if slices.ContainsFunc(routes[id], func(other Entry) bool {
			return other.Provider == e.Provider && other.SharedBudget.CredentialScope == e.SharedBudget.CredentialScope
		}) {
			continue
		}
		routes[id] = append(routes[id], e)
		if !claimed[id] {
			active = append(active, e)
			claimed[id] = true
		}
	}
	return active, routes
}
