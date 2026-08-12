package proxy

import (
	"context"
	"sync"
)

type codexRuntimeDecision uint8

const (
	codexRuntimeDecisionNone codexRuntimeDecision = iota
	codexRuntimeDecisionAffinityReuse
	codexRuntimeDecisionFairnessSelect
	codexRuntimeDecisionTerminalDefault
)

// codexRuntimeObservability owns process-lifetime routing and replay aggregates.
// Its mutation surface remains private so request callers cannot mint telemetry.
type codexRuntimeObservability struct {
	mu sync.Mutex

	affinityReuse      uint64
	fairnessSelect     uint64
	terminalDefault    uint64
	currentReplayBytes uint64
	peakReplayBytes    uint64
}

type codexRuntimeObservabilitySnapshot struct {
	AffinityReuse      uint64 `json:"affinity_reuse"`
	FairnessSelect     uint64 `json:"fairness_select"`
	TerminalDefault    uint64 `json:"terminal_default"`
	CurrentReplayBytes uint64 `json:"current_replay_bytes"`
	PeakReplayBytes    uint64 `json:"peak_replay_bytes"`
}

var codexProcessRuntimeObservability codexRuntimeObservability

func (observability *codexRuntimeObservability) recordDecision(ctx context.Context, decision codexRuntimeDecision) {
	name := decision.name()
	if observability == nil || name == "" {
		return
	}
	observability.mu.Lock()
	switch decision {
	case codexRuntimeDecisionAffinityReuse:
		observability.affinityReuse++
	case codexRuntimeDecisionFairnessSelect:
		observability.fairnessSelect++
	case codexRuntimeDecisionTerminalDefault:
		observability.terminalDefault++
	}
	observability.mu.Unlock()
	noteCodexObservation(ctx, codexObservationFields{Decision: name})
}

func (decision codexRuntimeDecision) name() string {
	switch decision {
	case codexRuntimeDecisionAffinityReuse:
		return "affinity_reuse"
	case codexRuntimeDecisionFairnessSelect:
		return "fairness_select"
	case codexRuntimeDecisionTerminalDefault:
		return "terminal_default"
	default:
		return ""
	}
}

func (observability *codexRuntimeObservability) ownReplayBytes(encoded, decoded []byte) uint64 {
	owned := uint64(len(encoded) + len(decoded))
	if observability == nil || owned == 0 {
		return owned
	}
	observability.mu.Lock()
	defer observability.mu.Unlock()
	observability.currentReplayBytes += owned
	observability.peakReplayBytes = max(observability.peakReplayBytes, observability.currentReplayBytes)
	return owned
}

func (observability *codexRuntimeObservability) releaseReplayBytes(owned uint64) {
	if observability == nil || owned == 0 {
		return
	}
	observability.mu.Lock()
	defer observability.mu.Unlock()
	observability.currentReplayBytes -= owned
}

func (observability *codexRuntimeObservability) snapshot() codexRuntimeObservabilitySnapshot {
	if observability == nil {
		return codexRuntimeObservabilitySnapshot{}
	}
	observability.mu.Lock()
	defer observability.mu.Unlock()
	return codexRuntimeObservabilitySnapshot{
		AffinityReuse:      observability.affinityReuse,
		FairnessSelect:     observability.fairnessSelect,
		TerminalDefault:    observability.terminalDefault,
		CurrentReplayBytes: observability.currentReplayBytes,
		PeakReplayBytes:    observability.peakReplayBytes,
	}
}
