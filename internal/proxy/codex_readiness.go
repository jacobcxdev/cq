package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const codexReadinessMarkerMaxBytes = 64 << 10

// CodexRoutingMode controls turn-aware routing for one transport.
type CodexRoutingMode string

const (
	CodexRoutingOff     CodexRoutingMode = "off"
	CodexRoutingObserve CodexRoutingMode = "observe"
	CodexRoutingEnforce CodexRoutingMode = "enforce"
)

func (m CodexRoutingMode) validate(field string) error {
	switch m {
	case CodexRoutingOff, CodexRoutingObserve, CodexRoutingEnforce:
		return nil
	default:
		return fmt.Errorf("invalid %s %q: must be \"off\", \"observe\", or \"enforce\"", field, m)
	}
}

// CodexRoutingTransport names one independently gated transport.
type CodexRoutingTransport string

const (
	CodexRoutingHTTP      CodexRoutingTransport = "http"
	CodexRoutingWebSocket CodexRoutingTransport = "websocket"
)

const (
	CodexReadinessMarkerVersion = 3
	CodexRoutingJournalVersion  = 1
	CurrentCodexParserSchema    = 1
	CurrentCodexLeaseSchema     = 3
	// CodexHTTPReadinessSemanticsRevision invalidates proof produced before
	// conservative HTTP routing and replay became one frozen runtime contract.
	CodexHTTPReadinessSemanticsRevision      = "http-conservative-routing-v3"
	CodexHTTPFixtureHash                     = "618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1"
	CodexWebSocketReadinessSemanticsRevision = "websocket-terminating-routing-v1"
	CodexWebSocketFixtureHash                = "858d90f3e827194cdfc0dbf5e11d35dfb25e7afc395cf547f17a30936889f9f9"
)

var codexHTTPRequiredGates = []string{
	"frozen-single-transform-envelope",
	"warm-affinity",
	"deterministic-fallback",
	"terminal-default-once",
	"exact-pre-admission-hard429-replay",
	"admitted-no-migration",
	"v2-journal-runtime",
	"installed-listener",
}

var codexWebSocketRequiredGates = []string{
	"strong-frame-authority",
	"portable-pre-admission-hard429-rotation",
	"same-account-candidate-auth-recovery",
	"admitted-no-migration",
	"persistent-account-upstream",
	"upstream-generation-fence",
	"canonical-terminal-error",
	"compression-subprotocol",
	"installed-isolated-client",
}

// CodexReadinessMarker is explicit, versioned proof for one enforcement tuple.
type CodexReadinessMarker struct {
	Version                int                   `json:"version"`
	Transport              CodexRoutingTransport `json:"transport"`
	CQBuild                string                `json:"cq_build"`
	ParserSchema           int                   `json:"parser_schema"`
	LeaseSchema            int                   `json:"lease_schema"`
	SemanticsRevision      string                `json:"semantics_revision"`
	ClientBuild            string                `json:"client_build"`
	RetryBudget            int                   `json:"retry_budget"`
	FixtureHash            string                `json:"fixture_hash"`
	CQExecutableSHA256     string                `json:"cq_executable_sha256"`
	ClientExecutableSHA256 string                `json:"client_executable_sha256"`
	ServiceKind            string                `json:"service_kind"`
	ServiceIdentitySHA256  string                `json:"service_identity_sha256"`
	InstalledResult        string                `json:"installed_result"`
	CompletedGates         []string              `json:"completed_gates"`
	ValidatedAt            time.Time             `json:"validated_at"`
}

// CodexTransportRequirements describes the exact runtime tuple a marker must match.
type CodexTransportRequirements struct {
	Transport          CodexRoutingTransport
	CQBuild            string
	ParserSchema       int
	LeaseSchema        int
	SemanticsRevision  string
	ClientBuild        string
	RetryBudget        int
	FixtureHash        string
	RequiredGates      []string
	ObserveImplemented bool
	EnforceImplemented bool
	installedArtifacts codexInstalledArtifactRequirement
}

type codexInstalledArtifactRequirement struct {
	cqExecutableSHA256     [sha256.Size]byte
	clientExecutableSHA256 [sha256.Size]byte
	serviceKind            codexInstalledListenerServiceKind
	serviceIdentitySHA256  [sha256.Size]byte
}

func (requirement codexInstalledArtifactRequirement) valid() bool {
	if requirement.cqExecutableSHA256 == ([sha256.Size]byte{}) ||
		requirement.clientExecutableSHA256 == ([sha256.Size]byte{}) ||
		requirement.serviceIdentitySHA256 == ([sha256.Size]byte{}) {
		return false
	}
	switch requirement.serviceKind {
	case codexInstalledListenerServiceLaunchd, codexInstalledListenerServiceHomebrew:
		return true
	default:
		return false
	}
}

func (requirement codexInstalledArtifactRequirement) cqExecutableHex() string {
	return hex.EncodeToString(requirement.cqExecutableSHA256[:])
}

func (requirement codexInstalledArtifactRequirement) clientExecutableHex() string {
	return hex.EncodeToString(requirement.clientExecutableSHA256[:])
}

func (requirement codexInstalledArtifactRequirement) serviceIdentityHex() string {
	return hex.EncodeToString(requirement.serviceIdentitySHA256[:])
}

// CodexReadinessTuple is the exact runtime identity covered by validation.
// It contains no credentials, account identity, or turn identity.
type CodexReadinessTuple struct {
	Transport         CodexRoutingTransport
	CQBuild           string
	ParserSchema      int
	LeaseSchema       int
	SemanticsRevision string
	ClientBuild       string
	RetryBudget       int
	FixtureHash       string
}

// CodexHTTPReadinessEvidenceSource identifies whether validation exercised the
// installed listener or only isolated synthetic listeners.
type CodexHTTPReadinessEvidenceSource string

const (
	CodexHTTPReadinessEvidenceSynthetic         CodexHTTPReadinessEvidenceSource = "synthetic-only"
	CodexHTTPReadinessEvidenceInstalledListener CodexHTTPReadinessEvidenceSource = "installed-listener"
)

// CodexHTTPReadinessEvidence is positive, privacy-safe proof supplied by the
// post-handler acceptance runner. Synthetic corpus success alone is not proof.
type CodexHTTPReadinessEvidence struct {
	Source     CodexHTTPReadinessEvidenceSource
	Tuple      CodexReadinessTuple
	Gates      CodexHTTPReadinessGateEvidence
	Acceptance CodexHTTPAcceptanceResult
}

// CodexHTTPReadinessGateEvidence contains only aggregate, privacy-safe
// measurements. Positive case counts prove every semantic path was exercised;
// zero violation counts prove promotion had no observed unsafe outcome.
type CodexHTTPReadinessGateEvidence struct {
	Stage11CorpusTurns                  uint64
	InstalledTurns                      uint64
	FrozenSingleTransformEnvelopeCases  uint64
	WarmAffinityCases                   uint64
	DeterministicFallbackCases          uint64
	TerminalDefaultOnceCases            uint64
	ExactPreAdmissionHard429ReplayCases uint64
	AdmittedNoMigrationCases            uint64
	V2JournalRuntimeCases               uint64
	RoutingMismatches                   uint64
	UnknownLifecycleEvents              uint64
	RawIdentifierLeaks                  uint64
	AutomaticAuthWrites                 uint64
}

type CodexWebSocketReadinessEvidenceSource string

const (
	CodexWebSocketReadinessEvidenceSynthetic         CodexWebSocketReadinessEvidenceSource = "synthetic-only"
	CodexWebSocketReadinessEvidenceInstalledIsolated CodexWebSocketReadinessEvidenceSource = "installed-isolated-client"
)

type CodexWebSocketReadinessEvidence struct {
	Source     CodexWebSocketReadinessEvidenceSource
	Tuple      CodexReadinessTuple
	Gates      CodexWebSocketReadinessGateEvidence
	Acceptance CodexWebSocketAcceptanceResult
}

type CodexWebSocketReadinessGateEvidence struct {
	StrongFrameAuthorityCases             uint64
	PortablePreAdmissionHard429Rotations  uint64
	SameAccountCandidateAuthRecoveryCases uint64
	AdmittedNoMigrationCases              uint64
	PersistentAccountUpstreamCases        uint64
	UpstreamGenerationFenceCases          uint64
	CanonicalTerminalErrorCases           uint64
	CompressionSubprotocolCases           uint64
	RoutingMismatches                     uint64
	UnknownLifecycleEvents                uint64
	LateGenerationMutations               uint64
	RawIdentifierLeaks                    uint64
	AutomaticAuthWrites                   uint64
}

type CodexWebSocketAcceptanceResult struct {
	InstalledVersion      string
	DownstreamConnections uint64
	WebSocketRequests     uint64
	UpstreamDials         uint64
	UnexpectedRoutes      uint64
	EgressAttempts        uint64
	PongVerified          bool
}

// CodexModeStatus is immutable for one proxy process.
type CodexModeStatus struct {
	Configured                  CodexRoutingMode `json:"configured"`
	Effective                   CodexRoutingMode `json:"effective"`
	InhibitionReason            string           `json:"inhibition_reason,omitempty"`
	Limitation                  string           `json:"limitation,omitempty"`
	ConnectionSticky            bool             `json:"connection_sticky,omitempty"`
	CapacitySkewPct             int              `json:"capacity_skew_pct,omitempty"`
	ModeEpoch                   uint64           `json:"mode_epoch"`
	ShadowEpoch                 uint64           `json:"shadow_epoch,omitempty"`
	AuthoritativeEpoch          uint64           `json:"authoritative_epoch,omitempty"`
	RetainedAuthoritativeEpochs []uint64         `json:"retained_authoritative_epochs,omitempty"`
}

// CodexRoutingRuntime contains restart-scoped HTTP and WebSocket mode state.
type CodexRoutingRuntime struct {
	HTTP      CodexModeStatus `json:"http"`
	WebSocket CodexModeStatus `json:"websocket"`
}

type codexRoutingJournal struct {
	Version   int            `json:"version"`
	NextEpoch uint64         `json:"next_epoch"`
	HTTP      codexModeTrack `json:"http"`
	WebSocket codexModeTrack `json:"websocket"`
}

type codexModeTrack struct {
	CodexModeStatus
	RuntimeFingerprint string `json:"runtime_fingerprint,omitempty"`
}

// DefaultCodexRoutingRequirements returns the current binary's safe capability floor.
func DefaultCodexRoutingRequirements(cqBuild, clientBuild string) (CodexTransportRequirements, CodexTransportRequirements) {
	common := CodexTransportRequirements{
		CQBuild:            cqBuild,
		ParserSchema:       CurrentCodexParserSchema,
		LeaseSchema:        CurrentCodexLeaseSchema,
		ClientBuild:        clientBuild,
		ObserveImplemented: true,
	}
	httpReq := common
	httpReq.Transport = CodexRoutingHTTP
	httpReq.SemanticsRevision = CodexHTTPReadinessSemanticsRevision
	httpReq.RetryBudget = 1
	httpReq.FixtureHash = CodexHTTPFixtureHash
	httpReq.RequiredGates = append([]string(nil), codexHTTPRequiredGates...)
	httpReq.EnforceImplemented = true
	wsReq := common
	wsReq.Transport = CodexRoutingWebSocket
	wsReq.SemanticsRevision = CodexWebSocketReadinessSemanticsRevision
	wsReq.RetryBudget = 1
	wsReq.FixtureHash = CodexWebSocketFixtureHash
	wsReq.RequiredGates = append([]string(nil), codexWebSocketRequiredGates...)
	wsReq.EnforceImplemented = true
	return httpReq, wsReq
}

// OpenCodexRoutingRuntime resolves modes once for process lifetime.
func OpenCodexRoutingRuntime(cfg *Config, cqBuild, clientBuild string) (*CodexRoutingRuntime, error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return nil, err
	}
	httpReq, wsReq := DefaultCodexRoutingRequirements(cqBuild, clientBuild)
	return openCodexRoutingRuntimeAt(paths.StateDir, cfg, httpReq, wsReq)
}

func openCodexRoutingRuntimeAt(dir string, cfg *Config, httpReq, wsReq CodexTransportRequirements) (*CodexRoutingRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("proxy config is nil")
	}
	configuredHTTP := cfg.CodexTurnRouting
	configuredWS := cfg.CodexWSTurnRouting
	if configuredHTTP == "" {
		configuredHTTP = CodexRoutingOff
	}
	if configuredWS == "" {
		configuredWS = CodexRoutingOff
	}
	if err := configuredHTTP.validate("codex_turn_routing"); err != nil {
		return nil, err
	}
	if err := configuredWS.validate("codex_ws_turn_routing"); err != nil {
		return nil, err
	}

	journal, err := loadCodexRoutingJournal(dir)
	if err != nil {
		return nil, err
	}
	httpEffective, httpFingerprint, err := resolveCodexMode(configuredHTTP, httpReq)
	if err != nil {
		return nil, err
	}
	wsEffective, wsFingerprint, err := resolveCodexMode(configuredWS, wsReq)
	if err != nil {
		return nil, err
	}
	journal.HTTP = advanceCodexModeTrack(journal.HTTP, configuredHTTP, httpEffective, "", httpFingerprint, &journal.NextEpoch)
	journal.WebSocket = advanceCodexModeTrack(journal.WebSocket, configuredWS, wsEffective, "", wsFingerprint, &journal.NextEpoch)
	journal.Version = CodexRoutingJournalVersion
	if err := saveJSONFile(filepath.Join(dir, "codex-routing-mode.json"), &journal); err != nil {
		return nil, fmt.Errorf("save Codex routing mode journal: %w", err)
	}
	return &CodexRoutingRuntime{HTTP: journal.HTTP.CodexModeStatus, WebSocket: journal.WebSocket.CodexModeStatus}, nil
}

type codexInstalledArtifactCapture func(context.Context, string) (codexInstalledArtifactRequirement, error)

func captureCodexInstalledArtifactsSafely(capture codexInstalledArtifactCapture, clientBuild string) (installedArtifacts codexInstalledArtifactRequirement, returnErr error) {
	if capture == nil {
		return codexInstalledArtifactRequirement{}, errCodexInstalledProcessAttestation
	}
	defer func() {
		if recover() != nil {
			installedArtifacts = codexInstalledArtifactRequirement{}
			returnErr = errCodexInstalledProcessAttestation
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), codexInstalledProcessProofTimeout)
	defer cancel()
	return capture(ctx, clientBuild)
}

func resolveCodexMode(configured CodexRoutingMode, requirements CodexTransportRequirements) (CodexRoutingMode, string, error) {
	transport := string(requirements.Transport)
	switch requirements.Transport {
	case CodexRoutingHTTP:
		transport = "HTTP"
	case CodexRoutingWebSocket:
		transport = "WebSocket"
	}
	switch configured {
	case CodexRoutingOff:
		return CodexRoutingOff, "", nil
	case CodexRoutingObserve:
		if !requirements.ObserveImplemented {
			return CodexRoutingOff, "", fmt.Errorf("Codex %s observation implementation unavailable", transport)
		}
		return CodexRoutingObserve, requirementsFingerprint(requirements), nil
	case CodexRoutingEnforce:
		if !requirements.EnforceImplemented {
			return CodexRoutingOff, "", fmt.Errorf("Codex %s enforcement implementation unavailable", transport)
		}
		return CodexRoutingEnforce, requirementsFingerprint(requirements), nil
	default:
		return CodexRoutingOff, "", fmt.Errorf("invalid configured Codex routing mode %q", configured)
	}
}

func advanceCodexModeTrack(previous codexModeTrack, configured, effective CodexRoutingMode, reason, fingerprint string, next *uint64) codexModeTrack {
	changed := previous.ModeEpoch == 0 || previous.Configured != configured || previous.Effective != effective || previous.RuntimeFingerprint != fingerprint
	if !changed {
		previous.InhibitionReason = reason
		return previous
	}
	(*next)++
	epoch := *next
	retained := append([]uint64(nil), previous.RetainedAuthoritativeEpochs...)
	if previous.AuthoritativeEpoch != 0 {
		retained = appendUniqueEpoch(retained, previous.AuthoritativeEpoch)
	}
	status := CodexModeStatus{
		Configured:                  configured,
		Effective:                   effective,
		InhibitionReason:            reason,
		ModeEpoch:                   epoch,
		RetainedAuthoritativeEpochs: retained,
	}
	switch effective {
	case CodexRoutingObserve:
		status.ShadowEpoch = epoch
	case CodexRoutingEnforce:
		status.AuthoritativeEpoch = epoch
	}
	return codexModeTrack{CodexModeStatus: status, RuntimeFingerprint: fingerprint}
}

func appendUniqueEpoch(epochs []uint64, epoch uint64) []uint64 {
	for _, existing := range epochs {
		if existing == epoch {
			return epochs
		}
	}
	return append(epochs, epoch)
}

func loadCodexRoutingJournal(dir string) (codexRoutingJournal, error) {
	path := filepath.Join(dir, "codex-routing-mode.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return codexRoutingJournal{Version: CodexRoutingJournalVersion}, nil
	}
	if err != nil {
		return codexRoutingJournal{}, fmt.Errorf("read Codex routing mode journal: %w", err)
	}
	var journal codexRoutingJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return codexRoutingJournal{}, fmt.Errorf("parse Codex routing mode journal: %w", err)
	}
	if journal.Version != CodexRoutingJournalVersion {
		return codexRoutingJournal{}, fmt.Errorf("unsupported Codex routing mode journal version %d", journal.Version)
	}
	return journal, nil
}

type codexReadinessMarkerLoadOps struct {
	fileSystem fsutil.FileSystem
}

func defaultCodexReadinessMarkerLoadOps() codexReadinessMarkerLoadOps {
	return codexReadinessMarkerLoadOps{fileSystem: fsutil.OSFileSystem{}}
}

// LoadCodexReadinessMarker reads explicit validation proof for one transport.
func LoadCodexReadinessMarker(dir string, transport CodexRoutingTransport) (CodexReadinessMarker, error) {
	return loadCodexReadinessMarkerWithOps(dir, transport, defaultCodexReadinessMarkerLoadOps())
}

func loadCodexReadinessMarkerWithOps(dir string, transport CodexRoutingTransport, ops codexReadinessMarkerLoadOps) (CodexReadinessMarker, error) {
	if transport != CodexRoutingHTTP && transport != CodexRoutingWebSocket {
		return CodexReadinessMarker{}, fmt.Errorf("invalid readiness marker transport %q", transport)
	}
	if ops.fileSystem == nil {
		return CodexReadinessMarker{}, fsutil.ErrSecureCapabilityUnavailable
	}
	data, err := readCodexReadinessMarkerWithoutCommitPoison(ops.fileSystem, dir, transport)
	if err != nil {
		return CodexReadinessMarker{}, err
	}
	var marker CodexReadinessMarker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return CodexReadinessMarker{}, err
	}
	canonical, err := canonicalCodexReadinessMarkerJSON(marker)
	if err != nil {
		return CodexReadinessMarker{}, err
	}
	if !bytes.Equal(data, canonical) {
		return CodexReadinessMarker{}, fmt.Errorf("readiness marker is not canonical JSON")
	}
	if err := validateCodexReadinessMarkerArtifactBinding(marker); err != nil {
		return CodexReadinessMarker{}, err
	}
	return marker, nil
}

func readCodexReadinessMarkerWithoutCommitPoison(
	fileSystem fsutil.FileSystem,
	dir string,
	transport CodexRoutingTransport,
) ([]byte, error) {
	inspector, ok := fileSystem.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := fileSystem.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := fsutil.ValidateSecureDirectory(fileSystem, dir); err != nil {
		return nil, err
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fence := func() error {
		return fsutil.ValidateSecureDirectoryHandle(inspector, directory, dir)
	}
	if err := fence(); err != nil {
		return nil, err
	}
	poisonName := codexReadinessPoisonName(transport)
	if err := requireCodexReadinessPoisonAbsent(directory, poisonName); err != nil {
		return nil, err
	}
	data, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		inspector,
		directory,
		filepath.Base(codexReadinessPath(dir, transport)),
		codexReadinessMarkerMaxBytes,
	)
	if err != nil {
		return nil, err
	}
	if err := fence(); err != nil {
		return nil, err
	}
	if err := requireCodexReadinessPoisonAbsent(directory, poisonName); err != nil {
		return nil, err
	}
	if err := fence(); err != nil {
		return nil, err
	}
	return data, nil
}

func canonicalCodexReadinessMarkerJSON(marker CodexReadinessMarker) ([]byte, error) {
	data, err := json.MarshalIndent(&marker, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// LoadDefaultCodexReadinessMarker reads proof from CQ's runtime state.
func LoadDefaultCodexReadinessMarker(transport CodexRoutingTransport) (CodexReadinessMarker, error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return CodexReadinessMarker{}, err
	}
	return LoadCodexReadinessMarker(paths.StateDir, transport)
}

// ValidateCodexReadinessMarker rejects any stale or incomplete tuple dimension.
func ValidateCodexReadinessMarker(marker CodexReadinessMarker, required CodexTransportRequirements) error {
	if required.Transport != CodexRoutingHTTP && required.Transport != CodexRoutingWebSocket {
		return fmt.Errorf("readiness requirements have invalid transport")
	}
	if required.CQBuild == "" || required.ParserSchema <= 0 || required.LeaseSchema <= 0 || required.SemanticsRevision == "" || required.ClientBuild == "" || required.RetryBudget < 0 || required.FixtureHash == "" || !validUniqueGates(required.RequiredGates) {
		return fmt.Errorf("readiness requirements are incomplete")
	}
	checks := []struct {
		ok     bool
		reason string
	}{
		{marker.Version == CodexReadinessMarkerVersion, "readiness marker version mismatch"},
		{marker.Transport == required.Transport, "readiness marker transport mismatch"},
		{marker.CQBuild == required.CQBuild, "readiness marker CQ build mismatch"},
		{marker.ParserSchema == required.ParserSchema, "readiness marker parser schema mismatch"},
		{marker.LeaseSchema == required.LeaseSchema, "readiness marker lease schema mismatch"},
		{marker.SemanticsRevision == required.SemanticsRevision, "readiness marker semantics revision mismatch"},
		{marker.ClientBuild == required.ClientBuild, "readiness marker client build mismatch"},
		{marker.RetryBudget == required.RetryBudget, "readiness marker retry budget mismatch"},
		{marker.FixtureHash == required.FixtureHash, "readiness marker fixture hash mismatch"},
		{marker.InstalledResult == "passed", "readiness marker installed result not passed"},
		{!marker.ValidatedAt.IsZero(), "readiness marker validation time missing"},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("%s", check.reason)
		}
	}
	if err := validateCodexReadinessMarkerArtifactBinding(marker); err != nil {
		return err
	}
	if required.Transport == CodexRoutingHTTP && required.installedArtifacts.valid() {
		checks := []struct {
			ok     bool
			reason string
		}{
			{marker.CQExecutableSHA256 == required.installedArtifacts.cqExecutableHex(), "readiness marker CQ executable mismatch"},
			{marker.ClientExecutableSHA256 == required.installedArtifacts.clientExecutableHex(), "readiness marker client executable mismatch"},
			{marker.ServiceKind == string(required.installedArtifacts.serviceKind), "readiness marker service kind mismatch"},
			{marker.ServiceIdentitySHA256 == required.installedArtifacts.serviceIdentityHex(), "readiness marker service identity mismatch"},
		}
		for _, check := range checks {
			if !check.ok {
				return fmt.Errorf("%s", check.reason)
			}
		}
	}
	if !sameUniqueGates(marker.CompletedGates, required.RequiredGates) {
		return fmt.Errorf("readiness marker gate set mismatch")
	}
	return nil
}

func validateCodexReadinessMarkerArtifactBinding(marker CodexReadinessMarker) error {
	if marker.Transport != CodexRoutingHTTP {
		return nil
	}
	if !validCodexReadinessSHA256(marker.CQExecutableSHA256) ||
		!validCodexReadinessSHA256(marker.ClientExecutableSHA256) ||
		!validCodexReadinessSHA256(marker.ServiceIdentitySHA256) {
		return fmt.Errorf("readiness marker installed artifact digest invalid")
	}
	switch codexInstalledListenerServiceKind(marker.ServiceKind) {
	case codexInstalledListenerServiceLaunchd, codexInstalledListenerServiceHomebrew:
		return nil
	default:
		return fmt.Errorf("readiness marker service kind invalid")
	}
}

func validCodexReadinessSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && !bytes.Equal(decoded, make([]byte, sha256.Size))
}

// buildCodexHTTPReadinessMarker validates installed evidence against the exact
// running tuple. Only the package-private sealed startup harness may call it.
func buildCodexHTTPReadinessMarker(evidence CodexHTTPReadinessEvidence, required CodexTransportRequirements, validatedAt time.Time) (CodexReadinessMarker, error) {
	if required.Transport != CodexRoutingHTTP {
		return CodexReadinessMarker{}, fmt.Errorf("HTTP readiness requires HTTP transport")
	}
	if evidence.Source != CodexHTTPReadinessEvidenceInstalledListener {
		return CodexReadinessMarker{}, fmt.Errorf("HTTP readiness requires installed-listener evidence")
	}
	if !required.installedArtifacts.valid() {
		return CodexReadinessMarker{}, fmt.Errorf("HTTP readiness requires installed artifact identity")
	}
	if evidence.Tuple != readinessTuple(required) {
		return CodexReadinessMarker{}, fmt.Errorf("HTTP readiness evidence tuple mismatch")
	}
	completedGates, err := codexHTTPCompletedReadinessGates(evidence.Gates)
	if err != nil {
		return CodexReadinessMarker{}, err
	}
	if !sameUniqueGates(completedGates, required.RequiredGates) {
		return CodexReadinessMarker{}, fmt.Errorf("HTTP readiness derived gate set mismatch")
	}
	if err := validateCodexHTTPAcceptanceEvidence(evidence.Acceptance, required.ClientBuild); err != nil {
		return CodexReadinessMarker{}, err
	}
	marker := CodexReadinessMarker{
		Version:                CodexReadinessMarkerVersion,
		Transport:              required.Transport,
		CQBuild:                required.CQBuild,
		ParserSchema:           required.ParserSchema,
		LeaseSchema:            required.LeaseSchema,
		SemanticsRevision:      required.SemanticsRevision,
		ClientBuild:            required.ClientBuild,
		RetryBudget:            required.RetryBudget,
		FixtureHash:            required.FixtureHash,
		CQExecutableSHA256:     required.installedArtifacts.cqExecutableHex(),
		ClientExecutableSHA256: required.installedArtifacts.clientExecutableHex(),
		ServiceKind:            string(required.installedArtifacts.serviceKind),
		ServiceIdentitySHA256:  required.installedArtifacts.serviceIdentityHex(),
		InstalledResult:        "passed",
		CompletedGates:         completedGates,
		ValidatedAt:            validatedAt,
	}
	if err := ValidateCodexReadinessMarker(marker, required); err != nil {
		return CodexReadinessMarker{}, err
	}
	return marker, nil
}

func buildCodexWebSocketReadinessMarker(evidence CodexWebSocketReadinessEvidence, required CodexTransportRequirements, validatedAt time.Time) (CodexReadinessMarker, error) {
	if required.Transport != CodexRoutingWebSocket {
		return CodexReadinessMarker{}, fmt.Errorf("WebSocket readiness requires WebSocket transport")
	}
	if evidence.Source != CodexWebSocketReadinessEvidenceInstalledIsolated {
		return CodexReadinessMarker{}, fmt.Errorf("WebSocket readiness requires installed isolated-client evidence")
	}
	if evidence.Tuple != readinessTuple(required) {
		return CodexReadinessMarker{}, fmt.Errorf("WebSocket readiness evidence tuple mismatch")
	}
	completedGates, err := codexWebSocketCompletedReadinessGates(evidence.Gates)
	if err != nil {
		return CodexReadinessMarker{}, err
	}
	if !sameUniqueGates(completedGates, required.RequiredGates) {
		return CodexReadinessMarker{}, fmt.Errorf("WebSocket readiness derived gate set mismatch")
	}
	if err := validateCodexWebSocketAcceptanceEvidence(evidence.Acceptance, required.ClientBuild); err != nil {
		return CodexReadinessMarker{}, err
	}
	marker := CodexReadinessMarker{
		Version:           CodexReadinessMarkerVersion,
		Transport:         required.Transport,
		CQBuild:           required.CQBuild,
		ParserSchema:      required.ParserSchema,
		LeaseSchema:       required.LeaseSchema,
		SemanticsRevision: required.SemanticsRevision,
		ClientBuild:       required.ClientBuild,
		RetryBudget:       required.RetryBudget,
		FixtureHash:       required.FixtureHash,
		InstalledResult:   "passed",
		CompletedGates:    completedGates,
		ValidatedAt:       validatedAt,
	}
	if err := ValidateCodexReadinessMarker(marker, required); err != nil {
		return CodexReadinessMarker{}, err
	}
	return marker, nil
}

func codexWebSocketCompletedReadinessGates(evidence CodexWebSocketReadinessGateEvidence) ([]string, error) {
	cases := []struct {
		count uint64
		gate  string
	}{
		{evidence.StrongFrameAuthorityCases, "strong-frame-authority"},
		{evidence.PortablePreAdmissionHard429Rotations, "portable-pre-admission-hard429-rotation"},
		{evidence.SameAccountCandidateAuthRecoveryCases, "same-account-candidate-auth-recovery"},
		{evidence.AdmittedNoMigrationCases, "admitted-no-migration"},
		{evidence.PersistentAccountUpstreamCases, "persistent-account-upstream"},
		{evidence.UpstreamGenerationFenceCases, "upstream-generation-fence"},
		{evidence.CanonicalTerminalErrorCases, "canonical-terminal-error"},
		{evidence.CompressionSubprotocolCases, "compression-subprotocol"},
	}
	completed := make([]string, 0, len(codexWebSocketRequiredGates))
	for _, measured := range cases {
		if measured.count == 0 {
			return nil, fmt.Errorf("Codex WebSocket readiness gate %q has no measured case", measured.gate)
		}
		completed = append(completed, measured.gate)
	}
	if evidence.RoutingMismatches != 0 || evidence.UnknownLifecycleEvents != 0 || evidence.LateGenerationMutations != 0 ||
		evidence.RawIdentifierLeaks != 0 || evidence.AutomaticAuthWrites != 0 {
		return nil, fmt.Errorf("Codex WebSocket readiness observed unsafe isolated evidence")
	}
	return append(completed, "installed-isolated-client"), nil
}

func validateCodexWebSocketAcceptanceEvidence(result CodexWebSocketAcceptanceResult, clientBuild string) error {
	if result.InstalledVersion != clientBuild || result.DownstreamConnections == 0 || result.WebSocketRequests == 0 || result.UpstreamDials == 0 ||
		result.UnexpectedRoutes != 0 || result.EgressAttempts != 0 || !result.PongVerified {
		return fmt.Errorf("Codex WebSocket installed isolated-client evidence incomplete")
	}
	return nil
}

func codexHTTPCompletedReadinessGates(evidence CodexHTTPReadinessGateEvidence) ([]string, error) {
	if evidence.Stage11CorpusTurns < 1_000 {
		return nil, fmt.Errorf("Codex HTTP readiness requires at least 1000 corpus turns")
	}
	if evidence.InstalledTurns < 20 {
		return nil, fmt.Errorf("Codex HTTP readiness requires at least 20 installed turns")
	}
	cases := []struct {
		count uint64
		gate  string
	}{
		{evidence.FrozenSingleTransformEnvelopeCases, "frozen-single-transform-envelope"},
		{evidence.WarmAffinityCases, "warm-affinity"},
		{evidence.DeterministicFallbackCases, "deterministic-fallback"},
		{evidence.TerminalDefaultOnceCases, "terminal-default-once"},
		{evidence.ExactPreAdmissionHard429ReplayCases, "exact-pre-admission-hard429-replay"},
		{evidence.AdmittedNoMigrationCases, "admitted-no-migration"},
		{evidence.V2JournalRuntimeCases, "v2-journal-runtime"},
	}
	completed := make([]string, 0, len(codexHTTPRequiredGates))
	for _, measured := range cases {
		if measured.count == 0 {
			return nil, fmt.Errorf("Codex HTTP readiness gate %q has no measured case", measured.gate)
		}
		completed = append(completed, measured.gate)
	}
	if evidence.RoutingMismatches != 0 || evidence.UnknownLifecycleEvents != 0 ||
		evidence.RawIdentifierLeaks != 0 || evidence.AutomaticAuthWrites != 0 {
		return nil, fmt.Errorf("Codex HTTP readiness observed unsafe installed evidence")
	}
	completed = append(completed, "installed-listener")
	return completed, nil
}

func readinessTuple(required CodexTransportRequirements) CodexReadinessTuple {
	return CodexReadinessTuple{
		Transport:         required.Transport,
		CQBuild:           required.CQBuild,
		ParserSchema:      required.ParserSchema,
		LeaseSchema:       required.LeaseSchema,
		SemanticsRevision: required.SemanticsRevision,
		ClientBuild:       required.ClientBuild,
		RetryBudget:       required.RetryBudget,
		FixtureHash:       required.FixtureHash,
	}
}

func validateCodexHTTPAcceptanceEvidence(result CodexHTTPAcceptanceResult, clientBuild string) error {
	if result.Turns < 20 || result.Requests < result.Turns*2 || result.SelectorCalls != result.Turns ||
		result.ContinuityErrors != 0 || result.UnknownEvents != 0 {
		return fmt.Errorf("Codex HTTP readiness corpus evidence incomplete")
	}
	if result.InstalledVersion != clientBuild || result.InstalledRequests == 0 ||
		result.InstalledModelRequests == 0 || result.InstalledAttempts == 0 ||
		result.InstalledSelectorCalls == 0 || result.InstalledStrongKeys == 0 ||
		result.InstalledZstdRequests == 0 || result.InstalledUnknownEvents != 0 ||
		result.InstalledContinuityErrors != 0 || result.InstalledQuiescentLeases == 0 ||
		result.HeadroomRequests == 0 || result.HeadroomParseErrors != 0 ||
		result.UnexpectedRoutes != 0 || result.EgressAttempts != 0 ||
		result.InstalledResolutions == 0 || !result.PongVerified {
		return fmt.Errorf("Codex HTTP installed-listener evidence incomplete")
	}
	return nil
}

func validUniqueGates(gates []string) bool {
	if len(gates) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(gates))
	for _, gate := range gates {
		if gate == "" {
			return false
		}
		if _, duplicate := seen[gate]; duplicate {
			return false
		}
		seen[gate] = struct{}{}
	}
	return true
}

func sameUniqueGates(actual, required []string) bool {
	if len(actual) != len(required) || !validUniqueGates(actual) || !validUniqueGates(required) {
		return false
	}
	want := make(map[string]struct{}, len(required))
	for _, gate := range required {
		want[gate] = struct{}{}
	}
	for _, gate := range actual {
		if _, ok := want[gate]; !ok {
			return false
		}
	}
	return true
}

func codexReadinessPath(dir string, transport CodexRoutingTransport) string {
	return filepath.Join(dir, "codex-readiness-"+string(transport)+".json")
}

func codexReadinessPoisonName(transport CodexRoutingTransport) string {
	return ".codex-readiness-" + string(transport) + ".commit-in-progress"
}

func requirementsFingerprint(requirements CodexTransportRequirements) string {
	value := struct {
		Transport         CodexRoutingTransport
		ParserSchema      int
		LeaseSchema       int
		SemanticsRevision string
		RetryBudget       int
	}{
		Transport:         requirements.Transport,
		ParserSchema:      requirements.ParserSchema,
		LeaseSchema:       requirements.LeaseSchema,
		SemanticsRevision: requirements.SemanticsRevision,
		RetryBudget:       requirements.RetryBudget,
	}
	data, _ := json.Marshal(value)
	return hashBytes(data)
}

func markerFingerprint(marker CodexReadinessMarker) string {
	copy := marker
	copy.ValidatedAt = time.Time{}
	copy.CompletedGates = append([]string(nil), marker.CompletedGates...)
	sort.Strings(copy.CompletedGates)
	data, _ := json.Marshal(copy)
	return hashBytes(data)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func saveJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ConfiguredModesDiffer reports a restart-required config change.
func (r *CodexRoutingRuntime) ConfiguredModesDiffer(cfg *Config) bool {
	if r == nil || cfg == nil {
		return false
	}
	httpMode := cfg.CodexTurnRouting
	if httpMode == "" {
		httpMode = CodexRoutingOff
	}
	wsMode := cfg.CodexWSTurnRouting
	if wsMode == "" {
		wsMode = CodexRoutingOff
	}
	return r.HTTP.Configured != httpMode || r.WebSocket.Configured != wsMode
}
