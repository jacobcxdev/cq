package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	codexPrimerSharedCapabilityFamily = "shared-non-spark"
	codexPrimerSharedRuleRevision     = "shared-visible-non-spark-v1"
)

type CodexPrimerTarget struct {
	ResetAt            time.Time
	ScopeKind          codex.WindowScopeKind
	CapabilityFamily   string
	CompatibleModelIDs []string
	ModelID            string
	PolicyRevision     string
	CapabilityHash     string
	Windows            []codex.WindowDescriptor
}

type CodexPrimerUnresolved struct {
	RawLimitName string
	Code         string
}

type codexPrimerTargetGroup struct {
	resetAt     time.Time
	scopeKind   codex.WindowScopeKind
	family      string
	windows     []codex.WindowDescriptor
	compatible  map[string]modelregistry.Entry
	selected    []modelregistry.Entry
	forced      []modelregistry.Entry
	policyFacts map[string]bool
}

func PlanCodexPrimerTargets(descriptors []codex.WindowDescriptor, overrides map[string]string, entries []modelregistry.Entry) ([]CodexPrimerTarget, []CodexPrimerUnresolved) {
	return planCodexPrimerTargetsWithPolicy(descriptors, overrides, entries, defaultCodexPrimerProviderAdapter{}, codexPrimerSharedRuleRevision)
}

func planCodexPrimerTargetsWithPolicy(descriptors []codex.WindowDescriptor, overrides map[string]string, entries []modelregistry.Entry, adapter codexPrimerProviderAdapter, sharedRuleRevision string) ([]CodexPrimerTarget, []CodexPrimerUnresolved) {
	if adapter == nil {
		adapter = defaultCodexPrimerProviderAdapter{}
	}
	byEpoch := make(map[int64][]codex.WindowDescriptor)
	for _, descriptor := range descriptors {
		if descriptor.ResetAt.IsZero() {
			continue
		}
		byEpoch[descriptor.ResetAt.Unix()] = append(byEpoch[descriptor.ResetAt.Unix()], descriptor)
	}
	var targets []CodexPrimerTarget
	var unresolved []CodexPrimerUnresolved
	for epoch, windows := range byEpoch {
		groups := make(map[string]*codexPrimerTargetGroup)
		for _, window := range windows {
			var resolution codexPrimerModelResolution
			var err error
			if window.ScopeKind == codex.WindowScopeShared {
				resolution, err = resolveSharedCodexPrimerModel(window.RawLimitName, overrides, entries)
			} else {
				resolution, err = resolveCodexPrimerModelWithAdapter(window.Scope, overrides, entries, adapter)
			}
			if err != nil {
				unresolved = append(unresolved, CodexPrimerUnresolved{RawLimitName: window.RawLimitName, Code: codexPrimerUnresolvedCode(err)})
				continue
			}
			key := fmt.Sprintf("%d\x00%s", window.ScopeKind, resolution.Family)
			group := groups[key]
			if group == nil {
				group = &codexPrimerTargetGroup{
					resetAt: time.Unix(epoch, 0), scopeKind: window.ScopeKind, family: resolution.Family,
					compatible: make(map[string]modelregistry.Entry), policyFacts: make(map[string]bool),
				}
				groups[key] = group
			}
			group.windows = append(group.windows, window)
			group.selected = append(group.selected, resolution.Selected)
			if resolution.Stage == codexPrimerResolutionOverride {
				group.forced = append(group.forced, resolution.Selected)
				scope := window.Scope
				if window.ScopeKind == codex.WindowScopeShared {
					scope = window.RawLimitName
				}
				group.policyFacts["override:"+normalisePrimerPolicyText(scope)+"="+resolution.Override] = true
			}
			if resolution.Stage == codexPrimerResolutionProviderAdapter {
				group.policyFacts["adapter:"+resolution.AdapterRevision+":"+normalisePrimerPolicyText(window.Scope)+"="+strings.Join(resolution.AdapterFamilies, ",")] = true
			}
			if window.ScopeKind == codex.WindowScopeShared {
				group.policyFacts["shared-rule:"+sharedRuleRevision] = true
			}
			for _, entry := range resolution.Compatible {
				if current, ok := group.compatible[entry.ID]; !ok || primerModelPolicyFact(entry) < primerModelPolicyFact(current) {
					group.compatible[entry.ID] = entry
				}
			}
		}
		for _, group := range groups {
			targets = append(targets, group.target())
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if !targets[i].ResetAt.Equal(targets[j].ResetAt) {
			return targets[i].ResetAt.Before(targets[j].ResetAt)
		}
		if targets[i].ScopeKind != targets[j].ScopeKind {
			return targets[i].ScopeKind < targets[j].ScopeKind
		}
		if targets[i].CapabilityFamily != targets[j].CapabilityFamily {
			return targets[i].CapabilityFamily < targets[j].CapabilityFamily
		}
		return targets[i].ModelID < targets[j].ModelID
	})
	sort.Slice(unresolved, func(i, j int) bool {
		if unresolved[i].RawLimitName != unresolved[j].RawLimitName {
			return unresolved[i].RawLimitName < unresolved[j].RawLimitName
		}
		return unresolved[i].Code < unresolved[j].Code
	})
	return targets, unresolved
}

func (group *codexPrimerTargetGroup) target() CodexPrimerTarget {
	compatible := make([]modelregistry.Entry, 0, len(group.compatible))
	for _, entry := range group.compatible {
		compatible = append(compatible, entry)
	}
	sort.Slice(compatible, func(i, j int) bool { return compatible[i].ID < compatible[j].ID })
	compatibleIDs := make([]string, 0, len(compatible))
	policyFacts := make([]string, 0, len(compatible)+len(group.policyFacts))
	for _, entry := range compatible {
		compatibleIDs = append(compatibleIDs, entry.ID)
		policyFacts = append(policyFacts, "model:"+primerModelPolicyFact(entry))
	}
	for fact := range group.policyFacts {
		policyFacts = append(policyFacts, fact)
	}
	sort.Strings(policyFacts)
	selected := group.selected
	if len(group.forced) != 0 {
		selected = group.forced
	}
	selected = append([]modelregistry.Entry(nil), selected...)
	sortPrimerModels(selected)
	sort.Slice(group.windows, func(i, j int) bool {
		if group.windows[i].RawLimitName != group.windows[j].RawLimitName {
			return group.windows[i].RawLimitName < group.windows[j].RawLimitName
		}
		if group.windows[i].WindowName != group.windows[j].WindowName {
			return group.windows[i].WindowName < group.windows[j].WindowName
		}
		return group.windows[i].Scope < group.windows[j].Scope
	})
	return CodexPrimerTarget{
		ResetAt: group.resetAt, ScopeKind: group.scopeKind, CapabilityFamily: group.family,
		CompatibleModelIDs: compatibleIDs, ModelID: selected[0].ID,
		PolicyRevision: primerPolicyDigest(append([]string{
			"codex-primer-policy-v1", fmt.Sprintf("scope-kind:%d", group.scopeKind), "family:" + group.family,
		}, policyFacts...)...),
		CapabilityHash: primerPolicyDigest(append([]string{
			"codex-primer-capability-v1", fmt.Sprintf("scope-kind:%d", group.scopeKind), "family:" + group.family,
		}, compatibleIDs...)...),
		Windows: group.windows,
	}
}

func resolveSharedCodexPrimerModel(scope string, overrides map[string]string, entries []modelregistry.Entry) (codexPrimerModelResolution, error) {
	visible := visibleCodexModels(entries)
	if override, ok := overrides[scope]; ok {
		matches := exactPrimerModels(override, visible)
		if len(matches) != 1 || isSparkPrimerModel(matches[0]) {
			return codexPrimerModelResolution{}, newPrimerModelError("override_incompatible")
		}
		return codexPrimerModelResolution{
			Selected: matches[0], Compatible: []modelregistry.Entry{matches[0]},
			Family: codexPrimerSharedCapabilityFamily, Stage: codexPrimerResolutionOverride,
			Override: strings.ToLower(strings.TrimSpace(override)),
		}, nil
	}
	var compatible []modelregistry.Entry
	for _, entry := range visible {
		if !isSparkPrimerModel(entry) {
			compatible = append(compatible, entry)
		}
	}
	if len(compatible) == 0 {
		return codexPrimerModelResolution{}, newPrimerModelError("model_unresolved")
	}
	sortPrimerModels(compatible)
	return codexPrimerModelResolution{
		Selected: compatible[0], Compatible: compatible, Family: codexPrimerSharedCapabilityFamily,
	}, nil
}

func preferredSharedCodexPrimerModel(entries []modelregistry.Entry) (modelregistry.Entry, error) {
	resolution, err := resolveSharedCodexPrimerModel("", nil, entries)
	if err != nil {
		return modelregistry.Entry{}, err
	}
	return resolution.Selected, nil
}

func isSparkPrimerModel(entry modelregistry.Entry) bool {
	return containsPrimerTokens(primerTokens(entry.ID+" "+entry.DisplayName+" "+strings.Join(entry.Aliases, " ")), []string{"spark"})
}

func primerModelPolicyFact(entry modelregistry.Entry) string {
	aliasSet := make(map[string]bool)
	for _, alias := range entry.Aliases {
		alias = normalisePrimerPolicyText(alias)
		if alias != "" {
			aliasSet[alias] = true
		}
	}
	aliases := make([]string, 0, len(aliasSet))
	for alias := range aliasSet {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return strings.Join([]string{
		normalisePrimerPolicyText(entry.ID),
		normalisePrimerPolicyText(entry.DisplayName),
		strings.Join(aliases, ","),
	}, "|")
}

func normalisePrimerPolicyText(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func primerPolicyDigest(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(digest, "%d:%s\n", len(part), part)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func codexPrimerUnresolvedCode(err error) string {
	if modelErr, ok := err.(*primerModelError); ok && modelErr.code == "override_incompatible" {
		return modelErr.code
	}
	return "model_unresolved"
}
