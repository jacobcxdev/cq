package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const codexCanaryRollbackReceiptVersion = 2

type codexCanaryScenarioName string

const (
	codexCanaryScenarioLongTurnQuotaDepletion          codexCanaryScenarioName = "long_turn_quota_depletion"
	codexCanaryScenarioParallelShortTurns              codexCanaryScenarioName = "parallel_short_turns"
	codexCanaryScenarioNextTurnAffinityReuse           codexCanaryScenarioName = "next_turn_affinity_reuse"
	codexCanaryScenarioNecessaryReselection            codexCanaryScenarioName = "necessary_reselection"
	codexCanaryScenarioSameLaneSupersession            codexCanaryScenarioName = "same_lane_supersession"
	codexCanaryScenarioTerminalRoutingDefault          codexCanaryScenarioName = "terminal_routing_default"
	codexCanaryScenarioLateSameTurn429NoAlternate      codexCanaryScenarioName = "late_same_turn_hard_429_no_alternate"
	codexCanaryScenarioQuiescentRestart                codexCanaryScenarioName = "quiescent_restart"
	codexCanaryScenarioCodexBarObserveNoSwitch         codexCanaryScenarioName = "codexbar_observe_no_switch"
	codexCanaryScenarioExplicitSwitchActivationReceipt codexCanaryScenarioName = "explicit_switch_activation_receipt"
)

var requiredCodexCanaryScenarios = [...]codexCanaryScenarioName{
	codexCanaryScenarioLongTurnQuotaDepletion,
	codexCanaryScenarioParallelShortTurns,
	codexCanaryScenarioNextTurnAffinityReuse,
	codexCanaryScenarioNecessaryReselection,
	codexCanaryScenarioSameLaneSupersession,
	codexCanaryScenarioTerminalRoutingDefault,
	codexCanaryScenarioLateSameTurn429NoAlternate,
	codexCanaryScenarioQuiescentRestart,
	codexCanaryScenarioCodexBarObserveNoSwitch,
	codexCanaryScenarioExplicitSwitchActivationReceipt,
}

type codexCanaryScenarioEvidence struct {
	name   codexCanaryScenarioName
	digest [sha256.Size]byte
}

// CodexCanaryRollbackReceipt is deliberately opaque. The installed serving
// process must drain native sessions and bind its sealed proof before this
// package can construct one. Code-only callers cannot mint a receipt.
type CodexCanaryRollbackReceipt struct {
	version                  int
	readinessFingerprint     string
	cqBuild                  string
	clientBuild              string
	finalEnvelopeGeneration  uint64
	finalEnvelopeDigest      [sha256.Size]byte
	finalCountersDigest      [sha256.Size]byte
	stopRequestDigest        [sha256.Size]byte
	processBindingDigest     [sha256.Size]byte
	activationReceiptDigest  [sha256.Size]byte
	scenarios                []codexCanaryScenarioEvidence
	completedAt              time.Time
	fromMode                 CodexRoutingMode
	toMode                   CodexRoutingMode
	activeSessionsAtMutation uint64
	exactAuthorityRoutes     uint64
	unseenLegacyRoutes       uint64
	shadowPromotions         uint64
	systemAuthMutations      uint64
	credentialMutations      uint64
	canaryWriteErrors        uint64
	unprovenMutations        uint64
	installedServiceDrained  bool
	seal                     [sha256.Size]byte
}

type codexCanaryFinalEvidence struct {
	state              CodexCanaryState
	envelopeGeneration uint64
	envelopeDigest     [sha256.Size]byte
	countersDigest     [sha256.Size]byte
}

// EvaluateCodexCanaryPromotion is read-only. It accepts only the exact stopped
// signed envelope held by a securely opened recorder, the current marker, and
// an opaque installed-service rollback receipt. There is intentionally no
// code-only receipt constructor.
func EvaluateCodexCanaryPromotion(now time.Time, required CodexTransportRequirements, marker CodexReadinessMarker, recorder *CodexCanaryRecorder, rollback CodexCanaryRollbackReceipt) error {
	evidence, err := codexCanaryFinalPromotionEvidence(recorder)
	if err != nil {
		return err
	}
	if err := validateCodexCanaryPromotionState(now, required, marker, evidence.state); err != nil {
		return err
	}
	if err := rollback.validate(marker, evidence); err != nil {
		return err
	}
	return nil
}

func codexCanaryFinalPromotionEvidence(recorder *CodexCanaryRecorder) (codexCanaryFinalEvidence, error) {
	if recorder == nil {
		return codexCanaryFinalEvidence{}, codexCanaryNotPromotable("stopped signed canary unavailable")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.fs == nil || recorder.path == "" || recorder.generation == 0 || len(recorder.key) != codexCanaryIntegrityKeyBytes {
		return codexCanaryFinalEvidence{}, codexCanaryNotPromotable("stopped signed canary unavailable")
	}
	state := recorder.state
	state.ProtectedDigests = append([]CodexCanaryProtectedDigest(nil), recorder.state.ProtectedDigests...)
	envelope := codexCanaryEnvelope{
		Version:    CodexCanaryVersion,
		Generation: recorder.generation,
		State:      state,
	}
	mac, err := codexCanaryEnvelopeMAC(recorder.key, envelope)
	if err != nil {
		return codexCanaryFinalEvidence{}, codexCanaryNotPromotable("final signed envelope invalid")
	}
	envelope.MAC = mac
	canonical, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return codexCanaryFinalEvidence{}, codexCanaryNotPromotable("final signed envelope invalid")
	}
	persisted, err := fsutil.ReadSecureFile(recorder.fs, recorder.path, codexCanaryStateMaxBytes)
	if err != nil || !bytes.Equal(persisted, canonical) {
		return codexCanaryFinalEvidence{}, codexCanaryNotPromotable("final signed envelope changed")
	}
	return codexCanaryFinalEvidence{
		state:              state,
		envelopeGeneration: recorder.generation,
		envelopeDigest:     sha256.Sum256(persisted),
		countersDigest:     codexCanaryCountersDigest(state),
	}, nil
}

func codexCanaryCountersDigest(state CodexCanaryState) [sha256.Size]byte {
	value := struct {
		Version                 int    `json:"version"`
		AdmittedTurns           uint64 `json:"admitted_turns"`
		KeyedMismatches         uint64 `json:"keyed_mismatches"`
		AutomaticHashChanges    uint64 `json:"automatic_protected_state_changes"`
		SecretLeaks             uint64 `json:"secret_leaks"`
		UnexplainedLifecycles   uint64 `json:"unexplained_lifecycles"`
		LiveSessionRepairs      uint64 `json:"live_session_repairs"`
		ProtectedStateFailures  uint64 `json:"protected_state_failures"`
		ConsecutiveCalendarDays int    `json:"consecutive_calendar_days"`
	}{
		Version:                 state.Version,
		AdmittedTurns:           state.AdmittedTurns,
		KeyedMismatches:         state.KeyedMismatches,
		AutomaticHashChanges:    state.AutomaticHashChanges,
		SecretLeaks:             state.SecretLeaks,
		UnexplainedLifecycles:   state.UnexplainedLifecycles,
		LiveSessionRepairs:      state.LiveSessionRepairs,
		ProtectedStateFailures:  state.ProtectedStateFailures,
		ConsecutiveCalendarDays: state.ConsecutiveCalendarDays,
	}
	encoded, _ := json.Marshal(value)
	return sha256.Sum256(append([]byte("cq-codex-canary-counters-v2\x00"), encoded...))
}

func validateCodexCanaryPromotionState(now time.Time, required CodexTransportRequirements, marker CodexReadinessMarker, state CodexCanaryState) error {
	if now.IsZero() || now.Location() == nil {
		return codexCanaryNotPromotable("validation time unavailable")
	}
	now = now.UTC()
	if err := ValidateCodexReadinessMarker(marker, required); err != nil {
		return codexCanaryNotPromotable("current readiness marker invalid")
	}
	tuple, err := BuildCodexCanaryTuple(required, marker)
	if err != nil || state.Tuple != tuple {
		return codexCanaryNotPromotable("current readiness tuple mismatch")
	}
	if state.Version != CodexCanaryVersion || !validCodexCanaryRandomID(state.RunID) || state.Active || state.StartedAt.IsZero() || state.EndedAt.IsZero() || state.LastObservedAt.IsZero() || !validCodexCanaryFinalisation(state) {
		return codexCanaryNotPromotable("canary lifecycle incomplete")
	}
	startedAt := state.StartedAt.UTC()
	endedAt := state.EndedAt.UTC()
	lastObservedAt := state.LastObservedAt.UTC()
	validatedAt := marker.ValidatedAt.UTC()
	if validatedAt.After(startedAt) || startedAt.After(lastObservedAt) || lastObservedAt.After(endedAt) || endedAt.After(now) {
		return codexCanaryNotPromotable("canary lifecycle timestamps incoherent")
	}
	if endedAt.Sub(startedAt) < 7*24*time.Hour {
		return codexCanaryNotPromotable("seven elapsed days incomplete")
	}
	calendarSpan := int(canaryCalendarDay(lastObservedAt).Sub(canaryCalendarDay(startedAt))/(24*time.Hour)) + 1
	if state.ConsecutiveCalendarDays < 7 || state.ConsecutiveCalendarDays > calendarSpan {
		return codexCanaryNotPromotable("service-observed day evidence incomplete")
	}
	if state.AdmittedTurns < 100 {
		return codexCanaryNotPromotable("installed admitted-turn threshold incomplete")
	}
	if state.KeyedMismatches != 0 || state.AutomaticHashChanges != 0 || state.SecretLeaks != 0 || state.UnexplainedLifecycles != 0 || state.LiveSessionRepairs != 0 || state.ProtectedStateFailures != 0 {
		return codexCanaryNotPromotable("failure counter is nonzero")
	}
	if err := validateCodexCanaryProtectedEvidence(state.ProtectedDigests); err != nil {
		return err
	}
	return nil
}

func validateCodexCanaryProtectedEvidence(protected []CodexCanaryProtectedDigest) error {
	if len(protected) != len(requiredCodexCanaryProtection) {
		return codexCanaryNotPromotable("incomplete protected evidence")
	}
	required := make(map[CodexCanaryProtectionKind]bool, len(requiredCodexCanaryProtection))
	for _, kind := range requiredCodexCanaryProtection {
		required[kind] = true
	}
	seen := make(map[CodexCanaryProtectionKind]bool, len(protected))
	for _, item := range protected {
		decoded, err := hex.DecodeString(item.Digest)
		if !required[item.Kind] || seen[item.Kind] || err != nil || len(decoded) != sha256.Size {
			return codexCanaryNotPromotable("invalid protected evidence")
		}
		seen[item.Kind] = true
	}
	return nil
}

func (receipt CodexCanaryRollbackReceipt) validate(marker CodexReadinessMarker, evidence codexCanaryFinalEvidence) error {
	if receipt.seal == ([sha256.Size]byte{}) || receipt.seal != receipt.expectedSeal() {
		return codexCanaryNotPromotable("installed rollback receipt unavailable")
	}
	return receipt.validateFields(marker, evidence)
}

func (receipt CodexCanaryRollbackReceipt) validateFields(marker CodexReadinessMarker, evidence codexCanaryFinalEvidence) error {
	state := evidence.state
	if receipt.version != codexCanaryRollbackReceiptVersion || receipt.readinessFingerprint != markerFingerprint(marker) || receipt.cqBuild != state.Tuple.CQBuild || receipt.clientBuild != state.Tuple.ClientBuild {
		return codexCanaryNotPromotable("installed rollback binding mismatch")
	}
	if receipt.finalEnvelopeGeneration == 0 || receipt.finalEnvelopeGeneration != evidence.envelopeGeneration || receipt.finalEnvelopeDigest == ([sha256.Size]byte{}) || receipt.finalEnvelopeDigest != evidence.envelopeDigest || receipt.finalCountersDigest == ([sha256.Size]byte{}) || receipt.finalCountersDigest != evidence.countersDigest {
		return codexCanaryNotPromotable("installed rollback final-state binding mismatch")
	}
	if receipt.stopRequestDigest == ([sha256.Size]byte{}) || receipt.processBindingDigest == ([sha256.Size]byte{}) {
		return codexCanaryNotPromotable("installed rollback authority binding mismatch")
	}
	finalisation := state.Finalisation
	stopRequestDigest, stopErr := hex.DecodeString(finalisation.StopRequestDigest)
	processBindingDigest, processErr := hex.DecodeString(finalisation.ProcessBindingDigest)
	if stopErr != nil || processErr != nil || !bytes.Equal(stopRequestDigest, receipt.stopRequestDigest[:]) || !bytes.Equal(processBindingDigest, receipt.processBindingDigest[:]) {
		return codexCanaryNotPromotable("installed rollback finalisation binding mismatch")
	}
	if receipt.activationReceiptDigest == ([sha256.Size]byte{}) {
		return codexCanaryNotPromotable("exact activation receipt unavailable")
	}
	if err := validateCodexCanaryScenarioEvidence(receipt.scenarios); err != nil {
		return err
	}
	if receipt.completedAt.IsZero() || !receipt.completedAt.UTC().Equal(state.EndedAt.UTC()) {
		return codexCanaryNotPromotable("installed rollback time invalid")
	}
	if receipt.fromMode != CodexRoutingEnforce || (receipt.toMode != CodexRoutingOff && receipt.toMode != CodexRoutingObserve) {
		return codexCanaryNotPromotable("installed rollback mode transition invalid")
	}
	if !receipt.installedServiceDrained || receipt.activeSessionsAtMutation != 0 || receipt.exactAuthorityRoutes == 0 || receipt.unseenLegacyRoutes == 0 {
		return codexCanaryNotPromotable("installed drained rollback evidence incomplete")
	}
	if receipt.shadowPromotions != 0 || receipt.systemAuthMutations != 0 || receipt.credentialMutations != 0 || receipt.canaryWriteErrors != 0 || receipt.unprovenMutations != 0 {
		return codexCanaryNotPromotable("installed rollback observed unsafe mutation")
	}
	return nil
}

func validateCodexCanaryScenarioEvidence(scenarios []codexCanaryScenarioEvidence) error {
	if len(scenarios) != len(requiredCodexCanaryScenarios) {
		return codexCanaryNotPromotable("installed scenario evidence incomplete")
	}
	for index, required := range requiredCodexCanaryScenarios {
		if scenarios[index].name != required || scenarios[index].digest == ([sha256.Size]byte{}) {
			return codexCanaryNotPromotable("installed scenario evidence invalid")
		}
	}
	return nil
}

func (receipt CodexCanaryRollbackReceipt) expectedSeal() [sha256.Size]byte {
	type canonicalScenario struct {
		Name   codexCanaryScenarioName `json:"name"`
		Digest [sha256.Size]byte       `json:"digest"`
	}
	scenarios := make([]canonicalScenario, len(receipt.scenarios))
	for index, scenario := range receipt.scenarios {
		scenarios[index] = canonicalScenario{Name: scenario.name, Digest: scenario.digest}
	}
	value := struct {
		Version                  int                 `json:"version"`
		ReadinessFingerprint     string              `json:"readiness_fingerprint"`
		CQBuild                  string              `json:"cq_build"`
		ClientBuild              string              `json:"client_build"`
		FinalEnvelopeGeneration  uint64              `json:"final_envelope_generation"`
		FinalEnvelopeDigest      [sha256.Size]byte   `json:"final_envelope_digest"`
		FinalCountersDigest      [sha256.Size]byte   `json:"final_counters_digest"`
		StopRequestDigest        [sha256.Size]byte   `json:"stop_request_digest"`
		ProcessBindingDigest     [sha256.Size]byte   `json:"process_binding_digest"`
		ActivationReceiptDigest  [sha256.Size]byte   `json:"activation_receipt_digest"`
		Scenarios                []canonicalScenario `json:"scenarios"`
		CompletedAt              time.Time           `json:"completed_at"`
		FromMode                 CodexRoutingMode    `json:"from_mode"`
		ToMode                   CodexRoutingMode    `json:"to_mode"`
		ActiveSessionsAtMutation uint64              `json:"active_sessions_at_mutation"`
		ExactAuthorityRoutes     uint64              `json:"exact_authority_routes"`
		UnseenLegacyRoutes       uint64              `json:"unseen_legacy_routes"`
		ShadowPromotions         uint64              `json:"shadow_promotions"`
		SystemAuthMutations      uint64              `json:"system_auth_mutations"`
		CredentialMutations      uint64              `json:"credential_mutations"`
		CanaryWriteErrors        uint64              `json:"canary_write_errors"`
		UnprovenMutations        uint64              `json:"unproven_mutations"`
		InstalledServiceDrained  bool                `json:"installed_service_drained"`
	}{
		Version: receipt.version, ReadinessFingerprint: receipt.readinessFingerprint,
		CQBuild: receipt.cqBuild, ClientBuild: receipt.clientBuild,
		FinalEnvelopeGeneration: receipt.finalEnvelopeGeneration,
		FinalEnvelopeDigest:     receipt.finalEnvelopeDigest, FinalCountersDigest: receipt.finalCountersDigest,
		StopRequestDigest: receipt.stopRequestDigest, ProcessBindingDigest: receipt.processBindingDigest,
		ActivationReceiptDigest: receipt.activationReceiptDigest, Scenarios: scenarios,
		CompletedAt: receipt.completedAt.UTC(), FromMode: receipt.fromMode, ToMode: receipt.toMode,
		ActiveSessionsAtMutation: receipt.activeSessionsAtMutation, ExactAuthorityRoutes: receipt.exactAuthorityRoutes,
		UnseenLegacyRoutes: receipt.unseenLegacyRoutes, ShadowPromotions: receipt.shadowPromotions,
		SystemAuthMutations: receipt.systemAuthMutations, CredentialMutations: receipt.credentialMutations,
		CanaryWriteErrors: receipt.canaryWriteErrors, UnprovenMutations: receipt.unprovenMutations,
		InstalledServiceDrained: receipt.installedServiceDrained,
	}
	encoded, _ := json.Marshal(value)
	return sha256.Sum256(append([]byte("cq-codex-canary-rollback-v2\x00"), encoded...))
}

func codexCanaryNotPromotable(reason string) error {
	if reason == "" {
		reason = "evidence incomplete"
	}
	return fmt.Errorf("%w: %s", ErrCodexCanaryNotPromotable, reason)
}
