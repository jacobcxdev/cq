package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"sort"
	"sync"
)

type codexInstalledHTTPProbePath uint8

const (
	codexInstalledHTTPProbeResponses codexInstalledHTTPProbePath = iota + 1
	codexInstalledHTTPProbeCompact
)

type codexInstalledHTTPSelection uint8

const (
	codexInstalledHTTPSelectionOrdinary codexInstalledHTTPSelection = iota + 1
	codexInstalledHTTPSelectionBound
	codexInstalledHTTPSelectionWarmAffinity
	codexInstalledHTTPSelectionDeterministicFallback
)

type codexInstalledHTTPRejectKind uint8

const (
	codexInstalledHTTPRejectOther codexInstalledHTTPRejectKind = iota + 1
	codexInstalledHTTPRejectAuth
	codexInstalledHTTPRejectExactHard429
)

type codexInstalledHTTPTerminalKind uint8

const (
	codexInstalledHTTPTerminalCompleted codexInstalledHTTPTerminalKind = iota + 1
	codexInstalledHTTPTerminalProviderFailed
	codexInstalledHTTPTerminalRejected
	codexInstalledHTTPTerminalIndeterminate
)

type codexInstalledHTTPDispatchFacts struct {
	selection              codexInstalledHTTPSelection
	selectedValue          PoolValue
	affinityReuse          bool
	fairnessSelect         bool
	eligibleCompetitors    uint32
	naturalWinnerDisplaced bool
	affinityUnavailable    bool
	terminalDefaultOrdinal uint32
	routeCount             uint32
}

type codexInstalledHTTPPlanFacts struct {
	strongTurn         bool
	strongRequest      bool
	zstd               bool
	headroom           bool
	transformed        bool
	inspectCalls       uint8
	freezeCalls        uint8
	encodeCalls        uint8
	modelRewrites      uint8
	headroomTransforms uint8
	initialAdmitted    bool
	durableV2          bool
	requestGeneration  uint64
	attemptGeneration  uint64
	dispatch           codexInstalledHTTPDispatchFacts
}

type codexInstalledHTTPAttemptFacts struct {
	routeOrdinal    uint32
	terminalDefault bool
	digest          [sha256.Size]byte
	dispatched      bool
	reject          codexInstalledHTTPRejectKind
	admitted        bool
}

type codexInstalledHTTPLifecycleFacts struct {
	begin      bool
	dispatched bool
	terminal   codexInstalledHTTPTerminalKind
	drained    bool
	invalid    bool
	requestGen uint64
	attemptGen uint64
}

type codexInstalledHTTPProbeContextKey struct{}

type codexInstalledHTTPGateTrace struct {
	mu            sync.Mutex
	probe         *codexInstalledHTTPGateProbe
	path          codexInstalledHTTPProbePath
	planned       bool
	finished      bool
	invalid       bool
	plan          codexInstalledHTTPPlanFacts
	attempts      []codexInstalledHTTPAttemptFacts
	lifecycle     codexInstalledHTTPLifecycleFacts
	relayed       bool
	relayAccepted bool
	relayRejected bool
}

// codexInstalledHTTPGateTraceView is the immutable, mutex-free view evaluated
// after a trace is finished. Keeping it separate prevents accidental copying
// of the live trace lock.
type codexInstalledHTTPGateTraceView struct {
	path          codexInstalledHTTPProbePath
	planned       bool
	invalid       bool
	plan          codexInstalledHTTPPlanFacts
	attempts      []codexInstalledHTTPAttemptFacts
	lifecycle     codexInstalledHTTPLifecycleFacts
	relayed       bool
	relayAccepted bool
	relayRejected bool
}

func (probe *codexInstalledHTTPGateProbe) begin(path codexInstalledHTTPProbePath) *codexInstalledHTTPGateTrace {
	if probe == nil || (path != codexInstalledHTTPProbeResponses && path != codexInstalledHTTPProbeCompact) {
		return nil
	}
	probe.mu.Lock()
	if probe.activeTraces == 0 {
		probe.idle = make(chan struct{})
	}
	probe.activeTraces++
	probe.mu.Unlock()
	return &codexInstalledHTTPGateTrace{probe: probe, path: path}
}

func codexInstalledHTTPTraceFromContext(ctx context.Context) *codexInstalledHTTPGateTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(codexInstalledHTTPProbeContextKey{}).(*codexInstalledHTTPGateTrace)
	return trace
}

func withCodexInstalledHTTPTrace(ctx context.Context, trace *codexInstalledHTTPGateTrace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, codexInstalledHTTPProbeContextKey{}, trace)
}

func (trace *codexInstalledHTTPGateTrace) recordInspect() {
	trace.recordTransformOperation(func(plan *codexInstalledHTTPPlanFacts) *uint8 { return &plan.inspectCalls }, false)
}

func (trace *codexInstalledHTTPGateTrace) recordFreeze() {
	trace.recordTransformOperation(func(plan *codexInstalledHTTPPlanFacts) *uint8 { return &plan.freezeCalls }, false)
}

func (trace *codexInstalledHTTPGateTrace) recordModelRewrite() {
	trace.recordTransformOperation(func(plan *codexInstalledHTTPPlanFacts) *uint8 { return &plan.modelRewrites }, true)
}

func (trace *codexInstalledHTTPGateTrace) recordHeadroomTransform() {
	trace.recordTransformOperation(func(plan *codexInstalledHTTPPlanFacts) *uint8 { return &plan.headroomTransforms }, true)
}

func (trace *codexInstalledHTTPGateTrace) recordEncode() {
	trace.recordTransformOperation(func(plan *codexInstalledHTTPPlanFacts) *uint8 { return &plan.encodeCalls }, false)
}

func (trace *codexInstalledHTTPGateTrace) recordTransformOperation(
	field func(*codexInstalledHTTPPlanFacts) *uint8,
	transformed bool,
) {
	if trace == nil || field == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.finished || trace.planned {
		trace.invalid = true
		return
	}
	value := field(&trace.plan)
	if *value == ^uint8(0) {
		trace.invalid = true
		return
	}
	*value++
	if transformed {
		trace.plan.transformed = true
	}
}

func (trace *codexInstalledHTTPGateTrace) plannedRequest(facts codexInstalledHTTPPlanFacts) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	facts.inspectCalls = trace.plan.inspectCalls
	facts.freezeCalls = trace.plan.freezeCalls
	facts.encodeCalls = trace.plan.encodeCalls
	facts.modelRewrites = trace.plan.modelRewrites
	facts.headroomTransforms = trace.plan.headroomTransforms
	facts.transformed = trace.plan.transformed
	if trace.finished || trace.planned || facts.inspectCalls != 1 || facts.freezeCalls != 1 ||
		facts.dispatch.routeCount == 0 || facts.dispatch.selection == 0 ||
		facts.dispatch.terminalDefaultOrdinal > facts.dispatch.routeCount {
		trace.invalid = true
		return
	}
	trace.planned = true
	trace.plan = facts
}

type codexInstalledHTTPAttemptTrace struct {
	trace *codexInstalledHTTPGateTrace
	index int
}

func (trace *codexInstalledHTTPGateTrace) prepareAttempt(replay *CodexRequestReplay, routeOrdinal uint32) *codexInstalledHTTPAttemptTrace {
	if trace == nil {
		return nil
	}
	digest, err := trace.digestReplay(replay)
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if err != nil || trace.finished || !trace.planned || routeOrdinal == 0 || routeOrdinal > trace.plan.dispatch.routeCount {
		trace.invalid = true
		return nil
	}
	trace.attempts = append(trace.attempts, codexInstalledHTTPAttemptFacts{
		routeOrdinal:    routeOrdinal,
		terminalDefault: routeOrdinal == trace.plan.dispatch.terminalDefaultOrdinal,
		digest:          digest,
	})
	return &codexInstalledHTTPAttemptTrace{trace: trace, index: len(trace.attempts) - 1}
}

func (attempt *codexInstalledHTTPAttemptTrace) dispatched(ok bool) {
	if attempt == nil || attempt.trace == nil {
		return
	}
	trace := attempt.trace
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.finished || attempt.index < 0 || attempt.index >= len(trace.attempts) || trace.attempts[attempt.index].dispatched {
		trace.invalid = true
		return
	}
	trace.attempts[attempt.index].dispatched = ok
	if !ok {
		trace.invalid = true
	}
}

func (attempt *codexInstalledHTTPAttemptTrace) rejected(kind codexInstalledHTTPRejectKind) {
	if attempt == nil || attempt.trace == nil {
		return
	}
	trace := attempt.trace
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.finished || kind == 0 || attempt.index < 0 || attempt.index >= len(trace.attempts) ||
		!trace.attempts[attempt.index].dispatched || trace.attempts[attempt.index].reject != 0 || trace.attempts[attempt.index].admitted {
		trace.invalid = true
		return
	}
	trace.attempts[attempt.index].reject = kind
}

func (attempt *codexInstalledHTTPAttemptTrace) admitted() {
	if attempt == nil || attempt.trace == nil {
		return
	}
	trace := attempt.trace
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.finished || attempt.index < 0 || attempt.index >= len(trace.attempts) ||
		!trace.attempts[attempt.index].dispatched || trace.attempts[attempt.index].reject != 0 || trace.attempts[attempt.index].admitted {
		trace.invalid = true
		return
	}
	trace.attempts[attempt.index].admitted = true
}

func (trace *codexInstalledHTTPGateTrace) relayedResponse(accepted, rejected bool, err error) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.finished || trace.relayed || err != nil || accepted == rejected {
		trace.invalid = true
		return
	}
	trace.relayed = true
	trace.relayAccepted = accepted
	trace.relayRejected = rejected
}

func (trace *codexInstalledHTTPGateTrace) finish() {
	if trace == nil || trace.probe == nil {
		return
	}
	trace.mu.Lock()
	if trace.finished {
		trace.invalid = true
		trace.mu.Unlock()
		return
	}
	trace.finished = true
	view := codexInstalledHTTPGateTraceView{
		path: trace.path, planned: trace.planned, invalid: trace.invalid,
		plan: trace.plan, attempts: append([]codexInstalledHTTPAttemptFacts(nil), trace.attempts...),
		lifecycle: trace.lifecycle, relayed: trace.relayed,
		relayAccepted: trace.relayAccepted, relayRejected: trace.relayRejected,
	}
	trace.attempts = nil
	trace.plan = codexInstalledHTTPPlanFacts{}
	trace.mu.Unlock()
	defer trace.probe.endTrace()
	trace.probe.recordTrace(view)
}

func (probe *codexInstalledHTTPGateProbe) endTrace() {
	if probe == nil {
		return
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.activeTraces == 0 {
		return
	}
	probe.activeTraces--
	if probe.activeTraces == 0 {
		close(probe.idle)
		probe.idle = nil
	}
}

type codexInstalledHTTPReplayMeter struct {
	probe *codexInstalledHTTPGateProbe
}

func codexInstalledHTTPReplayMeterFromContext(ctx context.Context) *codexInstalledHTTPReplayMeter {
	trace := codexInstalledHTTPTraceFromContext(ctx)
	if trace == nil || trace.probe == nil {
		return nil
	}
	return &codexInstalledHTTPReplayMeter{probe: trace.probe}
}

func (meter *codexInstalledHTTPReplayMeter) retain(bytes uint64) bool {
	if meter == nil || meter.probe == nil || bytes == 0 {
		return false
	}
	probe := meter.probe
	probe.mu.Lock()
	defer probe.mu.Unlock()
	current := probe.health.Diagnostics.ReplayEnvelopeCurrentBytes
	if ^uint64(0)-current < bytes {
		probe.generation++
		probe.health.Diagnostics.ReplayEnvelopeErrors++
		return false
	}
	probe.generation++
	current += bytes
	probe.health.Diagnostics.ReplayEnvelopeCurrentBytes = current
	if current > probe.health.Diagnostics.ReplayEnvelopePeakBytes {
		probe.health.Diagnostics.ReplayEnvelopePeakBytes = current
	}
	return true
}

func (meter *codexInstalledHTTPReplayMeter) release(bytes uint64) {
	if meter == nil || meter.probe == nil || bytes == 0 {
		return
	}
	probe := meter.probe
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.generation++
	if bytes > probe.health.Diagnostics.ReplayEnvelopeCurrentBytes {
		probe.health.Diagnostics.ReplayEnvelopeCurrentBytes = 0
		probe.health.Diagnostics.ReplayEnvelopeErrors++
		return
	}
	probe.health.Diagnostics.ReplayEnvelopeCurrentBytes -= bytes
}

func (trace *codexInstalledHTTPGateTrace) digestReplay(replay *CodexRequestReplay) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	if trace == nil || trace.probe == nil || replay == nil {
		return zero, errCodexInstalledListenerAcceptance
	}
	body, err := replay.Body()
	if err != nil {
		return zero, err
	}
	encoded, readErr := io.ReadAll(body)
	closeErr := body.Close()
	defer clearBytes(encoded)
	if readErr != nil || closeErr != nil {
		return zero, errCodexInstalledListenerAcceptance
	}
	decoded, err := replay.DecodedBody()
	if err != nil {
		clearBytes(decoded)
		return zero, errCodexInstalledListenerAcceptance
	}
	defer clearBytes(decoded)
	contentLength, err := replay.ContentLength()
	if err != nil || contentLength < 0 || contentLength != int64(len(encoded)) {
		return zero, errCodexInstalledListenerAcceptance
	}
	header, err := replay.Header()
	if err != nil {
		return zero, err
	}
	model, err := replay.EffectiveModel()
	if err != nil {
		clear(header)
		return zero, err
	}
	trace.probe.mu.Lock()
	key := trace.probe.owner
	trace.probe.mu.Unlock()
	trace.mu.Lock()
	plan := trace.plan
	trace.mu.Unlock()
	mac := hmac.New(sha256.New, key[:])
	writeCodexInstalledProbeMACField(mac, []byte("cq-codex-replay-proof-v2"))
	writeCodexInstalledProbeMACField(mac, encoded)
	writeCodexInstalledProbeMACField(mac, decoded)
	var fixed [16]byte
	binary.BigEndian.PutUint64(fixed[:8], uint64(contentLength))
	fixed[8] = plan.inspectCalls
	fixed[9] = plan.freezeCalls
	fixed[10] = plan.encodeCalls
	fixed[11] = plan.modelRewrites
	fixed[12] = plan.headroomTransforms
	if plan.transformed {
		fixed[13] = 1
	}
	writeCodexInstalledProbeMACField(mac, fixed[:14])
	writeCodexInstalledProbeMACField(mac, []byte("cq-codex-replay-headers-v1"))
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(keys)))
	writeCodexInstalledProbeMACField(mac, count[:])
	for _, key := range keys {
		values := header[key]
		writeCodexInstalledProbeMACField(mac, []byte(key))
		binary.BigEndian.PutUint64(count[:], uint64(len(values)))
		writeCodexInstalledProbeMACField(mac, count[:])
		for _, value := range values {
			writeCodexInstalledProbeMACField(mac, []byte(value))
		}
	}
	writeCodexInstalledProbeMACField(mac, []byte(model))
	clear(header)
	var digest [sha256.Size]byte
	copy(digest[:], mac.Sum(nil))
	return digest, nil
}

func writeCodexInstalledProbeMACField(writer io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func (probe *codexInstalledHTTPGateProbe) recordTrace(trace codexInstalledHTTPGateTraceView) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.generation++
	health := &probe.health
	health.ProductionHandlerRequests++
	health.Acceptance.Requests++
	health.Acceptance.InstalledRequests++
	switch trace.path {
	case codexInstalledHTTPProbeResponses:
		health.NativeResponsesRequests++
	case codexInstalledHTTPProbeCompact:
		health.NativeCompactRequests++
	default:
		trace.invalid = true
	}
	if !trace.planned || !trace.relayed || trace.lifecycle.invalid || !validCodexInstalledHTTPTraceAttempts(trace) {
		trace.invalid = true
	}
	if trace.invalid {
		health.Gates.UnknownLifecycleEvents++
		health.Acceptance.UnknownEvents++
		health.Acceptance.InstalledUnknownEvents++
		return
	}
	dispatched := uint64(0)
	for _, attempt := range trace.attempts {
		if attempt.dispatched {
			dispatched++
		}
	}
	health.Acceptance.InstalledAttempts += dispatched
	health.Acceptance.InstalledResolutions += dispatched
	if trace.plan.strongRequest {
		health.Acceptance.InstalledStrongKeys++
	}
	if trace.plan.strongTurn {
		health.StrongTurns++
		health.Gates.InstalledTurns++
		health.Acceptance.Turns++
		health.Acceptance.SelectorCalls++
		health.Acceptance.InstalledSelectorCalls++
	}
	if trace.plan.zstd {
		health.Acceptance.InstalledZstdRequests++
	}
	if trace.plan.headroom {
		health.Acceptance.HeadroomRequests++
	}
	if trace.attempts[0].routeOrdinal == 1 && trace.attempts[0].dispatched {
		if trace.plan.dispatch.affinityReuse {
			health.Diagnostics.AffinityReuseSelections++
		}
		if trace.plan.dispatch.fairnessSelect {
			health.Diagnostics.FairnessSelections++
		}
	}
	terminalDefaultDispatched := false
	for _, attempt := range trace.attempts {
		if attempt.dispatched && attempt.terminalDefault {
			terminalDefaultDispatched = true
		}
	}
	if terminalDefaultDispatched {
		health.Diagnostics.TerminalDefaultAttempts++
	}
	if trace.lifecycle.drained {
		health.Acceptance.InstalledQuiescentLeases++
	}
	if codexInstalledHTTPFrozenGate(trace) {
		health.Gates.FrozenSingleTransformEnvelopeCases++
	}
	if codexInstalledHTTPWarmGate(trace) {
		health.Gates.WarmAffinityCases++
	}
	if codexInstalledHTTPDeterministicGate(trace) {
		health.Gates.DeterministicFallbackCases++
	}
	if codexInstalledHTTPDefaultGate(trace) {
		health.Gates.TerminalDefaultOnceCases++
	}
	if codexInstalledHTTPHard429ReplayGate(trace) {
		health.Gates.ExactPreAdmissionHard429ReplayCases++
	}
	if codexInstalledHTTPAdmittedNoMigrationGate(trace) {
		health.Gates.AdmittedNoMigrationCases++
	}
	if codexInstalledHTTPV2RuntimeGate(trace) {
		health.Gates.V2JournalRuntimeCases++
	}
}

func validCodexInstalledHTTPTraceAttempts(trace codexInstalledHTTPGateTraceView) bool {
	if len(trace.attempts) == 0 {
		return false
	}
	for _, attempt := range trace.attempts {
		if !attempt.dispatched || attempt.digest == ([sha256.Size]byte{}) ||
			(attempt.reject == 0) == (!attempt.admitted) {
			return false
		}
	}
	return true
}

func codexInstalledHTTPCleanAccepted(trace codexInstalledHTTPGateTraceView) bool {
	return trace.relayAccepted && trace.lifecycle.terminal == codexInstalledHTTPTerminalCompleted && trace.lifecycle.drained
}

func codexInstalledHTTPFrozenGate(trace codexInstalledHTTPGateTraceView) bool {
	if !trace.plan.transformed || trace.plan.inspectCalls != 1 || trace.plan.freezeCalls != 1 || trace.plan.encodeCalls != 1 ||
		trace.plan.modelRewrites > 1 || trace.plan.headroomTransforms > 1 || len(trace.attempts) < 2 || !trace.relayed {
		return false
	}
	digest := trace.attempts[0].digest
	routes := make(map[uint32]struct{}, len(trace.attempts))
	for _, attempt := range trace.attempts {
		if attempt.digest != digest {
			return false
		}
		routes[attempt.routeOrdinal] = struct{}{}
	}
	return len(routes) >= 2
}

func codexInstalledHTTPWarmGate(trace codexInstalledHTTPGateTraceView) bool {
	return trace.plan.dispatch.selection == codexInstalledHTTPSelectionWarmAffinity &&
		trace.plan.dispatch.eligibleCompetitors > 0 && trace.plan.dispatch.naturalWinnerDisplaced &&
		trace.attempts[0].admitted && codexInstalledHTTPCleanAccepted(trace)
}

func codexInstalledHTTPDeterministicGate(trace codexInstalledHTTPGateTraceView) bool {
	return trace.plan.dispatch.selection == codexInstalledHTTPSelectionDeterministicFallback &&
		trace.plan.dispatch.affinityUnavailable && !trace.attempts[0].terminalDefault &&
		trace.attempts[0].admitted && codexInstalledHTTPCleanAccepted(trace)
}

func codexInstalledHTTPDefaultGate(trace codexInstalledHTTPGateTraceView) bool {
	terminalDefaultDispatches := 0
	for _, attempt := range trace.attempts {
		if attempt.terminalDefault {
			terminalDefaultDispatches++
			if terminalDefaultDispatches != 1 || attempt.routeOrdinal != trace.plan.dispatch.terminalDefaultOrdinal || attempt.admitted || attempt.reject == 0 {
				return false
			}
			continue
		}
		if terminalDefaultDispatches != 0 {
			return false
		}
		if attempt.reject != codexInstalledHTTPRejectAuth && attempt.reject != codexInstalledHTTPRejectExactHard429 {
			return false
		}
	}
	return terminalDefaultDispatches == 1 && trace.relayRejected && trace.lifecycle.terminal == codexInstalledHTTPTerminalRejected
}

func codexInstalledHTTPHard429ReplayGate(trace codexInstalledHTTPGateTraceView) bool {
	if trace.plan.initialAdmitted || len(trace.attempts) < 2 || !codexInstalledHTTPCleanAccepted(trace) {
		return false
	}
	first := trace.attempts[0]
	if first.reject != codexInstalledHTTPRejectExactHard429 {
		return false
	}
	for _, attempt := range trace.attempts[1:] {
		if attempt.routeOrdinal != first.routeOrdinal && attempt.digest == first.digest && attempt.admitted {
			return true
		}
	}
	return false
}

func codexInstalledHTTPAdmittedNoMigrationGate(trace codexInstalledHTTPGateTraceView) bool {
	if !trace.plan.initialAdmitted || len(trace.attempts) != 1 || !trace.relayRejected ||
		trace.lifecycle.terminal != codexInstalledHTTPTerminalRejected {
		return false
	}
	return trace.attempts[0].reject == codexInstalledHTTPRejectExactHard429
}

func codexInstalledHTTPV2RuntimeGate(trace codexInstalledHTTPGateTraceView) bool {
	return trace.plan.durableV2 && trace.plan.requestGeneration != 0 && trace.plan.attemptGeneration != 0 &&
		trace.lifecycle.begin && trace.lifecycle.dispatched && trace.lifecycle.terminal != 0 && trace.lifecycle.drained &&
		!trace.lifecycle.invalid && trace.lifecycle.requestGen >= trace.plan.requestGeneration &&
		trace.lifecycle.attemptGen >= trace.plan.attemptGeneration
}
