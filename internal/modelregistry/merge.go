package modelregistry

// MergeResult is the output of Merge.
type MergeResult struct {
	// Active is the merged set of entries to be stored in the Catalog.
	// It contains all native entries plus overlay entries that have no native counterpart.
	Active []Entry
	// Prunable contains overlay entries that were shadowed by a native entry.
	// Callers may persist a pruned overlay file to disk to remove stale entries.
	Prunable []Entry
}

// Merge combines native entries with overlay entries into a single authoritative list.
//
// Rules:
//  1. Native entries always win over overlays with the same (provider, id) pair.
//     The overlay is added to MergeResult.Prunable.
//  2. Overlay entries with no native counterpart remain active.
//     If the overlay entry has missing metadata (DisplayName, Description,
//     ContextWindow, MaxOutputTokens, Visibility, Priority), InferClone is used
//     to find the best matching native and copy those fields.
//     The overlay ID/Provider/Source/CloneFrom are never changed.
//  3. Entries with the same ID but different providers never conflict.
func Merge(natives, overlays []Entry) MergeResult {
	// Build a lookup set of native (provider, id) pairs.
	type key struct {
		provider Provider
		id       string
	}
	nativeSet := make(map[key]struct{}, len(natives))
	for _, n := range natives {
		nativeSet[key{n.Provider, n.ID}] = struct{}{}
	}

	active := make([]Entry, 0, len(natives)+len(overlays))
	active = append(active, natives...)

	var prunable []Entry
	for _, ov := range overlays {
		k := key{ov.Provider, ov.ID}
		if _, conflict := nativeSet[k]; conflict {
			prunable = append(prunable, ov)
			continue
		}
		// Overlay has no native counterpart — try to infer missing metadata.
		ov = inferOverlayMetadata(ov, natives)
		active = append(active, ov)
	}

	return MergeResult{
		Active:   active,
		Prunable: prunable,
	}
}

// inferOverlayMetadata fills in missing metadata on an overlay entry using
// InferClone. Only fields that are zero-valued in the overlay are filled;
// the overlay's ID, Provider, Source, and CloneFrom are never changed.
func inferOverlayMetadata(ov Entry, natives []Entry) Entry {
	// Only attempt inference when at least one metadata field is missing.
	if ov.DisplayName != "" && ov.Description != "" &&
		ov.ContextWindow != 0 && ov.Visibility != "" {
		return ov
	}

	clone, ok := InferClone(ov, natives)
	if !ok {
		return ov
	}

	if ov.DisplayName == "" {
		ov.DisplayName = clone.DisplayName
	}
	if ov.Description == "" {
		ov.Description = clone.Description
	}
	if ov.ContextWindow == 0 {
		ov.ContextWindow = clone.ContextWindow
	}
	if ov.MaxContextWindow == 0 {
		ov.MaxContextWindow = clone.MaxContextWindow
	}
	if ov.MaxOutputTokens == 0 {
		ov.MaxOutputTokens = clone.MaxOutputTokens
	}
	if ov.Visibility == "" {
		ov.Visibility = clone.Visibility
	}
	if ov.Priority == 0 {
		ov.Priority = clone.Priority
	}
	// Record where the metadata was inferred from.
	ov.InferredFrom = clone.ID

	return ov
}
