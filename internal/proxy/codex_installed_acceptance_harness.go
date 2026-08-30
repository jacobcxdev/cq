package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errCodexInstalledListenerAcceptance = errors.New("Codex installed-listener acceptance unavailable")

type codexInstalledListenerServiceKind string

const (
	codexInstalledListenerServiceLaunchd     codexInstalledListenerServiceKind = "launchd"
	codexInstalledListenerServiceHomebrew    codexInstalledListenerServiceKind = "homebrew"
	codexInstalledListenerServiceSystemdUser codexInstalledListenerServiceKind = "systemd-user"
	codexInstalledListenerServiceEphemeral   codexInstalledListenerServiceKind = "ephemeral"
)

// codexInstalledListenerProcessBinding is process-local proof metadata. Only
// its non-secret executable and loaded-service identities survive in markers;
// the PID and listener generation remain process-local.
type codexInstalledListenerProcessBinding struct {
	CQBuild                string
	ClientBuild            string
	PID                    int
	ServiceKind            codexInstalledListenerServiceKind
	Persistent             bool
	ExecutableSHA256       [sha256.Size]byte
	ClientExecutableSHA256 [sha256.Size]byte
	ServiceIdentitySHA256  [sha256.Size]byte
	ListenerBinding        [sha256.Size]byte
}

type codexInstalledListenerProcessLease interface {
	Binding() codexInstalledListenerProcessBinding
	Snapshot(context.Context) (codexInstalledHTTPProbeSnapshot, error)
	Revalidate(context.Context) error
	Release()
}

type codexInstalledListenerAuthority interface {
	Acquire(context.Context, CodexReadinessTuple) (codexInstalledListenerProcessLease, error)
}

type codexInstalledClientBuildProbe interface {
	Probe(context.Context) (string, error)
}

type codexInstalledHTTPExercise interface {
	Run(context.Context) error
}

type codexInstalledHTTPQuiescer interface {
	CloseAndDrain(context.Context) error
}

// CodexInstalledHTTPValidationGuard is a deny-only operation fence. Acquire
// returns a release function while the exact consumed startup intent remains
// uncancelled. It cannot provide readiness evidence or publish a marker.
type CodexInstalledHTTPValidationGuard interface {
	Acquire() (release func(), err error)
}

const codexInstalledHTTPValidationQuiesceTimeout = 5 * time.Second

// codexStage11CorpusBuildProvenanceSHA256 is supplied only by the reviewed
// release build/CI invocation. Ordinary builds leave it empty and installed
// validation fails closed.
var codexStage11CorpusBuildProvenanceSHA256 string

type codexStage11ReviewedManifest struct {
	Revision         string `json:"revision"`
	TranscriptSHA256 string `json:"transcript_sha256"`
	SmokeSHA256      string `json:"smoke_sha256"`
	CategorySchema   string `json:"category_schema"`
	CaseCount        uint64 `json:"case_count"`
}

//go:embed testdata/codex_stage11_reviewed_manifest.json
var codexStage11ReviewedManifestJSON []byte

var codexStage11Reviewed = mustLoadCodexStage11ReviewedManifest()

func mustLoadCodexStage11ReviewedManifest() codexStage11ReviewedManifest {
	var manifest codexStage11ReviewedManifest
	if err := json.Unmarshal(codexStage11ReviewedManifestJSON, &manifest); err != nil ||
		manifest.Revision == "" || !isCodexStage11LowerHexSHA256(manifest.TranscriptSHA256) || !isCodexStage11LowerHexSHA256(manifest.SmokeSHA256) ||
		manifest.CategorySchema == "" || manifest.CaseCount == 0 {
		panic("invalid embedded Codex Stage 11 reviewed manifest")
	}
	return manifest
}

func isCodexStage11LowerHexSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

// codexStage11CorpusBuildManifest is immutable build provenance, not runtime
// evidence. The corpus remains code-only; CI execution validates the frozen
// transcript and category schema represented by this manifest.
type codexStage11CorpusBuildManifest struct {
	cqBuild              string
	fixtureRevision      string
	transcriptSHA256     string
	smokeSHA256          string
	caseCount            uint64
	categorySchemaSHA256 [sha256.Size]byte
	seal                 *codexStage11CorpusBuildManifestSeal
}

type codexStage11CorpusBuildManifestSeal struct {
	cqBuild              string
	fixtureRevision      string
	transcriptSHA256     string
	smokeSHA256          string
	caseCount            uint64
	categorySchemaSHA256 [sha256.Size]byte
}

func (manifest codexStage11CorpusBuildManifest) valid(required CodexTransportRequirements) bool {
	wantSchema := sha256.Sum256([]byte(codexStage11Reviewed.CategorySchema))
	return manifest.seal != nil && manifest.cqBuild == manifest.seal.cqBuild &&
		manifest.fixtureRevision == manifest.seal.fixtureRevision &&
		manifest.transcriptSHA256 == manifest.seal.transcriptSHA256 && manifest.smokeSHA256 == manifest.seal.smokeSHA256 &&
		manifest.caseCount == manifest.seal.caseCount &&
		manifest.categorySchemaSHA256 == manifest.seal.categorySchemaSHA256 &&
		manifest.cqBuild == required.CQBuild && manifest.fixtureRevision == codexStage11Reviewed.Revision &&
		manifest.transcriptSHA256 == codexStage11Reviewed.TranscriptSHA256 &&
		manifest.smokeSHA256 == codexStage11Reviewed.SmokeSHA256 && manifest.caseCount == codexStage11Reviewed.CaseCount &&
		manifest.categorySchemaSHA256 == wantSchema
}

func loadCodexStage11CorpusBuildManifest(cqBuild, proofSHA256 string) (codexStage11CorpusBuildManifest, error) {
	if cqBuild == "" || strings.ContainsRune(cqBuild, '\x00') || proofSHA256 != strings.TrimSpace(proofSHA256) {
		return codexStage11CorpusBuildManifest{}, errCodexInstalledListenerAcceptance
	}
	proof, err := hex.DecodeString(proofSHA256)
	if err != nil || len(proof) != sha256.Size {
		return codexStage11CorpusBuildManifest{}, errCodexInstalledListenerAcceptance
	}
	schemaSHA256 := sha256.Sum256([]byte(codexStage11Reviewed.CategorySchema))
	fields := []string{
		"cq-codex-stage11-build-provenance-v1",
		cqBuild,
		codexStage11Reviewed.Revision,
		codexStage11Reviewed.TranscriptSHA256,
		codexStage11Reviewed.SmokeSHA256,
		hex.EncodeToString(schemaSHA256[:]),
		strconv.FormatUint(codexStage11Reviewed.CaseCount, 10),
	}
	hash := sha256.New()
	for index, field := range fields {
		if index > 0 {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(field))
	}
	if subtle.ConstantTimeCompare(proof, hash.Sum(nil)) != 1 {
		return codexStage11CorpusBuildManifest{}, errCodexInstalledListenerAcceptance
	}
	manifest := codexStage11CorpusBuildManifest{
		cqBuild:              cqBuild,
		fixtureRevision:      codexStage11Reviewed.Revision,
		transcriptSHA256:     codexStage11Reviewed.TranscriptSHA256,
		smokeSHA256:          codexStage11Reviewed.SmokeSHA256,
		caseCount:            codexStage11Reviewed.CaseCount,
		categorySchemaSHA256: schemaSHA256,
	}
	manifest.seal = &codexStage11CorpusBuildManifestSeal{
		cqBuild:              manifest.cqBuild,
		fixtureRevision:      manifest.fixtureRevision,
		transcriptSHA256:     manifest.transcriptSHA256,
		smokeSHA256:          manifest.smokeSHA256,
		caseCount:            manifest.caseCount,
		categorySchemaSHA256: manifest.categorySchemaSHA256,
	}
	return manifest, nil
}

// codexInstalledHTTPAuditAuthority captures the independent pre-run state
// needed to prove the installed client's model-catalogue and route counts,
// that it leaked no raw identifiers, rewrote no automatic-auth authority,
// and attempted no live egress.
type codexInstalledHTTPAuditAuthority interface {
	Begin(context.Context, CodexReadinessTuple, codexInstalledListenerProcessBinding) (codexInstalledHTTPAuditLease, error)
}

type codexInstalledHTTPAuditLease interface {
	Complete(context.Context) (codexInstalledHTTPSealedAuditProof, error)
	Release()
}

type codexInstalledHTTPSealedAuditProof struct {
	tuple               CodexReadinessTuple
	binding             codexInstalledListenerProcessBinding
	rawIdentifierLeaks  uint64
	automaticAuthWrites uint64
	egressAttempts      uint64
	modelRequests       uint64
	unexpectedRoutes    uint64
	exactClientPong     bool
	seal                *codexInstalledHTTPAuditProofSeal
}

type codexInstalledHTTPAuditProofSeal struct {
	tuple               CodexReadinessTuple
	binding             codexInstalledListenerProcessBinding
	rawIdentifierLeaks  uint64
	automaticAuthWrites uint64
	egressAttempts      uint64
	modelRequests       uint64
	unexpectedRoutes    uint64
	exactClientPong     bool
}

func (proof codexInstalledHTTPSealedAuditProof) valid(tuple CodexReadinessTuple, binding codexInstalledListenerProcessBinding) bool {
	return proof.seal != nil && proof.tuple == proof.seal.tuple && proof.binding == proof.seal.binding &&
		proof.rawIdentifierLeaks == proof.seal.rawIdentifierLeaks && proof.automaticAuthWrites == proof.seal.automaticAuthWrites &&
		proof.egressAttempts == proof.seal.egressAttempts && proof.modelRequests == proof.seal.modelRequests &&
		proof.unexpectedRoutes == proof.seal.unexpectedRoutes && proof.exactClientPong == proof.seal.exactClientPong &&
		proof.tuple == tuple && proof.binding == binding
}

type codexInstalledListenerHarnessDependencies struct {
	authority      codexInstalledListenerAuthority
	clientBuild    codexInstalledClientBuildProbe
	exercise       codexInstalledHTTPExercise
	audit          codexInstalledHTTPAuditAuthority
	quiesce        codexInstalledHTTPQuiescer
	quiesceTimeout time.Duration
	corpus         codexStage11CorpusBuildManifest
	guard          CodexInstalledHTTPValidationGuard
	runtime        *codexRuntimeObservability
	admissions     codexInstalledNativeHTTPAdmissionAuthority
}

// codexInstalledListenerHarness serialises one installed acceptance run. Its
// authority owns an already verified ServingAttestor/process-proof lease; this
// type never creates a listener, handler, service, or alternate endpoint.
type codexInstalledListenerHarness struct {
	mu           sync.Mutex
	running      bool
	dependencies codexInstalledListenerHarnessDependencies
}

// codexInstalledHTTPSealedProof is the only installed-listener proof consumed
// by readiness and canary promotion. Its private seal is created after process
// revalidation and process-owned probe delta validation both succeed.
type codexInstalledHTTPSealedProof struct {
	evidence   CodexHTTPReadinessEvidence
	binding    codexInstalledListenerProcessBinding
	observedAt time.Time
	seal       *codexInstalledHTTPProofSeal
}

type codexInstalledHTTPProofSeal struct {
	evidence   CodexHTTPReadinessEvidence
	binding    codexInstalledListenerProcessBinding
	observedAt time.Time
}

func (proof codexInstalledHTTPSealedProof) validate(required CodexTransportRequirements) error {
	if proof.seal == nil || proof.observedAt.IsZero() || proof.evidence != proof.seal.evidence ||
		proof.binding != proof.seal.binding || !proof.observedAt.Equal(proof.seal.observedAt) ||
		!validCodexInstalledListenerProcessBinding(proof.binding, required) {
		return errCodexInstalledListenerAcceptance
	}
	boundRequired, err := bindCodexInstalledArtifacts(required, proof.binding)
	if err != nil {
		return errCodexInstalledListenerAcceptance
	}
	if _, err := buildCodexHTTPReadinessMarker(proof.evidence, boundRequired, proof.observedAt); err != nil {
		return errCodexInstalledListenerAcceptance
	}
	return nil
}

func (proof codexInstalledHTTPSealedProof) readinessEvidence() CodexHTTPReadinessEvidence {
	if proof.seal == nil || proof.evidence != proof.seal.evidence || proof.binding != proof.seal.binding || !proof.observedAt.Equal(proof.seal.observedAt) {
		return CodexHTTPReadinessEvidence{}
	}
	return proof.evidence
}

func (proof codexInstalledHTTPSealedProof) processBinding() codexInstalledListenerProcessBinding {
	if proof.seal == nil || proof.binding != proof.seal.binding {
		return codexInstalledListenerProcessBinding{}
	}
	return proof.binding
}

func (proof codexInstalledHTTPSealedProof) canaryProcessBindingDigest() [sha256.Size]byte {
	if proof.seal == nil || proof.binding != proof.seal.binding {
		return [sha256.Size]byte{}
	}
	hash := sha256.New()
	writeCodexInstalledProbeMACField(hash, []byte("cq-codex-installed-http-process-binding-v1"))
	writeCodexInstalledProbeMACField(hash, []byte(proof.binding.CQBuild))
	writeCodexInstalledProbeMACField(hash, []byte(proof.binding.ClientBuild))
	writeCodexInstalledProbeMACField(hash, []byte(proof.binding.ServiceKind))
	var fixed [9]byte
	binary.BigEndian.PutUint64(fixed[:8], uint64(proof.binding.PID))
	if proof.binding.Persistent {
		fixed[8] = 1
	}
	writeCodexInstalledProbeMACField(hash, fixed[:])
	writeCodexInstalledProbeMACField(hash, proof.binding.ExecutableSHA256[:])
	writeCodexInstalledProbeMACField(hash, proof.binding.ClientExecutableSHA256[:])
	writeCodexInstalledProbeMACField(hash, proof.binding.ServiceIdentitySHA256[:])
	writeCodexInstalledProbeMACField(hash, proof.binding.ListenerBinding[:])
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func (harness *codexInstalledListenerHarness) Run(ctx context.Context, required CodexTransportRequirements) (codexInstalledHTTPSealedProof, error) {
	return harness.run(ctx, required, nil)
}

// RunAndCommit keeps the exact serving/process lease held through marker
// construction and the caller's atomic persistence operation.
func (harness *codexInstalledListenerHarness) RunAndCommit(
	ctx context.Context,
	required CodexTransportRequirements,
	commit func(CodexReadinessMarker) error,
) (codexInstalledHTTPSealedProof, error) {
	if commit == nil {
		return codexInstalledHTTPSealedProof{}, errCodexInstalledListenerAcceptance
	}
	return harness.run(ctx, required, commit)
}

func (harness *codexInstalledListenerHarness) run(
	ctx context.Context,
	required CodexTransportRequirements,
	commit func(CodexReadinessMarker) error,
) (codexInstalledHTTPSealedProof, error) {
	if ctx == nil {
		return codexInstalledHTTPSealedProof{}, errCodexInstalledListenerAcceptance
	}
	if err := ctx.Err(); err != nil {
		return codexInstalledHTTPSealedProof{}, err
	}
	if harness == nil || harness.dependencies.authority == nil || harness.dependencies.clientBuild == nil || harness.dependencies.exercise == nil ||
		harness.dependencies.audit == nil || harness.dependencies.quiesce == nil || harness.dependencies.runtime == nil ||
		harness.dependencies.admissions == nil ||
		(commit != nil && harness.dependencies.guard == nil) {
		return codexInstalledHTTPSealedProof{}, errCodexInstalledListenerAcceptance
	}
	if required.Transport != CodexRoutingHTTP || strings.TrimSpace(required.CQBuild) == "" || strings.TrimSpace(required.ClientBuild) == "" {
		return codexInstalledHTTPSealedProof{}, errCodexInstalledListenerAcceptance
	}

	harness.mu.Lock()
	if harness.running {
		harness.mu.Unlock()
		return codexInstalledHTTPSealedProof{}, errCodexInstalledListenerAcceptance
	}
	harness.running = true
	harness.mu.Unlock()
	defer func() {
		harness.mu.Lock()
		harness.running = false
		harness.mu.Unlock()
	}()

	tuple := readinessTuple(required)
	lease, err := harness.dependencies.authority.Acquire(ctx, tuple)
	if err != nil || lease == nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("serving lease")
	}
	defer lease.Release()
	binding := lease.Binding()
	if !validCodexInstalledListenerProcessBinding(binding, required) {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("process binding")
	}
	boundRequired, err := bindCodexInstalledArtifacts(required, binding)
	if err != nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("artifact binding")
	}
	manifest := harness.dependencies.corpus
	if !manifest.valid(required) {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("corpus provenance")
	}
	auditLease, err := harness.dependencies.audit.Begin(ctx, tuple, binding)
	if err != nil || auditLease == nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("audit lease")
	}
	defer auditLease.Release()

	clientBuild, err := harness.dependencies.clientBuild.Probe(ctx)
	if err != nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("client build before")
	}
	if err := ctx.Err(); err != nil {
		return codexInstalledHTTPSealedProof{}, err
	}
	if clientBuild != required.ClientBuild {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("client build mismatch")
	}
	if commit != nil {
		release, err := acquireCodexInstalledHTTPValidationGuard(harness.dependencies.guard)
		if err != nil || releaseCodexInstalledHTTPValidationGuard(release) != nil {
			return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("request guard before")
		}
	}

	runtimeBefore := harness.dependencies.runtime.snapshot()
	admissionsBefore := harness.dependencies.admissions.nativeHTTPAdmissionSnapshot()
	before, err := lease.Snapshot(ctx)
	if err != nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("snapshot before")
	}
	exerciseErr := harness.dependencies.exercise.Run(ctx)
	quiesceTimeout := harness.dependencies.quiesceTimeout
	if quiesceTimeout <= 0 {
		quiesceTimeout = codexInstalledHTTPValidationQuiesceTimeout
	}
	quiesceCtx, cancelQuiesce := context.WithTimeout(ctx, quiesceTimeout)
	quiesceErr := harness.dependencies.quiesce.CloseAndDrain(quiesceCtx)
	cancelQuiesce()
	if exerciseErr != nil || quiesceErr != nil {
		if err := ctx.Err(); err != nil {
			return codexInstalledHTTPSealedProof{}, err
		}
		if exerciseErr != nil {
			return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("client exercise")
		}
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("native drain")
	}
	after, snapshotErr := lease.Snapshot(ctx)
	runtimeAfter := harness.dependencies.runtime.snapshot()
	admissionsAfter := harness.dependencies.admissions.nativeHTTPAdmissionSnapshot()
	if revalidateErr := lease.Revalidate(ctx); revalidateErr != nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("process revalidation")
	}
	clientBuildAfter, clientBuildErr := harness.dependencies.clientBuild.Probe(ctx)
	if snapshotErr != nil {
		if err := ctx.Err(); err != nil {
			return codexInstalledHTTPSealedProof{}, err
		}
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("snapshot after")
	}
	if err := ctx.Err(); err != nil {
		return codexInstalledHTTPSealedProof{}, err
	}
	if clientBuildErr != nil || clientBuildAfter != clientBuild {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("client build after")
	}
	auditProof, err := auditLease.Complete(ctx)
	if err != nil || !auditProof.valid(tuple, binding) {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("audit completion")
	}
	result, err := deriveCodexInstalledHTTPProbeResult(before, after, binding, required.ClientBuild)
	if err != nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("probe result")
	}
	result.Gates.Stage11CorpusTurns = manifest.caseCount
	result.Gates.RawIdentifierLeaks = auditProof.rawIdentifierLeaks
	result.Gates.AutomaticAuthWrites = auditProof.automaticAuthWrites
	result.Acceptance.EgressAttempts = auditProof.egressAttempts
	result.Acceptance.InstalledModelRequests = auditProof.modelRequests
	result.Acceptance.UnexpectedRoutes = auditProof.unexpectedRoutes
	result.Acceptance.PongVerified = auditProof.exactClientPong
	if !validCodexInstalledHTTPProbeResult(result, binding, required) {
		return codexInstalledHTTPSealedProof{}, fmt.Errorf(
			"%w: request evidence vector: handler=%d responses=%d compact=%d turns=%d gates=%+v acceptance=%+v diagnostics=%+v",
			errCodexInstalledListenerAcceptance,
			result.ProductionHandlerRequests, result.NativeResponsesRequests, result.NativeCompactRequests, result.StrongTurns,
			result.Gates, result.Acceptance, result.Diagnostics,
		)
	}
	if !validCodexInstalledRuntimeObservability(runtimeBefore, runtimeAfter, result.Diagnostics) {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("runtime evidence vector")
	}
	if !validCodexInstalledNativeHTTPAdmissions(admissionsBefore, admissionsAfter) {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("admission evidence vector")
	}

	evidence := CodexHTTPReadinessEvidence{
		Source:     CodexHTTPReadinessEvidenceInstalledListener,
		Tuple:      tuple,
		Gates:      result.Gates,
		Acceptance: result.Acceptance,
	}
	observedAt := time.Now().UTC()
	proof := codexInstalledHTTPSealedProof{evidence: evidence, binding: binding, observedAt: observedAt}
	proof.seal = &codexInstalledHTTPProofSeal{evidence: evidence, binding: binding, observedAt: observedAt}
	if err := proof.validate(required); err != nil {
		return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("sealed proof")
	}
	if commit != nil {
		release, err := acquireCodexInstalledHTTPValidationGuard(harness.dependencies.guard)
		if err != nil {
			return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("request guard final")
		}
		marker, err := buildCodexHTTPReadinessMarker(evidence, boundRequired, observedAt)
		commitErr := err
		if commitErr == nil {
			commitErr = commit(marker)
		}
		releaseErr := releaseCodexInstalledHTTPValidationGuard(release)
		if commitErr != nil || releaseErr != nil {
			return codexInstalledHTTPSealedProof{}, codexInstalledHarnessStageError("marker commit")
		}
	}
	return proof, nil
}

func codexInstalledHarnessStageError(stage string) error {
	return fmt.Errorf("%w: %s", errCodexInstalledListenerAcceptance, stage)
}

func acquireCodexInstalledHTTPValidationGuard(guard CodexInstalledHTTPValidationGuard) (release func(), returnErr error) {
	if guard == nil {
		return nil, errCodexInstalledListenerAcceptance
	}
	defer func() {
		if recover() != nil {
			release = nil
			returnErr = errCodexInstalledListenerAcceptance
		}
	}()
	release, err := guard.Acquire()
	if err != nil || release == nil {
		return nil, errCodexInstalledListenerAcceptance
	}
	return release, nil
}

func releaseCodexInstalledHTTPValidationGuard(release func()) (returnErr error) {
	if release == nil {
		return errCodexInstalledListenerAcceptance
	}
	defer func() {
		if recover() != nil {
			returnErr = errCodexInstalledListenerAcceptance
		}
	}()
	release()
	return nil
}

func validCodexInstalledListenerProcessBinding(binding codexInstalledListenerProcessBinding, required CodexTransportRequirements) bool {
	if binding.CQBuild != required.CQBuild || binding.ClientBuild != required.ClientBuild || binding.PID <= 1 || !binding.Persistent ||
		binding.ExecutableSHA256 == ([sha256.Size]byte{}) ||
		binding.ClientExecutableSHA256 == ([sha256.Size]byte{}) ||
		binding.ServiceIdentitySHA256 == ([sha256.Size]byte{}) ||
		binding.ListenerBinding == ([sha256.Size]byte{}) {
		return false
	}
	switch binding.ServiceKind {
	case codexInstalledListenerServiceLaunchd, codexInstalledListenerServiceHomebrew, codexInstalledListenerServiceSystemdUser:
		return true
	default:
		return false
	}
}

func bindCodexInstalledArtifacts(required CodexTransportRequirements, binding codexInstalledListenerProcessBinding) (CodexTransportRequirements, error) {
	if !validCodexInstalledListenerProcessBinding(binding, required) {
		return CodexTransportRequirements{}, errCodexInstalledListenerAcceptance
	}
	required.installedArtifacts = codexInstalledArtifactRequirement{
		cqExecutableSHA256:     binding.ExecutableSHA256,
		clientExecutableSHA256: binding.ClientExecutableSHA256,
		serviceKind:            binding.ServiceKind,
		serviceIdentitySHA256:  binding.ServiceIdentitySHA256,
	}
	return required, nil
}

func validCodexInstalledHTTPProbeResult(result codexInstalledHTTPProbeResult, binding codexInstalledListenerProcessBinding, required CodexTransportRequirements) bool {
	if result.ListenerBinding != binding.ListenerBinding ||
		result.ProductionHandlerRequests != 41 || result.NativeResponsesRequests != 39 || result.NativeCompactRequests != 2 ||
		result.StrongTurns != 20 ||
		result.Gates != (CodexHTTPReadinessGateEvidence{
			Stage11CorpusTurns:                  codexStage11Reviewed.CaseCount,
			InstalledTurns:                      20,
			FrozenSingleTransformEnvelopeCases:  2,
			WarmAffinityCases:                   1,
			DeterministicFallbackCases:          1,
			TerminalDefaultOnceCases:            1,
			ExactPreAdmissionHard429ReplayCases: 2,
			AdmittedNoMigrationCases:            1,
			V2JournalRuntimeCases:               39,
		}) ||
		result.Acceptance != (CodexHTTPAcceptanceResult{
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
		}) ||
		result.Diagnostics.AffinityReuseSelections != 1 || result.Diagnostics.FairnessSelections != 19 ||
		result.Diagnostics.TerminalDefaultAttempts != 1 || result.Diagnostics.ReplayEnvelopeErrors != 0 ||
		result.Diagnostics.ReplayEnvelopeCurrentBytes != 0 || result.Diagnostics.ReplayEnvelopePeakBytes == 0 ||
		result.Acceptance.InstalledVersion != required.ClientBuild {
		return false
	}
	evidence := CodexHTTPReadinessEvidence{
		Source:     CodexHTTPReadinessEvidenceInstalledListener,
		Tuple:      readinessTuple(required),
		Gates:      result.Gates,
		Acceptance: result.Acceptance,
	}
	boundRequired, err := bindCodexInstalledArtifacts(required, binding)
	if err != nil {
		return false
	}
	_, err = buildCodexHTTPReadinessMarker(evidence, boundRequired, time.Unix(1, 0).UTC())
	return err == nil
}

func validCodexInstalledNativeHTTPAdmissions(before, after codexInstalledNativeHTTPAdmissionSnapshot) bool {
	delta, err := deltaUint64(before.FirstAuthoritative, after.FirstAuthoritative)
	return err == nil && delta == 20 && !before.PromotionBlocked && !after.PromotionBlocked
}

func validCodexInstalledRuntimeObservability(
	before, after codexRuntimeObservabilitySnapshot,
	diagnostics codexInstalledHTTPAggregateDiagnostics,
) bool {
	affinity, affinityErr := deltaUint64(before.AffinityReuse, after.AffinityReuse)
	fairness, fairnessErr := deltaUint64(before.FairnessSelect, after.FairnessSelect)
	terminal, terminalErr := deltaUint64(before.TerminalDefault, after.TerminalDefault)
	return affinityErr == nil && fairnessErr == nil && terminalErr == nil &&
		affinity == 1 && fairness == 21 && terminal == 1 &&
		before.CurrentReplayBytes == 0 && after.CurrentReplayBytes == 0 &&
		after.PeakReplayBytes == max(before.PeakReplayBytes, diagnostics.ReplayEnvelopePeakBytes)
}
