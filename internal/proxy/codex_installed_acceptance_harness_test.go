package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testCodexInstalledValidationTime = time.Date(2026, 8, 10, 3, 45, 0, 0, time.UTC)

func TestCodexInstalledListenerHarnessReturnsBoundTypedEvidence(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe}
	exercise := &testCodexInstalledHTTPExercise{run: func() { setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding)) }}

	evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: lease},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise:    exercise,
		audit:       testCodexInstalledHTTPAuditAuthority{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Source != CodexHTTPReadinessEvidenceInstalledListener || evidence.Tuple != readinessTuple(required) {
		t.Fatalf("evidence identity = %#v", evidence)
	}
	want := testCodexInstalledHTTPProbeResult(required, binding)
	want.Gates.Stage11CorpusTurns = codexStage11Reviewed.CaseCount
	if evidence.Gates != want.Gates || evidence.Acceptance != want.Acceptance {
		t.Fatalf("evidence payload = %#v", evidence)
	}
	if lease.revalidations != 1 || lease.releases != 1 {
		t.Fatalf("lease revalidations/releases = %d/%d", lease.revalidations, lease.releases)
	}
	if _, err := buildCodexHTTPReadinessMarker(evidence, required, testCodexInstalledValidationTime); err != nil {
		t.Fatalf("typed evidence did not build marker: %v", err)
	}
}

func TestCodexInstalledListenerHarnessRejectsCorpusCountersFromLiveProbe(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe}
	exercise := &testCodexInstalledHTTPExercise{run: func() {
		result := testCodexInstalledHTTPProbeResult(required, binding)
		result.Gates.Stage11CorpusTurns = 1_000
		setTestCodexInstalledProbeResult(probe, result)
	}}

	evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: lease},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise:    exercise,
		audit:       testCodexInstalledHTTPAuditAuthority{},
	})
	if err == nil || evidence != (CodexHTTPReadinessEvidence{}) {
		t.Fatalf("live probe minted corpus evidence: %#v, %v", evidence, err)
	}
}

func TestCodexInstalledListenerHarnessRejectsUnboundCorpusManifest(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	for _, mutate := range []func(*codexStage11CorpusBuildManifest){
		func(manifest *codexStage11CorpusBuildManifest) { manifest.cqBuild = "other-build" },
		func(manifest *codexStage11CorpusBuildManifest) { manifest.transcriptSHA256 = strings.Repeat("0", 64) },
		func(manifest *codexStage11CorpusBuildManifest) { manifest.caseCount = 999 },
		func(manifest *codexStage11CorpusBuildManifest) { manifest.categorySchemaSHA256 = [sha256.Size]byte{} },
		func(manifest *codexStage11CorpusBuildManifest) {
			manifest.categorySchemaSHA256 = sha256.Sum256([]byte("other-schema"))
		},
	} {
		manifest := currentCodexStage11CorpusBuildManifest(required.CQBuild)
		mutate(&manifest)
		if manifest.valid(required) {
			t.Fatalf("unbound manifest remained valid: %#v", manifest)
		}
	}
}

func TestCodexInstalledListenerHarnessBindsReviewedStage11BuildProvenance(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	manifest := currentCodexStage11CorpusBuildManifest(required.CQBuild)
	wantSchema := "stage11-category-schema-v2\n" +
		"simple|http_enforce|durable_v2\n" +
		"tool_loop|ws_observe|live_shadow|zero_ws_journal\n" +
		"succession|http_enforce|durable_v2\n" +
		"parallel|http_enforce|durable_v2\n" +
		"subagents|http_enforce|durable_v2\n" +
		"prewarm|ws_observe_plus_http_enforce|live_prewarm_zero_ws_journal_plus_durable_v2\n" +
		"compaction|http_enforce|durable_v2\n" +
		"reconnect|ws_observe|live_shadow|zero_ws_journal\n" +
		"cross_protocol_observe_consistent|legacy_http_observe_plus_ws_observe|same_actual_account|zero_continuity_errors|zero_ws_journal\n" +
		"delayed_stale|http_enforce|durable_v2\n" +
		"malformed_metadata|http_enforce|durable_v2\n" +
		"capability|websocket_observe_only|ws_routing_enforcement_unavailable\n"
	wantSchemaSHA256 := sha256.Sum256([]byte(wantSchema))

	if manifest.fixtureRevision != "stage11-corpus-transcript-v2\n" ||
		manifest.transcriptSHA256 != "f457c633d18fb199a3fd6fa25209b3e7cebebcacaaa3569f8ef34501492dbf75" ||
		manifest.smokeSHA256 != "d75adc9740ff14bc46949a129b002b4e7b02cc5162aa3c1fb4f349b96fbdde51" ||
		manifest.categorySchemaSHA256 != wantSchemaSHA256 ||
		required.FixtureHash != CodexHTTPFixtureHash || required.FixtureHash == manifest.transcriptSHA256 || !manifest.valid(required) {
		t.Fatalf("Stage11 build provenance is not the reviewed v2 transcript: %#v", manifest)
	}
}

func TestCodexInstalledListenerHarnessRequiresExternalStage11BuildProvenance(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	for _, proof := range []string{"", strings.Repeat("0", sha256.Size*2)} {
		if manifest, err := loadCodexStage11CorpusBuildManifest(required.CQBuild, proof); err == nil || manifest.seal != nil {
			t.Fatalf("untrusted build proof loaded manifest=%#v error=%v", manifest, err)
		}
	}
	const reviewedProof = "5c8f27d613bdefd0a79816d56069ae8561da205d699febc2d248afc81b710a20"
	manifest, err := loadCodexStage11CorpusBuildManifest(required.CQBuild, reviewedProof)
	if err != nil || !manifest.valid(required) {
		t.Fatalf("external reviewed proof manifest=%#v error=%v", manifest, err)
	}
	if manifest, err := loadCodexStage11CorpusBuildManifest("other-build", reviewedProof); err == nil || manifest.seal != nil {
		t.Fatalf("cross-build proof loaded manifest=%#v error=%v", manifest, err)
	}
}

func TestCodexStage11BuildProvenanceMatchesReviewedReleaseTuple(t *testing.T) {
	const build = "0.21.3"
	const reviewedProof = "bb44beba8777c53101f880e4d5c039cc976cf43d4f99cbd129850cddfc224969"
	manifest, err := loadCodexStage11CorpusBuildManifest(build, reviewedProof)
	if err != nil || !manifest.valid(func() CodexTransportRequirements {
		required, _ := DefaultCodexRoutingRequirements(build, "0.146.0")
		return required
	}()) {
		t.Fatalf("reviewed release proof was rejected: %v", err)
	}
}

func TestCodexInstalledListenerHarnessRejectsUnsafeIndependentAudit(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	for _, audit := range []testCodexInstalledHTTPAuditAuthority{
		{rawIdentifierLeaks: 1},
		{automaticAuthWrites: 1},
		{egressAttempts: 1},
		{unexpectedRoutes: 1},
		{missingModelRequests: true},
		{wrongPong: true},
	} {
		probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
		evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
			authority:   testCodexInstalledListenerAuthority{lease: &testCodexInstalledListenerLease{binding: binding, probe: probe}},
			clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
			exercise: &testCodexInstalledHTTPExercise{run: func() {
				setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
			}},
			audit: audit,
		})
		if err == nil || evidence != (CodexHTTPReadinessEvidence{}) {
			t.Fatalf("unsafe audit minted evidence: %#v, %v", evidence, err)
		}
	}
}

func TestCodexInstalledListenerHarnessUsesExactSealedAuditModelRequestCount(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	exercise := &testCodexInstalledHTTPExercise{run: func() {
		result := testCodexInstalledHTTPProbeResult(required, binding)
		result.Acceptance.InstalledModelRequests = 0
		setTestCodexInstalledProbeResult(probe, result)
	}}
	evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: &testCodexInstalledListenerLease{binding: binding, probe: probe}},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise:    exercise,
		audit:       testCodexInstalledHTTPAuditAuthority{modelRequests: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Acceptance.InstalledModelRequests != 2 {
		t.Fatalf("installed model-catalogue requests = %d, want sealed audit count 2", evidence.Acceptance.InstalledModelRequests)
	}
}

func TestCodexInstalledListenerHarnessSealsProcessBindingAndEvidence(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	harness := newTestCodexInstalledListenerHarness(required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: &testCodexInstalledListenerLease{binding: binding, probe: probe}},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		audit:       testCodexInstalledHTTPAuditAuthority{},
		quiesce:     &testCodexInstalledHTTPQuiescer{},
		corpus:      currentCodexStage11CorpusBuildManifest(required.CQBuild),
		guard:       &testCodexInstalledHTTPValidationGuard{},
		exercise: &testCodexInstalledHTTPExercise{run: func() {
			setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
		}},
	})
	proof, err := harness.Run(context.Background(), required)
	if err != nil || proof.validate(required) != nil {
		t.Fatalf("sealed proof = %#v, %v", proof, err)
	}
	if proof.processBinding() != binding || proof.canaryProcessBindingDigest() == ([sha256.Size]byte{}) {
		t.Fatal("sealed proof lost its exact process binding")
	}
	tamperedBinding := proof
	tamperedBinding.binding.PID++
	if tamperedBinding.processBinding() != (codexInstalledListenerProcessBinding{}) ||
		tamperedBinding.canaryProcessBindingDigest() != ([sha256.Size]byte{}) {
		t.Fatal("tampered process binding remained consumable through sealed accessors")
	}
	proof.evidence.Gates.WarmAffinityCases = 0
	if proof.validate(required) == nil || proof.readinessEvidence() != (CodexHTTPReadinessEvidence{}) {
		t.Fatal("tampered sealed proof remained valid or lost fail-closed payload semantics")
	}
}

func TestCodexInstalledListenerHarnessCommitsMarkerWhileProcessLeaseIsHeld(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe}
	harness := newTestCodexInstalledListenerHarness(required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: lease},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		audit:       testCodexInstalledHTTPAuditAuthority{},
		quiesce:     &testCodexInstalledHTTPQuiescer{},
		corpus:      currentCodexStage11CorpusBuildManifest(required.CQBuild),
		guard:       &testCodexInstalledHTTPValidationGuard{},
		exercise: &testCodexInstalledHTTPExercise{run: func() {
			setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
		}},
	})
	commits := 0
	proof, err := harness.RunAndCommit(context.Background(), required, func(marker CodexReadinessMarker) error {
		commits++
		lease.mu.Lock()
		released := lease.releases
		revalidated := lease.revalidations
		lease.mu.Unlock()
		if released != 0 || revalidated != 1 {
			t.Fatalf("commit observed release/revalidation = %d/%d", released, revalidated)
		}
		return ValidateCodexReadinessMarker(marker, required)
	})
	if err != nil || proof.validate(required) != nil || commits != 1 || lease.releases != 1 {
		t.Fatalf("commit result proof=%#v error=%v commits=%d releases=%d", proof, err, commits, lease.releases)
	}

	probe = newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	lease = &testCodexInstalledListenerLease{binding: binding, probe: probe}
	harness.dependencies.authority = testCodexInstalledListenerAuthority{lease: lease}
	harness.dependencies.exercise = &testCodexInstalledRuntimeExercise{
		delegate: &testCodexInstalledHTTPExercise{run: func() {
			setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
		}},
		runtime:    harness.dependencies.runtime,
		admissions: harness.dependencies.admissions.(*testCodexInstalledNativeHTTPAdmissionAuthority),
	}
	private := errors.New("private marker writer failure")
	if failed, err := harness.RunAndCommit(context.Background(), required, func(CodexReadinessMarker) error { return private }); err == nil || failed.seal != nil {
		t.Fatalf("failed commit returned proof=%#v error=%v", failed, err)
	}
	if lease.releases != 1 {
		t.Fatalf("failed commit releases = %d, want 1", lease.releases)
	}
}

func TestCodexInstalledListenerHarnessClosesAdmissionBeforeFinalProofAndCommit(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	quiescer := &testCodexInstalledHTTPQuiescer{}
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe, requireQuiesced: quiescer.Closed}
	harness := newTestCodexInstalledListenerHarness(required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: lease},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise: &testCodexInstalledHTTPExercise{run: func() {
			setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
		}},
		audit:   testCodexInstalledHTTPAuditAuthority{},
		quiesce: quiescer,
		corpus:  currentCodexStage11CorpusBuildManifest(required.CQBuild),
		guard:   &testCodexInstalledHTTPValidationGuard{},
	})
	commits := 0
	if _, err := harness.RunAndCommit(context.Background(), required, func(CodexReadinessMarker) error {
		commits++
		if !quiescer.Closed() {
			t.Fatal("marker commit preceded native admission close")
		}
		return nil
	}); err != nil {
		t.Fatalf("run and commit: %v", err)
	}
	if commits != 1 || quiescer.Calls() != 1 {
		t.Fatalf("commits/quiesce calls = %d/%d, want 1/1", commits, quiescer.Calls())
	}
}

func TestCodexInstalledListenerHarnessRejectsCancellationAfterExerciseBeforeCommit(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	guard := &testCodexInstalledHTTPValidationGuard{failAt: 2}
	harness := newTestCodexInstalledListenerHarness(required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: &testCodexInstalledListenerLease{binding: binding, probe: probe}},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise: &testCodexInstalledHTTPExercise{run: func() {
			setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
		}},
		audit:   testCodexInstalledHTTPAuditAuthority{},
		quiesce: &testCodexInstalledHTTPQuiescer{},
		corpus:  currentCodexStage11CorpusBuildManifest(required.CQBuild),
		guard:   guard,
	})
	commits := 0
	if proof, err := harness.RunAndCommit(context.Background(), required, func(CodexReadinessMarker) error {
		commits++
		return nil
	}); err == nil || proof.seal != nil {
		t.Fatalf("cancelled intent minted proof=%#v error=%v", proof, err)
	}
	if guard.calls != 2 || commits != 0 {
		t.Fatalf("guard calls/commits = %d/%d, want 2/0", guard.calls, commits)
	}
}

func TestCodexInstalledListenerHarnessBoundsDripBodyDrainBeforeCommit(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	quiescer := &testCodexInstalledHTTPQuiescer{blockUntilDone: true}
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe}
	harness := newTestCodexInstalledListenerHarness(required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: lease},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise: &testCodexInstalledHTTPExercise{run: func() {
			setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
		}},
		audit:          testCodexInstalledHTTPAuditAuthority{},
		quiesce:        quiescer,
		quiesceTimeout: 20 * time.Millisecond,
		corpus:         currentCodexStage11CorpusBuildManifest(required.CQBuild),
		guard:          &testCodexInstalledHTTPValidationGuard{},
	})
	commits := 0
	started := time.Now()
	if proof, err := harness.RunAndCommit(context.Background(), required, func(CodexReadinessMarker) error {
		commits++
		return nil
	}); err == nil || proof.seal != nil {
		t.Fatalf("blocked drain minted proof=%#v error=%v", proof, err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("blocked drain returned after %v, want bounded failure", elapsed)
	}
	if !quiescer.SawDeadline() || commits != 0 {
		t.Fatalf("blocked drain deadline/commits = %t/%d, want true/0", quiescer.SawDeadline(), commits)
	}
}

func TestCodexInstalledListenerHarnessRejectsUnboundOrSyntheticProof(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	baseBinding := testCodexInstalledListenerBinding(required.CQBuild)
	baseResult := testCodexInstalledHTTPProbeResult(required, baseBinding)

	tests := []struct {
		name   string
		mutate func(*codexInstalledListenerProcessBinding, *codexInstalledHTTPProbeResult)
	}{
		{name: "ephemeral process", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.ServiceKind = codexInstalledListenerServiceEphemeral
		}},
		{name: "not persistent", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.Persistent = false
		}},
		{name: "missing pid", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) { binding.PID = 0 }},
		{name: "missing executable digest", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.ExecutableSHA256 = [sha256.Size]byte{}
		}},
		{name: "missing client executable digest", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.ClientExecutableSHA256 = [sha256.Size]byte{}
		}},
		{name: "missing service digest", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.ServiceIdentitySHA256 = [sha256.Size]byte{}
		}},
		{name: "missing listener binding", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.ListenerBinding = [sha256.Size]byte{}
		}},
		{name: "wrong cq build", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.CQBuild = "other-build"
		}},
		{name: "wrong bound client build", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.ClientBuild = "codex-cli other"
		}},
		{name: "listener mismatch", mutate: func(binding *codexInstalledListenerProcessBinding, _ *codexInstalledHTTPProbeResult) {
			binding.ListenerBinding = sha256.Sum256([]byte("other-listener"))
		}},
		{name: "no production responses", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.NativeResponsesRequests = 0
		}},
		{name: "no production compact", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.NativeCompactRequests = 0
		}},
		{name: "handler count mismatch", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.ProductionHandlerRequests--
		}},
		{name: "extra production request", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.ProductionHandlerRequests++
		}},
		{name: "extra response path", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.NativeResponsesRequests++
		}},
		{name: "turn count mismatch", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.StrongTurns--
		}},
		{name: "incomplete semantic gate", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Gates.WarmAffinityCases = 0
		}},
		{name: "extra semantic gate", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Gates.WarmAffinityCases++
		}},
		{name: "unsafe event", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Gates.AutomaticAuthWrites = 1
		}},
		{name: "no affinity diagnostics", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Diagnostics.AffinityReuseSelections = 0
		}},
		{name: "no fairness diagnostics", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Diagnostics.FairnessSelections = 0
		}},
		{name: "extra fairness diagnostics", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Diagnostics.FairnessSelections++
		}},
		{name: "no terminal default diagnostics", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Diagnostics.TerminalDefaultAttempts = 0
		}},
		{name: "no replay byte diagnostics", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Diagnostics.ReplayEnvelopePeakBytes = 0
		}},
		{name: "retained replay bytes", mutate: func(_ *codexInstalledListenerProcessBinding, result *codexInstalledHTTPProbeResult) {
			result.Diagnostics.ReplayEnvelopeCurrentBytes = 1
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := baseBinding
			result := baseResult
			test.mutate(&binding, &result)
			probe := newTestCodexInstalledHTTPGateProbe(t, baseBinding.ListenerBinding)
			lease := &testCodexInstalledListenerLease{binding: binding, probe: probe}
			exercise := &testCodexInstalledHTTPExercise{run: func() { setTestCodexInstalledProbeResult(probe, result) }}
			evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
				authority:   testCodexInstalledListenerAuthority{lease: lease},
				clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
				exercise:    exercise,
				audit:       testCodexInstalledHTTPAuditAuthority{},
			})
			if err == nil {
				t.Fatal("expected installed-listener proof rejection")
			}
			if evidence != (CodexHTTPReadinessEvidence{}) {
				t.Fatalf("rejected proof leaked partial evidence: %#v", evidence)
			}
			if lease.releases != 1 {
				t.Fatalf("lease releases = %d, want 1", lease.releases)
			}
		})
	}
}

func TestCodexInstalledListenerHarnessRejectsExtraModelRequest(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe}
	exercise := &testCodexInstalledHTTPExercise{run: func() {
		setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding))
	}}
	evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: lease},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise:    exercise,
		audit:       testCodexInstalledHTTPAuditAuthority{modelRequests: 3},
	})
	if err == nil || evidence != (CodexHTTPReadinessEvidence{}) {
		t.Fatalf("extra model request evidence = %#v, %v", evidence, err)
	}
}

func TestCodexInstalledRuntimeObservabilityRequiresExactProductionDelta(t *testing.T) {
	diagnostics := codexInstalledHTTPAggregateDiagnostics{ReplayEnvelopePeakBytes: 4096}
	before := codexRuntimeObservabilitySnapshot{PeakReplayBytes: 1024}
	after := codexRuntimeObservabilitySnapshot{
		AffinityReuse: 1, FairnessSelect: 21, TerminalDefault: 1, PeakReplayBytes: 4096,
	}
	if !validCodexInstalledRuntimeObservability(before, after, diagnostics) {
		t.Fatal("exact production runtime delta rejected")
	}
	for _, mutate := range []func(*codexRuntimeObservabilitySnapshot){
		func(value *codexRuntimeObservabilitySnapshot) { value.AffinityReuse++ },
		func(value *codexRuntimeObservabilitySnapshot) { value.FairnessSelect++ },
		func(value *codexRuntimeObservabilitySnapshot) { value.TerminalDefault++ },
		func(value *codexRuntimeObservabilitySnapshot) { value.CurrentReplayBytes = 1 },
		func(value *codexRuntimeObservabilitySnapshot) { value.PeakReplayBytes++ },
	} {
		mutated := after
		mutate(&mutated)
		if validCodexInstalledRuntimeObservability(before, mutated, diagnostics) {
			t.Fatalf("non-exact runtime delta accepted: %#v", mutated)
		}
	}
}

func TestCodexInstalledNativeHTTPAdmissionsRequireExactUnblockedDelta(t *testing.T) {
	before := codexInstalledNativeHTTPAdmissionSnapshot{FirstAuthoritative: 4}
	after := codexInstalledNativeHTTPAdmissionSnapshot{FirstAuthoritative: 24}
	if !validCodexInstalledNativeHTTPAdmissions(before, after) {
		t.Fatal("exact first-authoritative admission delta rejected")
	}
	for _, mutated := range []codexInstalledNativeHTTPAdmissionSnapshot{
		{FirstAuthoritative: 23},
		{FirstAuthoritative: 25},
		{FirstAuthoritative: 3},
		{FirstAuthoritative: 24, PromotionBlocked: true},
	} {
		if validCodexInstalledNativeHTTPAdmissions(before, mutated) {
			t.Fatalf("non-exact or blocked admission delta accepted: %#v", mutated)
		}
	}
	blockedBefore := before
	blockedBefore.PromotionBlocked = true
	if validCodexInstalledNativeHTTPAdmissions(blockedBefore, after) {
		t.Fatal("pre-existing promotion block accepted")
	}
}

func TestCodexInstalledListenerHarnessRejectsMissingRuntimeObservability(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	harness := &codexInstalledListenerHarness{dependencies: codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise:    &testCodexInstalledHTTPExercise{},
		audit:       testCodexInstalledHTTPAuditAuthority{},
		quiesce:     &testCodexInstalledHTTPQuiescer{},
		corpus:      currentCodexStage11CorpusBuildManifest(required.CQBuild),
	}}
	if proof, err := harness.Run(context.Background(), required); err == nil || proof != (codexInstalledHTTPSealedProof{}) {
		t.Fatalf("missing runtime observability minted proof = %#v, %v", proof, err)
	}
}

func TestCodexInstalledListenerHarnessProbesExactClientBuild(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe}
	exercise := &testCodexInstalledHTTPExercise{run: func() { setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding)) }}
	client := &testCodexInstalledClientBuildProbe{build: "codex-cli other"}

	evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
		authority: testCodexInstalledListenerAuthority{lease: lease}, clientBuild: client, exercise: exercise,
		audit: testCodexInstalledHTTPAuditAuthority{},
	})
	if err == nil {
		t.Fatal("expected exact client build rejection")
	}
	if evidence != (CodexHTTPReadinessEvidence{}) || exercise.calls != 0 || client.calls != 1 || lease.releases != 1 {
		t.Fatalf("rejected client evidence/calls = %#v/%d/%d/%d", evidence, exercise.calls, client.calls, lease.releases)
	}
}

func TestCodexInstalledListenerHarnessRevalidatesSameProcessAfterExercise(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	private := "private-listener-path-and-pid"
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	lease := &testCodexInstalledListenerLease{binding: binding, probe: probe, revalidateErr: errors.New(private)}
	exercise := &testCodexInstalledHTTPExercise{run: func() { setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding)) }}

	evidence, err := runTestCodexInstalledListenerHTTPAcceptance(context.Background(), required, codexInstalledListenerHarnessDependencies{
		authority:   testCodexInstalledListenerAuthority{lease: lease},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise:    exercise,
		audit:       testCodexInstalledHTTPAuditAuthority{},
		quiesce:     &testCodexInstalledHTTPQuiescer{},
	})
	if err == nil || strings.Contains(err.Error(), private) {
		t.Fatalf("private authority error leaked or missing: %v", err)
	}
	if evidence != (CodexHTTPReadinessEvidence{}) || lease.revalidations != 1 || lease.releases != 1 {
		t.Fatalf("rejected process evidence = %#v, revalidate/release=%d/%d", evidence, lease.revalidations, lease.releases)
	}
}

func TestCodexInstalledListenerHarnessSerialisesRunsAndHonoursCancellation(t *testing.T) {
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	probe := newTestCodexInstalledHTTPGateProbe(t, binding.ListenerBinding)
	entered := make(chan struct{})
	release := make(chan struct{})
	exercise := &testCodexInstalledHTTPExercise{
		run:     func() { setTestCodexInstalledProbeResult(probe, testCodexInstalledHTTPProbeResult(required, binding)) },
		entered: entered,
		release: release,
	}
	harness := newTestCodexInstalledListenerHarness(required, codexInstalledListenerHarnessDependencies{
		authority: testCodexInstalledListenerAuthority{leaseFactory: func() *testCodexInstalledListenerLease {
			return &testCodexInstalledListenerLease{binding: binding, probe: probe}
		}},
		clientBuild: &testCodexInstalledClientBuildProbe{build: required.ClientBuild},
		exercise:    exercise,
		audit:       testCodexInstalledHTTPAuditAuthority{},
		quiesce:     &testCodexInstalledHTTPQuiescer{},
		corpus:      currentCodexStage11CorpusBuildManifest(required.CQBuild),
	})

	firstDone := make(chan error, 1)
	go func() {
		_, err := harness.Run(context.Background(), required)
		firstDone <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if proof, err := harness.Run(ctx, required); !errors.Is(err, context.Canceled) || proof.seal != nil {
		t.Fatalf("concurrent cancelled run = %#v, %v", proof, err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

type testCodexInstalledListenerAuthority struct {
	lease        *testCodexInstalledListenerLease
	leaseFactory func() *testCodexInstalledListenerLease
}

type testCodexInstalledHTTPValidationGuard struct {
	calls  int
	failAt int
}

func (guard *testCodexInstalledHTTPValidationGuard) Acquire() (func(), error) {
	guard.calls++
	if guard.calls == guard.failAt {
		return nil, errors.New("request cancelled")
	}
	return func() {}, nil
}

func (authority testCodexInstalledListenerAuthority) Acquire(context.Context, CodexReadinessTuple) (codexInstalledListenerProcessLease, error) {
	if authority.leaseFactory != nil {
		return authority.leaseFactory(), nil
	}
	return authority.lease, nil
}

type testCodexInstalledListenerLease struct {
	mu              sync.Mutex
	binding         codexInstalledListenerProcessBinding
	probe           *codexInstalledHTTPGateProbe
	requireQuiesced func() bool
	revalidateErr   error
	revalidations   int
	releases        int
	snapshots       int
}

func (lease *testCodexInstalledListenerLease) Binding() codexInstalledListenerProcessBinding {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.binding
}

func (lease *testCodexInstalledListenerLease) Snapshot(ctx context.Context) (codexInstalledHTTPProbeSnapshot, error) {
	lease.mu.Lock()
	binding := lease.binding.ListenerBinding
	probe := lease.probe
	lease.snapshots++
	snapshots := lease.snapshots
	requireQuiesced := lease.requireQuiesced
	lease.mu.Unlock()
	if snapshots > 1 && requireQuiesced != nil && !requireQuiesced() {
		return codexInstalledHTTPProbeSnapshot{}, errors.New("native admission remained open before final snapshot")
	}
	return probe.snapshot(ctx, binding)
}

func (lease *testCodexInstalledListenerLease) Revalidate(context.Context) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.revalidations++
	return lease.revalidateErr
}

func (lease *testCodexInstalledListenerLease) Release() {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.releases++
}

type testCodexInstalledClientBuildProbe struct {
	build string
	err   error
	calls int
}

func (probe *testCodexInstalledClientBuildProbe) Probe(context.Context) (string, error) {
	probe.calls++
	return probe.build, probe.err
}

type testCodexInstalledHTTPExercise struct {
	err     error
	calls   int
	run     func()
	entered chan struct{}
	release chan struct{}
}

type testCodexInstalledHTTPAuditAuthority struct {
	rawIdentifierLeaks   uint64
	automaticAuthWrites  uint64
	egressAttempts       uint64
	modelRequests        uint64
	missingModelRequests bool
	unexpectedRoutes     uint64
	wrongPong            bool
}

func (authority testCodexInstalledHTTPAuditAuthority) Begin(
	_ context.Context,
	tuple CodexReadinessTuple,
	binding codexInstalledListenerProcessBinding,
) (codexInstalledHTTPAuditLease, error) {
	modelRequests := authority.modelRequests
	if modelRequests == 0 && !authority.missingModelRequests {
		modelRequests = 2
	}
	proof := codexInstalledHTTPSealedAuditProof{
		tuple: tuple, binding: binding,
		rawIdentifierLeaks: authority.rawIdentifierLeaks, automaticAuthWrites: authority.automaticAuthWrites,
		egressAttempts: authority.egressAttempts, modelRequests: modelRequests,
		unexpectedRoutes: authority.unexpectedRoutes, exactClientPong: !authority.wrongPong,
	}
	proof.seal = &codexInstalledHTTPAuditProofSeal{
		tuple: tuple, binding: binding,
		rawIdentifierLeaks: authority.rawIdentifierLeaks, automaticAuthWrites: authority.automaticAuthWrites,
		egressAttempts: authority.egressAttempts, modelRequests: modelRequests,
		unexpectedRoutes: authority.unexpectedRoutes, exactClientPong: !authority.wrongPong,
	}
	return &testCodexInstalledHTTPAuditLease{proof: proof}, nil
}

type testCodexInstalledHTTPAuditLease struct {
	proof codexInstalledHTTPSealedAuditProof
}

func (lease *testCodexInstalledHTTPAuditLease) Complete(context.Context) (codexInstalledHTTPSealedAuditProof, error) {
	return lease.proof, nil
}

func (*testCodexInstalledHTTPAuditLease) Release() {}

func (exercise *testCodexInstalledHTTPExercise) Run(ctx context.Context) error {
	exercise.calls++
	if exercise.entered != nil {
		close(exercise.entered)
	}
	if exercise.release != nil {
		select {
		case <-exercise.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if exercise.run != nil {
		exercise.run()
	}
	return exercise.err
}

type testCodexInstalledHTTPQuiescer struct {
	mu             sync.Mutex
	calls          int
	closed         bool
	blockUntilDone bool
	sawDeadline    bool
}

func (quiescer *testCodexInstalledHTTPQuiescer) CloseAndDrain(ctx context.Context) error {
	quiescer.mu.Lock()
	quiescer.calls++
	quiescer.closed = true
	_, quiescer.sawDeadline = ctx.Deadline()
	blockUntilDone := quiescer.blockUntilDone
	quiescer.mu.Unlock()
	if blockUntilDone {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (quiescer *testCodexInstalledHTTPQuiescer) Closed() bool {
	quiescer.mu.Lock()
	defer quiescer.mu.Unlock()
	return quiescer.closed
}

func (quiescer *testCodexInstalledHTTPQuiescer) Calls() int {
	quiescer.mu.Lock()
	defer quiescer.mu.Unlock()
	return quiescer.calls
}

func (quiescer *testCodexInstalledHTTPQuiescer) SawDeadline() bool {
	quiescer.mu.Lock()
	defer quiescer.mu.Unlock()
	return quiescer.sawDeadline
}

func testCodexInstalledListenerRequirements() CodexTransportRequirements {
	required, _ := DefaultCodexRoutingRequirements("cq-installed-test", "codex-cli installed-test")
	required, err := bindCodexInstalledArtifacts(required, testCodexInstalledListenerBinding(required.CQBuild))
	if err != nil {
		panic(err)
	}
	return required
}

func currentCodexStage11CorpusBuildManifest(cqBuild string) codexStage11CorpusBuildManifest {
	manifest := codexStage11CorpusBuildManifest{
		cqBuild:              cqBuild,
		fixtureRevision:      codexStage11Reviewed.Revision,
		transcriptSHA256:     codexStage11Reviewed.TranscriptSHA256,
		smokeSHA256:          codexStage11Reviewed.SmokeSHA256,
		caseCount:            codexStage11Reviewed.CaseCount,
		categorySchemaSHA256: sha256.Sum256([]byte(codexStage11Reviewed.CategorySchema)),
	}
	manifest.seal = &codexStage11CorpusBuildManifestSeal{
		cqBuild:              manifest.cqBuild,
		fixtureRevision:      manifest.fixtureRevision,
		transcriptSHA256:     manifest.transcriptSHA256,
		smokeSHA256:          manifest.smokeSHA256,
		caseCount:            manifest.caseCount,
		categorySchemaSHA256: manifest.categorySchemaSHA256,
	}
	return manifest
}

func testCodexInstalledListenerBinding(cqBuild string) codexInstalledListenerProcessBinding {
	return codexInstalledListenerProcessBinding{
		CQBuild:                cqBuild,
		ClientBuild:            "codex-cli installed-test",
		PID:                    4242,
		ServiceKind:            codexInstalledListenerServiceLaunchd,
		Persistent:             true,
		ExecutableSHA256:       sha256.Sum256([]byte("installed-cq-executable")),
		ClientExecutableSHA256: sha256.Sum256([]byte("installed-codex-executable")),
		ServiceIdentitySHA256:  sha256.Sum256([]byte("launchd-service-identity")),
		ListenerBinding:        sha256.Sum256([]byte("attested-listener-generation")),
	}
}

func testCodexInstalledHTTPProbeResult(required CodexTransportRequirements, binding codexInstalledListenerProcessBinding) codexInstalledHTTPProbeResult {
	return codexInstalledHTTPProbeResult{
		ListenerBinding:           binding.ListenerBinding,
		ProductionHandlerRequests: 41,
		NativeResponsesRequests:   39,
		NativeCompactRequests:     2,
		StrongTurns:               20,
		Diagnostics: codexInstalledHTTPAggregateDiagnostics{
			AffinityReuseSelections: 1,
			FairnessSelections:      19,
			TerminalDefaultAttempts: 1,
			ReplayEnvelopePeakBytes: 4096,
		},
		Gates: CodexHTTPReadinessGateEvidence{
			InstalledTurns:                      20,
			FrozenSingleTransformEnvelopeCases:  2,
			WarmAffinityCases:                   1,
			DeterministicFallbackCases:          1,
			TerminalDefaultOnceCases:            1,
			ExactPreAdmissionHard429ReplayCases: 2,
			AdmittedNoMigrationCases:            1,
			V2JournalRuntimeCases:               39,
		},
		Acceptance: CodexHTTPAcceptanceResult{
			Turns:                    20,
			Requests:                 41,
			SelectorCalls:            20,
			InstalledVersion:         required.ClientBuild,
			InstalledRequests:        41,
			InstalledModelRequests:   2,
			InstalledAttempts:        43,
			InstalledSelectorCalls:   20,
			InstalledStrongKeys:      41,
			InstalledZstdRequests:    41,
			InstalledQuiescentLeases: 39,
			HeadroomRequests:         41,
			InstalledResolutions:     43,
			PongVerified:             true,
		},
	}
}

func newTestCodexInstalledHTTPGateProbe(t *testing.T, binding [sha256.Size]byte) *codexInstalledHTTPGateProbe {
	t.Helper()
	probe, err := newCodexInstalledHTTPGateProbe(binding)
	if err != nil {
		t.Fatal(err)
	}
	return probe
}

func runTestCodexInstalledListenerHTTPAcceptance(
	ctx context.Context,
	required CodexTransportRequirements,
	dependencies codexInstalledListenerHarnessDependencies,
) (CodexHTTPReadinessEvidence, error) {
	proof, err := newTestCodexInstalledListenerHarness(required, dependencies).Run(ctx, required)
	if err != nil {
		return CodexHTTPReadinessEvidence{}, err
	}
	return proof.readinessEvidence(), nil
}

func newTestCodexInstalledListenerHarness(
	required CodexTransportRequirements,
	dependencies codexInstalledListenerHarnessDependencies,
) *codexInstalledListenerHarness {
	if dependencies.quiesce == nil {
		dependencies.quiesce = &testCodexInstalledHTTPQuiescer{}
	}
	if dependencies.corpus.seal == nil {
		dependencies.corpus = currentCodexStage11CorpusBuildManifest(required.CQBuild)
	}
	var runtime *codexRuntimeObservability
	if dependencies.runtime == nil {
		runtime = &codexRuntimeObservability{}
		dependencies.runtime = runtime
	}
	var admissions *testCodexInstalledNativeHTTPAdmissionAuthority
	if dependencies.admissions == nil {
		admissions = &testCodexInstalledNativeHTTPAdmissionAuthority{}
		dependencies.admissions = admissions
	}
	if runtime != nil || admissions != nil {
		dependencies.exercise = &testCodexInstalledRuntimeExercise{
			delegate:   dependencies.exercise,
			runtime:    runtime,
			admissions: admissions,
		}
	}
	return &codexInstalledListenerHarness{dependencies: dependencies}
}

type testCodexInstalledRuntimeExercise struct {
	delegate   codexInstalledHTTPExercise
	runtime    *codexRuntimeObservability
	admissions *testCodexInstalledNativeHTTPAdmissionAuthority
}

func (exercise *testCodexInstalledRuntimeExercise) Run(ctx context.Context) error {
	if exercise == nil || exercise.delegate == nil {
		return errCodexInstalledListenerAcceptance
	}
	if err := exercise.delegate.Run(ctx); err != nil {
		return err
	}
	if exercise.runtime != nil {
		exercise.runtime.mu.Lock()
		exercise.runtime.affinityReuse++
		exercise.runtime.fairnessSelect += 21
		exercise.runtime.terminalDefault++
		exercise.runtime.peakReplayBytes = max(exercise.runtime.peakReplayBytes, 4096)
		exercise.runtime.mu.Unlock()
	}
	if exercise.admissions != nil {
		exercise.admissions.firstAuthoritative.Add(20)
	}
	return nil
}

type testCodexInstalledNativeHTTPAdmissionAuthority struct {
	firstAuthoritative atomic.Uint64
	promotionBlocked   atomic.Bool
}

func (authority *testCodexInstalledNativeHTTPAdmissionAuthority) nativeHTTPAdmissionSnapshot() codexInstalledNativeHTTPAdmissionSnapshot {
	if authority == nil {
		return codexInstalledNativeHTTPAdmissionSnapshot{PromotionBlocked: true}
	}
	return codexInstalledNativeHTTPAdmissionSnapshot{
		FirstAuthoritative: authority.firstAuthoritative.Load(),
		PromotionBlocked:   authority.promotionBlocked.Load(),
	}
}

func setTestCodexInstalledProbeResult(probe *codexInstalledHTTPGateProbe, result codexInstalledHTTPProbeResult) {
	acceptance := result.Acceptance
	acceptance.InstalledVersion = ""
	acceptance.InstalledModelRequests = 0
	acceptance.UnexpectedRoutes = 0
	acceptance.PongVerified = false
	probe.mu.Lock()
	probe.generation++
	probe.health = codexInstalledHTTPProbeHealth{
		ProductionHandlerRequests: result.ProductionHandlerRequests,
		NativeResponsesRequests:   result.NativeResponsesRequests,
		NativeCompactRequests:     result.NativeCompactRequests,
		StrongTurns:               result.StrongTurns,
		Gates:                     result.Gates,
		Acceptance:                acceptance,
		Diagnostics:               result.Diagnostics,
	}
	probe.mu.Unlock()
}
