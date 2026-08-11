package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestEvaluateCodexCanaryPromotionCodeOnlyFixtureIsIneligible(t *testing.T) {
	now, required, marker, state := codexCanaryPromotionTestState(t)
	recorder, _ := openPromotionTestRecorder(t, state)

	err := EvaluateCodexCanaryPromotion(now, required, marker, recorder, CodexCanaryRollbackReceipt{})
	if !errors.Is(err, ErrCodexCanaryNotPromotable) {
		t.Fatalf("code-only fixture error = %v, want ErrCodexCanaryNotPromotable", err)
	}
}

func TestValidateCodexCanaryPromotionStateRejectsIncompleteEvidence(t *testing.T) {
	now, required, marker, eligible := codexCanaryPromotionTestState(t)
	if err := validateCodexCanaryPromotionState(now, required, marker, eligible); err != nil {
		t.Fatalf("eligible state: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*time.Time, *CodexTransportRequirements, *CodexReadinessMarker, *CodexCanaryState)
	}{
		{"current marker drift", func(_ *time.Time, _ *CodexTransportRequirements, marker *CodexReadinessMarker, _ *CodexCanaryState) {
			marker.SemanticsRevision = "stale"
		}},
		{"tuple drift", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.Tuple.ClientBuild = "stale"
		}},
		{"active run", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.Active = true
		}},
		{"missing finalisation", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.Finalisation = nil
		}},
		{"missing run ID", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.RunID = ""
		}},
		{"short elapsed time", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.StartedAt = state.EndedAt.Add(-6 * 24 * time.Hour)
		}},
		{"future end", func(now *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.EndedAt = now.Add(time.Hour)
		}},
		{"marker after start", func(_ *time.Time, _ *CodexTransportRequirements, marker *CodexReadinessMarker, state *CodexCanaryState) {
			marker.ValidatedAt = state.StartedAt.Add(time.Hour)
		}},
		{"too few turns", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.AdmittedTurns = 99
		}},
		{"too few service days", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.ConsecutiveCalendarDays = 6
		}},
		{"impossible service days", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.ConsecutiveCalendarDays = 100
		}},
		{"missing protection", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.ProtectedDigests = state.ProtectedDigests[:len(state.ProtectedDigests)-1]
		}},
		{"keyed mismatch", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.KeyedMismatches = 1
		}},
		{"protected change", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.AutomaticHashChanges = 1
		}},
		{"secret leak", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.SecretLeaks = 1
		}},
		{"unexplained lifecycle", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.UnexplainedLifecycles = 1
		}},
		{"live session repair", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.LiveSessionRepairs = 1
		}},
		{"protected state failure", func(_ *time.Time, _ *CodexTransportRequirements, _ *CodexReadinessMarker, state *CodexCanaryState) {
			state.ProtectedStateFailures = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testNow, testRequired, testMarker, testState := now, required, marker, eligible
			testState.ProtectedDigests = append([]CodexCanaryProtectedDigest(nil), eligible.ProtectedDigests...)
			test.mutate(&testNow, &testRequired, &testMarker, &testState)
			if err := validateCodexCanaryPromotionState(testNow, testRequired, testMarker, testState); !errors.Is(err, ErrCodexCanaryNotPromotable) {
				t.Fatalf("validation error = %v, want ErrCodexCanaryNotPromotable", err)
			}
		})
	}
}

func TestCodexCanaryFinalPromotionEvidenceBindsPersistedSignedEnvelope(t *testing.T) {
	_, _, _, state := codexCanaryPromotionTestState(t)
	recorder, fsys := openPromotionTestRecorder(t, state)
	evidence, err := codexCanaryFinalPromotionEvidence(recorder)
	if err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("/state/canary.json")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.envelopeGeneration != 1 || evidence.envelopeDigest != sha256.Sum256(data) {
		t.Fatalf("final envelope binding = generation %d digest %x", evidence.envelopeGeneration, evidence.envelopeDigest)
	}
	if evidence.countersDigest != codexCanaryCountersDigest(state) {
		t.Fatal("final counters digest does not bind canonical counters")
	}

	recorder.mu.Lock()
	recorder.state.AdmittedTurns++
	recorder.mu.Unlock()
	if _, err := codexCanaryFinalPromotionEvidence(recorder); !errors.Is(err, ErrCodexCanaryNotPromotable) {
		t.Fatalf("stale recorder error = %v, want ErrCodexCanaryNotPromotable", err)
	}
}

func TestCodexCanaryRollbackReceiptBindsFinalStateAndAuthorities(t *testing.T) {
	_, _, marker, state := codexCanaryPromotionTestState(t)
	recorder, _ := openPromotionTestRecorder(t, state)
	evidence, err := codexCanaryFinalPromotionEvidence(recorder)
	if err != nil {
		t.Fatal(err)
	}
	receipt := sealedRollbackTestReceipt(marker, evidence)
	if err := receipt.validate(marker, evidence); err != nil {
		t.Fatalf("sealed test receipt: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CodexCanaryRollbackReceipt)
	}{
		{"generation", func(receipt *CodexCanaryRollbackReceipt) { receipt.finalEnvelopeGeneration++ }},
		{"envelope digest", func(receipt *CodexCanaryRollbackReceipt) { receipt.finalEnvelopeDigest[0]++ }},
		{"counters digest", func(receipt *CodexCanaryRollbackReceipt) { receipt.finalCountersDigest[0]++ }},
		{"stop request", func(receipt *CodexCanaryRollbackReceipt) { receipt.stopRequestDigest = [sha256.Size]byte{} }},
		{"process binding", func(receipt *CodexCanaryRollbackReceipt) { receipt.processBindingDigest = [sha256.Size]byte{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := receipt
			changed.scenarios = append([]codexCanaryScenarioEvidence(nil), receipt.scenarios...)
			test.mutate(&changed)
			changed.seal = changed.expectedSeal()
			if err := changed.validate(marker, evidence); !errors.Is(err, ErrCodexCanaryNotPromotable) {
				t.Fatalf("validation error = %v, want ErrCodexCanaryNotPromotable", err)
			}
		})
	}
}

func TestCodexCanaryRollbackReceiptRequiresExactScenarioSetAndActivation(t *testing.T) {
	_, _, marker, state := codexCanaryPromotionTestState(t)
	recorder, _ := openPromotionTestRecorder(t, state)
	evidence, err := codexCanaryFinalPromotionEvidence(recorder)
	if err != nil {
		t.Fatal(err)
	}
	receipt := sealedRollbackTestReceipt(marker, evidence)
	tests := []struct {
		name   string
		mutate func(*CodexCanaryRollbackReceipt)
	}{
		{"missing scenario", func(receipt *CodexCanaryRollbackReceipt) {
			receipt.scenarios = receipt.scenarios[:len(receipt.scenarios)-1]
		}},
		{"duplicate scenario", func(receipt *CodexCanaryRollbackReceipt) {
			receipt.scenarios[1].name = receipt.scenarios[0].name
		}},
		{"unknown scenario", func(receipt *CodexCanaryRollbackReceipt) {
			receipt.scenarios[0].name = "unknown"
		}},
		{"reordered scenario", func(receipt *CodexCanaryRollbackReceipt) {
			receipt.scenarios[0], receipt.scenarios[1] = receipt.scenarios[1], receipt.scenarios[0]
		}},
		{"zero scenario digest", func(receipt *CodexCanaryRollbackReceipt) {
			receipt.scenarios[0].digest = [sha256.Size]byte{}
		}},
		{"missing activation receipt", func(receipt *CodexCanaryRollbackReceipt) {
			receipt.activationReceiptDigest = [sha256.Size]byte{}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := receipt
			changed.scenarios = append([]codexCanaryScenarioEvidence(nil), receipt.scenarios...)
			test.mutate(&changed)
			changed.seal = changed.expectedSeal()
			if err := changed.validate(marker, evidence); !errors.Is(err, ErrCodexCanaryNotPromotable) {
				t.Fatalf("validation error = %v, want ErrCodexCanaryNotPromotable", err)
			}
		})
	}
}

func TestCodexCanaryRollbackReceiptRequiresStage12ATaskAffinityScenarios(t *testing.T) {
	want := []codexCanaryScenarioName{
		codexCanaryScenarioLongTurnQuotaDepletion,
		codexCanaryScenarioParallelShortTurns,
		codexCanaryScenarioTaskAffinityBeforeFloor,
		codexCanaryScenarioTaskAffinityFloorFallback,
		codexCanaryScenarioSoftUnboundHard429Escape,
		codexCanaryScenarioSameLaneSupersession,
		codexCanaryScenarioTerminalRoutingDefault,
		codexCanaryScenarioLateSameTurn429NoAlternate,
		codexCanaryScenarioQuiescentRestart,
		codexCanaryScenarioCodexBarObserveNoSwitch,
		codexCanaryScenarioExplicitSwitchActivationReceipt,
	}
	if codexCanaryRollbackReceiptVersion != 3 {
		t.Fatalf("rollback receipt version = %d, want 3", codexCanaryRollbackReceiptVersion)
	}
	if len(requiredCodexCanaryScenarios) != len(want) {
		t.Fatalf("required scenarios = %v, want %v", requiredCodexCanaryScenarios, want)
	}
	for index, scenario := range want {
		if requiredCodexCanaryScenarios[index] != scenario {
			t.Fatalf("required scenario %d = %q, want %q", index, requiredCodexCanaryScenarios[index], scenario)
		}
	}
}

func codexCanaryPromotionTestState(t *testing.T) (time.Time, CodexTransportRequirements, CodexReadinessMarker, CodexCanaryState) {
	t.Helper()
	required := testCodexRequirements(CodexRoutingHTTP)
	marker := testCodexMarker(required)
	marker.ValidatedAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	tuple, err := BuildCodexCanaryTuple(required, marker)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := marker.ValidatedAt.Add(time.Hour)
	endedAt := startedAt.Add(7 * 24 * time.Hour)
	runID, err := newCodexCanaryRandomID()
	if err != nil {
		t.Fatal(err)
	}
	state := CodexCanaryState{
		Version:                 CodexCanaryVersion,
		RunID:                   runID,
		StartedAt:               startedAt,
		EndedAt:                 endedAt,
		LastObservedAt:          canaryCalendarDay(endedAt),
		Tuple:                   tuple,
		AdmittedTurns:           100,
		ConsecutiveCalendarDays: 7,
		ProtectedDigests:        canaryTestProtectedDigests(),
	}
	stopRequestDigest := sha256.Sum256([]byte("test stop request"))
	processBindingDigest := sha256.Sum256([]byte("test installed process binding"))
	countersDigest := codexCanaryCountersDigest(state)
	state.Finalisation = &codexCanaryFinalisation{
		StopRequestDigest:    hex.EncodeToString(stopRequestDigest[:]),
		ProcessBindingDigest: hex.EncodeToString(processBindingDigest[:]),
		CountersDigest:       hex.EncodeToString(countersDigest[:]),
	}
	return endedAt.Add(time.Hour), required, marker, state
}

func openPromotionTestRecorder(t *testing.T, state CodexCanaryState) (*CodexCanaryRecorder, *fsutil.MemFS) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", codexCanaryIntegrityKeyBytes))
	if err := fsys.WriteFile("/state/canary.json.key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &CodexCanaryRecorder{fs: fsys, path: "/state/canary.json", key: key, state: state}
	if err := recorder.persistLocked(); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenCodexCanary(fsys, "/state/canary.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	return opened, fsys
}

func sealedRollbackTestReceipt(marker CodexReadinessMarker, evidence codexCanaryFinalEvidence) CodexCanaryRollbackReceipt {
	scenarios := make([]codexCanaryScenarioEvidence, 0, len(requiredCodexCanaryScenarios))
	for _, name := range requiredCodexCanaryScenarios {
		scenarios = append(scenarios, codexCanaryScenarioEvidence{name: name, digest: sha256.Sum256([]byte("test:" + string(name)))})
	}
	receipt := CodexCanaryRollbackReceipt{
		version:                 codexCanaryRollbackReceiptVersion,
		readinessFingerprint:    markerFingerprint(marker),
		cqBuild:                 marker.CQBuild,
		clientBuild:             marker.ClientBuild,
		finalEnvelopeGeneration: evidence.envelopeGeneration,
		finalEnvelopeDigest:     evidence.envelopeDigest,
		finalCountersDigest:     evidence.countersDigest,
		stopRequestDigest:       mustDecodeCanaryTestDigest(evidence.state.Finalisation.StopRequestDigest),
		processBindingDigest:    mustDecodeCanaryTestDigest(evidence.state.Finalisation.ProcessBindingDigest),
		activationReceiptDigest: sha256.Sum256([]byte("test exact activation receipt")),
		scenarios:               scenarios,
		completedAt:             evidence.state.EndedAt,
		fromMode:                CodexRoutingEnforce,
		toMode:                  CodexRoutingOff,
		exactAuthorityRoutes:    1,
		unseenLegacyRoutes:      1,
		installedServiceDrained: true,
	}
	receipt.seal = receipt.expectedSeal()
	return receipt
}

func mustDecodeCanaryTestDigest(value string) [sha256.Size]byte {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		panic("invalid canary test digest")
	}
	var result [sha256.Size]byte
	copy(result[:], decoded)
	return result
}
