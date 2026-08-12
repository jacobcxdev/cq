package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
)

// codexInstalledHTTPProbeHealth is cumulative, process-owned health. Fields
// are incremented only by typed hooks in the production native HTTP path.
// It contains no request, header, identity, credential, path, or body data.
type codexInstalledHTTPProbeHealth struct {
	ProductionHandlerRequests uint64
	NativeResponsesRequests   uint64
	NativeCompactRequests     uint64
	StrongTurns               uint64
	Gates                     CodexHTTPReadinessGateEvidence
	Acceptance                CodexHTTPAcceptanceResult
	Diagnostics               codexInstalledHTTPAggregateDiagnostics
}

// codexInstalledHTTPAggregateDiagnostics contains counters and gauges only.
// It deliberately cannot carry route, account, request, header, or body data.
type codexInstalledHTTPAggregateDiagnostics struct {
	AffinityReuseSelections    uint64
	FairnessSelections         uint64
	TerminalDefaultAttempts    uint64
	ReplayEnvelopeCurrentBytes uint64
	ReplayEnvelopePeakBytes    uint64
	ReplayEnvelopeErrors       uint64
}

// codexInstalledHTTPGateProbe belongs to one installed listener generation.
// Snapshots from different probe owners can never be combined.
type codexInstalledHTTPGateProbe struct {
	mu              sync.Mutex
	listenerBinding [sha256.Size]byte
	owner           [sha256.Size]byte
	generation      uint64
	health          codexInstalledHTTPProbeHealth
	activeTraces    uint64
	idle            chan struct{}
}

func newCodexInstalledHTTPGateProbe(listenerBinding [sha256.Size]byte) (*codexInstalledHTTPGateProbe, error) {
	if listenerBinding == ([sha256.Size]byte{}) {
		return nil, errCodexInstalledListenerAcceptance
	}
	probe := &codexInstalledHTTPGateProbe{listenerBinding: listenerBinding}
	if _, err := rand.Read(probe.owner[:]); err != nil || probe.owner == ([sha256.Size]byte{}) {
		return nil, errCodexInstalledListenerAcceptance
	}
	return probe, nil
}

// codexInstalledHTTPProbeSnapshot is opaque outside the probe owner. The
// pointer and random owner token prevent an exercise from fabricating health.
type codexInstalledHTTPProbeSnapshot struct {
	probe           *codexInstalledHTTPGateProbe
	owner           [sha256.Size]byte
	listenerBinding [sha256.Size]byte
	generation      uint64
	health          codexInstalledHTTPProbeHealth
}

func (probe *codexInstalledHTTPGateProbe) snapshot(ctx context.Context, listenerBinding [sha256.Size]byte) (codexInstalledHTTPProbeSnapshot, error) {
	if ctx == nil || probe == nil || listenerBinding == ([sha256.Size]byte{}) {
		return codexInstalledHTTPProbeSnapshot{}, errCodexInstalledListenerAcceptance
	}
	for {
		if err := ctx.Err(); err != nil {
			return codexInstalledHTTPProbeSnapshot{}, err
		}
		probe.mu.Lock()
		if probe.listenerBinding != listenerBinding || probe.owner == ([sha256.Size]byte{}) {
			probe.mu.Unlock()
			return codexInstalledHTTPProbeSnapshot{}, errCodexInstalledListenerAcceptance
		}
		if probe.activeTraces != 0 {
			idle := probe.idle
			probe.mu.Unlock()
			select {
			case <-ctx.Done():
				return codexInstalledHTTPProbeSnapshot{}, ctx.Err()
			case <-idle:
			}
			continue
		}
		snapshot := codexInstalledHTTPProbeSnapshot{
			probe:           probe,
			owner:           probe.owner,
			listenerBinding: probe.listenerBinding,
			generation:      probe.generation,
			health:          probe.health,
		}
		probe.mu.Unlock()
		return snapshot, nil
	}
}

func (snapshot codexInstalledHTTPProbeSnapshot) validFor(probe *codexInstalledHTTPGateProbe, binding [sha256.Size]byte) bool {
	if probe == nil || snapshot.probe != probe || snapshot.listenerBinding != binding || binding == ([sha256.Size]byte{}) {
		return false
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return snapshot.owner != ([sha256.Size]byte{}) && snapshot.owner == probe.owner && probe.listenerBinding == binding
}

// codexInstalledHTTPProbeResult is derived only from two sealed snapshots.
// It never accepts exercise-supplied gate labels or counters.
type codexInstalledHTTPProbeResult struct {
	ListenerBinding           [sha256.Size]byte
	ProductionHandlerRequests uint64
	NativeResponsesRequests   uint64
	NativeCompactRequests     uint64
	StrongTurns               uint64
	Gates                     CodexHTTPReadinessGateEvidence
	Acceptance                CodexHTTPAcceptanceResult
	Diagnostics               codexInstalledHTTPAggregateDiagnostics
}

func deriveCodexInstalledHTTPProbeResult(
	before codexInstalledHTTPProbeSnapshot,
	after codexInstalledHTTPProbeSnapshot,
	binding codexInstalledListenerProcessBinding,
	clientBuild string,
) (codexInstalledHTTPProbeResult, error) {
	var result codexInstalledHTTPProbeResult
	if before.probe == nil || before.probe != after.probe ||
		!before.validFor(before.probe, binding.ListenerBinding) ||
		!after.validFor(before.probe, binding.ListenerBinding) ||
		before.generation != 0 || before.health != (codexInstalledHTTPProbeHealth{}) ||
		after.generation < before.generation {
		return result, errCodexInstalledListenerAcceptance
	}
	var err error
	result.ListenerBinding = binding.ListenerBinding
	result.ProductionHandlerRequests, err = deltaUint64(before.health.ProductionHandlerRequests, after.health.ProductionHandlerRequests)
	if err != nil {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	result.NativeResponsesRequests, err = deltaUint64(before.health.NativeResponsesRequests, after.health.NativeResponsesRequests)
	if err != nil {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	result.NativeCompactRequests, err = deltaUint64(before.health.NativeCompactRequests, after.health.NativeCompactRequests)
	if err != nil {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	result.StrongTurns, err = deltaUint64(before.health.StrongTurns, after.health.StrongTurns)
	if err != nil {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	result.Gates, err = deltaCodexHTTPReadinessGateEvidence(before.health.Gates, after.health.Gates)
	if err != nil {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	if result.Gates.Stage11CorpusTurns != 0 || result.Gates.RawIdentifierLeaks != 0 || result.Gates.AutomaticAuthWrites != 0 {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	result.Acceptance, err = deltaCodexHTTPAcceptanceResult(before.health.Acceptance, after.health.Acceptance)
	if err != nil {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	result.Acceptance.InstalledVersion = clientBuild
	result.Acceptance.InstalledModelRequests = 0
	result.Acceptance.UnexpectedRoutes = 0
	result.Acceptance.PongVerified = false
	result.Diagnostics, err = deltaCodexInstalledHTTPAggregateDiagnostics(before.health.Diagnostics, after.health.Diagnostics)
	if err != nil {
		return codexInstalledHTTPProbeResult{}, errCodexInstalledListenerAcceptance
	}
	return result, nil
}

func deltaCodexInstalledHTTPAggregateDiagnostics(before, after codexInstalledHTTPAggregateDiagnostics) (codexInstalledHTTPAggregateDiagnostics, error) {
	if before.ReplayEnvelopeCurrentBytes != 0 || after.ReplayEnvelopeCurrentBytes != 0 || before.ReplayEnvelopePeakBytes != 0 ||
		before.ReplayEnvelopeErrors != 0 || after.ReplayEnvelopeErrors != 0 {
		return codexInstalledHTTPAggregateDiagnostics{}, errCodexInstalledListenerAcceptance
	}
	affinity, err := deltaUint64(before.AffinityReuseSelections, after.AffinityReuseSelections)
	if err != nil {
		return codexInstalledHTTPAggregateDiagnostics{}, err
	}
	fairness, err := deltaUint64(before.FairnessSelections, after.FairnessSelections)
	if err != nil {
		return codexInstalledHTTPAggregateDiagnostics{}, err
	}
	terminalDefault, err := deltaUint64(before.TerminalDefaultAttempts, after.TerminalDefaultAttempts)
	if err != nil {
		return codexInstalledHTTPAggregateDiagnostics{}, err
	}
	if after.ReplayEnvelopePeakBytes == 0 {
		return codexInstalledHTTPAggregateDiagnostics{}, errCodexInstalledListenerAcceptance
	}
	return codexInstalledHTTPAggregateDiagnostics{
		AffinityReuseSelections: affinity,
		FairnessSelections:      fairness,
		TerminalDefaultAttempts: terminalDefault,
		ReplayEnvelopePeakBytes: after.ReplayEnvelopePeakBytes,
	}, nil
}

func deltaCodexHTTPReadinessGateEvidence(before, after CodexHTTPReadinessGateEvidence) (CodexHTTPReadinessGateEvidence, error) {
	values := [13][2]uint64{
		{before.Stage11CorpusTurns, after.Stage11CorpusTurns},
		{before.InstalledTurns, after.InstalledTurns},
		{before.FrozenSingleTransformEnvelopeCases, after.FrozenSingleTransformEnvelopeCases},
		{before.WarmAffinityCases, after.WarmAffinityCases},
		{before.DeterministicFallbackCases, after.DeterministicFallbackCases},
		{before.TerminalDefaultOnceCases, after.TerminalDefaultOnceCases},
		{before.ExactPreAdmissionHard429ReplayCases, after.ExactPreAdmissionHard429ReplayCases},
		{before.AdmittedNoMigrationCases, after.AdmittedNoMigrationCases},
		{before.V2JournalRuntimeCases, after.V2JournalRuntimeCases},
		{before.RoutingMismatches, after.RoutingMismatches},
		{before.UnknownLifecycleEvents, after.UnknownLifecycleEvents},
		{before.RawIdentifierLeaks, after.RawIdentifierLeaks},
		{before.AutomaticAuthWrites, after.AutomaticAuthWrites},
	}
	var delta [13]uint64
	for index, value := range values {
		var err error
		delta[index], err = deltaUint64(value[0], value[1])
		if err != nil {
			return CodexHTTPReadinessGateEvidence{}, err
		}
	}
	return CodexHTTPReadinessGateEvidence{
		Stage11CorpusTurns:                  delta[0],
		InstalledTurns:                      delta[1],
		FrozenSingleTransformEnvelopeCases:  delta[2],
		WarmAffinityCases:                   delta[3],
		DeterministicFallbackCases:          delta[4],
		TerminalDefaultOnceCases:            delta[5],
		ExactPreAdmissionHard429ReplayCases: delta[6],
		AdmittedNoMigrationCases:            delta[7],
		V2JournalRuntimeCases:               delta[8],
		RoutingMismatches:                   delta[9],
		UnknownLifecycleEvents:              delta[10],
		RawIdentifierLeaks:                  delta[11],
		AutomaticAuthWrites:                 delta[12],
	}, nil
}

func deltaCodexHTTPAcceptanceResult(before, after CodexHTTPAcceptanceResult) (CodexHTTPAcceptanceResult, error) {
	ints := [5][2]int{
		{before.Turns, after.Turns},
		{before.Requests, after.Requests},
		{before.SelectorCalls, after.SelectorCalls},
		{before.InstalledSelectorCalls, after.InstalledSelectorCalls},
		{before.InstalledQuiescentLeases, after.InstalledQuiescentLeases},
	}
	var integerDelta [5]int
	for index, value := range ints {
		if value[1] < value[0] {
			return CodexHTTPAcceptanceResult{}, errors.New("Codex installed acceptance counter regressed")
		}
		integerDelta[index] = value[1] - value[0]
	}
	uints := [14][2]uint64{
		{before.ContinuityErrors, after.ContinuityErrors},
		{before.UnknownEvents, after.UnknownEvents},
		{before.InstalledRequests, after.InstalledRequests},
		{before.InstalledModelRequests, after.InstalledModelRequests},
		{before.InstalledAttempts, after.InstalledAttempts},
		{before.InstalledStrongKeys, after.InstalledStrongKeys},
		{before.InstalledZstdRequests, after.InstalledZstdRequests},
		{before.InstalledUnknownEvents, after.InstalledUnknownEvents},
		{before.InstalledContinuityErrors, after.InstalledContinuityErrors},
		{before.HeadroomRequests, after.HeadroomRequests},
		{before.HeadroomParseErrors, after.HeadroomParseErrors},
		{before.UnexpectedRoutes, after.UnexpectedRoutes},
		{before.EgressAttempts, after.EgressAttempts},
		{before.InstalledResolutions, after.InstalledResolutions},
	}
	var unsignedDelta [14]uint64
	for index, value := range uints {
		var err error
		unsignedDelta[index], err = deltaUint64(value[0], value[1])
		if err != nil {
			return CodexHTTPAcceptanceResult{}, err
		}
	}
	return CodexHTTPAcceptanceResult{
		Turns:                     integerDelta[0],
		Requests:                  integerDelta[1],
		SelectorCalls:             integerDelta[2],
		ContinuityErrors:          unsignedDelta[0],
		UnknownEvents:             unsignedDelta[1],
		InstalledRequests:         unsignedDelta[2],
		InstalledModelRequests:    unsignedDelta[3],
		InstalledAttempts:         unsignedDelta[4],
		InstalledSelectorCalls:    integerDelta[3],
		InstalledStrongKeys:       unsignedDelta[5],
		InstalledZstdRequests:     unsignedDelta[6],
		InstalledUnknownEvents:    unsignedDelta[7],
		InstalledContinuityErrors: unsignedDelta[8],
		InstalledQuiescentLeases:  integerDelta[4],
		HeadroomRequests:          unsignedDelta[9],
		HeadroomParseErrors:       unsignedDelta[10],
		UnexpectedRoutes:          unsignedDelta[11],
		EgressAttempts:            unsignedDelta[12],
		InstalledResolutions:      unsignedDelta[13],
	}, nil
}

func deltaUint64(before, after uint64) (uint64, error) {
	if after < before {
		return 0, errors.New("Codex installed acceptance counter regressed")
	}
	return after - before, nil
}
