package proxy

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func ResolveCodexPrimerModel(scope string, overrides map[string]string, entries []modelregistry.Entry) (modelregistry.Entry, error) {
	visible := visibleCodexModels(entries)
	if override, ok := overrides[scope]; ok {
		if entry, ok := exactPrimerModel(override, visible); ok {
			return entry, nil
		}
		return modelregistry.Entry{}, fmt.Errorf("Codex primer model override is unavailable")
	}
	if entry, ok := exactPrimerModel(scope, visible); ok {
		return entry, nil
	}
	scopeTokens := primerTokens(scope)
	if len(scopeTokens) == 0 {
		return modelregistry.Entry{}, fmt.Errorf("Codex primer scope is unresolved")
	}
	var matches []modelregistry.Entry
	families := make(map[string]bool)
	for _, entry := range visible {
		if !containsPrimerTokens(primerTokens(entry.ID+" "+entry.DisplayName+" "+strings.Join(entry.Aliases, " ")), scopeTokens) {
			continue
		}
		matches = append(matches, entry)
		families[primerFamily(entry.ID)] = true
	}
	if len(matches) == 0 || len(families) != 1 {
		return modelregistry.Entry{}, fmt.Errorf("Codex primer scope is unresolved or ambiguous")
	}
	sortPrimerModels(matches)
	return matches[0], nil
}

func ValidateCodexPrimerOverrides(overrides map[string]string, entries []modelregistry.Entry) error {
	scopes := make([]string, 0, len(overrides))
	for scope := range overrides {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	for _, scope := range scopes {
		if _, err := ResolveCodexPrimerModel(scope, overrides, entries); err != nil {
			return fmt.Errorf("Codex primer override %q: %w", scope, err)
		}
	}
	return nil
}

func ValidateCodexPrimerRegistry(entries []modelregistry.Entry) error {
	_, err := preferredSharedCodexPrimerModel(entries)
	return err
}

func visibleCodexModels(entries []modelregistry.Entry) []modelregistry.Entry {
	models := make([]modelregistry.Entry, 0, len(entries))
	for _, entry := range entries {
		visibility := strings.ToLower(entry.Visibility)
		if entry.Provider != modelregistry.ProviderCodex || visibility == "hidden" || visibility == "hide" || entry.ID == "" {
			continue
		}
		models = append(models, entry)
	}
	return models
}

func exactPrimerModel(value string, entries []modelregistry.Entry) (modelregistry.Entry, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, entry := range entries {
		if strings.ToLower(entry.ID) == value || strings.ToLower(entry.DisplayName) == value {
			return entry, true
		}
		for _, alias := range entry.Aliases {
			if strings.ToLower(alias) == value {
				return entry, true
			}
		}
	}
	return modelregistry.Entry{}, false
}

func primerTokens(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func containsPrimerTokens(haystack, needles []string) bool {
	present := make(map[string]bool, len(haystack))
	for _, token := range haystack {
		present[token] = true
	}
	for _, token := range needles {
		if !present[token] {
			return false
		}
	}
	return true
}

func primerFamily(id string) string {
	var family []string
	for _, token := range primerTokens(id) {
		numeric := true
		for _, r := range token {
			if !unicode.IsDigit(r) {
				numeric = false
				break
			}
		}
		if !numeric {
			family = append(family, token)
		}
	}
	return strings.Join(family, "-")
}

func sortPrimerModels(entries []modelregistry.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		priority := func(value int) int {
			if value <= 0 {
				return int(^uint(0) >> 1)
			}
			return value
		}
		if priority(entries[i].Priority) != priority(entries[j].Priority) {
			return priority(entries[i].Priority) < priority(entries[j].Priority)
		}
		return entries[i].ID > entries[j].ID
	})
}
