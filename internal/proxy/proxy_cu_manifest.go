package proxy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const maxCUManifestBytes = 64 << 10
const maxCUCaptureStreamBytes = 8 << 20

var (
	cuIDPattern                  = regexp.MustCompile(`^CU-[0-9]$`)
	cuPackagePattern             = regexp.MustCompile(`^\./[A-Za-z0-9_./-]+$`)
	cuTopTestPattern             = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
	cuFullTestPattern            = regexp.MustCompile(`^Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_.-]+)*$`)
	releaseCommandPurposePattern = regexp.MustCompile(`^(?:ancestry|release-(?:floor|target)-(?:build|vet|race)|role-(?:launcher|supervisor|worker)-build|verify-CU-[0-9])$`)
	decimalPattern               = regexp.MustCompile(`^(?:0|[1-9][0-9]*)$`)
)

const frozenReviewAggregateSHA256 = "3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be"

// CUManifestV1 is the closed manifest header shared by every construction
// unit. Unit-specific test selections are added after the header validates.
type CUManifestV1 struct {
	SchemaVersion                    int               `json:"schema_version"`
	Kind                             string            `json:"kind"`
	BlueprintSHA256                  string            `json:"blueprint_sha256"`
	ReviewAttestationAggregateSHA256 string            `json:"review_attestation_aggregate_sha256"`
	ReviewAuthorityBaselineCommit    string            `json:"review_authority_baseline_commit"`
	Unit                             string            `json:"unit"`
	RaceCount                        int               `json:"race_count"`
	Packages                         []CUTestPackageV1 `json:"packages"`
}

// CUTestPackageV1 names the exact test events required from one package.
type CUTestPackageV1 struct {
	Package          string   `json:"package"`
	TopLevelTests    []string `json:"top_level_tests"`
	FullTestIDs      []string `json:"full_test_ids"`
	MinimumPassCount int      `json:"minimum_pass_count"`
}

// CanonicalCUManifestV1 returns the exact checked-in manifest bytes for one
// construction unit. A later unit remains unavailable until it adds a
// non-empty exact selection here.
func CanonicalCUManifestV1(cuID string) ([]byte, error) {
	if cuID != "CU-0" {
		return nil, fmt.Errorf("construction unit %q has no manifest", cuID)
	}
	cmdSelection := newCUTestPackage("./cmd/cq",
		"TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState",
		"TestGlobalHelpAndVersionPreserveAbsentHomeAndXDGEnvironment",
		"TestManualHelpTextDocumentsEachCommandPath",
		"TestPureGlobalInspectionHandlesOrdinaryUsageBeforeCompatibility",
		"TestPureGlobalInspectionPreservesBareHelpUsageError",
		"TestPureGlobalInspectionPropagatesHelpWriteError",
		"TestRootHelpShowsFullCLISurface",
		"TestRunModelsHelpDoesNotRefresh",
		"TestRunProxyCodexDefaultHelpDoesNotCreateConfig",
	)
	cmdSelection.FullTestIDs = []string{
		"TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState",
		"TestGlobalHelpAndVersionPreserveAbsentHomeAndXDGEnvironment",
		"TestManualHelpTextDocumentsEachCommandPath",
		"TestManualHelpTextDocumentsEachCommandPath/agent",
		"TestManualHelpTextDocumentsEachCommandPath/models_list",
		"TestManualHelpTextDocumentsEachCommandPath/models_overlay_add",
		"TestManualHelpTextDocumentsEachCommandPath/models_overlay_remove",
		"TestManualHelpTextDocumentsEachCommandPath/proxy_codex_default",
		"TestManualHelpTextDocumentsEachCommandPath/proxy_pin",
		"TestManualHelpTextDocumentsEachCommandPath/proxy_prime",
		"TestManualHelpTextDocumentsEachCommandPath/proxy_start",
		"TestManualHelpTextDocumentsEachCommandPath/proxy_validate_HTTP",
		"TestManualHelpTextDocumentsEachCommandPath/refresh",
		"TestPureGlobalInspectionHandlesOrdinaryUsageBeforeCompatibility",
		"TestPureGlobalInspectionPreservesBareHelpUsageError",
		"TestPureGlobalInspectionPropagatesHelpWriteError",
		"TestRootHelpShowsFullCLISurface",
		"TestRunModelsHelpDoesNotRefresh",
		"TestRunProxyCodexDefaultHelpDoesNotCreateConfig",
	}
	cmdSelection.MinimumPassCount = len(cmdSelection.FullTestIDs)
	proxySelection := newCUTestPackage("./internal/proxy",
		"TestAppendCanonicalJSONUsesECMAScriptNumberSerialisation",
		"TestBlueprintReviewCapOrderingVectors",
		"TestBlueprintReviewNegativeVectorMatrix",
		"TestBlueprintReviewPositiveVectorMatrix",
		"TestBlueprintReviewStreamingAndDigestVectors",
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming",
		"TestBuildEnvironmentDigestV1RejectsToolchainAndSecretDrift",
		"TestCanonicalCUManifestV1ProvidesPinnedCU0Selection",
		"TestCanonicalJSONV1RejectsNonFiniteNumber",
		"TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping",
		"TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory",
		"TestExecutionResultDigestV1MatchesLiteralFraming",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture",
		"TestNewCUReportV1TreatsForgedReportOutputAsCapturedBytes",
		"TestParseBlueprintReviewResultAcceptsCleanAndNotClean",
		"TestParseBlueprintReviewResultRejectsAbsentOrStaleAuthority",
		"TestParseBlueprintReviewResultRejectsFindingOrderAndOversizeInput",
		"TestParseBlueprintReviewSiblingAcceptsOneByteStreaming",
		"TestParseBlueprintReviewSiblingEnforcesRecordCapBeforeDecodeAndJCS",
		"TestParseBlueprintReviewSiblingRejectsRecordDigestCorruption",
		"TestParseCUManifestAcceptsClosedCU0Selection",
		"TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount",
		"TestParseCUManifestRejectsUnknownMemberBeforeDispatch",
		"TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality",
		"TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality",
		"TestVerifyBlueprintReviewAcceptsFrozenRound44",
		"TestVerifyBlueprintReviewRejectsSymlinkAuthorityFiles",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles",
		"TestVerifyReleaseGraphV1AcceptsSignedFloorAndRejectsSubstitution",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry",
		"TestVerifyReleaseGraphV1RejectsIncompleteGraph",
	)
	proxySelection.FullTestIDs = append(proxySelection.FullTestIDs,
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming/extra",
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming/missing",
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming/reordered",
		"TestBlueprintReviewCapOrderingVectors/record_over_cap_before_decode",
		"TestBlueprintReviewCapOrderingVectors/result_over_cap_before_decode",
		"TestBlueprintReviewCapOrderingVectors/sibling_over_cap_before_decode",
		"TestBlueprintReviewNegativeVectorMatrix/ASCII_hex_variant",
		"TestBlueprintReviewNegativeVectorMatrix/BOM",
		"TestBlueprintReviewNegativeVectorMatrix/clean_nonempty",
		"TestBlueprintReviewNegativeVectorMatrix/digest_mismatch",
		"TestBlueprintReviewNegativeVectorMatrix/doubled_final_lf",
		"TestBlueprintReviewNegativeVectorMatrix/duplicate_finding_id",
		"TestBlueprintReviewNegativeVectorMatrix/duplicate_member",
		"TestBlueprintReviewNegativeVectorMatrix/empty_finding_member",
		"TestBlueprintReviewNegativeVectorMatrix/empty_task_label",
		"TestBlueprintReviewNegativeVectorMatrix/every_truncation_boundary",
		"TestBlueprintReviewNegativeVectorMatrix/finding_65",
		"TestBlueprintReviewNegativeVectorMatrix/integer_width",
		"TestBlueprintReviewNegativeVectorMatrix/invalid_finding_id",
		"TestBlueprintReviewNegativeVectorMatrix/invalid_task_grammar",
		"TestBlueprintReviewNegativeVectorMatrix/invalid_unicode",
		"TestBlueprintReviewNegativeVectorMatrix/key_order_variant",
		"TestBlueprintReviewNegativeVectorMatrix/later_blueprint_edit",
		"TestBlueprintReviewNegativeVectorMatrix/lens_enum_drift",
		"TestBlueprintReviewNegativeVectorMatrix/malformed_timestamp",
		"TestBlueprintReviewNegativeVectorMatrix/missing_final_lf",
		"TestBlueprintReviewNegativeVectorMatrix/missing_kind",
		"TestBlueprintReviewNegativeVectorMatrix/noncanonical_order",
		"TestBlueprintReviewNegativeVectorMatrix/nonterminal_lf",
		"TestBlueprintReviewNegativeVectorMatrix/not_clean_empty",
		"TestBlueprintReviewNegativeVectorMatrix/oversized_finding_member",
		"TestBlueprintReviewNegativeVectorMatrix/record_559_bytes",
		"TestBlueprintReviewNegativeVectorMatrix/record_digest_mismatch",
		"TestBlueprintReviewNegativeVectorMatrix/record_order_variant",
		"TestBlueprintReviewNegativeVectorMatrix/result_2097153_bytes",
		"TestBlueprintReviewNegativeVectorMatrix/reused_task_label",
		"TestBlueprintReviewNegativeVectorMatrix/round_above_safe",
		"TestBlueprintReviewNegativeVectorMatrix/round_zero",
		"TestBlueprintReviewNegativeVectorMatrix/schema_mismatch",
		"TestBlueprintReviewNegativeVectorMatrix/sibling_4290_bytes",
		"TestBlueprintReviewNegativeVectorMatrix/stale_baseline",
		"TestBlueprintReviewNegativeVectorMatrix/stale_blueprint",
		"TestBlueprintReviewNegativeVectorMatrix/task_label_257",
		"TestBlueprintReviewNegativeVectorMatrix/trailing_byte",
		"TestBlueprintReviewNegativeVectorMatrix/uint64_endian_variant",
		"TestBlueprintReviewNegativeVectorMatrix/unicode_normalisation",
		"TestBlueprintReviewNegativeVectorMatrix/unicode_task_label",
		"TestBlueprintReviewNegativeVectorMatrix/unknown_top_member",
		"TestBlueprintReviewNegativeVectorMatrix/verdict_enum_drift",
		"TestBlueprintReviewNegativeVectorMatrix/whitespace",
		"TestBlueprintReviewNegativeVectorMatrix/wrong_kind",
		"TestBlueprintReviewNegativeVectorMatrix/wrong_severity_order",
		"TestBlueprintReviewPositiveVectorMatrix/canonical_clean",
		"TestBlueprintReviewPositiveVectorMatrix/exact_4289_byte_sibling",
		"TestBlueprintReviewPositiveVectorMatrix/exact_558_byte_record",
		"TestBlueprintReviewPositiveVectorMatrix/maximum_legal_members",
		"TestBlueprintReviewPositiveVectorMatrix/not_clean_critical",
		"TestBlueprintReviewPositiveVectorMatrix/not_clean_high",
		"TestBlueprintReviewPositiveVectorMatrix/not_clean_low",
		"TestBlueprintReviewPositiveVectorMatrix/not_clean_medium",
		"TestBlueprintReviewPositiveVectorMatrix/one_byte_task_labels",
		"TestBlueprintReviewPositiveVectorMatrix/one_finding",
		"TestBlueprintReviewPositiveVectorMatrix/sixty_four_findings",
		"TestBlueprintReviewPositiveVectorMatrix/two_hundred_fifty_six_byte_task_labels",
		"TestBuildEnvironmentDigestV1RejectsToolchainAndSecretDrift/locale",
		"TestBuildEnvironmentDigestV1RejectsToolchainAndSecretDrift/secret",
		"TestBuildEnvironmentDigestV1RejectsToolchainAndSecretDrift/timezone",
		"TestBuildEnvironmentDigestV1RejectsToolchainAndSecretDrift/toolchain",
		"TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory/absolute_cwd",
		"TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory/empty_argv",
		"TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory/nul_argv",
		"TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory/purpose",
		"TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory/traversal_cwd",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/nonzero",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/oversize",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/race_disabled",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/signal",
		"TestParseBlueprintReviewResultRejectsAbsentOrStaleAuthority/absent_baseline",
		"TestParseBlueprintReviewResultRejectsAbsentOrStaleAuthority/absent_blueprint",
		"TestParseBlueprintReviewResultRejectsAbsentOrStaleAuthority/stale_baseline",
		"TestParseBlueprintReviewResultRejectsAbsentOrStaleAuthority/stale_blueprint",
		"TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount/duplicate_test",
		"TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount/empty_selection",
		"TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount/race_count",
		"TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality/floor_missing",
		"TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality/floor_plus_one",
		"TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality/target_reordered",
		"TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality/directory",
		"TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality/missing",
		"TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality/plus_one",
		"TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality/reordered",
		"TestVerifyBlueprintReviewRejectsSymlinkAuthorityFiles/blueprint",
		"TestVerifyBlueprintReviewRejectsSymlinkAuthorityFiles/sibling",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/exact_floor",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/manifest_substitution",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/missing_file",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/nested_directory",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/payload_substitution",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/symlink",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/unknown_file",
		"TestVerifyReleaseGraphV1AcceptsSignedFloorAndRejectsSubstitution/bundle_digest",
		"TestVerifyReleaseGraphV1AcceptsSignedFloorAndRejectsSubstitution/report_source",
		"TestVerifyReleaseGraphV1AcceptsSignedFloorAndRejectsSubstitution/role_payload",
		"TestVerifyReleaseGraphV1AcceptsSignedFloorAndRejectsSubstitution/signature",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry/ABI_substitution",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry/CU_missing",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry/ancestry_signature",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry/missing_ancestry",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry/role_swap",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry/same_source",
		"TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry/wrong_merge_base",
	)
	sort.Strings(proxySelection.FullTestIDs)
	proxySelection.MinimumPassCount = len(proxySelection.FullTestIDs)
	proxyCUSelection := newCUTestPackage("./internal/tools/proxycu",
		"TestNewOSCommandRunnerClosesAmbientGoEnvironment",
		"TestOSCommandRunnerContainsPipeHoldingGrandchild",
		"TestRunRejectsAbsentAndUnmanifestedCU",
		"TestShellWrappersCloseAmbientGoConfiguration",
		"TestShellWrappersVerifyFrozenReviewAndSelfTest",
		"TestVerifyTestEventsAcceptsExactRunAndPass",
		"TestVerifyTestEventsRejectsCorruptEvidence",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution",
		"TestVerifyUnitRejectsChildFailure",
		"TestVerifyUnitRunsListThenExactRaceSelection",
	)
	proxyCUSelection.FullTestIDs = append(proxyCUSelection.FullTestIDs,
		"TestVerifyTestEventsRejectsCorruptEvidence/absent",
		"TestVerifyTestEventsRejectsCorruptEvidence/duplicate",
		"TestVerifyTestEventsRejectsCorruptEvidence/extra",
		"TestVerifyTestEventsRejectsCorruptEvidence/malformed",
		"TestVerifyTestEventsRejectsCorruptEvidence/no_tests",
		"TestVerifyTestEventsRejectsCorruptEvidence/race_disabled",
		"TestVerifyTestEventsRejectsCorruptEvidence/skipped",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/blank_line",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/concatenated_object",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/duplicate_terminal",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/missing_terminal",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/package_substitution",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/padded_object",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/whitespace_line",
	)
	sort.Strings(proxyCUSelection.FullTestIDs)
	proxyCUSelection.MinimumPassCount = len(proxyCUSelection.FullTestIDs)
	releaseSelection := newCUTestPackage("./internal/tools/proxyrelease",
		"TestBuildProxyReleaseShellEntryRejectsMissingManifest",
		"TestCaptureConstructionUnitBindsArgvStdoutStderrAndReportBytes",
		"TestCaptureConstructionUnitBuildsReportOnlyFromCompletedCapture",
		"TestCaptureConstructionUnitRecoversRunnerPanic",
		"TestOSReleaseRunnerContainsPipeHoldingGrandchild",
		"TestParseReleaseBuildManifestAcceptsClosedRequestAndRejectsUnknown",
		"TestReadReleaseBuildManifestRejectsSymlink",
		"TestSourceTreeDigestV1HashesExactGitListing",
	)
	releaseSelection.FullTestIDs = append(releaseSelection.FullTestIDs,
		"TestCaptureConstructionUnitBindsArgvStdoutStderrAndReportBytes/report-shaped_bytes",
		"TestCaptureConstructionUnitBindsArgvStdoutStderrAndReportBytes/stderr",
		"TestCaptureConstructionUnitBindsArgvStdoutStderrAndReportBytes/stdout",
	)
	sort.Strings(releaseSelection.FullTestIDs)
	releaseSelection.MinimumPassCount = len(releaseSelection.FullTestIDs)
	selections := []CUTestPackageV1{
		cmdSelection,
		proxySelection,
		proxyCUSelection,
		releaseSelection,
	}
	return CanonicalJSONV1(CUManifestV1{
		SchemaVersion:                    1,
		Kind:                             "construction_unit_verification_manifest_v1",
		BlueprintSHA256:                  frozenBlueprintSHA256,
		ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256,
		ReviewAuthorityBaselineCommit:    frozenReviewBaseline,
		Unit:                             cuID,
		RaceCount:                        1,
		Packages:                         selections,
	})
}

func newCUTestPackage(packagePath string, testNames ...string) CUTestPackageV1 {
	return CUTestPackageV1{
		Package:          packagePath,
		TopLevelTests:    testNames,
		FullTestIDs:      append([]string(nil), testNames...),
		MinimumPassCount: len(testNames),
	}
}

// CUReportV1 is the accepted out-of-band report for one verifier capture.
type CUReportV1 struct {
	SchemaVersion              int    `json:"schema_version"`
	Kind                       string `json:"kind"`
	CUID                       string `json:"cu_id"`
	VerificationManifestDigest string `json:"verification_manifest_digest"`
	InvocationDigest           string `json:"invocation_digest"`
	Outcome                    string `json:"outcome"`
	ExitCode                   int32  `json:"exit_code"`
	RaceEnabled                bool   `json:"race_enabled"`
	ExecutionResultDigest      string `json:"execution_result_digest"`
	StartedAt                  string `json:"started_at"`
	EndedAt                    string `json:"ended_at"`
}

// CUReportCaptureV1 contains only builder-observed execution facts. It has no
// accepted report or report-output field, preventing wrapper self-reference.
type CUReportCaptureV1 struct {
	CUID                       string
	VerificationManifestDigest string
	InvocationDigest           string
	ExitCode                   int32
	TerminationReason          string
	RaceEnabled                bool
	Stdout                     []byte
	Stderr                     []byte
	StartedAt                  time.Time
	EndedAt                    time.Time
}

// BuildEnvironmentEntryV1 is one member of the closed release-build
// environment vector. Callers retain the blueprint's byte order.
type BuildEnvironmentEntryV1 struct {
	Key   string
	Value string
}

// ReleaseBundleEntryV1 is one non-bundle regular file named by bundle.json.
type ReleaseBundleEntryV1 struct {
	RelativePath string `json:"relative_path"`
	Kind         string `json:"kind"`
	Digest       string `json:"digest"`
	Size         uint64 `json:"size"`
}

type ReleaseBuildAuthorityV1 struct {
	SchemaVersion                    int    `json:"schema_version"`
	AuthorityID                      string `json:"authority_id"`
	Ed25519PublicKey                 string `json:"ed25519_public_key"`
	RepositoryIdentityDigest         string `json:"repository_identity_digest"`
	BlueprintSHA256                  string `json:"blueprint_sha256"`
	ReviewAttestationAggregateSHA256 string `json:"review_attestation_aggregate_sha256"`
	ReviewAuthorityBaselineCommit    string `json:"review_authority_baseline_commit"`
	LineageRootCommit                string `json:"lineage_root_commit"`
	LineageRootTreeDigest            string `json:"lineage_root_tree_digest"`
	ToolchainIdentity                string `json:"toolchain_identity"`
	CreatedAt                        string `json:"created_at"`
}

type SourceAncestryReceiptV1 struct {
	SchemaVersion               int    `json:"schema_version"`
	Kind                        string `json:"kind"`
	ReleaseBuildAuthorityDigest string `json:"release_build_authority_digest"`
	RepositoryIdentityDigest    string `json:"repository_identity_digest"`
	FloorSourceCommit           string `json:"floor_source_commit"`
	FloorSourceTreeDigest       string `json:"floor_source_tree_digest"`
	TargetSourceCommit          string `json:"target_source_commit"`
	TargetSourceTreeDigest      string `json:"target_source_tree_digest"`
	MergeBaseCommit             string `json:"merge_base_commit"`
	VerificationCommandDigest   string `json:"verification_command_digest"`
	VerifiedAt                  string `json:"verified_at"`
	SignerPublicKey             string `json:"signer_public_key"`
	Signature                   string `json:"signature,omitempty"`
}

type ReleaseRoleExecutionV1 struct {
	Role                   string `json:"role"`
	BuildCommandDigest     string `json:"build_command_digest"`
	ArtifactPayloadDigest  string `json:"artifact_payload_digest"`
	ArtifactManifestDigest string `json:"artifact_manifest_digest"`
}

type ReleaseBuildReportV1 struct {
	SchemaVersion               int                      `json:"schema_version"`
	Kind                        string                   `json:"kind"`
	Purpose                     string                   `json:"purpose"`
	ReleaseBuildAuthorityDigest string                   `json:"release_build_authority_digest"`
	SourceCommit                string                   `json:"source_commit"`
	SourceTreeDigest            string                   `json:"source_tree_digest"`
	ToolchainIdentity           string                   `json:"toolchain_identity"`
	BuildEnvironmentDigest      string                   `json:"build_environment_digest"`
	CommandDigest               string                   `json:"command_digest"`
	RoleExecutions              []ReleaseRoleExecutionV1 `json:"role_executions"`
	Outcome                     string                   `json:"outcome"`
	ExitCode                    int32                    `json:"exit_code"`
	RaceEnabled                 bool                     `json:"race_enabled"`
	ExecutionResultDigest       string                   `json:"execution_result_digest"`
	StartedAt                   string                   `json:"started_at"`
	EndedAt                     string                   `json:"ended_at"`
	SignerPublicKey             string                   `json:"signer_public_key"`
	Signature                   string                   `json:"signature,omitempty"`
}

type ConstructionUnitReportSetV1 struct {
	SchemaVersion                                 int          `json:"schema_version"`
	Kind                                          string       `json:"kind"`
	Purpose                                       string       `json:"purpose"`
	ReleaseBuildAuthorityDigest                   string       `json:"release_build_authority_digest"`
	BlueprintSHA256                               string       `json:"blueprint_sha256"`
	ReviewAttestationAggregateSHA256              string       `json:"review_attestation_aggregate_sha256"`
	ReviewAuthorityBaselineCommit                 string       `json:"review_authority_baseline_commit"`
	LegacyAtomicWriterReachabilityCatalogueDigest string       `json:"legacy_atomic_writer_reachability_catalogue_digest"`
	SourceCommit                                  string       `json:"source_commit"`
	SourceTreeDigest                              string       `json:"source_tree_digest"`
	ToolchainIdentity                             string       `json:"toolchain_identity"`
	BuildEnvironmentDigest                        string       `json:"build_environment_digest"`
	Reports                                       []CUReportV1 `json:"reports"`
	SignerPublicKey                               string       `json:"signer_public_key"`
	Signature                                     string       `json:"signature,omitempty"`
}

type ReleaseArtifactManifestV1 struct {
	SchemaVersion               int      `json:"schema_version"`
	Role                        string   `json:"role"`
	ReleaseBuildAuthorityDigest string   `json:"release_build_authority_digest"`
	SourceCommit                string   `json:"source_commit"`
	SourceTreeDigest            string   `json:"source_tree_digest"`
	ToolchainIdentity           string   `json:"toolchain_identity"`
	BuildCommandDigest          string   `json:"build_command_digest"`
	BuildEnvironmentDigest      string   `json:"build_environment_digest"`
	Architecture                string   `json:"architecture"`
	BuildID                     string   `json:"build_id"`
	SupportedFeatures           []string `json:"supported_features"`
	MinimumFloorFeatures        []string `json:"minimum_floor_features"`
	LauncherABIDigest           *string  `json:"launcher_abi_digest"`
	PrivateABIDigest            *string  `json:"private_abi_digest"`
	CodeSignatureDigest         string   `json:"code_signature_digest"`
	ArtifactPayloadDigest       string   `json:"artifact_payload_digest"`
}

type ReleaseArtifactRoleV1 struct {
	Role                   string `json:"role"`
	ArtifactPayloadDigest  string `json:"artifact_payload_digest"`
	ArtifactManifestDigest string `json:"artifact_manifest_digest"`
}

type ReleaseArtifactSetV1 struct {
	SchemaVersion                                 int                     `json:"schema_version"`
	Purpose                                       string                  `json:"purpose"`
	ReleaseBuildAuthorityDigest                   string                  `json:"release_build_authority_digest"`
	SignerPublicKey                               string                  `json:"signer_public_key"`
	SourceCommit                                  string                  `json:"source_commit"`
	SourceTreeDigest                              string                  `json:"source_tree_digest"`
	ToolchainIdentity                             string                  `json:"toolchain_identity"`
	BuildEnvironmentDigest                        string                  `json:"build_environment_digest"`
	BuildReportDigest                             string                  `json:"build_report_digest"`
	VetReportDigest                               string                  `json:"vet_report_digest"`
	RaceTestReportDigest                          string                  `json:"race_test_report_digest"`
	ConstructionUnitReportSetDigest               string                  `json:"construction_unit_report_set_digest"`
	LegacyAtomicWriterReachabilityCatalogueDigest string                  `json:"legacy_atomic_writer_reachability_catalogue_digest"`
	SourceAncestryReceiptDigest                   *string                 `json:"source_ancestry_receipt_digest"`
	LauncherABIDigest                             *string                 `json:"launcher_abi_digest"`
	RequiredLauncherABIDigest                     *string                 `json:"required_launcher_abi_digest"`
	Roles                                         []ReleaseArtifactRoleV1 `json:"roles"`
	SupportedFeatures                             []string                `json:"supported_features"`
	MinimumFloorFeatures                          []string                `json:"minimum_floor_features"`
	SetSignature                                  string                  `json:"set_signature,omitempty"`
}

type ReleaseBundleV1 struct {
	SchemaVersion                   int                    `json:"schema_version"`
	Purpose                         string                 `json:"purpose"`
	ReleaseBuildAuthorityDigest     string                 `json:"release_build_authority_digest"`
	ReleaseArtifactSetDigest        string                 `json:"release_artifact_set_digest"`
	ConstructionUnitReportSetDigest string                 `json:"construction_unit_report_set_digest"`
	SourceAncestryReceiptDigest     *string                `json:"source_ancestry_receipt_digest"`
	Entries                         []ReleaseBundleEntryV1 `json:"entries"`
	SignerPublicKey                 string                 `json:"signer_public_key"`
	BundleSignature                 string                 `json:"bundle_signature,omitempty"`
}

type ReleaseGraphV1 struct {
	Authority         ReleaseBuildAuthorityV1
	Ancestry          *SourceAncestryReceiptV1
	BuildReport       ReleaseBuildReportV1
	VetReport         ReleaseBuildReportV1
	RaceReport        ReleaseBuildReportV1
	CUReportSet       ConstructionUnitReportSetV1
	ArtifactManifests []ReleaseArtifactManifestV1
	ArtifactSet       ReleaseArtifactSetV1
	Bundle            ReleaseBundleV1
}

func VerifyReleaseGraphV1(graph ReleaseGraphV1) error {
	authority := graph.Authority
	if authority.SchemaVersion != 1 || authority.AuthorityID == "" || !lowerHex64Pattern.MatchString(authority.RepositoryIdentityDigest) || !lowerHex40Pattern.MatchString(authority.LineageRootCommit) || !lowerHex64Pattern.MatchString(authority.LineageRootTreeDigest) {
		return fmt.Errorf("release graph has invalid V1 authority identity")
	}
	if authority.BlueprintSHA256 != frozenBlueprintSHA256 || authority.ReviewAttestationAggregateSHA256 != frozenReviewAggregateSHA256 || authority.ReviewAuthorityBaselineCommit != frozenReviewBaseline {
		return fmt.Errorf("release authority does not match frozen review authority")
	}
	if authority.ToolchainIdentity != "go1.26.1 darwin/arm64" {
		return fmt.Errorf("release authority toolchain is not pinned Go 1.26.1 darwin/arm64")
	}
	if err := validateReleaseTimestamp(authority.CreatedAt, "authority created_at"); err != nil {
		return err
	}
	publicKey, err := decodeEd25519PublicKey(authority.Ed25519PublicKey)
	if err != nil {
		return err
	}
	authorityDigest, err := releaseObjectDigestV1("cq/release-build-authority/v1\x00", authority)
	if err != nil {
		return err
	}
	purpose := graph.ArtifactSet.Purpose
	if purpose != "floor" && purpose != "target" {
		return fmt.Errorf("release artifact set has invalid purpose %q", purpose)
	}
	set := graph.ArtifactSet
	if set.SchemaVersion != 1 || set.ReleaseBuildAuthorityDigest != authorityDigest || set.SignerPublicKey != authority.Ed25519PublicKey || set.ToolchainIdentity != authority.ToolchainIdentity || !lowerHex64Pattern.MatchString(set.BuildEnvironmentDigest) {
		return fmt.Errorf("release artifact set authority projection mismatch")
	}
	if err := verifyReleaseSignature(publicKey, set.SetSignature, releaseArtifactSetSignable(set)); err != nil {
		return fmt.Errorf("verify release artifact set signature: %w", err)
	}
	if err := validateReleaseFeatureList(set.SupportedFeatures, "supported_features"); err != nil {
		return err
	}
	if err := validateReleaseFeatureList(set.MinimumFloorFeatures, "minimum_floor_features"); err != nil {
		return err
	}
	expectedRoles := []string{"supervisor", "worker"}
	if purpose == "target" {
		expectedRoles = []string{"launcher", "supervisor", "worker"}
	}
	if len(set.Roles) != len(expectedRoles) || len(graph.ArtifactManifests) != len(expectedRoles) {
		return fmt.Errorf("%s release graph role cardinality mismatch", purpose)
	}
	if purpose == "floor" {
		if set.SourceAncestryReceiptDigest != nil || set.LauncherABIDigest != nil || set.RequiredLauncherABIDigest == nil || graph.Ancestry != nil || set.SourceCommit != authority.LineageRootCommit || set.SourceTreeDigest != authority.LineageRootTreeDigest {
			return fmt.Errorf("floor release graph lineage or ABI nullability mismatch")
		}
	} else {
		if set.SourceAncestryReceiptDigest == nil || set.LauncherABIDigest == nil || set.RequiredLauncherABIDigest != nil || graph.Ancestry == nil {
			return fmt.Errorf("target release graph ancestry or ABI nullability mismatch")
		}
		if err := verifySourceAncestry(graph.Ancestry, authority, authorityDigest, set, publicKey); err != nil {
			return err
		}
		ancestryDigest, err := releaseObjectDigestV1("cq/source-ancestry-receipt/v1\x00", *graph.Ancestry)
		if err != nil || *set.SourceAncestryReceiptDigest != ancestryDigest {
			return fmt.Errorf("target ancestry receipt digest mismatch")
		}
	}
	reportByKind := map[string]ReleaseBuildReportV1{
		"build": graph.BuildReport,
		"vet":   graph.VetReport,
		"race":  graph.RaceReport,
	}
	reportDigestByKind := make(map[string]string, 3)
	for _, kind := range []string{"build", "vet", "race"} {
		report := reportByKind[kind]
		if err := verifyReleaseBuildReport(report, kind, purpose, authority, authorityDigest, set, expectedRoles, publicKey); err != nil {
			return err
		}
		digest, err := releaseObjectDigestV1("cq/release-build-report/v1\x00", report)
		if err != nil {
			return err
		}
		reportDigestByKind[kind] = digest
	}
	if set.BuildReportDigest != reportDigestByKind["build"] || set.VetReportDigest != reportDigestByKind["vet"] || set.RaceTestReportDigest != reportDigestByKind["race"] {
		return fmt.Errorf("release artifact set report digest substitution")
	}
	if err := verifyCUReportSet(graph.CUReportSet, purpose, authority, authorityDigest, set, publicKey); err != nil {
		return err
	}
	cuSetDigest, err := releaseObjectDigestV1("cq/construction-unit-report-set/v1\x00", graph.CUReportSet)
	if err != nil || set.ConstructionUnitReportSetDigest != cuSetDigest {
		return fmt.Errorf("construction-unit report set digest mismatch")
	}
	manifestDigests := make(map[string]string, len(expectedRoles))
	manifestByRole := make(map[string]ReleaseArtifactManifestV1, len(expectedRoles))
	for index, role := range expectedRoles {
		setRole := set.Roles[index]
		manifest := graph.ArtifactManifests[index]
		if setRole.Role != role || manifest.Role != role {
			return fmt.Errorf("release role %d is not exact %q", index, role)
		}
		if err := verifyReleaseArtifactManifest(manifest, authorityDigest, set); err != nil {
			return err
		}
		digest, err := releaseObjectDigestV1("cq/release-artifact-manifest/v1\x00", manifest)
		if err != nil {
			return err
		}
		if setRole.ArtifactManifestDigest != digest || setRole.ArtifactPayloadDigest != manifest.ArtifactPayloadDigest {
			return fmt.Errorf("release role %q manifest or payload digest mismatch", role)
		}
		manifestDigests[role] = digest
		manifestByRole[role] = manifest
	}
	if err := verifyReleaseABIEqualities(purpose, set, manifestByRole); err != nil {
		return err
	}
	for index, execution := range graph.BuildReport.RoleExecutions {
		role := expectedRoles[index]
		manifest := manifestByRole[role]
		if execution.Role != role || execution.BuildCommandDigest != manifest.BuildCommandDigest || execution.ArtifactPayloadDigest != manifest.ArtifactPayloadDigest || execution.ArtifactManifestDigest != manifestDigests[role] {
			return fmt.Errorf("build report role execution %q is not a role bijection", role)
		}
	}
	artifactSetDigest, err := releaseObjectDigestV1("cq/release-artifact-set/v1\x00", set)
	if err != nil {
		return err
	}
	if err := verifyReleaseBundle(graph.Bundle, purpose, authority, authorityDigest, set, artifactSetDigest, cuSetDigest, reportDigestByKind, manifestByRole, manifestDigests, publicKey); err != nil {
		return err
	}
	return nil
}

func VerifyReleaseBundleDirectoryV1(root string, graph ReleaseGraphV1) error {
	if err := VerifyReleaseGraphV1(graph); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("release bundle root must be a directory and not a symlink")
	}
	expectedFiles := make(map[string]ReleaseBundleEntryV1, len(graph.Bundle.Entries)+1)
	for _, entry := range graph.Bundle.Entries {
		expectedFiles[entry.RelativePath] = entry
	}
	expectedDirectories := map[string]bool{"manifests": true, "payloads": true, "reports": true}
	descendants := 0
	var aggregateBytes uint64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		descendants++
		if descendants > 17 {
			return fmt.Errorf("release bundle exceeds 17 descendants")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if strings.Count(relative, "/") > 1 {
			return fmt.Errorf("release bundle exceeds depth two")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release bundle contains link %q", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if !expectedDirectories[relative] {
				return fmt.Errorf("release bundle contains unknown directory %q", relative)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release bundle contains non-regular file %q", relative)
		}
		if relative != "bundle.json" {
			if _, ok := expectedFiles[relative]; !ok {
				return fmt.Errorf("release bundle contains unknown file %q", relative)
			}
		}
		aggregateBytes += uint64(info.Size())
		if aggregateBytes > 1<<30 {
			return fmt.Errorf("release bundle exceeds 1 GiB")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if descendants != len(graph.Bundle.Entries)+4 {
		return fmt.Errorf("release bundle descendant cardinality mismatch")
	}
	objects := map[string]any{
		"bundle.json":                     graph.Bundle,
		"release-build-authority.json":    graph.Authority,
		"release-artifact-set.json":       graph.ArtifactSet,
		"reports/build.json":              graph.BuildReport,
		"reports/vet.json":                graph.VetReport,
		"reports/race.json":               graph.RaceReport,
		"reports/construction-units.json": graph.CUReportSet,
	}
	if graph.Ancestry != nil {
		objects["source-ancestry.json"] = *graph.Ancestry
	}
	for _, manifest := range graph.ArtifactManifests {
		objects["manifests/"+manifest.Role+".json"] = manifest
	}
	for relative, object := range objects {
		canonical, err := CanonicalJSONV1(object)
		if err != nil {
			return err
		}
		data, err := readStaticNoFollow(filepath.Join(root, filepath.FromSlash(relative)), 64<<10)
		if err != nil {
			return fmt.Errorf("read release object %q: %w", relative, err)
		}
		if !bytes.Equal(data, canonical) {
			return fmt.Errorf("release object %q is not the exact canonical graph object", relative)
		}
		if relative != "bundle.json" && uint64(len(data)) != expectedFiles[relative].Size {
			return fmt.Errorf("release object %q size mismatch", relative)
		}
	}
	for _, role := range graph.ArtifactSet.Roles {
		relative := "payloads/" + role.Role
		size, digest, err := hashStaticNoFollow(filepath.Join(root, filepath.FromSlash(relative)), 268435456)
		if err != nil {
			return fmt.Errorf("hash release payload %q: %w", relative, err)
		}
		entry := expectedFiles[relative]
		if size != entry.Size || digest != entry.Digest || digest != role.ArtifactPayloadDigest {
			return fmt.Errorf("release payload %q bytes do not match graph", relative)
		}
	}
	return nil
}

func hashStaticNoFollow(path string, limit uint64) (uint64, string, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return 0, "", fmt.Errorf("payload path is not a regular non-symlink file")
	}
	if before.Size() < 0 || uint64(before.Size()) > limit {
		return 0, "", fmt.Errorf("payload exceeds %d bytes", limit)
	}
	opener, ok := any(fsutil.OSFileSystem{}).(fsutil.NoFollowFileOpener)
	if !ok {
		return 0, "", fsutil.ErrSecureCapabilityUnavailable
	}
	file, err := opener.OpenNoFollow(path)
	if err != nil {
		return 0, "", err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(before, openedInfo) {
		_ = file.Close()
		return 0, "", fmt.Errorf("payload changed before read")
	}
	hash := sha256.New()
	count, readErr := io.Copy(hash, io.LimitReader(file, int64(limit)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || count > int64(limit) {
		return 0, "", fmt.Errorf("read bounded payload: %v %v", readErr, closeErr)
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, after) {
		return 0, "", fmt.Errorf("payload changed during read")
	}
	return uint64(count), hex.EncodeToString(hash.Sum(nil)), nil
}

func releaseObjectDigestV1(domain string, value any) (string, error) {
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		return "", err
	}
	if len(canonical) > 64<<10 {
		return "", fmt.Errorf("release object exceeds 64 KiB")
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, domain)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(canonical)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func decodeEd25519PublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize || encoded != strings.ToLower(encoded) {
		return nil, fmt.Errorf("invalid Ed25519 public key encoding")
	}
	return ed25519.PublicKey(decoded), nil
}

func verifyReleaseSignature(publicKey ed25519.PublicKey, encodedSignature string, signable any) error {
	signature, err := hex.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize || encodedSignature != strings.ToLower(encodedSignature) {
		return fmt.Errorf("invalid Ed25519 signature encoding")
	}
	canonical, err := CanonicalJSONV1(signable)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, canonical, signature) {
		return fmt.Errorf("Ed25519 signature mismatch")
	}
	return nil
}

func releaseArtifactSetSignable(value ReleaseArtifactSetV1) ReleaseArtifactSetV1 {
	value.SetSignature = ""
	return value
}

func releaseBuildReportSignable(value ReleaseBuildReportV1) ReleaseBuildReportV1 {
	value.Signature = ""
	return value
}

func cuReportSetSignable(value ConstructionUnitReportSetV1) ConstructionUnitReportSetV1 {
	value.Signature = ""
	return value
}

func sourceAncestrySignable(value SourceAncestryReceiptV1) SourceAncestryReceiptV1 {
	value.Signature = ""
	return value
}

func releaseBundleSignable(value ReleaseBundleV1) ReleaseBundleV1 {
	value.BundleSignature = ""
	return value
}

func validateReleaseTimestamp(value, field string) error {
	parsed, err := time.Parse("2006-01-02T15:04:05Z", value)
	if err != nil || parsed.Format("2006-01-02T15:04:05Z") != value {
		return fmt.Errorf("%s must be UTC whole seconds", field)
	}
	return nil
}

func validateReleaseFeatureList(features []string, field string) error {
	if len(features) == 0 {
		return fmt.Errorf("release %s is empty", field)
	}
	for index, feature := range features {
		if feature == "" || !utf8.ValidString(feature) || strings.IndexByte(feature, 0) >= 0 || (index > 0 && feature <= features[index-1]) {
			return fmt.Errorf("release %s is invalid or unordered", field)
		}
	}
	return nil
}

func verifySourceAncestry(receipt *SourceAncestryReceiptV1, authority ReleaseBuildAuthorityV1, authorityDigest string, set ReleaseArtifactSetV1, publicKey ed25519.PublicKey) error {
	if receipt.SchemaVersion != 1 || receipt.Kind != "source_ancestry_v1" || receipt.ReleaseBuildAuthorityDigest != authorityDigest || receipt.RepositoryIdentityDigest != authority.RepositoryIdentityDigest || receipt.FloorSourceCommit != authority.LineageRootCommit || receipt.FloorSourceTreeDigest != authority.LineageRootTreeDigest || receipt.MergeBaseCommit != receipt.FloorSourceCommit || receipt.TargetSourceCommit != set.SourceCommit || receipt.TargetSourceTreeDigest != set.SourceTreeDigest || receipt.TargetSourceCommit == receipt.FloorSourceCommit || receipt.SignerPublicKey != authority.Ed25519PublicKey || !lowerHex64Pattern.MatchString(receipt.VerificationCommandDigest) {
		return fmt.Errorf("source ancestry receipt projection mismatch")
	}
	if err := validateReleaseTimestamp(receipt.VerifiedAt, "ancestry verified_at"); err != nil {
		return err
	}
	if err := verifyReleaseSignature(publicKey, receipt.Signature, sourceAncestrySignable(*receipt)); err != nil {
		return fmt.Errorf("verify ancestry signature: %w", err)
	}
	return nil
}

func verifyReleaseBuildReport(report ReleaseBuildReportV1, kind, purpose string, authority ReleaseBuildAuthorityV1, authorityDigest string, set ReleaseArtifactSetV1, expectedRoles []string, publicKey ed25519.PublicKey) error {
	if report.SchemaVersion != 1 || report.Kind != kind || report.Purpose != purpose || report.ReleaseBuildAuthorityDigest != authorityDigest || report.SourceCommit != set.SourceCommit || report.SourceTreeDigest != set.SourceTreeDigest || report.ToolchainIdentity != authority.ToolchainIdentity || report.BuildEnvironmentDigest != set.BuildEnvironmentDigest || report.Outcome != "passed" || report.ExitCode != 0 || report.SignerPublicKey != authority.Ed25519PublicKey || !lowerHex64Pattern.MatchString(report.CommandDigest) || !lowerHex64Pattern.MatchString(report.ExecutionResultDigest) {
		return fmt.Errorf("%s release report projection mismatch", kind)
	}
	if report.RaceEnabled != (kind == "race") {
		return fmt.Errorf("%s release report race_enabled mismatch", kind)
	}
	wantExecutions := 0
	if kind == "build" {
		wantExecutions = len(expectedRoles)
	}
	if len(report.RoleExecutions) != wantExecutions {
		return fmt.Errorf("%s release report role execution cardinality mismatch", kind)
	}
	for index, execution := range report.RoleExecutions {
		if execution.Role != expectedRoles[index] || !lowerHex64Pattern.MatchString(execution.BuildCommandDigest) || !lowerHex64Pattern.MatchString(execution.ArtifactPayloadDigest) || !lowerHex64Pattern.MatchString(execution.ArtifactManifestDigest) {
			return fmt.Errorf("build report role execution is invalid")
		}
	}
	if err := validateReleaseTimestamp(report.StartedAt, kind+" started_at"); err != nil {
		return err
	}
	if err := validateReleaseTimestamp(report.EndedAt, kind+" ended_at"); err != nil || report.EndedAt < report.StartedAt {
		return fmt.Errorf("%s release report time interval is invalid", kind)
	}
	if err := verifyReleaseSignature(publicKey, report.Signature, releaseBuildReportSignable(report)); err != nil {
		return fmt.Errorf("verify %s report signature: %w", kind, err)
	}
	return nil
}

func verifyCUReportSet(reportSet ConstructionUnitReportSetV1, purpose string, authority ReleaseBuildAuthorityV1, authorityDigest string, set ReleaseArtifactSetV1, publicKey ed25519.PublicKey) error {
	if reportSet.SchemaVersion != 1 || reportSet.Kind != "construction_unit_report_set_v1" || reportSet.Purpose != purpose || reportSet.ReleaseBuildAuthorityDigest != authorityDigest || reportSet.BlueprintSHA256 != authority.BlueprintSHA256 || reportSet.ReviewAttestationAggregateSHA256 != authority.ReviewAttestationAggregateSHA256 || reportSet.ReviewAuthorityBaselineCommit != authority.ReviewAuthorityBaselineCommit || reportSet.LegacyAtomicWriterReachabilityCatalogueDigest != set.LegacyAtomicWriterReachabilityCatalogueDigest || reportSet.SourceCommit != set.SourceCommit || reportSet.SourceTreeDigest != set.SourceTreeDigest || reportSet.ToolchainIdentity != authority.ToolchainIdentity || reportSet.BuildEnvironmentDigest != set.BuildEnvironmentDigest || reportSet.SignerPublicKey != authority.Ed25519PublicKey {
		return fmt.Errorf("construction-unit report set projection mismatch")
	}
	if !lowerHex64Pattern.MatchString(reportSet.LegacyAtomicWriterReachabilityCatalogueDigest) {
		return fmt.Errorf("construction-unit report set cardinality mismatch")
	}
	if err := ValidateCUReportSetV1(purpose, reportSet.Reports); err != nil {
		return fmt.Errorf("construction-unit report set cardinality mismatch")
	}
	for _, report := range reportSet.Reports {
		if !lowerHex64Pattern.MatchString(report.VerificationManifestDigest) || !lowerHex64Pattern.MatchString(report.InvocationDigest) || !lowerHex64Pattern.MatchString(report.ExecutionResultDigest) {
			return fmt.Errorf("CU report %q has empty evidence", report.CUID)
		}
		if err := validateReleaseTimestamp(report.StartedAt, report.CUID+" started_at"); err != nil {
			return err
		}
		if err := validateReleaseTimestamp(report.EndedAt, report.CUID+" ended_at"); err != nil || report.EndedAt < report.StartedAt {
			return fmt.Errorf("CU report %q time interval is invalid", report.CUID)
		}
	}
	if err := verifyReleaseSignature(publicKey, reportSet.Signature, cuReportSetSignable(reportSet)); err != nil {
		return fmt.Errorf("verify construction-unit report set signature: %w", err)
	}
	return nil
}

func verifyReleaseArtifactManifest(manifest ReleaseArtifactManifestV1, authorityDigest string, set ReleaseArtifactSetV1) error {
	if manifest.SchemaVersion != 1 || manifest.ReleaseBuildAuthorityDigest != authorityDigest || manifest.SourceCommit != set.SourceCommit || manifest.SourceTreeDigest != set.SourceTreeDigest || manifest.ToolchainIdentity != set.ToolchainIdentity || manifest.BuildEnvironmentDigest != set.BuildEnvironmentDigest || manifest.Architecture == "" || manifest.BuildID == "" || !lowerHex64Pattern.MatchString(manifest.BuildCommandDigest) || !lowerHex64Pattern.MatchString(manifest.CodeSignatureDigest) || !lowerHex64Pattern.MatchString(manifest.ArtifactPayloadDigest) {
		return fmt.Errorf("release artifact manifest %q projection mismatch", manifest.Role)
	}
	if err := validateReleaseFeatureList(manifest.SupportedFeatures, manifest.Role+" supported_features"); err != nil {
		return err
	}
	if err := validateReleaseFeatureList(manifest.MinimumFloorFeatures, manifest.Role+" minimum_floor_features"); err != nil {
		return err
	}
	switch manifest.Role {
	case "launcher":
		if manifest.LauncherABIDigest == nil || manifest.PrivateABIDigest != nil {
			return fmt.Errorf("launcher ABI nullability mismatch")
		}
	case "supervisor":
		if manifest.LauncherABIDigest == nil || manifest.PrivateABIDigest == nil {
			return fmt.Errorf("supervisor ABI nullability mismatch")
		}
	case "worker":
		if manifest.LauncherABIDigest != nil || manifest.PrivateABIDigest == nil {
			return fmt.Errorf("worker ABI nullability mismatch")
		}
	default:
		return fmt.Errorf("unknown artifact role %q", manifest.Role)
	}
	for _, digest := range []*string{manifest.LauncherABIDigest, manifest.PrivateABIDigest} {
		if digest != nil && !lowerHex64Pattern.MatchString(*digest) {
			return fmt.Errorf("artifact role %q has invalid ABI digest", manifest.Role)
		}
	}
	return nil
}

func verifyReleaseABIEqualities(purpose string, set ReleaseArtifactSetV1, manifests map[string]ReleaseArtifactManifestV1) error {
	supervisor := manifests["supervisor"]
	worker := manifests["worker"]
	if supervisor.PrivateABIDigest == nil || worker.PrivateABIDigest == nil || *supervisor.PrivateABIDigest != *worker.PrivateABIDigest {
		return fmt.Errorf("supervisor and worker private ABI mismatch")
	}
	if purpose == "floor" {
		if supervisor.LauncherABIDigest == nil || set.RequiredLauncherABIDigest == nil || *supervisor.LauncherABIDigest != *set.RequiredLauncherABIDigest {
			return fmt.Errorf("floor required launcher ABI mismatch")
		}
		return nil
	}
	launcher := manifests["launcher"]
	if launcher.LauncherABIDigest == nil || supervisor.LauncherABIDigest == nil || set.LauncherABIDigest == nil || *launcher.LauncherABIDigest != *set.LauncherABIDigest || *supervisor.LauncherABIDigest != *set.LauncherABIDigest {
		return fmt.Errorf("target launcher ABI mismatch")
	}
	return nil
}

func verifyReleaseBundle(bundle ReleaseBundleV1, purpose string, authority ReleaseBuildAuthorityV1, authorityDigest string, set ReleaseArtifactSetV1, setDigest, cuSetDigest string, reportDigests map[string]string, manifests map[string]ReleaseArtifactManifestV1, manifestDigests map[string]string, publicKey ed25519.PublicKey) error {
	if bundle.SchemaVersion != 1 || bundle.Purpose != purpose || bundle.ReleaseBuildAuthorityDigest != authorityDigest || bundle.ReleaseArtifactSetDigest != setDigest || bundle.ConstructionUnitReportSetDigest != cuSetDigest || bundle.SignerPublicKey != authority.Ed25519PublicKey {
		return fmt.Errorf("release bundle projection mismatch")
	}
	if purpose == "floor" {
		if bundle.SourceAncestryReceiptDigest != nil {
			return fmt.Errorf("floor bundle contains ancestry")
		}
	} else if bundle.SourceAncestryReceiptDigest == nil || set.SourceAncestryReceiptDigest == nil || *bundle.SourceAncestryReceiptDigest != *set.SourceAncestryReceiptDigest {
		return fmt.Errorf("target bundle ancestry mismatch")
	}
	if err := ValidateReleaseBundleEntriesV1(purpose, bundle.Entries); err != nil {
		return err
	}
	expected := map[string]string{
		"release-build-authority.json":    authorityDigest,
		"release-artifact-set.json":       setDigest,
		"reports/build.json":              reportDigests["build"],
		"reports/vet.json":                reportDigests["vet"],
		"reports/race.json":               reportDigests["race"],
		"reports/construction-units.json": cuSetDigest,
	}
	for role, manifest := range manifests {
		expected["manifests/"+role+".json"] = manifestDigests[role]
		expected["payloads/"+role] = manifest.ArtifactPayloadDigest
	}
	if purpose == "target" {
		expected["source-ancestry.json"] = *set.SourceAncestryReceiptDigest
	}
	for _, entry := range bundle.Entries {
		if entry.Digest != expected[entry.RelativePath] {
			return fmt.Errorf("bundle entry %q digest mismatch", entry.RelativePath)
		}
	}
	if err := verifyReleaseSignature(publicKey, bundle.BundleSignature, releaseBundleSignable(bundle)); err != nil {
		return fmt.Errorf("verify release bundle signature: %w", err)
	}
	return nil
}

var releaseBuildEnvironmentKeys = [...]string{
	"CGO_ENABLED",
	"GOAMD64",
	"GOARCH",
	"GOARM",
	"GOARM64",
	"GOEXPERIMENT",
	"GOFLAGS",
	"GOOS",
	"GOTOOLCHAIN",
	"LC_ALL",
	"SOURCE_DATE_EPOCH",
	"TZ",
}

// BuildEnvironmentDigestV1 computes the literal digest of the closed,
// byte-sorted release-build environment vector.
func BuildEnvironmentDigestV1(entries []BuildEnvironmentEntryV1) (string, error) {
	if len(entries) != len(releaseBuildEnvironmentKeys) {
		return "", fmt.Errorf("build environment has %d entries, want %d", len(entries), len(releaseBuildEnvironmentKeys))
	}
	encodedLength := uint64(4)
	for index, entry := range entries {
		if entry.Key != releaseBuildEnvironmentKeys[index] {
			return "", fmt.Errorf("build environment key %d is %q, want %q", index, entry.Key, releaseBuildEnvironmentKeys[index])
		}
		if !utf8.ValidString(entry.Value) || strings.IndexByte(entry.Value, 0) >= 0 {
			return "", fmt.Errorf("build environment value for %q is invalid", entry.Key)
		}
		if err := validateReleaseBuildEnvironmentValue(entry); err != nil {
			return "", err
		}
		if uint64(len(entry.Key)) > uint64(^uint32(0)) || uint64(len(entry.Value)) > uint64(^uint32(0)) {
			return "", fmt.Errorf("build environment member exceeds uint32")
		}
		encodedLength += 8 + uint64(len(entry.Key)) + uint64(len(entry.Value))
		if encodedLength > uint64(^uint32(0)) {
			return "", fmt.Errorf("build environment vector exceeds uint32")
		}
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, "cq/release-build-environment/v1\x00")
	var field [4]byte
	binary.BigEndian.PutUint32(field[:], uint32(encodedLength))
	_, _ = hash.Write(field[:])
	binary.BigEndian.PutUint32(field[:], uint32(len(entries)))
	_, _ = hash.Write(field[:])
	for _, entry := range entries {
		if err := writeUint32FramedString(hash, entry.Key); err != nil {
			return "", err
		}
		if err := writeUint32FramedString(hash, entry.Value); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ValidateCUReportSetV1 enforces the exact floor or target CU sequence.
func ValidateCUReportSetV1(purpose string, reports []CUReportV1) error {
	want := 0
	switch purpose {
	case "floor":
		want = 9
	case "target":
		want = 10
	default:
		return fmt.Errorf("invalid release purpose %q", purpose)
	}
	if len(reports) != want {
		return fmt.Errorf("%s CU report set has %d reports, want %d", purpose, len(reports), want)
	}
	for index, report := range reports {
		wantID := fmt.Sprintf("CU-%d", index)
		if report.CUID != wantID {
			return fmt.Errorf("CU report %d is %q, want %q", index, report.CUID, wantID)
		}
		if report.SchemaVersion != 1 || report.Kind != "construction_unit_report_v1" || report.Outcome != "passed" || report.ExitCode != 0 || !report.RaceEnabled {
			return fmt.Errorf("CU report %q is not a passed race-enabled V1 report", report.CUID)
		}
	}
	return nil
}

// ValidateReleaseBundleEntriesV1 enforces the exact sorted non-bundle files.
func ValidateReleaseBundleEntriesV1(purpose string, entries []ReleaseBundleEntryV1) error {
	expected := []string{
		"manifests/supervisor.json",
		"manifests/worker.json",
		"payloads/supervisor",
		"payloads/worker",
		"release-artifact-set.json",
		"release-build-authority.json",
		"reports/build.json",
		"reports/construction-units.json",
		"reports/race.json",
		"reports/vet.json",
	}
	switch purpose {
	case "floor":
	case "target":
		expected = append(expected, "manifests/launcher.json", "payloads/launcher", "source-ancestry.json")
		sort.Strings(expected)
	default:
		return fmt.Errorf("invalid release purpose %q", purpose)
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("%s release bundle has %d entries, want %d", purpose, len(entries), len(expected))
	}
	for index, entry := range entries {
		if entry.RelativePath != expected[index] {
			return fmt.Errorf("release bundle entry %d is %q, want %q", index, entry.RelativePath, expected[index])
		}
		if entry.Kind != "file" || !lowerHex64Pattern.MatchString(entry.Digest) || entry.Size == 0 {
			return fmt.Errorf("release bundle entry %q is invalid", entry.RelativePath)
		}
	}
	return nil
}

// ExecutionResultDigestV1 computes the literal release execution-result
// digest without newline, encoding, terminal, or truncation normalisation.
func ExecutionResultDigestV1(exitCode int32, terminationReason string, stdout, stderr []byte) (string, error) {
	switch terminationReason {
	case "exited", "signalled", "timeout":
	default:
		return "", fmt.Errorf("invalid termination reason %q", terminationReason)
	}
	if len(stdout) > maxCUCaptureStreamBytes || len(stderr) > maxCUCaptureStreamBytes {
		return "", fmt.Errorf("captured output exceeds %d bytes per stream", maxCUCaptureStreamBytes)
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, "cq/release-execution-result/v1\x00")
	var exit [4]byte
	binary.BigEndian.PutUint32(exit[:], uint32(exitCode))
	_, _ = hash.Write(exit[:])
	if err := writeUint32FramedString(hash, terminationReason); err != nil {
		return "", err
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(stdout)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(stdout)
	binary.BigEndian.PutUint64(length[:], uint64(len(stderr)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(stderr)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// VerificationManifestDigestV1 digests the exact canonical checked-in
// manifest bytes for one construction unit.
func VerificationManifestDigestV1(cuID string) (string, error) {
	manifest, err := CanonicalCUManifestV1(cuID)
	if err != nil {
		return "", err
	}
	if len(manifest) > maxCUManifestBytes {
		return "", fmt.Errorf("CU manifest exceeds %d bytes", maxCUManifestBytes)
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, "cq/construction-unit-verification-manifest/v1\x00")
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(manifest)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(manifest)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// CommandDigestV1 computes the literal release-command digest.
func CommandDigestV1(purpose, workingDirectory string, argv []string) (string, error) {
	if !utf8.ValidString(purpose) || !utf8.ValidString(workingDirectory) {
		return "", fmt.Errorf("command purpose or working directory is invalid UTF-8")
	}
	if !releaseCommandPurposePattern.MatchString(purpose) {
		return "", fmt.Errorf("invalid release command purpose %q", purpose)
	}
	if workingDirectory == "" || filepath.IsAbs(workingDirectory) || filepath.Clean(workingDirectory) != workingDirectory || workingDirectory == ".." || strings.HasPrefix(workingDirectory, "../") || strings.ContainsAny(workingDirectory, "\\\x00") {
		return "", fmt.Errorf("release working directory must be canonical and repository-relative")
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("release command argv is empty")
	}
	if uint64(len(argv)) > uint64(^uint32(0)) {
		return "", fmt.Errorf("command argument count exceeds uint32")
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, "cq/release-command/v1\x00")
	if err := writeUint32FramedString(hash, purpose); err != nil {
		return "", err
	}
	if err := writeUint32FramedString(hash, workingDirectory); err != nil {
		return "", err
	}
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(argv)))
	_, _ = hash.Write(count[:])
	for _, argument := range argv {
		if !utf8.ValidString(argument) || strings.IndexByte(argument, 0) >= 0 {
			return "", fmt.Errorf("command argument is invalid UTF-8")
		}
		if err := writeUint32FramedString(hash, argument); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateReleaseBuildEnvironmentValue(entry BuildEnvironmentEntryV1) error {
	valid := false
	switch entry.Key {
	case "CGO_ENABLED":
		valid = entry.Value == "0" || entry.Value == "1"
	case "GOAMD64":
		valid = entry.Value == "" || entry.Value == "v1" || entry.Value == "v2" || entry.Value == "v3" || entry.Value == "v4"
	case "GOARCH", "GOOS":
		valid = regexp.MustCompile(`^[a-z0-9]+$`).MatchString(entry.Value)
	case "GOARM":
		valid = entry.Value == "" || entry.Value == "5" || entry.Value == "6" || entry.Value == "7"
	case "GOARM64":
		valid = entry.Value == "" || regexp.MustCompile(`^v8\.[0-9]+$`).MatchString(entry.Value)
	case "GOEXPERIMENT":
		valid = entry.Value == ""
	case "GOFLAGS":
		valid = entry.Value == "-trimpath" || entry.Value == "-trimpath -buildvcs=true"
	case "GOTOOLCHAIN":
		valid = entry.Value == "go1.26.1"
	case "LC_ALL":
		valid = entry.Value == "C"
	case "SOURCE_DATE_EPOCH":
		valid = decimalPattern.MatchString(entry.Value)
	case "TZ":
		valid = entry.Value == "UTC"
	}
	if !valid {
		return fmt.Errorf("release build environment value for %q is outside the closed policy", entry.Key)
	}
	return nil
}

// NewCUReportV1 constructs the accepted entry only from a complete capture.
func NewCUReportV1(capture CUReportCaptureV1) (CUReportV1, error) {
	var report CUReportV1
	if !cuIDPattern.MatchString(capture.CUID) {
		return report, fmt.Errorf("invalid CU ID %q", capture.CUID)
	}
	if !lowerHex64Pattern.MatchString(capture.VerificationManifestDigest) || !lowerHex64Pattern.MatchString(capture.InvocationDigest) {
		return report, fmt.Errorf("invalid CU manifest or invocation digest")
	}
	if capture.ExitCode != 0 || capture.TerminationReason != "exited" || !capture.RaceEnabled {
		return report, fmt.Errorf("CU capture did not pass with race enabled")
	}
	if err := validateCUCaptureTime(capture.StartedAt, "started_at"); err != nil {
		return report, err
	}
	if err := validateCUCaptureTime(capture.EndedAt, "ended_at"); err != nil {
		return report, err
	}
	if capture.EndedAt.Before(capture.StartedAt) {
		return report, fmt.Errorf("CU capture ended before it started")
	}
	resultDigest, err := ExecutionResultDigestV1(capture.ExitCode, capture.TerminationReason, capture.Stdout, capture.Stderr)
	if err != nil {
		return report, err
	}
	return CUReportV1{
		SchemaVersion:              1,
		Kind:                       "construction_unit_report_v1",
		CUID:                       capture.CUID,
		VerificationManifestDigest: capture.VerificationManifestDigest,
		InvocationDigest:           capture.InvocationDigest,
		Outcome:                    "passed",
		ExitCode:                   capture.ExitCode,
		RaceEnabled:                capture.RaceEnabled,
		ExecutionResultDigest:      resultDigest,
		StartedAt:                  capture.StartedAt.Format("2006-01-02T15:04:05Z"),
		EndedAt:                    capture.EndedAt.Format("2006-01-02T15:04:05Z"),
	}, nil
}

func validateCUCaptureTime(value time.Time, field string) error {
	_, offset := value.Zone()
	if offset != 0 || value.Nanosecond() != 0 {
		return fmt.Errorf("%s must be UTC seconds", field)
	}
	return nil
}

func writeUint32FramedString(writer io.Writer, value string) error {
	if uint64(len(value)) > uint64(^uint32(0)) {
		return fmt.Errorf("framed string exceeds uint32")
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	if _, err := writer.Write(length[:]); err != nil {
		return err
	}
	_, err := io.WriteString(writer, value)
	return err
}

// ParseCUManifestV1 reads and parses one bounded, closed CU manifest.
func ParseCUManifestV1(reader io.Reader) (CUManifestV1, error) {
	var manifest CUManifestV1
	data, err := readCUBytes(reader, maxCUManifestBytes)
	if err != nil {
		return manifest, err
	}
	decoded, err := decodeStrictJSON(data)
	if err != nil {
		return manifest, fmt.Errorf("decode CU manifest: %w", err)
	}
	canonical, err := appendCanonicalJSON(make([]byte, 0, len(data)), decoded)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(canonical, data) {
		return manifest, fmt.Errorf("CU manifest is not canonical JCS")
	}
	if err := decodeClosedJSON(data, &manifest); err != nil {
		return manifest, fmt.Errorf("decode CU manifest schema: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return manifest, fmt.Errorf("CU manifest schema_version %d, want 1", manifest.SchemaVersion)
	}
	if manifest.Kind != "construction_unit_verification_manifest_v1" {
		return manifest, fmt.Errorf("CU manifest kind %q", manifest.Kind)
	}
	if manifest.BlueprintSHA256 != frozenBlueprintSHA256 || manifest.ReviewAttestationAggregateSHA256 != frozenReviewAggregateSHA256 || manifest.ReviewAuthorityBaselineCommit != frozenReviewBaseline {
		return manifest, fmt.Errorf("CU manifest authority digest mismatch")
	}
	if !cuIDPattern.MatchString(manifest.Unit) {
		return manifest, fmt.Errorf("CU manifest unit %q is invalid", manifest.Unit)
	}
	if manifest.RaceCount != 1 {
		return manifest, fmt.Errorf("CU manifest race_count %d, want 1", manifest.RaceCount)
	}
	if len(manifest.Packages) == 0 {
		return manifest, fmt.Errorf("CU manifest package selection is empty")
	}
	priorPackage := ""
	for index := range manifest.Packages {
		selection := &manifest.Packages[index]
		if !cuPackagePattern.MatchString(selection.Package) || strings.Contains(selection.Package, "..") {
			return manifest, fmt.Errorf("CU manifest package %q is invalid", selection.Package)
		}
		if index > 0 && selection.Package <= priorPackage {
			return manifest, fmt.Errorf("CU manifest packages are not ordered and unique")
		}
		priorPackage = selection.Package
		if err := validateCUTestIDs(selection); err != nil {
			return manifest, fmt.Errorf("CU manifest package %q: %w", selection.Package, err)
		}
	}
	return manifest, nil
}

func validateCUTestIDs(selection *CUTestPackageV1) error {
	if len(selection.TopLevelTests) == 0 || len(selection.FullTestIDs) == 0 {
		return fmt.Errorf("test selection is empty")
	}
	if selection.MinimumPassCount != len(selection.FullTestIDs) {
		return fmt.Errorf("minimum_pass_count %d, want %d", selection.MinimumPassCount, len(selection.FullTestIDs))
	}
	for index, name := range selection.TopLevelTests {
		if !cuTopTestPattern.MatchString(name) || (index > 0 && name <= selection.TopLevelTests[index-1]) {
			return fmt.Errorf("top-level tests are invalid, duplicated, or unordered")
		}
	}
	for index, name := range selection.FullTestIDs {
		if !cuFullTestPattern.MatchString(name) || (index > 0 && name <= selection.FullTestIDs[index-1]) {
			return fmt.Errorf("full test IDs are invalid, duplicated, or unordered")
		}
		root := strings.SplitN(name, "/", 2)[0]
		position := sort.SearchStrings(selection.TopLevelTests, root)
		if position == len(selection.TopLevelTests) || selection.TopLevelTests[position] != root {
			return fmt.Errorf("full test ID %q has no top-level selection", name)
		}
	}
	for _, top := range selection.TopLevelTests {
		position := sort.SearchStrings(selection.FullTestIDs, top)
		if position == len(selection.FullTestIDs) || selection.FullTestIDs[position] != top {
			return fmt.Errorf("top-level test %q is absent from full test IDs", top)
		}
	}
	return nil
}

func readCUBytes(reader io.Reader, limit int64) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.Grow(int(limit))
	written, err := io.CopyN(&buffer, reader, limit+1)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read bounded input: %w", err)
	}
	if written > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return buffer.Bytes(), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return fmt.Errorf("unexpected trailing JSON value")
}
