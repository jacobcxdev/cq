package proxy

import (
	"context"
	"sort"
	"strings"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexRoutePolicyCandidate is a secret-free account/model route considered by policy.
type CodexRoutePolicyCandidate struct {
	Choice RouteChoice
	// RequiredCapacity aligns one-to-one with Choice.RequiredBuckets.
	RequiredCapacity  []CapacityView
	Compatible        bool
	Routable          bool
	ProvisionalLeases int
}

// CodexRoutePolicyHints contains only opaque routing identities.
type CodexRoutePolicyHints struct {
	AffinityAccountKey codex.AccountKey
	DefaultAccountKey  codex.AccountKey
}

// CodexRoutePlanStatus describes the frozen plan's terminal disposition.
type CodexRoutePlanStatus string

const (
	CodexRoutePlanReady               CodexRoutePlanStatus = "ready"
	CodexRoutePlanDefaultMissing      CodexRoutePlanStatus = "default_missing"
	CodexRoutePlanDefaultUnresolved   CodexRoutePlanStatus = "default_unresolved"
	CodexRoutePlanDefaultIncompatible CodexRoutePlanStatus = "default_incompatible"
	CodexRoutePlanDefaultUnroutable   CodexRoutePlanStatus = "default_unroutable"
	CodexRoutePlanCanceled            CodexRoutePlanStatus = "canceled"
	CodexRoutePlanInvalidCandidate    CodexRoutePlanStatus = "invalid_candidate"
)

// CodexRoutePolicyError is a credential-free terminal policy failure.
type CodexRoutePolicyError struct {
	Status CodexRoutePlanStatus
	cause  error
}

func (e *CodexRoutePolicyError) Error() string {
	return "Codex route policy terminal failure: " + string(e.Status)
}

func (e *CodexRoutePolicyError) Unwrap() error {
	return e.cause
}

// CodexRoutePlan is an immutable, eagerly materialised sequence of route choices.
type CodexRoutePlan struct {
	choices           []RouteChoice
	defaultAccountKey codex.AccountKey
	status            CodexRoutePlanStatus
}

// BuildCodexRoutePlan freezes an ordered route plan without inspecting credentials or system identity.
func BuildCodexRoutePlan(ctx context.Context, candidates []CodexRoutePolicyCandidate, hints CodexRoutePolicyHints) (CodexRoutePlan, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			plan := CodexRoutePlan{status: CodexRoutePlanCanceled}
			return plan, &CodexRoutePolicyError{Status: plan.status, cause: err}
		}
	}
	for _, candidate := range candidates {
		if !codexRoutePolicyCandidateValid(candidate) {
			plan := CodexRoutePlan{status: CodexRoutePlanInvalidCandidate}
			return plan, &CodexRoutePolicyError{Status: plan.status}
		}
	}
	eligible := make([]CodexRoutePolicyCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		capacity := codexRoutePolicyCapacity(candidate)
		if candidate.Choice.AccountKey == "" || !candidate.Compatible || !candidate.Routable || capacity.State == CapacityZero {
			continue
		}
		candidate = cloneCodexRoutePolicyCandidate(candidate)
		eligible = append(eligible, candidate)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return codexRoutePolicyCandidateLess(eligible[i], eligible[j], hints.AffinityAccountKey)
	})

	choices := make([]RouteChoice, 0, len(eligible))
	var frozenChoice RouteChoice
	haveFrozenChoice := false
	if len(eligible) > 0 {
		frozenChoice = cloneRoutePolicyChoice(eligible[0].Choice)
		haveFrozenChoice = true
	}
	plannedAccounts := make(map[codex.AccountKey]bool, len(eligible))
	for _, candidate := range eligible {
		if !codexRoutePolicyCompatibleWithFrozen(candidate.Choice, frozenChoice) || plannedAccounts[candidate.Choice.AccountKey] {
			continue
		}
		choices = append(choices, codexRoutePolicyChoiceForAccount(frozenChoice, candidate.Choice.AccountKey))
		plannedAccounts[candidate.Choice.AccountKey] = true
	}
	status := CodexRoutePlanReady
	if hints.DefaultAccountKey == "" {
		status = CodexRoutePlanDefaultMissing
	} else {
		defaultResolved := false
		defaultCompatible := false
		defaultRoutable := false
		defaultCandidates := make([]CodexRoutePolicyCandidate, 0, 1)
		for _, candidate := range candidates {
			if candidate.Choice.AccountKey != hints.DefaultAccountKey {
				continue
			}
			defaultResolved = true
			if !candidate.Compatible || (haveFrozenChoice && !codexRoutePolicyCompatibleWithFrozen(candidate.Choice, frozenChoice)) {
				continue
			}
			defaultCompatible = true
			if !candidate.Routable {
				continue
			}
			defaultRoutable = true
			candidate = cloneCodexRoutePolicyCandidate(candidate)
			defaultCandidates = append(defaultCandidates, candidate)
		}
		defaultPresent := false
		for _, choice := range choices {
			if choice.AccountKey == hints.DefaultAccountKey {
				defaultPresent = true
				break
			}
		}
		if !defaultPresent {
			defaultAppended := false
			if len(defaultCandidates) > 0 {
				sort.SliceStable(defaultCandidates, func(i, j int) bool {
					return codexRoutePolicyCandidateLess(defaultCandidates[i], defaultCandidates[j], "")
				})
				candidate := defaultCandidates[0]
				if haveFrozenChoice {
					choices = append(choices, codexRoutePolicyChoiceForAccount(frozenChoice, candidate.Choice.AccountKey))
				} else {
					choices = append(choices, cloneRoutePolicyChoice(candidate.Choice))
				}
				defaultAppended = true
			}
			if !defaultAppended {
				switch {
				case !defaultResolved:
					status = CodexRoutePlanDefaultUnresolved
				case !defaultCompatible:
					status = CodexRoutePlanDefaultIncompatible
				case !defaultRoutable:
					status = CodexRoutePlanDefaultUnroutable
				}
			}
		}
	}
	var defaultAccountKey codex.AccountKey
	for _, choice := range choices {
		if choice.AccountKey == hints.DefaultAccountKey {
			defaultAccountKey = codex.AccountKey(strings.Clone(string(hints.DefaultAccountKey)))
			break
		}
	}
	return CodexRoutePlan{choices: choices, defaultAccountKey: defaultAccountKey, status: status}, nil
}

func codexRoutePolicyCandidateValid(candidate CodexRoutePolicyCandidate) bool {
	if len(candidate.Choice.RequiredBuckets) == 0 || len(candidate.RequiredCapacity) != len(candidate.Choice.RequiredBuckets) {
		return false
	}
	for _, view := range candidate.RequiredCapacity {
		switch view.State {
		case CapacityUnknown, CapacityZero:
		case CapacityPositive:
			if view.RemainingPct < 1 || view.RemainingPct > 100 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Status returns the plan's typed terminal disposition.
func (p CodexRoutePlan) Status() CodexRoutePlanStatus {
	return p.status
}

// TerminalError returns the typed failure to surface after all frozen choices are exhausted.
func (p CodexRoutePlan) TerminalError() error {
	if p.status == CodexRoutePlanReady {
		return nil
	}
	return &CodexRoutePolicyError{Status: p.status}
}

// EffectiveModel returns the immutable model selected for every attempt.
func (p CodexRoutePlan) EffectiveModel() string {
	if len(p.choices) == 0 {
		return ""
	}
	return p.choices[0].EffectiveModel
}

// DefaultAccountKey returns the configured default's immutable role when it is
// present in the frozen choices.
func (p CodexRoutePlan) DefaultAccountKey() codex.AccountKey {
	return p.defaultAccountKey
}

// Choices returns a deep clone of the complete frozen attempt order.
func (p CodexRoutePlan) Choices() []RouteChoice {
	choices := make([]RouteChoice, len(p.choices))
	for i := range p.choices {
		choices[i] = cloneRoutePolicyChoice(p.choices[i])
	}
	return choices
}

func cloneRoutePolicyChoice(choice RouteChoice) RouteChoice {
	choice.RequiredBuckets = append([]CapacityBucket(nil), choice.RequiredBuckets...)
	return choice
}

func codexRoutePolicyNative(choice RouteChoice) bool {
	return codexRoutePolicySameModel(choice.RequestedModel, choice.EffectiveModel)
}

func codexRoutePolicyCompatibleWithFrozen(candidate, frozen RouteChoice) bool {
	if !codexRoutePolicySameModel(candidate.RequestedModel, frozen.RequestedModel) ||
		!codexRoutePolicySameModel(candidate.EffectiveModel, frozen.EffectiveModel) {
		return false
	}
	if len(candidate.RequiredBuckets) != len(frozen.RequiredBuckets) {
		return false
	}
	for i := range candidate.RequiredBuckets {
		if candidate.RequiredBuckets[i] != frozen.RequiredBuckets[i] {
			return false
		}
	}
	return true
}

func codexRoutePolicySameModel(left, right string) bool {
	return strings.EqualFold(ParseModel(left), ParseModel(right))
}

func codexRoutePolicyChoiceForAccount(frozen RouteChoice, accountKey codex.AccountKey) RouteChoice {
	choice := cloneRoutePolicyChoice(frozen)
	choice.AccountKey = accountKey
	return choice
}

func codexRoutePolicyCandidateLess(left, right CodexRoutePolicyCandidate, affinity codex.AccountKey) bool {
	leftAffinity := affinity != "" && left.Choice.AccountKey == affinity
	rightAffinity := affinity != "" && right.Choice.AccountKey == affinity
	if leftAffinity != rightAffinity {
		return leftAffinity
	}
	leftView := codexRoutePolicyCapacity(left)
	rightView := codexRoutePolicyCapacity(right)
	leftCapacity := codexRoutePolicyCapacityRank(leftView.State)
	rightCapacity := codexRoutePolicyCapacityRank(rightView.State)
	if leftCapacity != rightCapacity {
		return leftCapacity > rightCapacity
	}
	if leftView.State == CapacityPositive && leftView.RemainingPct != rightView.RemainingPct {
		return leftView.RemainingPct > rightView.RemainingPct
	}
	leftNative := codexRoutePolicyNative(left.Choice)
	rightNative := codexRoutePolicyNative(right.Choice)
	if leftNative != rightNative {
		return leftNative
	}
	if left.ProvisionalLeases != right.ProvisionalLeases {
		return left.ProvisionalLeases < right.ProvisionalLeases
	}
	if left.Choice.AccountKey != right.Choice.AccountKey {
		return left.Choice.AccountKey < right.Choice.AccountKey
	}
	return codexRoutePolicyChoiceLess(left.Choice, right.Choice)
}

func codexRoutePolicyCapacityRank(state CapacityState) int {
	switch state {
	case CapacityPositive:
		return 1
	case CapacityUnknown:
		return 0
	default:
		return -1
	}
}

func codexRoutePolicyCapacity(candidate CodexRoutePolicyCandidate) CapacityView {
	if len(candidate.RequiredCapacity) == 0 {
		return CapacityView{State: CapacityUnknown, RemainingPct: -1}
	}
	unknown := false
	remaining := 101
	for _, view := range candidate.RequiredCapacity {
		switch view.State {
		case CapacityZero:
			return CapacityView{State: CapacityZero, RemainingPct: 0}
		case CapacityUnknown:
			unknown = true
		case CapacityPositive:
			if view.RemainingPct < remaining {
				remaining = view.RemainingPct
			}
		default:
			unknown = true
		}
	}
	if unknown {
		return CapacityView{State: CapacityUnknown, RemainingPct: -1}
	}
	return CapacityView{State: CapacityPositive, RemainingPct: remaining}
}

func cloneCodexRoutePolicyCandidate(candidate CodexRoutePolicyCandidate) CodexRoutePolicyCandidate {
	candidate.Choice = cloneRoutePolicyChoice(candidate.Choice)
	candidate.RequiredCapacity = append([]CapacityView(nil), candidate.RequiredCapacity...)
	return candidate
}

func codexRoutePolicyChoiceLess(left, right RouteChoice) bool {
	if left.EffectiveModel != right.EffectiveModel {
		return left.EffectiveModel < right.EffectiveModel
	}
	if left.RequestedModel != right.RequestedModel {
		return left.RequestedModel < right.RequestedModel
	}
	limit := min(len(left.RequiredBuckets), len(right.RequiredBuckets))
	for i := 0; i < limit; i++ {
		if left.RequiredBuckets[i] != right.RequiredBuckets[i] {
			return left.RequiredBuckets[i] < right.RequiredBuckets[i]
		}
	}
	return len(left.RequiredBuckets) < len(right.RequiredBuckets)
}
