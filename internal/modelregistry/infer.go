package modelregistry

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// InferClone determines the best native entry to use as a clone source for
// overlay.
//
// Rules (in priority order):
//  1. If overlay.CloneFrom is set, look for a same-provider native with that
//     ID. Return it if found; return (Entry{}, false) if not found.
//  2. Otherwise score every same-provider native by token similarity and return
//     the best match if it meets the confidence threshold.
//
// Tokenisation: the model ID is split on '-', '_', and '.'. Tokens that are
// entirely numeric or look like an 8-digit date (YYYYMMDD) are treated as
// version/date signals and weighted less than family tokens.
//
// The returned Entry has InferredFrom set to the source native's ID so callers
// can annotate the overlay without mutating the original.
func InferClone(overlay Entry, natives []Entry) (Entry, bool) {
	// --- Fast path: explicit clone_from ---
	if overlay.CloneFrom != "" {
		for _, n := range natives {
			if n.Provider == overlay.Provider && n.ID == overlay.CloneFrom {
				result := n
				result.InferredFrom = n.ID
				return result, true
			}
		}
		return Entry{}, false
	}

	// --- Heuristic path ---
	overlayToks := tokenise(overlay.ID)

	type scored struct {
		entry        Entry
		score        float64
		leadingScore int
		numTokens    []int
	}

	var candidates []scored
	for _, n := range natives {
		if n.Provider != overlay.Provider {
			continue
		}
		nativeToks := tokenise(n.ID)
		s, leading, latest := similarity(overlayToks, nativeToks)
		if s > 0 {
			candidates = append(candidates, scored{n, s, leading, latest})
		}
	}

	if len(candidates) == 0 {
		return Entry{}, false
	}

	// Sort by family overlap, then leading-token overlap, then latest numeric/date signal,
	// then ID for deterministic tie-breaking.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].leadingScore != candidates[j].leadingScore {
			return candidates[i].leadingScore > candidates[j].leadingScore
		}
		if cmp := compareNumericSignals(candidates[i].numTokens, candidates[j].numTokens); cmp != 0 {
			return cmp > 0
		}
		return candidates[i].entry.ID < candidates[j].entry.ID
	})

	best := candidates[0]

	// Confidence threshold: the best non-numeric token overlap must be at least
	// half of the overlay's non-numeric tokens. This prevents low-signal matches
	// like "o1" being chosen for "gpt-o3" when there is no substantive overlap.
	nonNumOverlay := nonNumericTokenCount(overlayToks)
	if nonNumOverlay > 0 {
		// best.score is the fraction of shared non-numeric tokens relative to
		// the union; require at least 0.5 overlap.
		if best.score < 0.5 {
			return Entry{}, false
		}
	}

	result := best.entry
	result.InferredFrom = best.entry.ID
	return result, true
}

// tokenise splits id into lowercase tokens on '-', '_', and '.'.
func tokenise(id string) []string {
	f := func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	}
	parts := strings.FieldsFunc(strings.ToLower(id), f)
	// Remove empty strings that can appear from consecutive separators.
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isNumericToken returns true when the token consists entirely of digits,
// including 8-digit date-like tokens such as YYYYMMDD.
func isNumericToken(tok string) bool {
	if len(tok) == 0 {
		return false
	}
	for _, r := range tok {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// similarity returns ranking signals in priority order:
//   - Jaccard-like overlap of non-numeric tokens.
//   - Leading non-numeric tokens matching in the same position.
//   - Numeric/date signals from the native candidate.
func similarity(a, b []string) (nonNumScore float64, leadingScore int, numTokens []int) {
	aNonNum := nonNumericTokens(a)
	bNonNum := nonNumericTokens(b)

	aSet := tokenSet(aNonNum)
	bSet := tokenSet(bNonNum)
	intersection := 0
	for tok := range aSet {
		if bSet[tok] {
			intersection++
		}
	}
	union := len(aSet) + len(bSet) - intersection
	if union > 0 {
		nonNumScore = float64(intersection) / float64(union)
	}

	minLen := len(aNonNum)
	if len(bNonNum) < minLen {
		minLen = len(bNonNum)
	}
	for i := 0; i < minLen; i++ {
		if aNonNum[i] != bNonNum[i] {
			break
		}
		leadingScore++
	}

	for _, tok := range numericTokens(b) {
		value, err := strconv.Atoi(tok)
		if err == nil {
			numTokens = append(numTokens, value)
		}
	}

	return nonNumScore, leadingScore, numTokens
}

func nonNumericTokens(toks []string) []string {
	var out []string
	for _, t := range toks {
		if !isNumericToken(t) {
			out = append(out, t)
		}
	}
	return out
}

func tokenSet(toks []string) map[string]bool {
	m := make(map[string]bool, len(toks))
	for _, t := range toks {
		m[t] = true
	}
	return m
}

func compareNumericSignals(a, b []int) int {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	for i := 0; i < minLen; i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return len(a) - len(b)
}

func numericTokens(toks []string) []string {
	var out []string
	for _, t := range toks {
		if isNumericToken(t) {
			out = append(out, t)
		}
	}
	return out
}

func nonNumericTokenCount(toks []string) int {
	n := 0
	for _, t := range toks {
		if !isNumericToken(t) {
			n++
		}
	}
	return n
}
