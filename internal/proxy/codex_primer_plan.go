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
			target := preferredScopedTarget(byModel)
			if target == nil {
				entry, err := preferredSharedCodexPrimerModel(entries)
				if err != nil {
					for _, window := range shared {
						unresolved = append(unresolved, CodexPrimerUnresolved{RawLimitName: window.RawLimitName, Code: "model_unresolved"})
					}
				} else {
					target = &CodexPrimerTarget{ResetAt: time.Unix(epoch, 0), ModelID: entry.ID}
					byModel[entry.ID] = target
				}
			}
			if target != nil {
				target.Windows = append(target.Windows, shared...)
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

func preferredScopedTarget(targets map[string]*CodexPrimerTarget) *CodexPrimerTarget {
	var ordered []*CodexPrimerTarget
	for _, target := range targets {
		ordered = append(ordered, target)
	}
	sort.Slice(ordered, func(i, j int) bool {
		iSpark := containsPrimerTokens(primerTokens(ordered[i].ModelID), []string{"spark"})
		jSpark := containsPrimerTokens(primerTokens(ordered[j].ModelID), []string{"spark"})
		if iSpark != jSpark {
			return iSpark
		}
		return ordered[i].ModelID < ordered[j].ModelID
	})
	if len(ordered) == 0 {
		return nil
	}
	return ordered[0]
}

func preferredSharedCodexPrimerModel(entries []modelregistry.Entry) (modelregistry.Entry, error) {
	visible := visibleCodexModels(entries)
	var spark []modelregistry.Entry
	for _, entry := range visible {
		if containsPrimerTokens(primerTokens(entry.ID+" "+entry.DisplayName+" "+strings.Join(entry.Aliases, " ")), []string{"spark"}) {
			spark = append(spark, entry)
		}
	}
	if len(spark) != 0 {
		sortPrimerModels(spark)
		return spark[0], nil
	}
	if len(visible) == 0 {
		return modelregistry.Entry{}, &primerModelError{}
	}
	sortPrimerModels(visible)
	return visible[0], nil
}

type primerModelError struct{}

func (*primerModelError) Error() string { return "Codex primer model unavailable" }
