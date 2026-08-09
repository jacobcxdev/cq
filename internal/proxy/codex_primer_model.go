package proxy

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

type codexPrimerProviderAdapter interface {
	Revision() string
	ResolveFamilies(scope string) []string
}

type defaultCodexPrimerProviderAdapter struct{}

func (defaultCodexPrimerProviderAdapter) Revision() string {
	return "codex-provider-v1:codex_spark=gpt-codex-spark"
}

func (defaultCodexPrimerProviderAdapter) ResolveFamilies(scope string) []string {
	if strings.EqualFold(strings.TrimSpace(scope), "codex_spark") {
		return []string{"gpt-codex-spark"}
	}
	return nil
}

type codexPrimerResolutionStage string

const (
	codexPrimerResolutionOverride        codexPrimerResolutionStage = "override"
	codexPrimerResolutionExact           codexPrimerResolutionStage = "exact"
	codexPrimerResolutionTokenFamily     codexPrimerResolutionStage = "token_family"
	codexPrimerResolutionProviderAdapter codexPrimerResolutionStage = "provider_adapter"
)

type codexPrimerModelResolution struct {
	Selected        modelregistry.Entry
	Compatible      []modelregistry.Entry
	Family          string
	Stage           codexPrimerResolutionStage
	Override        string
	AdapterRevision string
	AdapterFamilies []string
}

func ResolveCodexPrimerModel(scope string, overrides map[string]string, entries []modelregistry.Entry) (modelregistry.Entry, error) {
	resolution, err := resolveCodexPrimerModelWithAdapter(scope, overrides, entries, defaultCodexPrimerProviderAdapter{})
	if err != nil {
		return modelregistry.Entry{}, err
	}
	return resolution.Selected, nil
}

func resolveCodexPrimerModelWithAdapter(scope string, overrides map[string]string, entries []modelregistry.Entry, adapter codexPrimerProviderAdapter) (codexPrimerModelResolution, error) {
	visible := visibleCodexModels(entries)
	if override, ok := overrides[scope]; ok {
		matches := exactPrimerModels(override, visible)
		if len(matches) == 1 {
			return codexPrimerModelResolution{
				Selected: matches[0], Compatible: []modelregistry.Entry{matches[0]},
				Family: primerCapabilityFamily(matches[0].ID), Stage: codexPrimerResolutionOverride,
				Override: strings.ToLower(strings.TrimSpace(override)),
			}, nil
		}
		return codexPrimerModelResolution{}, newPrimerModelError("override_incompatible")
	}
	if matches := exactPrimerModels(scope, visible); len(matches) != 0 {
		if len(matches) != 1 {
			return codexPrimerModelResolution{}, newPrimerModelError("model_ambiguous")
		}
		return primerResolutionForFamily(matches[0], visible, codexPrimerResolutionExact), nil
	}
	scopeTokens := primerTokens(scope)
	if len(scopeTokens) != 0 {
		var matches []modelregistry.Entry
		families := make(map[string]bool)
		for _, entry := range visible {
			if !containsPrimerTokens(primerTokens(entry.ID+" "+entry.DisplayName+" "+strings.Join(entry.Aliases, " ")), scopeTokens) {
				continue
			}
			matches = append(matches, entry)
			families[primerCapabilityFamily(entry.ID)] = true
		}
		if len(matches) != 0 {
			if len(families) != 1 {
				return codexPrimerModelResolution{}, newPrimerModelError("model_ambiguous")
			}
			sortPrimerModels(matches)
			return primerResolutionForFamily(matches[0], visible, codexPrimerResolutionTokenFamily), nil
		}
	}
	if adapter == nil {
		adapter = defaultCodexPrimerProviderAdapter{}
	}
	families := normalisedPrimerFamilies(adapter.ResolveFamilies(scope))
	adapterRevision := strings.TrimSpace(adapter.Revision())
	if len(families) != 1 || adapterRevision == "" {
		return codexPrimerModelResolution{}, newPrimerModelError("model_unresolved")
	}
	compatible := primerModelsInFamily(visible, families[0])
	if len(compatible) == 0 {
		return codexPrimerModelResolution{}, newPrimerModelError("model_unresolved")
	}
	sortPrimerModels(compatible)
	return codexPrimerModelResolution{
		Selected: compatible[0], Compatible: compatible, Family: families[0],
		Stage: codexPrimerResolutionProviderAdapter, AdapterRevision: adapterRevision, AdapterFamilies: families,
	}, nil
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

func exactPrimerModels(value string, entries []modelregistry.Entry) []modelregistry.Entry {
	value = strings.ToLower(strings.TrimSpace(value))
	var matches []modelregistry.Entry
	for _, entry := range entries {
		matched := strings.ToLower(strings.TrimSpace(entry.ID)) == value || strings.ToLower(strings.TrimSpace(entry.DisplayName)) == value
		for _, alias := range entry.Aliases {
			matched = matched || strings.ToLower(strings.TrimSpace(alias)) == value
		}
		if matched {
			matches = append(matches, entry)
		}
	}
	return matches
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

func primerCapabilityFamily(id string) string {
	family := primerFamily(id)
	if family != "" {
		return family
	}
	return strings.ToLower(strings.TrimSpace(id))
}

func primerModelsInFamily(entries []modelregistry.Entry, family string) []modelregistry.Entry {
	var matches []modelregistry.Entry
	for _, entry := range entries {
		if primerCapabilityFamily(entry.ID) == family {
			matches = append(matches, entry)
		}
	}
	return matches
}

func primerResolutionForFamily(selected modelregistry.Entry, visible []modelregistry.Entry, stage codexPrimerResolutionStage) codexPrimerModelResolution {
	family := primerCapabilityFamily(selected.ID)
	return codexPrimerModelResolution{
		Selected: selected, Compatible: primerModelsInFamily(visible, family), Family: family, Stage: stage,
	}
}

func normalisedPrimerFamilies(families []string) []string {
	set := make(map[string]bool)
	for _, family := range families {
		family = strings.ToLower(strings.TrimSpace(family))
		if family != "" {
			set[family] = true
		}
	}
	result := make([]string, 0, len(set))
	for family := range set {
		result = append(result, family)
	}
	sort.Strings(result)
	return result
}

type primerModelError struct {
	code string
}

func newPrimerModelError(code string) *primerModelError {
	return &primerModelError{code: code}
}

func (e *primerModelError) Error() string {
	if e.code == "" {
		return "Codex primer model unavailable"
	}
	return fmt.Sprintf("Codex primer model unavailable: %s", e.code)
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
