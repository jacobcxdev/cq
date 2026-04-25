package modelregistry

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateSnapshot returns an error if snap contains the same non-empty model
// ID under two or more different providers. Comparison is case-insensitive:
// "Foo" and "foo" are considered the same ID. Duplicate IDs within the same
// provider are allowed.
//
// The error message is deterministic: conflicts are sorted by the canonical
// (lower-cased) ID, and each conflict lists providers in alphabetical order.
func ValidateSnapshot(snap Snapshot) error {
	// Map each lower-cased ID to the set of providers that own it.
	type providerSet map[Provider]struct{}
	idProviders := make(map[string]providerSet, len(snap.Entries))

	for _, e := range snap.Entries {
		if e.ID == "" {
			continue
		}
		key := strings.ToLower(e.ID)
		if idProviders[key] == nil {
			idProviders[key] = make(providerSet)
		}
		idProviders[key][e.Provider] = struct{}{}
	}

	// Collect conflicts: lower-cased IDs that appear in 2+ distinct providers.
	type conflict struct {
		id        string // lower-cased canonical form
		providers []string
	}
	var conflicts []conflict
	for key, ps := range idProviders {
		if len(ps) < 2 {
			continue
		}
		providerNames := make([]string, 0, len(ps))
		for p := range ps {
			providerNames = append(providerNames, string(p))
		}
		sort.Strings(providerNames)
		conflicts = append(conflicts, conflict{id: key, providers: providerNames})
	}
	if len(conflicts) == 0 {
		return nil
	}

	// Sort conflicts by ID for a deterministic error message.
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].id < conflicts[j].id
	})

	parts := make([]string, len(conflicts))
	for i, c := range conflicts {
		parts[i] = fmt.Sprintf("%s (%s)", c.id, strings.Join(c.providers, ", "))
	}
	return fmt.Errorf("model registry contains duplicate model IDs across providers: %s", strings.Join(parts, "; "))
}
