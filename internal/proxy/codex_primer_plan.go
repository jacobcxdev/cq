package proxy

import (
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type CodexPrimerTarget struct {
	ResetAt time.Time
	ModelID string
	Windows []codex.WindowDescriptor
}

type CodexPrimerUnresolved struct {
	RawLimitName string
	Code         string
}

func PlanCodexPrimerTargets(descriptors []codex.WindowDescriptor, overrides map[string]string, entries []modelregistry.Entry) ([]CodexPrimerTarget, []CodexPrimerUnresolved) {
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
		byModel := make(map[string]*CodexPrimerTarget)
		var shared []codex.WindowDescriptor
		for _, window := range windows {
			if window.ScopeKind == codex.WindowScopeShared {
				shared = append(shared, window)
				continue
			}
			entry, err := ResolveCodexPrimerModel(window.Scope, overrides, entries)
			if err != nil {
				unresolved = append(unresolved, CodexPrimerUnresolved{RawLimitName: window.RawLimitName, Code: "model_unresolved"})
				continue
			}
			target := byModel[entry.ID]
			if target == nil {
				target = &CodexPrimerTarget{ResetAt: time.Unix(epoch, 0), ModelID: entry.ID}
				byModel[entry.ID] = target
			}
			target.Windows = append(target.Windows, window)
		}
		if len(shared) != 0 {
			sharedByModel := make(map[string]*CodexPrimerTarget)
			for _, window := range shared {
				entry, err := resolveSharedCodexPrimerModel(window.RawLimitName, overrides, entries)
				if err != nil {
					unresolved = append(unresolved, CodexPrimerUnresolved{RawLimitName: window.RawLimitName, Code: "model_unresolved"})
					continue
				}
				target := sharedByModel[entry.ID]
				if target == nil {
					target = &CodexPrimerTarget{ResetAt: time.Unix(epoch, 0), ModelID: entry.ID}
					sharedByModel[entry.ID] = target
				}
				target.Windows = append(target.Windows, window)
			}
			for _, target := range sharedByModel {
				targets = append(targets, *target)
			}
		}
		for _, target := range byModel {
			targets = append(targets, *target)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if !targets[i].ResetAt.Equal(targets[j].ResetAt) {
			return targets[i].ResetAt.Before(targets[j].ResetAt)
		}
		return targets[i].ModelID < targets[j].ModelID
	})
	sort.Slice(unresolved, func(i, j int) bool { return unresolved[i].RawLimitName < unresolved[j].RawLimitName })
	return targets, unresolved
}

func resolveSharedCodexPrimerModel(scope string, overrides map[string]string, entries []modelregistry.Entry) (modelregistry.Entry, error) {
	if override, ok := overrides[scope]; ok {
		entry, found := exactPrimerModel(override, visibleCodexModels(entries))
		if !found || isSparkPrimerModel(entry) {
			return modelregistry.Entry{}, &primerModelError{}
		}
		return entry, nil
	}
	return preferredSharedCodexPrimerModel(entries)
}

func preferredSharedCodexPrimerModel(entries []modelregistry.Entry) (modelregistry.Entry, error) {
	visible := visibleCodexModels(entries)
	var general []modelregistry.Entry
	for _, entry := range visible {
		if !isSparkPrimerModel(entry) {
			general = append(general, entry)
		}
	}
	if len(general) == 0 {
		return modelregistry.Entry{}, &primerModelError{}
	}
	sortPrimerModels(general)
	return general[0], nil
}

func isSparkPrimerModel(entry modelregistry.Entry) bool {
	return containsPrimerTokens(primerTokens(entry.ID+" "+entry.DisplayName+" "+strings.Join(entry.Aliases, " ")), []string{"spark"})
}

type primerModelError struct{}

func (*primerModelError) Error() string { return "Codex primer model unavailable" }
