package proxy

import (
	"context"
	"runtime"
	"sync"
	"time"
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
	persistenceWaiters uint64
	peakWaiters        uint64
	lastWaitMicros     uint64
	maxWaitMicros      uint64
	lastCommitMicros   uint64
	maxCommitMicros    uint64
	journalBytes       uint64
	journalLanes       uint64
	journalRecords     uint64
	stateFailures      uint64
	stateFailed        bool
}

type codexRuntimeObservabilitySnapshot struct {
	AffinityReuse      uint64 `json:"affinity_reuse"`
	FairnessSelect     uint64 `json:"fairness_select"`
	TerminalDefault    uint64 `json:"terminal_default"`
	CurrentReplayBytes uint64 `json:"current_replay_bytes"`
	PeakReplayBytes    uint64 `json:"peak_replay_bytes"`
	PersistenceWaiters uint64 `json:"persistence_waiters"`
	PeakWaiters        uint64 `json:"peak_persistence_waiters"`
	LastWaitMicros     uint64 `json:"last_persistence_wait_us"`
	MaxWaitMicros      uint64 `json:"max_persistence_wait_us"`
	LastCommitMicros   uint64 `json:"last_journal_commit_us"`
	MaxCommitMicros    uint64 `json:"max_journal_commit_us"`
	JournalBytes       uint64 `json:"journal_bytes"`
	JournalLanes       uint64 `json:"journal_lanes"`
	JournalRecords     uint64 `json:"journal_records"`
	StateFailures      uint64 `json:"state_failures"`
	StateFailed        bool   `json:"state_failed"`
	HeapAllocBytes     uint64 `json:"heap_alloc_bytes"`
	HeapSysBytes       uint64 `json:"heap_sys_bytes"`
	Goroutines         int    `json:"goroutines"`
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
	owned := uint64(len(encoded))
	if !sameByteView(encoded, decoded) {
		owned += uint64(len(decoded))
	}
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
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	observability.mu.Lock()
	snapshot := codexRuntimeObservabilitySnapshot{
		AffinityReuse:      observability.affinityReuse,
		FairnessSelect:     observability.fairnessSelect,
		TerminalDefault:    observability.terminalDefault,
		CurrentReplayBytes: observability.currentReplayBytes,
		PeakReplayBytes:    observability.peakReplayBytes,
		PersistenceWaiters: observability.persistenceWaiters,
		PeakWaiters:        observability.peakWaiters,
		LastWaitMicros:     observability.lastWaitMicros,
		MaxWaitMicros:      observability.maxWaitMicros,
		LastCommitMicros:   observability.lastCommitMicros,
		MaxCommitMicros:    observability.maxCommitMicros,
		JournalBytes:       observability.journalBytes,
		JournalLanes:       observability.journalLanes,
		JournalRecords:     observability.journalRecords,
		StateFailures:      observability.stateFailures,
		StateFailed:        observability.stateFailed,
		HeapAllocBytes:     memory.HeapAlloc,
		HeapSysBytes:       memory.HeapSys,
		Goroutines:         runtime.NumGoroutine(),
	}
	observability.mu.Unlock()
	return snapshot
}

func (observability *codexRuntimeObservability) beginPersistenceWait() func() {
	if observability == nil {
		return func() {}
	}
	started := time.Now()
	observability.mu.Lock()
	observability.persistenceWaiters++
	observability.peakWaiters = max(observability.peakWaiters, observability.persistenceWaiters)
	observability.mu.Unlock()
	return func() {
		elapsed := uint64(time.Since(started).Microseconds())
		observability.mu.Lock()
		if observability.persistenceWaiters > 0 {
			observability.persistenceWaiters--
		}
		observability.lastWaitMicros = elapsed
		observability.maxWaitMicros = max(observability.maxWaitMicros, elapsed)
		observability.mu.Unlock()
	}
}

func (observability *codexRuntimeObservability) recordJournalCommit(elapsed time.Duration, bytes, lanes, records int, failed bool) {
	if observability == nil {
		return
	}
	micros := uint64(elapsed.Microseconds())
	observability.mu.Lock()
	defer observability.mu.Unlock()
	observability.lastCommitMicros = micros
	observability.maxCommitMicros = max(observability.maxCommitMicros, micros)
	observability.recordJournalStateLocked(bytes, lanes, records, failed)
}

func (observability *codexRuntimeObservability) recordJournalState(bytes, lanes, records int, failed bool) {
	if observability == nil {
		return
	}
	observability.mu.Lock()
	defer observability.mu.Unlock()
	observability.recordJournalStateLocked(bytes, lanes, records, failed)
}

func (observability *codexRuntimeObservability) recordJournalStateLocked(bytes, lanes, records int, failed bool) {
	observability.journalBytes = uint64(max(bytes, 0))
	observability.journalLanes = uint64(max(lanes, 0))
	observability.journalRecords = uint64(max(records, 0))
	observability.stateFailed = failed
	if failed {
		observability.stateFailures++
	}
}
