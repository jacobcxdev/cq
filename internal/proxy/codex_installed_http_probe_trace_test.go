package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCodexInstalledHTTPProbePreservesTerminalDefaultSourceDecision(t *testing.T) {
	tests := []struct {
		name       string
		candidates []CodexRoutePolicyCandidate
		hints      CodexRoutePolicyHints
		want       uint32
	}{
		{
			name: "appended zero-capacity default", candidates: []CodexRoutePolicyCandidate{
				routePolicyCandidate("ordinary", CapacityPositive, 50),
				routePolicyCandidate("default", CapacityZero, 0),
			}, hints: CodexRoutePolicyHints{DefaultAccountKey: "default"}, want: 2,
		},
		{
			name: "ordinary eligible configured default", candidates: []CodexRoutePolicyCandidate{
				routePolicyCandidate("ordinary", CapacityPositive, 80),
				routePolicyCandidate("default", CapacityPositive, 50),
			}, hints: CodexRoutePolicyHints{DefaultAccountKey: "default"}, want: 0,
		},
		{
			name: "bound configured default", candidates: []CodexRoutePolicyCandidate{
				routePolicyCandidate("default", CapacityZero, 0),
			}, hints: CodexRoutePolicyHints{DefaultAccountKey: "default", BoundAccountKey: "default"}, want: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := BuildCodexRoutePlan(context.Background(), test.candidates, test.hints)
			if err != nil {
				t.Fatal(err)
			}
			facts := codexInstalledHTTPDispatchFactsForPolicy(test.candidates, plan, test.hints)
			if facts.terminalDefaultOrdinal != test.want {
				t.Fatalf("terminal default ordinal = %d, want %d", facts.terminalDefaultOrdinal, test.want)
			}
		})
	}
}

func TestCodexInstalledHTTPProbePreservesAffinityAndFairnessSourceDecisions(t *testing.T) {
	tests := []struct {
		name         string
		candidates   []CodexRoutePolicyCandidate
		hints        CodexRoutePolicyHints
		wantAffinity bool
		wantFairness bool
	}{
		{
			name: "eligible affinity naturally wins", candidates: []CodexRoutePolicyCandidate{
				routePolicyCandidate("affinity", CapacityPositive, 90),
				routePolicyCandidate("other", CapacityPositive, 10),
			}, hints: CodexRoutePolicyHints{AffinityAccountKey: "affinity", DefaultAccountKey: "other"}, wantAffinity: true,
		},
		{
			name: "no affinity", candidates: []CodexRoutePolicyCandidate{
				routePolicyCandidate("ordinary", CapacityPositive, 90),
			}, hints: CodexRoutePolicyHints{DefaultAccountKey: "ordinary"}, wantFairness: true,
		},
		{
			name: "unavailable affinity", candidates: []CodexRoutePolicyCandidate{
				routePolicyCandidate("affinity", CapacityZero, 0),
				routePolicyCandidate("ordinary", CapacityPositive, 90),
			}, hints: CodexRoutePolicyHints{AffinityAccountKey: "affinity", DefaultAccountKey: "ordinary"}, wantFairness: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := BuildCodexRoutePlan(context.Background(), test.candidates, test.hints)
			if err != nil {
				t.Fatal(err)
			}
			facts := codexInstalledHTTPDispatchFactsForPolicy(test.candidates, plan, test.hints)
			if facts.affinityReuse != test.wantAffinity || facts.fairnessSelect != test.wantFairness {
				t.Fatalf("decision facts = %#v", facts)
			}
		})
	}
}

func TestCodexInstalledHTTPGateProbeIsBoundToConcreteNativeHandler(t *testing.T) {
	binding := sha256.Sum256([]byte("listener"))
	probe := newTestCodexInstalledHTTPGateProbe(t, binding)
	planner := &codexNativeHTTPPlannerStub{err: errors.New("synthetic planner")}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	detach, err := handler.installCodexInstalledHTTPGateProbe(probe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.installCodexInstalledHTTPGateProbe(probe); err == nil {
		t.Fatal("concurrent probe installation succeeded")
	}
	request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	handler.TryServe(httptest.NewRecorder(), request, false)
	detach()

	probe.mu.Lock()
	health := probe.health
	probe.mu.Unlock()
	if health.ProductionHandlerRequests != 1 || health.NativeResponsesRequests != 1 ||
		health.Gates.UnknownLifecycleEvents != 1 || health.Gates.V2JournalRuntimeCases != 0 {
		t.Fatalf("fake native dependencies credited evidence: %#v", health)
	}
	if _, err := handler.installCodexInstalledHTTPGateProbe(probe); err != nil {
		t.Fatalf("probe did not detach: %v", err)
	}
}

func TestCodexInstalledHTTPGateProbeSnapshotWaitsForActiveTrace(t *testing.T) {
	binding := sha256.Sum256([]byte("listener"))
	probe := newTestCodexInstalledHTTPGateProbe(t, binding)
	trace := probe.begin(codexInstalledHTTPProbeResponses)
	if trace == nil {
		t.Fatal("begin() = nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := probe.snapshot(ctx, binding); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("snapshot() error = %v, want context deadline", err)
	}

	trace.finish()
	snapshot, err := probe.snapshot(context.Background(), binding)
	if err != nil {
		t.Fatalf("snapshot() after finish error = %v", err)
	}
	if snapshot.health.ProductionHandlerRequests != 1 {
		t.Fatalf("production requests = %d, want 1", snapshot.health.ProductionHandlerRequests)
	}
}

func TestCodexInstalledHTTPGateProbeDerivesSevenGatesFromSequences(t *testing.T) {
	probe := newTestCodexInstalledHTTPGateProbe(t, sha256.Sum256([]byte("listener")))
	digest := sha256.Sum256([]byte("frozen-envelope"))

	hardReplay := testCodexInstalledTrace(codexInstalledHTTPSelectionOrdinary, 2, digest)
	hardReplay.plan.transformed = true
	hardReplay.plan.encodeCalls = 1
	hardReplay.plan.headroomTransforms = 1
	hardReplay.attempts = []codexInstalledHTTPAttemptFacts{
		{routeOrdinal: 1, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectExactHard429},
		{routeOrdinal: 2, digest: digest, dispatched: true, admitted: true},
	}
	probe.recordTrace(hardReplay)

	warm := testCodexInstalledTrace(codexInstalledHTTPSelectionWarmAffinity, 2, digest)
	warm.plan.dispatch.eligibleCompetitors = 1
	warm.plan.dispatch.naturalWinnerDisplaced = true
	probe.recordTrace(warm)

	fallback := testCodexInstalledTrace(codexInstalledHTTPSelectionDeterministicFallback, 2, digest)
	fallback.plan.dispatch.affinityUnavailable = true
	probe.recordTrace(fallback)

	terminalDefault := testCodexInstalledTrace(codexInstalledHTTPSelectionOrdinary, 2, digest)
	terminalDefault.lifecycle.terminal = codexInstalledHTTPTerminalRejected
	terminalDefault.lifecycle.drained = false
	terminalDefault.relayAccepted = false
	terminalDefault.relayRejected = true
	terminalDefault.plan.dispatch.terminalDefaultOrdinal = 2
	terminalDefault.attempts = []codexInstalledHTTPAttemptFacts{
		{routeOrdinal: 1, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectAuth},
		{routeOrdinal: 2, terminalDefault: true, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectExactHard429},
	}
	probe.recordTrace(terminalDefault)

	admittedNoMigration := testCodexInstalledTrace(codexInstalledHTTPSelectionBound, 1, digest)
	admittedNoMigration.plan.initialAdmitted = true
	admittedNoMigration.lifecycle.terminal = codexInstalledHTTPTerminalRejected
	admittedNoMigration.lifecycle.drained = false
	admittedNoMigration.relayAccepted = false
	admittedNoMigration.relayRejected = true
	admittedNoMigration.attempts = []codexInstalledHTTPAttemptFacts{{
		routeOrdinal: 1, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectExactHard429,
	}}
	probe.recordTrace(admittedNoMigration)

	probe.mu.Lock()
	gates := probe.health.Gates
	diagnostics := probe.health.Diagnostics
	probe.mu.Unlock()
	if gates.FrozenSingleTransformEnvelopeCases != 1 ||
		gates.WarmAffinityCases != 1 ||
		gates.DeterministicFallbackCases != 1 ||
		gates.TerminalDefaultOnceCases != 1 ||
		gates.ExactPreAdmissionHard429ReplayCases != 1 ||
		gates.AdmittedNoMigrationCases != 1 ||
		gates.V2JournalRuntimeCases != 3 {
		t.Fatalf("derived gates = %#v", gates)
	}
	if diagnostics.AffinityReuseSelections != 1 || diagnostics.FairnessSelections != 1 ||
		diagnostics.TerminalDefaultAttempts != 1 {
		t.Fatalf("derived diagnostics = %#v", diagnostics)
	}
}

func TestCodexInstalledHTTPGateProbeRejectsDuplicateTerminalDefaultDispatch(t *testing.T) {
	digest := sha256.Sum256([]byte("frozen-envelope"))
	trace := testCodexInstalledTrace(codexInstalledHTTPSelectionOrdinary, 2, digest)
	trace.lifecycle.terminal = codexInstalledHTTPTerminalRejected
	trace.lifecycle.drained = false
	trace.relayAccepted = false
	trace.relayRejected = true
	trace.plan.dispatch.terminalDefaultOrdinal = 2
	trace.attempts = []codexInstalledHTTPAttemptFacts{
		{routeOrdinal: 1, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectAuth},
		{routeOrdinal: 2, terminalDefault: true, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectAuth},
		{routeOrdinal: 2, terminalDefault: true, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectExactHard429},
	}

	if codexInstalledHTTPDefaultGate(trace) {
		t.Fatal("duplicate terminal-default dispatch credited the once gate")
	}
}

func TestCodexInstalledHTTPGateProbeDoesNotInferModelCatalogueRequests(t *testing.T) {
	probe := newTestCodexInstalledHTTPGateProbe(t, sha256.Sum256([]byte("listener")))
	digest := sha256.Sum256([]byte("frozen-envelope"))
	probe.recordTrace(testCodexInstalledTrace(codexInstalledHTTPSelectionOrdinary, 1, digest))

	probe.mu.Lock()
	modelRequests := probe.health.Acceptance.InstalledModelRequests
	probe.mu.Unlock()
	if modelRequests != 0 {
		t.Fatalf("native traffic inferred %d model-catalogue requests", modelRequests)
	}
}

func TestCodexInstalledHTTPGateProbeAccountsReplayEnvelopeBytes(t *testing.T) {
	probe := newTestCodexInstalledHTTPGateProbe(t, sha256.Sum256([]byte("listener")))
	meter := &codexInstalledHTTPReplayMeter{probe: probe}
	encoded := []byte("1234")
	decoded := []byte("123456")
	retained := uint64(len(encoded) + len(decoded))
	if !meter.retain(retained) {
		t.Fatal("failed to meter base replay envelope")
	}
	envelope := &CodexRequestEnvelope{
		encoded: encoded, decoded: decoded, meter: meter, retainedBytes: retained,
	}
	first, err := envelope.Replay()
	if err != nil {
		t.Fatal(err)
	}
	second, err := envelope.Replay()
	if err != nil {
		t.Fatal(err)
	}
	body, err := first.Body()
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	second.Release()
	envelope.Release()
	probe.mu.Lock()
	retainedWithOpenBody := probe.health.Diagnostics.ReplayEnvelopeCurrentBytes
	probe.mu.Unlock()
	if retainedWithOpenBody != retained {
		t.Fatalf("bytes retained for open body = %d, want %d", retainedWithOpenBody, retained)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	first.Release()
	second.Release()
	envelope.Release()
	probe.mu.Lock()
	diagnostics := probe.health.Diagnostics
	probe.mu.Unlock()
	if diagnostics.ReplayEnvelopeCurrentBytes != 0 || diagnostics.ReplayEnvelopePeakBytes != retained || diagnostics.ReplayEnvelopeErrors != 0 {
		t.Fatalf("replay diagnostics = %#v", diagnostics)
	}
}

func TestCodexInstalledHTTPProbeDigestBindsEveryReplayHeader(t *testing.T) {
	probe := newTestCodexInstalledHTTPGateProbe(t, sha256.Sum256([]byte("listener")))
	trace := probe.begin(codexInstalledHTTPProbeResponses)
	headers := http.Header{
		"Content-Type":                           []string{"application/json"},
		"Content-Encoding":                       []string{"zstd"},
		"Accept":                                 []string{"text/event-stream"},
		"Accept-Encoding":                        []string{"gzip"},
		"User-Agent":                             []string{"codex-test"},
		"OpenAI-Beta":                            []string{"responses=2026"},
		"OpenAI-Alpha":                           []string{"alpha"},
		"X-Codex-Turn-Metadata":                  []string{"turn-a"},
		"X-Codex-Turn-State":                     []string{"state-a"},
		"X-Codex-Installation-Id":                []string{"installation-a"},
		"X-Codex-Parent-Thread-Id":               []string{"parent-a"},
		"X-Codex-Window-Id":                      []string{"window-a"},
		"X-OpenAI-Subagent":                      []string{"subagent-a"},
		"X-OpenAI-Memgen-Request":                []string{"memgen-a"},
		"X-OpenAI-Internal-Codex-Responses-Lite": []string{"lite-a"},
		"X-ResponsesAPI-Include-Timing-Metrics":  []string{"timing-a"},
	}
	headers = codexReplayHeaders(headers)
	baseline := testCodexInstalledReplayDigest(t, trace, headers)
	for key := range headers {
		mutated := headers.Clone()
		mutated[key] = []string{"mutated"}
		if got := testCodexInstalledReplayDigest(t, trace, mutated); got == baseline {
			t.Fatalf("replay digest ignored %s", key)
		}
	}
}

func TestCodexInstalledHTTPProbeDigestBindsDecodedBodyFramingAndTransforms(t *testing.T) {
	probe := newTestCodexInstalledHTTPGateProbe(t, sha256.Sum256([]byte("listener")))
	trace := probe.begin(codexInstalledHTTPProbeResponses)
	trace.plannedRequest(codexInstalledHTTPPlanFacts{
		inspectCalls: 1,
		freezeCalls:  1,
		dispatch: codexInstalledHTTPDispatchFacts{
			selection:  codexInstalledHTTPSelectionOrdinary,
			routeCount: 1,
		},
	})
	headers := http.Header{"Content-Type": []string{"application/json"}}
	baseline := testCodexInstalledReplayDigestParts(t, trace, []byte("encoded"), []byte("decoded-a"), headers)
	if got := testCodexInstalledReplayDigestParts(t, trace, []byte("encoded"), []byte("decoded-b"), headers); got == baseline {
		t.Fatal("replay digest ignored decoded inspection body")
	}
	if got := testCodexInstalledReplayDigestParts(t, trace, []byte("encoded-longer"), []byte("decoded-a"), headers); got == baseline {
		t.Fatal("replay digest ignored encoded framing length")
	}
	trace.mu.Lock()
	trace.plan.encodeCalls++
	trace.mu.Unlock()
	if got := testCodexInstalledReplayDigestParts(t, trace, []byte("encoded"), []byte("decoded-a"), headers); got == baseline {
		t.Fatal("replay digest ignored transform counts")
	}
}

func TestCodexInstalledHTTPGateProbeRejectsNearMatchSequences(t *testing.T) {
	digest := sha256.Sum256([]byte("frozen-envelope"))
	tests := []struct {
		name   string
		mutate func(*codexInstalledHTTPGateTraceView)
	}{
		{name: "single attempt frozen", mutate: func(trace *codexInstalledHTTPGateTraceView) {
			trace.plan.transformed = true
		}},
		{name: "warm without displaced winner", mutate: func(trace *codexInstalledHTTPGateTraceView) {
			trace.plan.dispatch.selection = codexInstalledHTTPSelectionWarmAffinity
			trace.plan.dispatch.eligibleCompetitors = 1
		}},
		{name: "fallback without unavailable affinity", mutate: func(trace *codexInstalledHTTPGateTraceView) {
			trace.plan.dispatch.selection = codexInstalledHTTPSelectionDeterministicFallback
		}},
		{name: "default merely planned", mutate: func(trace *codexInstalledHTTPGateTraceView) {
			trace.plan.dispatch.terminalDefaultOrdinal = 2
		}},
		{name: "near match hard limit", mutate: func(trace *codexInstalledHTTPGateTraceView) {
			trace.plan.dispatch.routeCount = 2
			trace.attempts = []codexInstalledHTTPAttemptFacts{
				{routeOrdinal: 1, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectOther},
				{routeOrdinal: 2, digest: digest, dispatched: true, admitted: true},
			}
		}},
		{name: "admitted migration", mutate: func(trace *codexInstalledHTTPGateTraceView) {
			trace.plan.initialAdmitted = true
			trace.plan.dispatch.routeCount = 2
			trace.attempts = []codexInstalledHTTPAttemptFacts{
				{routeOrdinal: 1, digest: digest, dispatched: true, reject: codexInstalledHTTPRejectExactHard429},
				{routeOrdinal: 2, digest: digest, dispatched: true, admitted: true},
			}
		}},
		{name: "fake lifecycle", mutate: func(trace *codexInstalledHTTPGateTraceView) {
			trace.plan.durableV2 = false
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace := testCodexInstalledTrace(codexInstalledHTTPSelectionOrdinary, 1, digest)
			test.mutate(&trace)
			switch test.name {
			case "single attempt frozen":
				if codexInstalledHTTPFrozenGate(trace) {
					t.Fatal("single attempt credited frozen gate")
				}
			case "warm without displaced winner":
				if codexInstalledHTTPWarmGate(trace) {
					t.Fatal("near-match warm choice credited")
				}
			case "fallback without unavailable affinity":
				if codexInstalledHTTPDeterministicGate(trace) {
					t.Fatal("near-match fallback credited")
				}
			case "default merely planned":
				if codexInstalledHTTPDefaultGate(trace) {
					t.Fatal("planned default credited")
				}
			case "near match hard limit":
				if codexInstalledHTTPHard429ReplayGate(trace) {
					t.Fatal("near-match 429 credited")
				}
			case "admitted migration":
				if codexInstalledHTTPAdmittedNoMigrationGate(trace) {
					t.Fatal("admitted migration credited")
				}
			case "fake lifecycle":
				if codexInstalledHTTPV2RuntimeGate(trace) {
					t.Fatal("fake lifecycle credited")
				}
			}
		})
	}
}

func testCodexInstalledTrace(selection codexInstalledHTTPSelection, routes uint32, digest [sha256.Size]byte) codexInstalledHTTPGateTraceView {
	return codexInstalledHTTPGateTraceView{
		path:    codexInstalledHTTPProbeResponses,
		planned: true,
		plan: codexInstalledHTTPPlanFacts{
			strongTurn: true, strongRequest: true, zstd: true, headroom: true,
			inspectCalls: 1, freezeCalls: 1,
			durableV2: true, requestGeneration: 1, attemptGeneration: 1,
			dispatch: codexInstalledHTTPDispatchFacts{
				selection: selection, routeCount: routes,
				affinityReuse:  selection == codexInstalledHTTPSelectionWarmAffinity,
				fairnessSelect: selection == codexInstalledHTTPSelectionDeterministicFallback,
			},
		},
		attempts: []codexInstalledHTTPAttemptFacts{{routeOrdinal: 1, digest: digest, dispatched: true, admitted: true}},
		lifecycle: codexInstalledHTTPLifecycleFacts{
			begin: true, dispatched: true, terminal: codexInstalledHTTPTerminalCompleted,
			drained: true, requestGen: 2, attemptGen: 2,
		},
		relayed: true, relayAccepted: true,
	}
}

func testCodexInstalledReplayDigest(t *testing.T, trace *codexInstalledHTTPGateTrace, headers http.Header) [sha256.Size]byte {
	return testCodexInstalledReplayDigestParts(t, trace, []byte("encoded"), []byte("decoded"), headers)
}

func testCodexInstalledReplayDigestParts(
	t *testing.T,
	trace *codexInstalledHTTPGateTrace,
	encoded []byte,
	decoded []byte,
	headers http.Header,
) [sha256.Size]byte {
	t.Helper()
	envelope, err := NewCodexRequestEnvelope(encoded, decoded, headers, "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	defer envelope.Release()
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Release()
	digest, err := trace.digestReplay(replay)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
