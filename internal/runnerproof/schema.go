package runnerproof

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ReceiptSchemaV1 = "cq-windows-runner-receipt-v1"
	receiptMaxBytes = 16 << 20
	zeroSHA256      = "0000000000000000000000000000000000000000000000000000000000000000"
)

type ReceiptKind string
type Capability string
type Architecture string
type Phase string

const (
	KindAdmission     ReceiptKind = "admission"
	KindQualification ReceiptKind = "qualification"
	KindCleanup       ReceiptKind = "cleanup"

	CapabilityGate1Native           Capability = "gate1-native"
	CapabilityNativeInteractive     Capability = "native-interactive"
	CapabilityGate4SourceProtected  Capability = "gate4-source-protected"
	CapabilityGate5ReleaseProtected Capability = "gate5-release-protected"

	ArchitectureAMD64 Architecture = "amd64"
	ArchitectureARM64 Architecture = "arm64"

	PhaseGate1 Phase = "gate1"
	PhaseGate2 Phase = "gate2"
	PhaseGate3 Phase = "gate3"
	PhaseGate4 Phase = "gate4"
	PhaseGate5 Phase = "gate5"
)

type RunIdentity struct {
	Repository string `json:"repository"`
	Workflow   string `json:"workflow"`
	Job        string `json:"job"`
	RunID      uint64 `json:"run_id"`
	RunAttempt uint32 `json:"run_attempt"`
	Commit     string `json:"commit"`
}

type ChainIdentity struct {
	Schema                string       `json:"schema"`
	Kind                  ReceiptKind  `json:"kind"`
	PreviousReceiptSHA256 string       `json:"previous_receipt_sha256"`
	LeaseGeneration       uint64       `json:"lease_generation"`
	IssuedAt              string       `json:"issued_at"`
	ExpiresAt             string       `json:"expires_at"`
	Nonce                 string       `json:"nonce"`
	Run                   RunIdentity  `json:"run"`
	Capabilities          []Capability `json:"capabilities"`
	Phase                 Phase        `json:"phase"`
}

type FileBinding struct {
	OpaqueID              string `json:"opaque_id"`
	Size                  int64  `json:"size"`
	SHA256                string `json:"sha256"`
	VolumeSerial          uint64 `json:"volume_serial"`
	FileID                string `json:"file_id"`
	Links                 uint64 `json:"links"`
	ReparseTag            uint32 `json:"reparse_tag"`
	PEMachine             uint16 `json:"pe_machine"`
	SourceInventorySHA256 string `json:"source_inventory_sha256"`
	OwnerSIDHash          string `json:"owner_sid_sha256"`
	SecurityClass         string `json:"security_class"`
	DACL_SHA256           string `json:"dacl_sha256"`
}

type RootBinding struct {
	Kind          string `json:"kind"`
	OpaqueID      string `json:"opaque_id"`
	PathSHA256    string `json:"path_sha256"`
	VolumeSerial  uint64 `json:"volume_serial"`
	FileID        string `json:"file_id"`
	OwnerSIDHash  string `json:"owner_sid_sha256"`
	SecurityClass string `json:"security_class"`
	DACL_SHA256   string `json:"dacl_sha256"`
}

type TokenBinding struct {
	OpaqueID        string `json:"opaque_id"`
	TokenUserSHA256 string `json:"token_user_sha256"`
	LogonSIDHash    string `json:"logon_sid_sha256"`
	SessionID       uint32 `json:"session_id"`
	ElevationType   string `json:"elevation_type"`
	IntegrityRID    uint32 `json:"integrity_rid"`
}

type OptionalFileBinding struct {
	Present bool        `json:"present"`
	Value   FileBinding `json:"value"`
}

type TreeEntryBinding struct {
	RelativePath  string `json:"relative_path"`
	Size          int64  `json:"size"`
	SHA256        string `json:"sha256"`
	VolumeSerial  uint64 `json:"volume_serial"`
	FileID        string `json:"file_id"`
	Links         uint64 `json:"links"`
	ReparseTag    uint32 `json:"reparse_tag"`
	OwnerSIDHash  string `json:"owner_sid_sha256"`
	SecurityClass string `json:"security_class"`
	DACL_SHA256   string `json:"dacl_sha256"`
}

type TreeBinding struct {
	Root           RootBinding        `json:"root"`
	Entries        []TreeEntryBinding `json:"entries"`
	ManifestSHA256 string             `json:"manifest_sha256"`
	TotalSize      int64              `json:"total_size"`
}

type OptionalTreeBinding struct {
	Present bool        `json:"present"`
	Value   TreeBinding `json:"value"`
}

type WindowsSDKLayoutVerifierBinding struct {
	Present                 bool                `json:"present"`
	Executable              OptionalFileBinding `json:"executable"`
	CSourceSHA256           string              `json:"c_source_sha256"`
	CompilerToolchainSHA256 string              `json:"compiler_toolchain_sha256"`
	WindowsSDKHeadersSHA256 string              `json:"windows_sdk_headers_sha256"`
	BuildProvenanceSHA256   string              `json:"build_provenance_sha256"`
	OutputSchemaSHA256      string              `json:"output_schema_sha256"`
}

type ProtectedInventory struct {
	Present                   bool                `json:"present"`
	PrincipalInventorySHA256  string              `json:"principal_inventory_sha256"`
	SessionInventorySHA256    string              `json:"session_inventory_sha256"`
	ControllerLauncher        OptionalFileBinding `json:"controller_launcher"`
	Broker                    OptionalFileBinding `json:"broker"`
	SecureDesktopAdapter      OptionalFileBinding `json:"secure_desktop_adapter"`
	BrokerProtocolVersion     uint32              `json:"broker_protocol_version"`
	BrokerConfigurationSHA256 string              `json:"broker_configuration_sha256"`
	CodexFixture              OptionalFileBinding `json:"codex_fixture"`
	CodexManifestSHA256       string              `json:"codex_manifest_sha256"`
	ArtifactMaterialiser      OptionalFileBinding `json:"artifact_materialiser"`
	EgressPolicySHA256        string              `json:"egress_policy_sha256"`
}

type NativeInteractiveInventory struct {
	Present                     bool                `json:"present"`
	PrincipalInventorySHA256    string              `json:"principal_inventory_sha256"`
	SessionInventorySHA256      string              `json:"session_inventory_sha256"`
	ControllerLauncher          OptionalFileBinding `json:"controller_launcher"`
	LauncherProtocolVersion     uint32              `json:"launcher_protocol_version"`
	LauncherConfigurationSHA256 string              `json:"launcher_configuration_sha256"`
}

type ControllerGeneration struct {
	Role           string       `json:"role"`
	OpaqueID       string       `json:"opaque_id"`
	PID            uint32       `json:"pid"`
	CreationTime   uint64       `json:"creation_time"`
	Token          TokenBinding `json:"token"`
	Profile        RootBinding  `json:"profile"`
	RoamingAppData RootBinding  `json:"roaming_app_data"`
	LocalAppData   RootBinding  `json:"local_app_data"`
}

type BrokerPromptEvidence struct {
	CaseID                 string       `json:"case_id"`
	LaunchOrdinal          uint32       `json:"launch_ordinal"`
	ControllerRole         string       `json:"controller_role"`
	ControllerOpaqueID     string       `json:"controller_opaque_id"`
	ControllerPID          uint32       `json:"controller_pid"`
	ControllerCreationTime uint64       `json:"controller_creation_time"`
	Helper                 FileBinding  `json:"helper"`
	HelperPID              uint32       `json:"helper_pid"`
	HelperCreationTime     uint64       `json:"helper_creation_time"`
	HelperToken            TokenBinding `json:"helper_token"`
	PromptNonce            string       `json:"prompt_nonce"`
	PromptKind             string       `json:"prompt_kind"`
	RequestedAt            string       `json:"requested_at"`
	DecidedAt              string       `json:"decided_at"`
	RequestCount           uint32       `json:"request_count"`
	DecisionCount          uint32       `json:"decision_count"`
	SecureDesktop          bool         `json:"secure_desktop"`
	RunAsVerb              bool         `json:"runas_verb"`
	Decision               string       `json:"decision"`
	DirectLaunchCount      uint32       `json:"direct_launch_count"`
}

type BrokerTranscript struct {
	Schema                uint32                 `json:"schema"`
	AdmissionSHA256       string                 `json:"admission_sha256"`
	HandoffSHA256         string                 `json:"handoff_sha256"`
	BrokerProtocolVersion uint32                 `json:"broker_protocol_version"`
	Prompts               []BrokerPromptEvidence `json:"prompts"`
}

type AdmissionPayload struct {
	Chain                             ChainIdentity                   `json:"chain"`
	Labels                            []string                        `json:"labels"`
	Architecture                      Architecture                    `json:"architecture"`
	WindowsCaptionSHA256              string                          `json:"windows_caption_sha256"`
	WindowsBuild                      uint32                          `json:"windows_build"`
	ImageSHA256                       string                          `json:"image_sha256"`
	LeaseID                           string                          `json:"lease_id"`
	QualificationDeadline             string                          `json:"qualification_deadline"`
	CleanupDeadline                   string                          `json:"cleanup_deadline"`
	Coordinator                       TokenBinding                    `json:"coordinator"`
	Profile                           RootBinding                     `json:"profile"`
	RoamingAppData                    RootBinding                     `json:"roaming_app_data"`
	LocalAppData                      RootBinding                     `json:"local_app_data"`
	RunnerTemp                        RootBinding                     `json:"runner_temp"`
	OutputRoot                        RootBinding                     `json:"output_root"`
	RepositorySource                  TreeBinding                     `json:"repository_source"`
	GoToolchain                       TreeBinding                     `json:"go_toolchain"`
	NativeTestHarness                 TreeBinding                     `json:"native_test_harness"`
	TCPTableSDKLayoutVerifier         WindowsSDKLayoutVerifierBinding `json:"tcp_table_sdk_layout_verifier"`
	RaceRuntime                       OptionalTreeBinding             `json:"race_runtime"`
	NativeInteractive                 NativeInteractiveInventory      `json:"native_interactive"`
	Protected                         ProtectedInventory              `json:"protected"`
	Artifact                          ArtifactBinding                 `json:"artifact"`
	SourceMaterialiser                FileBinding                     `json:"source_materialiser"`
	SourceManifest                    FileBinding                     `json:"source_manifest"`
	AdmissionVerifier                 FileBinding                     `json:"admission_verifier"`
	PhaseDriver                       OptionalFileBinding             `json:"phase_driver"`
	Qualifier                         FileBinding                     `json:"qualifier"`
	CleanupRequester                  FileBinding                     `json:"cleanup_requester"`
	CleanupObserver                   FileBinding                     `json:"cleanup_observer"`
	CleanupObserverAdapter            FileBinding                     `json:"cleanup_observer_adapter"`
	SummaryExporter                   FileBinding                     `json:"summary_exporter"`
	CleanupObserverClassSHA256        string                          `json:"cleanup_observer_class_sha256"`
	BootstrapPolicyVersion            uint32                          `json:"bootstrap_policy_version"`
	BootstrapPolicySHA256             string                          `json:"bootstrap_policy_sha256"`
	SourceBootstrapRequestSHA256      string                          `json:"source_bootstrap_request_sha256"`
	SourceMaterialisationResultSHA256 string                          `json:"source_materialisation_result_sha256"`
	SourceMaterialisedAt              string                          `json:"source_materialised_at"`
	BaselineSHA256                    string                          `json:"baseline_sha256"`
}

type MaterialisationBinding struct {
	Role string              `json:"role"`
	Kind string              `json:"kind"`
	File OptionalFileBinding `json:"file"`
	Tree OptionalTreeBinding `json:"tree"`
}

type ArtifactBinding struct {
	Present               bool   `json:"present"`
	NumericID             uint64 `json:"numeric_id"`
	GitHubArtifactSHA256  string `json:"github_artifact_sha256"`
	BundleSHA256          string `json:"bundle_sha256"`
	ReleaseManifestSHA256 string `json:"release_manifest_sha256"`
}

type QualificationPayload struct {
	Chain                  ChainIdentity            `json:"chain"`
	AdmissionSHA256        string                   `json:"admission_sha256"`
	Controllers            []ControllerGeneration   `json:"controllers"`
	Materialisations       []MaterialisationBinding `json:"materialisations"`
	HarnessInventorySHA256 string                   `json:"harness_inventory_sha256"`
	ResultSHA256           string                   `json:"result_sha256"`
	BrokerEvidenceSHA256   string                   `json:"broker_evidence_sha256"`
	EgressReadbackSHA256   string                   `json:"egress_readback_sha256"`
	DeniedProbesSHA256     string                   `json:"denied_probes_sha256"`
	Artifact               ArtifactBinding          `json:"artifact"`
}

type AbsenceSet struct {
	Accounts           bool `json:"accounts"`
	Profiles           bool `json:"profiles"`
	Sessions           bool `json:"sessions"`
	Processes          bool `json:"processes"`
	Pipes              bool `json:"pipes"`
	Tasks              bool `json:"tasks"`
	AppContainers      bool `json:"appcontainers"`
	WFPObjects         bool `json:"wfp_objects"`
	LoopbackExemptions bool `json:"loopback_exemptions"`
	NetworkPolicyDelta bool `json:"network_policy_delta"`
	Certificates       bool `json:"certificates"`
	Roots              bool `json:"roots"`
	RemoteMappings     bool `json:"remote_mappings"`
}

type CleanupPayload struct {
	Chain                  ChainIdentity `json:"chain"`
	Outcome                string        `json:"outcome"`
	AdmissionSHA256        string        `json:"admission_sha256"`
	QualificationSHA256    string        `json:"qualification_sha256"`
	Absence                AbsenceSet    `json:"absence"`
	NetworkReadbackSHA256  string        `json:"network_readback_sha256"`
	RestoredBaselineSHA256 string        `json:"restored_baseline_sha256"`
	RollbackMode           string        `json:"rollback_mode"`
	BaselineRestored       bool          `json:"baseline_restored"`
}

type SignedAdmission struct {
	Payload   AdmissionPayload `json:"payload"`
	KeyID     string           `json:"key_id"`
	Signature string           `json:"signature"`
}

type SignedQualification struct {
	Payload   QualificationPayload `json:"payload"`
	KeyID     string               `json:"key_id"`
	Signature string               `json:"signature"`
}

type SignedCleanup struct {
	Payload   CleanupPayload `json:"payload"`
	KeyID     string         `json:"key_id"`
	Signature string         `json:"signature"`
}

type ChainSummary struct {
	Schema              uint32          `json:"schema"`
	Run                 RunIdentity     `json:"run"`
	Capabilities        []Capability    `json:"capabilities"`
	Architecture        Architecture    `json:"architecture"`
	WindowsBuild        uint32          `json:"windows_build"`
	Artifact            ArtifactBinding `json:"artifact"`
	AdmissionSHA256     string          `json:"admission_sha256"`
	QualificationSHA256 string          `json:"qualification_sha256"`
	CleanupSHA256       string          `json:"cleanup_sha256"`
	Outcome             string          `json:"outcome"`
}

type TrustConfig struct {
	KeyID         string
	PublicKey     ed25519.PublicKey
	ProvisionerID string
	Labels        []string
	Architecture  Architecture
}

func DecodeAdmission(data []byte) (SignedAdmission, error) {
	return strictDecode[SignedAdmission](data, KindAdmission)
}

func DecodeQualification(data []byte) (SignedQualification, error) {
	return strictDecode[SignedQualification](data, KindQualification)
}

func DecodeCleanup(data []byte) (SignedCleanup, error) {
	return strictDecode[SignedCleanup](data, KindCleanup)
}

func CanonicalAdmission(receipt SignedAdmission) ([]byte, error) {
	return canonicalSigned(receipt, KindAdmission)
}

func CanonicalQualification(receipt SignedQualification) ([]byte, error) {
	return canonicalSigned(receipt, KindQualification)
}

func CanonicalCleanup(receipt SignedCleanup) ([]byte, error) {
	return canonicalSigned(receipt, KindCleanup)
}

func canonicalAdmissionPayload(payload AdmissionPayload) ([]byte, error) {
	return canonicalJSON(payload)
}

func canonicalQualificationPayload(payload QualificationPayload) ([]byte, error) {
	return canonicalJSON(payload)
}

func canonicalCleanupPayload(payload CleanupPayload) ([]byte, error) {
	return canonicalJSON(payload)
}

func CanonicalBrokerTranscript(transcript BrokerTranscript) ([]byte, error) {
	return canonicalJSON(transcript)
}

func canonicalSigned[T SignedAdmission | SignedQualification | SignedCleanup](receipt T, kind ReceiptKind) ([]byte, error) {
	chain, keyID, signature := signedFields(receipt)
	if chain.Schema != ReceiptSchemaV1 || chain.Kind != kind {
		return nil, fmt.Errorf("invalid %s receipt identity", kind)
	}
	if keyID == "" {
		return nil, errors.New("receipt key ID is empty")
	}
	if !validLowerHex(signature, ed25519.SignatureSize*2, false) {
		return nil, errors.New("receipt signature is not canonical Ed25519 hex")
	}
	return canonicalJSON(receipt)
}

func signedFields[T SignedAdmission | SignedQualification | SignedCleanup](receipt T) (ChainIdentity, string, string) {
	switch value := any(receipt).(type) {
	case SignedAdmission:
		return value.Payload.Chain, value.KeyID, value.Signature
	case SignedQualification:
		return value.Payload.Chain, value.KeyID, value.Signature
	case SignedCleanup:
		return value.Payload.Chain, value.KeyID, value.Signature
	default:
		panic("unreachable receipt type")
	}
}

func strictDecode[T SignedAdmission | SignedQualification | SignedCleanup](data []byte, kind ReceiptKind) (T, error) {
	var zero T
	if len(data) == 0 || len(data) > receiptMaxBytes {
		return zero, errors.New("receipt size is outside bounds")
	}
	if !utf8.Valid(data) {
		return zero, errors.New("receipt is not valid UTF-8")
	}
	if err := rejectDuplicateNullAndFloatJSON(data); err != nil {
		return zero, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var receipt T
	if err := decoder.Decode(&receipt); err != nil {
		return zero, fmt.Errorf("decode receipt: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return zero, err
	}
	canonical, err := canonicalSigned(receipt, kind)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(data, canonical) {
		return zero, errors.New("receipt is not canonical JSON")
	}
	return receipt, nil
}

func canonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	data := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	if len(data) > receiptMaxBytes {
		return nil, errors.New("canonical JSON exceeds receipt bound")
	}
	if err := rejectDuplicateNullAndFloatJSON(data); err != nil {
		return nil, fmt.Errorf("canonical JSON contains an unsupported value: %w", err)
	}
	return data, nil
}

func rejectDuplicateNullAndFloatJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON token: %w", err)
	}
	switch value := token.(type) {
	case nil:
		return errors.New("null is not permitted in receipts")
	case json.Number:
		if strings.ContainsAny(value.String(), ".eE") {
			return errors.New("floating-point values are not permitted in receipts")
		}
	case json.Delim:
		switch value {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("decode object key: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate object member %q", key)
				}
				seen[key] = struct{}{}
				if err := scanJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("malformed JSON object")
			}
		case '[':
			for decoder.More() {
				if err := scanJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("malformed JSON array")
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", value)
		}
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("receipt contains trailing JSON")
		}
		return fmt.Errorf("decode trailing receipt data: %w", err)
	}
	return nil
}

func receiptSignatureInput(kind ReceiptKind, payload []byte) []byte {
	prefix := []byte("cq.windows.runner.receipt.v1\n" + string(kind) + "\n")
	return append(prefix, payload...)
}

func receiptDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validLowerHex(value string, length int, allowZero bool) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	return allowZero || !bytes.Equal(decoded, make([]byte, len(decoded)))
}

func exactZero[T any](value T) bool {
	return reflect.ValueOf(value).IsZero()
}

func sortedUnique[T ~string](values []T) bool {
	return slices.IsSorted(values) && len(values) == len(slices.Compact(slices.Clone(values)))
}

func parseUTCSecond(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.Format(time.RFC3339) != value || !strings.HasSuffix(value, "Z") {
		return time.Time{}, errors.New("timestamp is not a canonical RFC3339 UTC second")
	}
	return parsed, nil
}

func VerifyAdmission(receipt SignedAdmission, prior *SignedCleanup, trust TrustConfig, expectedRun RunIdentity, expectedCapabilities []Capability, now time.Time) (string, error) {
	if err := validateTrust(trust); err != nil {
		return "", err
	}
	if err := verifyAdmissionSignature(receipt, trust); err != nil {
		return "", err
	}
	if err := validateAdmissionPayload(receipt.Payload, trust.Architecture); err != nil {
		return "", err
	}
	if receipt.Payload.Chain.Run != expectedRun {
		return "", errors.New("admission run identity mismatch")
	}
	if !slices.Equal(receipt.Payload.Chain.Capabilities, expectedCapabilities) {
		return "", errors.New("admission capability mismatch")
	}
	if !slices.Equal(receipt.Payload.Labels, trust.Labels) {
		return "", errors.New("admission runner label mismatch")
	}
	if receipt.Payload.Architecture != trust.Architecture {
		return "", errors.New("admission architecture mismatch")
	}
	issuedAt, _ := parseUTCSecond(receipt.Payload.Chain.IssuedAt)
	expiresAt, _ := parseUTCSecond(receipt.Payload.Chain.ExpiresAt)
	if now.Before(issuedAt) || !now.Before(expiresAt) {
		return "", errors.New("admission is not live")
	}
	if receipt.Payload.Chain.LeaseGeneration == 1 {
		if prior != nil || receipt.Payload.Chain.PreviousReceiptSHA256 != zeroSHA256 {
			return "", errors.New("initial admission has prior cleanup authority")
		}
	} else {
		if prior == nil || receipt.Payload.Chain.PreviousReceiptSHA256 == zeroSHA256 {
			return "", errors.New("later admission lacks prior cleanup authority")
		}
		priorDigest, err := verifyReconciledPriorCleanup(*prior, trust, receipt.Payload.Chain.LeaseGeneration)
		if err != nil {
			return "", err
		}
		if priorDigest != receipt.Payload.Chain.PreviousReceiptSHA256 {
			return "", errors.New("prior cleanup digest mismatch")
		}
	}
	canonical, err := CanonicalAdmission(receipt)
	if err != nil {
		return "", err
	}
	return receiptDigest(canonical), nil
}

func VerifyQualification(receipt SignedQualification, admission SignedAdmission, admissionSHA256 string, trust TrustConfig, now time.Time) (string, error) {
	if err := validateTrust(trust); err != nil {
		return "", err
	}
	if err := verifyAdmissionSignature(admission, trust); err != nil {
		return "", err
	}
	if err := validateAdmissionPayload(admission.Payload, trust.Architecture); err != nil {
		return "", err
	}
	canonicalAdmission, err := CanonicalAdmission(admission)
	if err != nil {
		return "", err
	}
	if computed := receiptDigest(canonicalAdmission); computed != admissionSHA256 {
		return "", errors.New("qualification admission digest input mismatch")
	}
	if err := verifyQualificationSignature(receipt, trust); err != nil {
		return "", err
	}
	if err := validateQualificationPayload(receipt.Payload, admission.Payload, admissionSHA256); err != nil {
		return "", err
	}
	issuedAt, _ := parseUTCSecond(receipt.Payload.Chain.IssuedAt)
	deadline, _ := parseUTCSecond(admission.Payload.QualificationDeadline)
	if issuedAt.After(deadline) || now.Before(issuedAt) {
		return "", errors.New("qualification is outside its admitted time bound")
	}
	canonical, err := CanonicalQualification(receipt)
	if err != nil {
		return "", err
	}
	return receiptDigest(canonical), nil
}

func VerifyCleanup(receipt SignedCleanup, admission SignedAdmission, qualification *SignedQualification, parentSHA256 string, trust TrustConfig, now time.Time) (string, error) {
	if err := validateTrust(trust); err != nil {
		return "", err
	}
	if err := verifyAdmissionSignature(admission, trust); err != nil {
		return "", err
	}
	if err := validateAdmissionPayload(admission.Payload, trust.Architecture); err != nil {
		return "", err
	}
	admissionBytes, err := CanonicalAdmission(admission)
	if err != nil {
		return "", err
	}
	admissionDigest := receiptDigest(admissionBytes)
	expectedParent := admissionDigest
	expectedOutcome := "aborted"
	var parentIssuedAt time.Time
	if qualification != nil {
		if err := verifyQualificationSignature(*qualification, trust); err != nil {
			return "", err
		}
		qualificationBytes, err := CanonicalQualification(*qualification)
		if err != nil {
			return "", err
		}
		expectedParent = receiptDigest(qualificationBytes)
		expectedOutcome = "qualified"
		parentIssuedAt, _ = parseUTCSecond(qualification.Payload.Chain.IssuedAt)
	} else {
		parentIssuedAt, _ = parseUTCSecond(admission.Payload.Chain.IssuedAt)
	}
	if parentSHA256 != expectedParent {
		return "", errors.New("cleanup parent digest input mismatch")
	}
	if err := verifyCleanupSignature(receipt, trust); err != nil {
		return "", err
	}
	if err := validateCleanupPayload(receipt.Payload, admission.Payload, admissionDigest, expectedParent, expectedOutcome); err != nil {
		return "", err
	}
	issuedAt, _ := parseUTCSecond(receipt.Payload.Chain.IssuedAt)
	cleanupDeadline, _ := parseUTCSecond(admission.Payload.CleanupDeadline)
	if issuedAt.Before(parentIssuedAt) || issuedAt.After(cleanupDeadline) || now.Before(issuedAt) {
		return "", errors.New("cleanup is outside its admitted time bound")
	}
	canonical, err := CanonicalCleanup(receipt)
	if err != nil {
		return "", err
	}
	return receiptDigest(canonical), nil
}

func VerifyGateChain(admission SignedAdmission, qualification SignedQualification, cleanup SignedCleanup, trust TrustConfig, expectedRun RunIdentity, expectedCapabilities []Capability, now time.Time) (ChainSummary, error) {
	admissionDigest, err := VerifyAdmission(admission, nil, trust, expectedRun, expectedCapabilities, now)
	if err != nil {
		return ChainSummary{}, err
	}
	qualificationDigest, err := VerifyQualification(qualification, admission, admissionDigest, trust, now)
	if err != nil {
		return ChainSummary{}, err
	}
	cleanupDigest, err := VerifyCleanup(cleanup, admission, &qualification, qualificationDigest, trust, now)
	if err != nil {
		return ChainSummary{}, err
	}
	if cleanup.Payload.Outcome != "qualified" {
		return ChainSummary{}, errors.New("gate cleanup outcome is not qualified")
	}
	return ChainSummary{
		Schema: 1, Run: expectedRun, Capabilities: slices.Clone(expectedCapabilities),
		Architecture: admission.Payload.Architecture, WindowsBuild: admission.Payload.WindowsBuild,
		Artifact: admission.Payload.Artifact, AdmissionSHA256: admissionDigest,
		QualificationSHA256: qualificationDigest, CleanupSHA256: cleanupDigest, Outcome: cleanup.Payload.Outcome,
	}, nil
}

func ValidateBrokerTranscript(transcript BrokerTranscript, capability Capability) error {
	if capability != CapabilityGate4SourceProtected && capability != CapabilityGate5ReleaseProtected {
		if transcript.Schema == 0 && transcript.AdmissionSHA256 == "" && transcript.HandoffSHA256 == "" && transcript.BrokerProtocolVersion == 0 && len(transcript.Prompts) == 0 {
			return nil
		}
		return errors.New("non-protected capability has broker transcript")
	}
	if transcript.Schema != 1 || !validLowerHex(transcript.AdmissionSHA256, 64, false) || !validLowerHex(transcript.HandoffSHA256, 64, false) || transcript.BrokerProtocolVersion == 0 {
		return errors.New("invalid broker transcript identity")
	}
	if len(transcript.Prompts) == 0 || len(transcript.Prompts) > 16 {
		return errors.New("broker transcript prompt count is outside bounds")
	}
	previous := ""
	seen := make(map[string]struct{}, len(transcript.Prompts))
	counts := make(map[string]uint32)
	for _, prompt := range transcript.Prompts {
		key := fmt.Sprintf("%s\x00%s\x00%010d\x00%s", prompt.ControllerRole, prompt.CaseID, prompt.LaunchOrdinal, prompt.PromptNonce)
		if key <= previous {
			return errors.New("broker prompts are not sorted uniquely")
		}
		previous = key
		if _, exists := seen[key]; exists {
			return errors.New("broker prompt replay")
		}
		seen[key] = struct{}{}
		if err := validateBrokerPrompt(prompt); err != nil {
			return err
		}
		group := prompt.ControllerRole + "\x00" + prompt.CaseID
		if prompt.LaunchOrdinal != counts[group] {
			return errors.New("broker prompt ordinals are not contiguous")
		}
		counts[group]++
	}
	expectedCounts := map[string]uint32{}
	for _, role := range []string{"limited-admin", "standard-user"} {
		if capability == CapabilityGate4SourceProtected {
			expectedCounts[role+"\x00installed-codex-http"] = 1
			expectedCounts[role+"\x00installed-confinement"] = 7
		} else {
			expectedCounts[role+"\x00release-protected-installed"] = 8
		}
	}
	if !reflect.DeepEqual(counts, expectedCounts) {
		return errors.New("broker prompt distribution does not match capability")
	}
	return nil
}

func validateTrust(trust TrustConfig) error {
	if trust.KeyID == "" || trust.ProvisionerID == "" || len(trust.PublicKey) != ed25519.PublicKeySize {
		return errors.New("invalid runner trust configuration")
	}
	if trust.Architecture != ArchitectureAMD64 && trust.Architecture != ArchitectureARM64 {
		return errors.New("unsupported runner trust architecture")
	}
	if len(trust.Labels) == 0 || !sortedUnique(trust.Labels) {
		return errors.New("runner trust labels are not sorted uniquely")
	}
	return nil
}

func validateAdmissionPayload(payload AdmissionPayload, architecture Architecture) error {
	if err := validateChain(payload.Chain, KindAdmission); err != nil {
		return err
	}
	if payload.Architecture != architecture || !sortedUnique(payload.Labels) || len(payload.Labels) == 0 {
		return errors.New("invalid admission platform authority")
	}
	for _, digest := range []string{payload.WindowsCaptionSHA256, payload.ImageSHA256, payload.CleanupObserverClassSHA256, payload.BootstrapPolicySHA256, payload.SourceBootstrapRequestSHA256, payload.SourceMaterialisationResultSHA256, payload.BaselineSHA256} {
		if !validLowerHex(digest, 64, false) {
			return errors.New("invalid admission digest")
		}
	}
	if payload.SourceBootstrapRequestSHA256 == payload.SourceMaterialisationResultSHA256 || payload.WindowsBuild == 0 || payload.LeaseID == "" || payload.BootstrapPolicyVersion == 0 {
		return errors.New("invalid admission inventory identity")
	}
	issuedAt, _ := parseUTCSecond(payload.Chain.IssuedAt)
	materialisedAt, err := parseUTCSecond(payload.SourceMaterialisedAt)
	if err != nil || !materialisedAt.Before(issuedAt) {
		return errors.New("source materialisation ordering is invalid")
	}
	qualificationDeadline, err := parseUTCSecond(payload.QualificationDeadline)
	if err != nil || !qualificationDeadline.After(issuedAt) {
		return errors.New("qualification deadline is invalid")
	}
	cleanupDeadline, err := parseUTCSecond(payload.CleanupDeadline)
	if err != nil || !cleanupDeadline.After(qualificationDeadline) {
		return errors.New("cleanup deadline is invalid")
	}
	if err := validateToken(payload.Coordinator, false); err != nil {
		return err
	}
	for _, root := range []RootBinding{payload.Profile, payload.RoamingAppData, payload.LocalAppData, payload.RunnerTemp, payload.OutputRoot} {
		if err := validateRoot(root); err != nil {
			return err
		}
	}
	if err := validateTreeBinding("cq-source", payload.RepositorySource, architecture); err != nil {
		return err
	}
	if err := validateTreeBinding("go-toolchain", payload.GoToolchain, architecture); err != nil {
		return err
	}
	if err := validateTreeBinding("native-test-harness", payload.NativeTestHarness, architecture); err != nil {
		return err
	}
	if architecture == ArchitectureAMD64 {
		if !payload.RaceRuntime.Present || validateTreeBinding("race-runtime", payload.RaceRuntime.Value, architecture) != nil {
			return errors.New("x64 admission lacks valid race runtime")
		}
	} else if !absentOptionalTree(payload.RaceRuntime) {
		return errors.New("ARM64 admission has race runtime")
	}
	for _, file := range []FileBinding{payload.SourceMaterialiser, payload.AdmissionVerifier, payload.Qualifier, payload.CleanupRequester, payload.CleanupObserver, payload.CleanupObserverAdapter, payload.SummaryExporter} {
		if err := validateFile(file, architecture); err != nil {
			return err
		}
	}
	if err := validateFile(payload.SourceManifest, ""); err != nil {
		return err
	}
	if err := validateInventoryForCapabilities(payload); err != nil {
		return err
	}
	return nil
}

func validateQualificationPayload(payload QualificationPayload, admission AdmissionPayload, admissionSHA256 string) error {
	if err := validateChain(payload.Chain, KindQualification); err != nil {
		return err
	}
	if payload.Chain.Run != admission.Chain.Run || payload.Chain.LeaseGeneration != admission.Chain.LeaseGeneration || payload.Chain.Phase != admission.Chain.Phase || !slices.Equal(payload.Chain.Capabilities, admission.Chain.Capabilities) {
		return errors.New("qualification chain identity mismatch")
	}
	if payload.Chain.PreviousReceiptSHA256 != admissionSHA256 || payload.AdmissionSHA256 != admissionSHA256 {
		return errors.New("qualification does not bind admission")
	}
	admissionIssued, _ := parseUTCSecond(admission.Chain.IssuedAt)
	issued, _ := parseUTCSecond(payload.Chain.IssuedAt)
	if issued.Before(admissionIssued) {
		return errors.New("qualification predates admission")
	}
	if !validLowerHex(payload.HarnessInventorySHA256, 64, false) || !validLowerHex(payload.ResultSHA256, 64, false) {
		return errors.New("qualification lacks result evidence")
	}
	protected := containsCapability(payload.Chain.Capabilities, CapabilityGate4SourceProtected) || containsCapability(payload.Chain.Capabilities, CapabilityGate5ReleaseProtected)
	if protected {
		for _, digest := range []string{payload.BrokerEvidenceSHA256, payload.EgressReadbackSHA256, payload.DeniedProbesSHA256} {
			if !validLowerHex(digest, 64, false) {
				return errors.New("protected qualification lacks policy evidence")
			}
		}
	} else if payload.BrokerEvidenceSHA256 != zeroSHA256 || payload.EgressReadbackSHA256 != zeroSHA256 || payload.DeniedProbesSHA256 != zeroSHA256 {
		return errors.New("unprotected qualification contains protected evidence")
	}
	if err := validateControllers(payload.Controllers, payload.Chain.Capabilities); err != nil {
		return err
	}
	if err := validateMaterialisations(payload.Materialisations, admission); err != nil {
		return err
	}
	return validateArtifact(payload.Artifact, containsCapability(payload.Chain.Capabilities, CapabilityGate5ReleaseProtected))
}

func validateCleanupPayload(payload CleanupPayload, admission AdmissionPayload, admissionSHA256, parentSHA256, outcome string) error {
	if err := validateChain(payload.Chain, KindCleanup); err != nil {
		return err
	}
	if payload.Chain.Run != admission.Chain.Run || payload.Chain.LeaseGeneration != admission.Chain.LeaseGeneration || payload.Chain.Phase != admission.Chain.Phase || !slices.Equal(payload.Chain.Capabilities, admission.Chain.Capabilities) {
		return errors.New("cleanup chain identity mismatch")
	}
	if payload.Chain.PreviousReceiptSHA256 != parentSHA256 || payload.AdmissionSHA256 != admissionSHA256 || payload.Outcome != outcome {
		return errors.New("cleanup chain parent mismatch")
	}
	if outcome == "qualified" {
		if payload.QualificationSHA256 != parentSHA256 {
			return errors.New("qualified cleanup lacks qualification binding")
		}
	} else if payload.QualificationSHA256 != zeroSHA256 {
		return errors.New("aborted cleanup has qualification binding")
	}
	if !completeAbsence(payload.Absence) || !payload.BaselineRestored || payload.RestoredBaselineSHA256 != admission.BaselineSHA256 || !validLowerHex(payload.NetworkReadbackSHA256, 64, false) || payload.RollbackMode == "" {
		return errors.New("cleanup does not prove complete restoration")
	}
	return nil
}

func validateChain(chain ChainIdentity, kind ReceiptKind) error {
	if chain.Schema != ReceiptSchemaV1 || chain.Kind != kind || chain.LeaseGeneration == 0 || !validLowerHex(chain.PreviousReceiptSHA256, 64, true) || !validLowerHex(chain.Nonce, 32, false) {
		return errors.New("invalid receipt chain identity")
	}
	issuedAt, err := parseUTCSecond(chain.IssuedAt)
	if err != nil {
		return err
	}
	expiresAt, err := parseUTCSecond(chain.ExpiresAt)
	if err != nil || !issuedAt.Before(expiresAt) {
		return errors.New("invalid receipt time interval")
	}
	if err := validateRun(chain.Run); err != nil {
		return err
	}
	if len(chain.Capabilities) == 0 || !sortedUnique(chain.Capabilities) {
		return errors.New("receipt capabilities are not sorted uniquely")
	}
	expected, ok := phaseCapabilities(chain.Phase)
	if !ok || !slices.Equal(chain.Capabilities, expected) {
		return errors.New("receipt phase and capabilities disagree")
	}
	return nil
}

func validateRun(run RunIdentity) error {
	if run.Repository == "" || run.Workflow == "" || run.Job == "" || run.RunID == 0 || run.RunAttempt == 0 || !validLowerHex(run.Commit, 40, false) {
		return errors.New("invalid workflow run identity")
	}
	return nil
}

func phaseCapabilities(phase Phase) ([]Capability, bool) {
	switch phase {
	case PhaseGate1:
		return []Capability{CapabilityGate1Native}, true
	case PhaseGate2, PhaseGate3:
		return []Capability{CapabilityGate1Native, CapabilityNativeInteractive}, true
	case PhaseGate4:
		return []Capability{CapabilityGate1Native, CapabilityGate4SourceProtected}, true
	case PhaseGate5:
		return []Capability{CapabilityGate1Native, CapabilityGate5ReleaseProtected}, true
	default:
		return nil, false
	}
}

func validateInventoryForCapabilities(payload AdmissionPayload) error {
	switch payload.Chain.Phase {
	case PhaseGate1:
		if !absentNativeInventory(payload.NativeInteractive) || !absentProtected(payload.Protected) || !absentOptionalFile(payload.PhaseDriver) {
			return errors.New("Gate 1 admission contains later-phase inventory")
		}
	case PhaseGate2, PhaseGate3:
		if err := validateNativeInventory(payload.NativeInteractive, payload.Architecture); err != nil {
			return err
		}
		if !absentProtected(payload.Protected) || !payload.PhaseDriver.Present || validateFile(payload.PhaseDriver.Value, payload.Architecture) != nil || payload.PhaseDriver.Value.OpaqueID != "cq-windows-gate23" {
			return errors.New("native-interactive admission inventory mismatch")
		}
	case PhaseGate4, PhaseGate5:
		if !absentNativeInventory(payload.NativeInteractive) || !payload.PhaseDriver.Present || validateFile(payload.PhaseDriver.Value, payload.Architecture) != nil {
			return errors.New("protected admission has invalid phase driver")
		}
		if err := validateProtected(payload.Protected, payload.Architecture, payload.Chain.Phase == PhaseGate5); err != nil {
			return err
		}
	}
	if (payload.Chain.Phase == PhaseGate3 || payload.Chain.Phase == PhaseGate4) != payload.TCPTableSDKLayoutVerifier.Present {
		return errors.New("SDK layout verifier phase mismatch")
	}
	if payload.TCPTableSDKLayoutVerifier.Present {
		if err := validateSDKVerifier(payload.TCPTableSDKLayoutVerifier, payload.NativeTestHarness, payload.Architecture); err != nil {
			return err
		}
	} else if !absentSDK(payload.TCPTableSDKLayoutVerifier) {
		return errors.New("absent SDK layout verifier is not canonical")
	}
	return validateArtifact(payload.Artifact, payload.Chain.Phase == PhaseGate5)
}

func validateTreeBinding(role string, tree TreeBinding, architecture Architecture) error {
	if err := validateRoot(tree.Root); err != nil {
		return err
	}
	entryLimit := 4096
	sizeLimit := int64(2 << 30)
	if role == "go-toolchain" {
		entryLimit = 32768
		sizeLimit = 8 << 30
	}
	if len(tree.Entries) == 0 || len(tree.Entries) > entryLimit || tree.TotalSize < 0 || tree.TotalSize > sizeLimit {
		return fmt.Errorf("%s tree is outside bounds", role)
	}
	var total int64
	previous := ""
	goCount := 0
	cacheCount := 0
	for _, entry := range tree.Entries {
		if entry.RelativePath == "" || path.IsAbs(entry.RelativePath) || path.Clean(entry.RelativePath) != entry.RelativePath || strings.Contains(entry.RelativePath, "\\") || strings.HasPrefix(entry.RelativePath, "../") || entry.RelativePath <= previous {
			return fmt.Errorf("%s tree paths are not canonical and sorted", role)
		}
		previous = entry.RelativePath
		if entry.Size < 0 || !validLowerHex(entry.SHA256, 64, false) || entry.VolumeSerial == 0 || entry.FileID == "" || entry.Links != 1 || entry.ReparseTag != 0 || !validLowerHex(entry.OwnerSIDHash, 64, false) || entry.SecurityClass == "" || !validLowerHex(entry.DACL_SHA256, 64, false) {
			return fmt.Errorf("%s tree entry is invalid", role)
		}
		if entry.Size > sizeLimit-total {
			return fmt.Errorf("%s tree total size overflows", role)
		}
		total += entry.Size
		if role == "go-toolchain" {
			if entry.RelativePath == "bin/go.exe" {
				goCount++
			}
			if strings.HasPrefix(entry.RelativePath, "cq-module-cache-v1/") {
				cacheCount++
				if entry.SecurityClass != "cq-read-only-tree-v1" {
					return errors.New("Go module cache entry is not read-only")
				}
			}
		}
	}
	if total != tree.TotalSize {
		return fmt.Errorf("%s tree total size mismatch", role)
	}
	canonicalEntries, err := canonicalJSON(tree.Entries)
	if err != nil || tree.ManifestSHA256 != receiptDigest(canonicalEntries) {
		return fmt.Errorf("%s tree manifest mismatch", role)
	}
	if role == "go-toolchain" && (goCount != 1 || cacheCount == 0) {
		return errors.New("Go toolchain lacks unique go.exe or module cache")
	}
	_ = architecture
	return nil
}

func validateSDKVerifier(verifier WindowsSDKLayoutVerifierBinding, harness TreeBinding, architecture Architecture) error {
	if !verifier.Present || !verifier.Executable.Present || validateFile(verifier.Executable.Value, architecture) != nil {
		return errors.New("invalid SDK layout verifier executable")
	}
	for _, digest := range []string{verifier.CSourceSHA256, verifier.CompilerToolchainSHA256, verifier.WindowsSDKHeadersSHA256, verifier.BuildProvenanceSHA256, verifier.OutputSchemaSHA256} {
		if !validLowerHex(digest, 64, false) {
			return errors.New("invalid SDK layout verifier provenance")
		}
	}
	for _, entry := range harness.Entries {
		if entry.RelativePath == "cq-windows-tcp-sdk-layout.exe" && fileMatchesEntry(verifier.Executable.Value, entry) {
			return nil
		}
	}
	return errors.New("SDK layout verifier is not uniquely bound to harness")
}

func validateArtifact(artifact ArtifactBinding, required bool) error {
	if !required {
		if artifact != (ArtifactBinding{}) {
			return errors.New("artifact is present outside Gate 5")
		}
		return nil
	}
	if !artifact.Present || artifact.NumericID == 0 || !validLowerHex(artifact.GitHubArtifactSHA256, 64, false) || !validLowerHex(artifact.BundleSHA256, 64, false) || !validLowerHex(artifact.ReleaseManifestSHA256, 64, false) {
		return errors.New("Gate 5 artifact binding is incomplete")
	}
	if artifact.GitHubArtifactSHA256 == artifact.BundleSHA256 || artifact.GitHubArtifactSHA256 == artifact.ReleaseManifestSHA256 || artifact.BundleSHA256 == artifact.ReleaseManifestSHA256 {
		return errors.New("Gate 5 artifact digests alias")
	}
	return nil
}

func validateRoot(root RootBinding) error {
	if root.Kind == "" || root.OpaqueID == "" || !validLowerHex(root.PathSHA256, 64, false) || root.VolumeSerial == 0 || root.FileID == "" || !validLowerHex(root.OwnerSIDHash, 64, false) || root.SecurityClass == "" || !validLowerHex(root.DACL_SHA256, 64, false) {
		return errors.New("invalid retained root binding")
	}
	return nil
}

func validateFile(file FileBinding, architecture Architecture) error {
	if file.OpaqueID == "" || file.Size <= 0 || !validLowerHex(file.SHA256, 64, false) || file.VolumeSerial == 0 || file.FileID == "" || file.Links != 1 || file.ReparseTag != 0 || !validLowerHex(file.SourceInventorySHA256, 64, false) || !validLowerHex(file.OwnerSIDHash, 64, false) || file.SecurityClass == "" || !validLowerHex(file.DACL_SHA256, 64, false) {
		return errors.New("invalid retained file binding")
	}
	if architecture != "" && file.PEMachine != machineForArchitecture(architecture) {
		return errors.New("file PE architecture mismatch")
	}
	if architecture == "" && file.PEMachine != 0 {
		return errors.New("non-executable file has PE machine")
	}
	return nil
}

func validateToken(token TokenBinding, interactive bool) error {
	if token.OpaqueID == "" || !validLowerHex(token.TokenUserSHA256, 64, false) || !validLowerHex(token.LogonSIDHash, 64, false) || token.ElevationType == "" || token.IntegrityRID == 0 {
		return errors.New("invalid token binding")
	}
	if interactive && (token.SessionID == 0 || token.ElevationType != "Default" || token.IntegrityRID != 0x2000) {
		return errors.New("interactive controller is not ordinary medium integrity")
	}
	return nil
}

func validateNativeInventory(inventory NativeInteractiveInventory, architecture Architecture) error {
	if !inventory.Present || !validLowerHex(inventory.PrincipalInventorySHA256, 64, false) || !validLowerHex(inventory.SessionInventorySHA256, 64, false) || !inventory.ControllerLauncher.Present || validateFile(inventory.ControllerLauncher.Value, architecture) != nil || inventory.LauncherProtocolVersion == 0 || !validLowerHex(inventory.LauncherConfigurationSHA256, 64, false) {
		return errors.New("invalid native-interactive inventory")
	}
	return nil
}

func validateProtected(inventory ProtectedInventory, architecture Architecture, gate5 bool) error {
	if !inventory.Present || !validLowerHex(inventory.PrincipalInventorySHA256, 64, false) || !validLowerHex(inventory.SessionInventorySHA256, 64, false) || inventory.BrokerProtocolVersion == 0 {
		return errors.New("invalid protected inventory")
	}
	for _, file := range []OptionalFileBinding{inventory.ControllerLauncher, inventory.Broker, inventory.SecureDesktopAdapter, inventory.CodexFixture} {
		if !file.Present || validateFile(file.Value, architecture) != nil {
			return errors.New("protected inventory lacks executable")
		}
	}
	for _, digest := range []string{inventory.BrokerConfigurationSHA256, inventory.CodexManifestSHA256, inventory.EgressPolicySHA256} {
		if !validLowerHex(digest, 64, false) {
			return errors.New("protected inventory lacks digest")
		}
	}
	if gate5 != inventory.ArtifactMaterialiser.Present {
		return errors.New("artifact materialiser phase mismatch")
	}
	if gate5 && validateFile(inventory.ArtifactMaterialiser.Value, architecture) != nil {
		return errors.New("invalid artifact materialiser")
	}
	return nil
}

func validateControllers(controllers []ControllerGeneration, capabilities []Capability) error {
	if containsCapability(capabilities, CapabilityNativeInteractive) {
		if len(controllers) != 1 || controllers[0].Role != "interactive-user" {
			return errors.New("native-interactive qualification requires one controller")
		}
		return validateController(controllers[0], true)
	}
	if containsCapability(capabilities, CapabilityGate4SourceProtected) || containsCapability(capabilities, CapabilityGate5ReleaseProtected) {
		if len(controllers) != 2 || controllers[0].Role != "limited-admin" || controllers[1].Role != "standard-user" {
			return errors.New("protected qualification requires two ordered controllers")
		}
		for _, controller := range controllers {
			if err := validateController(controller, false); err != nil {
				return err
			}
		}
		return nil
	}
	if len(controllers) != 0 {
		return errors.New("Gate 1 qualification has controllers")
	}
	return nil
}

func validateController(controller ControllerGeneration, ordinary bool) error {
	if controller.Role == "" || controller.OpaqueID == "" || controller.PID == 0 || controller.CreationTime == 0 {
		return errors.New("invalid controller generation")
	}
	if err := validateToken(controller.Token, ordinary); err != nil {
		return err
	}
	for _, root := range []RootBinding{controller.Profile, controller.RoamingAppData, controller.LocalAppData} {
		if err := validateRoot(root); err != nil {
			return err
		}
	}
	return nil
}

func validateMaterialisations(materialisations []MaterialisationBinding, admission AdmissionPayload) error {
	expected := []string{"cq-source", "cq.exe-source", "go-toolchain", "native-test-harness"}
	if admission.Architecture == ArchitectureAMD64 {
		expected = append(expected, "race-runtime")
	}
	if containsCapability(admission.Chain.Capabilities, CapabilityGate4SourceProtected) {
		expected = append(expected, "cq-wfp-helper-source", "codex-fixture", "phase-results")
	}
	if containsCapability(admission.Chain.Capabilities, CapabilityGate5ReleaseProtected) {
		expected = []string{"cq-source", "cq.exe-source", "go-toolchain", "native-test-harness", "source-suite-results", "cq-archive", "cq.exe", "cq-wfp-helper.exe", "codex-fixture", "release-manifest", "archive-results"}
		if admission.Architecture == ArchitectureAMD64 {
			expected = append(expected, "race-runtime")
		}
	}
	slices.Sort(expected)
	roles := make([]string, len(materialisations))
	for index, materialisation := range materialisations {
		roles[index] = materialisation.Role
		if index > 0 && roles[index] <= roles[index-1] {
			return errors.New("materialisations are not sorted uniquely")
		}
		if materialisation.Kind == "file" {
			if !materialisation.File.Present || !absentOptionalTree(materialisation.Tree) || validateFile(materialisation.File.Value, admission.Architecture) != nil {
				return errors.New("invalid file materialisation")
			}
		} else if materialisation.Kind == "tree" {
			if !materialisation.Tree.Present || !absentOptionalFile(materialisation.File) || validateTreeBinding(materialisation.Role, materialisation.Tree.Value, admission.Architecture) != nil {
				return errors.New("invalid tree materialisation")
			}
		} else {
			return errors.New("invalid materialisation kind")
		}
	}
	if !slices.Equal(roles, expected) {
		return errors.New("qualification materialisation roles mismatch")
	}
	return nil
}

func validateBrokerPrompt(prompt BrokerPromptEvidence) error {
	if prompt.CaseID == "" || prompt.ControllerRole == "" || prompt.ControllerOpaqueID == "" || prompt.ControllerPID == 0 || prompt.ControllerCreationTime == 0 || prompt.HelperPID == 0 || prompt.HelperCreationTime == 0 || !validLowerHex(prompt.PromptNonce, 32, false) {
		return errors.New("invalid broker prompt identity")
	}
	if err := validateFile(prompt.Helper, architectureFromMachine(prompt.Helper.PEMachine)); err != nil {
		return err
	}
	if err := validateToken(prompt.HelperToken, false); err != nil {
		return err
	}
	requested, err := parseUTCSecond(prompt.RequestedAt)
	if err != nil {
		return err
	}
	decided, err := parseUTCSecond(prompt.DecidedAt)
	if err != nil || decided.Before(requested) {
		return errors.New("invalid broker prompt timing")
	}
	if prompt.RequestCount != 1 || prompt.DecisionCount != 1 || !prompt.SecureDesktop || !prompt.RunAsVerb || prompt.Decision != "approved" || prompt.DirectLaunchCount != 0 {
		return errors.New("broker prompt lacks singular secure approval")
	}
	expectedKind := "credential"
	if prompt.ControllerRole == "limited-admin" {
		expectedKind = "consent"
	} else if prompt.ControllerRole != "standard-user" {
		return errors.New("unknown broker controller role")
	}
	if prompt.PromptKind != expectedKind {
		return errors.New("broker prompt kind mismatch")
	}
	return nil
}

func verifyReconciledPriorCleanup(receipt SignedCleanup, trust TrustConfig, nextGeneration uint64) (string, error) {
	if err := verifyCleanupSignature(receipt, trust); err != nil {
		return "", err
	}
	if err := validateChain(receipt.Payload.Chain, KindCleanup); err != nil {
		return "", err
	}
	if receipt.Payload.Chain.LeaseGeneration+1 != nextGeneration || (receipt.Payload.Outcome != "qualified" && receipt.Payload.Outcome != "aborted") || !completeAbsence(receipt.Payload.Absence) || !receipt.Payload.BaselineRestored || !validLowerHex(receipt.Payload.RestoredBaselineSHA256, 64, false) || !validLowerHex(receipt.Payload.NetworkReadbackSHA256, 64, false) {
		return "", errors.New("prior cleanup is not fully reconciled")
	}
	canonical, err := CanonicalCleanup(receipt)
	if err != nil {
		return "", err
	}
	return receiptDigest(canonical), nil
}

func verifyAdmissionSignature(receipt SignedAdmission, trust TrustConfig) error {
	payload, err := canonicalAdmissionPayload(receipt.Payload)
	if err != nil {
		return err
	}
	return verifySignature(KindAdmission, payload, receipt.KeyID, receipt.Signature, trust)
}

func verifyQualificationSignature(receipt SignedQualification, trust TrustConfig) error {
	payload, err := canonicalQualificationPayload(receipt.Payload)
	if err != nil {
		return err
	}
	return verifySignature(KindQualification, payload, receipt.KeyID, receipt.Signature, trust)
}

func verifyCleanupSignature(receipt SignedCleanup, trust TrustConfig) error {
	payload, err := canonicalCleanupPayload(receipt.Payload)
	if err != nil {
		return err
	}
	return verifySignature(KindCleanup, payload, receipt.KeyID, receipt.Signature, trust)
}

func verifySignature(kind ReceiptKind, payload []byte, keyID, encodedSignature string, trust TrustConfig) error {
	if keyID != trust.KeyID || !validLowerHex(encodedSignature, ed25519.SignatureSize*2, false) {
		return errors.New("receipt signer mismatch")
	}
	signature, _ := hex.DecodeString(encodedSignature)
	if !ed25519.Verify(trust.PublicKey, receiptSignatureInput(kind, payload), signature) {
		return errors.New("receipt signature is invalid")
	}
	return nil
}

func completeAbsence(absence AbsenceSet) bool {
	return absence.Accounts && absence.Profiles && absence.Sessions && absence.Processes && absence.Pipes && absence.Tasks && absence.AppContainers && absence.WFPObjects && absence.LoopbackExemptions && absence.NetworkPolicyDelta && absence.Certificates && absence.Roots && absence.RemoteMappings
}

func absentOptionalFile(value OptionalFileBinding) bool {
	return !value.Present && value.Value == (FileBinding{})
}

func absentOptionalTree(value OptionalTreeBinding) bool {
	return !value.Present && value.Value.Root == (RootBinding{}) && len(value.Value.Entries) == 0 && value.Value.ManifestSHA256 == "" && value.Value.TotalSize == 0
}

func absentNativeInventory(value NativeInteractiveInventory) bool {
	return !value.Present && value.PrincipalInventorySHA256 == zeroSHA256 && value.SessionInventorySHA256 == zeroSHA256 && absentOptionalFile(value.ControllerLauncher) && value.LauncherProtocolVersion == 0 && value.LauncherConfigurationSHA256 == zeroSHA256
}

func absentProtected(value ProtectedInventory) bool {
	return !value.Present && value.PrincipalInventorySHA256 == zeroSHA256 && value.SessionInventorySHA256 == zeroSHA256 && absentOptionalFile(value.ControllerLauncher) && absentOptionalFile(value.Broker) && absentOptionalFile(value.SecureDesktopAdapter) && value.BrokerProtocolVersion == 0 && value.BrokerConfigurationSHA256 == zeroSHA256 && absentOptionalFile(value.CodexFixture) && value.CodexManifestSHA256 == zeroSHA256 && absentOptionalFile(value.ArtifactMaterialiser) && value.EgressPolicySHA256 == zeroSHA256
}

func absentSDK(value WindowsSDKLayoutVerifierBinding) bool {
	return !value.Present && absentOptionalFile(value.Executable) && value.CSourceSHA256 == zeroSHA256 && value.CompilerToolchainSHA256 == zeroSHA256 && value.WindowsSDKHeadersSHA256 == zeroSHA256 && value.BuildProvenanceSHA256 == zeroSHA256 && value.OutputSchemaSHA256 == zeroSHA256
}

func fileMatchesEntry(file FileBinding, entry TreeEntryBinding) bool {
	return file.Size == entry.Size && file.SHA256 == entry.SHA256 && file.VolumeSerial == entry.VolumeSerial && file.FileID == entry.FileID && file.Links == entry.Links && file.ReparseTag == entry.ReparseTag && file.OwnerSIDHash == entry.OwnerSIDHash && file.SecurityClass == entry.SecurityClass && file.DACL_SHA256 == entry.DACL_SHA256
}

func machineForArchitecture(architecture Architecture) uint16 {
	if architecture == ArchitectureAMD64 {
		return 0x8664
	}
	if architecture == ArchitectureARM64 {
		return 0xaa64
	}
	return 0
}

func architectureFromMachine(machine uint16) Architecture {
	if machine == 0x8664 {
		return ArchitectureAMD64
	}
	if machine == 0xaa64 {
		return ArchitectureARM64
	}
	return ""
}

func containsCapability(capabilities []Capability, capability Capability) bool {
	return slices.Contains(capabilities, capability)
}
