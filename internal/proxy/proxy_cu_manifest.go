//go:build !windows

package proxy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
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
	if cuID == "CU-2" {
		return canonicalCU2ManifestV1()
	}
	if cuID == "CU-1" {
		return canonicalCU1ManifestV1()
	}
	if cuID != "CU-0" {
		return nil, fmt.Errorf("construction unit %q has no manifest", cuID)
	}
	cmdSelection := newCUTestPackage("./cmd/cq",
		"TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState",
		"TestGlobalHelpAndVersionPreserveAbsentHomeAndXDGEnvironment",
		"TestManualHelpInspectionAllowsLeafArgumentsBeforeHelp",
		"TestManualHelpInspectionPreservesEndpointHelpGrammar",
		"TestManualHelpTextDocumentsEachCommandPath",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors",
		"TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage",
		"TestPureGlobalInspectionHandlesOrdinaryUsageBeforeCompatibility",
		"TestPureGlobalInspectionLeavesValidCaptureWhitespacePathForHandler",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar",
		"TestPureGlobalInspectionPreservesBareHelpUsageError",
		"TestPureGlobalInspectionPreservesInvalidHelpUsage",
		"TestPureGlobalInspectionPropagatesHelpWriteError",
		"TestRootHelpShowsFullCLISurface",
		"TestRunModelsHelpDoesNotRefresh",
		"TestRunProxyCodexDefaultHelpDoesNotCreateConfig",
	)
	cmdSelection.FullTestIDs = []string{
		"TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState",
		"TestGlobalHelpAndVersionPreserveAbsentHomeAndXDGEnvironment",
		"TestManualHelpInspectionAllowsLeafArgumentsBeforeHelp",
		"TestManualHelpInspectionPreservesEndpointHelpGrammar",
		"TestManualHelpTextDocumentsEachCommandPath",
		"TestManualHelpTextDocumentsEachCommandPath/agent",
		"TestManualHelpTextDocumentsEachCommandPath/codex_canary",
		"TestManualHelpTextDocumentsEachCommandPath/codex_validate",
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
		"TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage",
		"TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage/agent",
		"TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage/models",
		"TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage/models_overlay",
		"TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage/proxy",
		"TestPureGlobalInspectionHandlesInterceptedZeroArgumentUsage/proxy_prime",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/codex_canary_implicit_help",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/codex_validate_implicit_help",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/models_overlay_help_unknown",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/models_overlay_unknown_help",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/proxy_endpoint_implicit_help",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/proxy_prime_nested_help_extra",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/proxy_prime_option_terminator_help",
		"TestPureGlobalInspectionMatchesNestedHandlerGrammar/proxy_prime_unknown_help",
		"TestPureGlobalInspectionPreservesInvalidHelpUsage",
		"TestPureGlobalInspectionPreservesBareHelpUsageError",
		"TestPureGlobalInspectionPropagatesHelpWriteError",
		"TestRootHelpShowsFullCLISurface",
		"TestRunModelsHelpDoesNotRefresh",
		"TestRunProxyCodexDefaultHelpDoesNotCreateConfig",
	}
	cmdSelection.FullTestIDs = append(cmdSelection.FullTestIDs,
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/agent_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/codex_canary_arity",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/codex_canary_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/codex_capture_required",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/codex_capture_value",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/codex_http_value",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/codex_validate_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/codex_websocket_flag",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/models_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/models_list_flag",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/models_list_provider",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/models_list_provider_value",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/models_overlay_add_flags",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/models_overlay_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/models_overlay_remove_clone",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_codex_default",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_endpoint_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_endpoint_inspect_arity",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_pin_arity",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_pin_flag",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_prime_arity",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_prime_command",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_start_port",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_status_flag",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_validate_port",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/proxy_validate_live_port",
		"TestPureGlobalInspectionHandlesCompleteInterceptedLexicalErrors/refresh_arguments",
		"TestPureGlobalInspectionLeavesValidCaptureWhitespacePathForHandler",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/agent",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/codex_canary",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/codex_validate",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/models_list",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/models_overlay",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/proxy",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/proxy_codex_default",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/proxy_start",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/proxy_status",
		"TestPureGlobalInspectionLexicalErrorsMatchHandlers/refresh",
	)
	sort.Strings(cmdSelection.FullTestIDs)
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
		"TestCommandDigestV1ArgvSubstitutionMatrix",
		"TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory",
		"TestExecutionResultDigestV1MatchesLiteralFraming",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture",
		"TestNewCUReportV1TreatsForgedReportOutputAsCapturedBytes",
		"TestOpenReleaseCanonicalStoresRejectsRootReplacementDuringRetainedOpen",
		"TestParseBlueprintReviewResultAcceptsCleanAndNotClean",
		"TestParseBlueprintReviewResultRejectsAbsentOrStaleAuthority",
		"TestParseBlueprintReviewResultRejectsFindingOrderAndOversizeInput",
		"TestParseBlueprintReviewSiblingAcceptsOneByteStreaming",
		"TestParseBlueprintReviewSiblingEnforcesRecordCapBeforeDecodeAndJCS",
		"TestParseBlueprintReviewSiblingRejectsRecordDigestCorruption",
		"TestParseCUManifestAcceptsClosedCU0Selection",
		"TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount",
		"TestParseCUManifestRejectsUnknownMemberBeforeDispatch",
		"TestReleaseAncestryEvidenceMustReturnSignedMergeBase",
		"TestReleaseBuildArgvV1RejectsPlaceholderCommands",
		"TestReleaseCanonicalRecoveryExcludesStagedInertTempsFromProjection",
		"TestReleaseCanonicalRecoveryLeavesMalformedTempsUntouchedWhenStoreIsOverCapacity",
		"TestReleaseCanonicalRecoveryRejectsGlobalOverCardinalityBeforeMutation",
		"TestReleaseCanonicalStoresCloseCrossTypeDurabilityBeforeUnrelatedRetry",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds",
		"TestReleaseCanonicalStoresPublishAndAdoptSixCurrentSchemas",
		"TestReleaseCanonicalStoresReconcileEveryPolicyBeforeUnrelatedPublish",
		"TestReleaseCanonicalStoresReconcileTypedTempsAndKeepFutureAndGCInactive",
		"TestReleaseCanonicalStoresRefuseAggregatePlusOneWithValidTempsBeforePromotion",
		"TestReleaseCanonicalStoresRefuseValidOverCapacityTempsBeforePromotion",
		"TestReleaseCanonicalStoresRejectDescriptorTypeCanonicalAndCapDrift",
		"TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema",
		"TestReleaseCanonicalStoresRejectMissingRequiredSignedMembers",
		"TestReleaseCanonicalStoresRejectProvenanceRootResidue",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema",
		"TestReleaseCanonicalStoresSerialiseConcurrentAuthorityBoundary",
		"TestReleaseCanonicalStoresSerialiseIndependentStoreInstances",
		"TestReleaseCanonicalTempCleanupJoinsUnlinkAndDirectorySyncFailures",
		"TestReleaseDescendantOpenFlagsAreNonblocking",
		"TestReleaseGraphStructureRejectsResignedFictionalEntrySize",
		"TestReleaseObjectDigestV1UsesPerTypeCaps",
		"TestReleaseReachabilityCatalogueRemainsInactiveUntilCU2Regeneration",
		"TestReleaseRoleEvidenceSizeCaps",
		"TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality",
		"TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality",
		"TestVerifyBlueprintReviewAcceptsFrozenRound44",
		"TestVerifyBlueprintReviewRejectsSymlinkAuthorityFiles",
		"TestVerifyPairedReleaseGraphsV1RemainsInactiveBeforeCU9",
		"TestVerifyReleaseBuildAuthorityV1RejectsSelfAuthorisedBundle",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedFloorAndRejectsSubstitution",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution",
		"TestVerifyReleaseGraphV1RejectsIncompleteGraph",
		"TestVerifyReleaseGraphV1RejectsUnavailableConstructionUnits",
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
		"TestBlueprintReviewNegativeVectorMatrix/invalid_severity",
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
		"TestBlueprintReviewNegativeVectorMatrix/record_uint32_endian_variant",
		"TestBlueprintReviewNegativeVectorMatrix/result_2097153_bytes",
		"TestBlueprintReviewNegativeVectorMatrix/result_every_truncation_boundary",
		"TestBlueprintReviewNegativeVectorMatrix/result_uint32_endian_variant",
		"TestBlueprintReviewNegativeVectorMatrix/reused_task_label",
		"TestBlueprintReviewNegativeVectorMatrix/round_above_safe",
		"TestBlueprintReviewNegativeVectorMatrix/round_non_integer",
		"TestBlueprintReviewNegativeVectorMatrix/round_zero",
		"TestBlueprintReviewNegativeVectorMatrix/schema_mismatch",
		"TestBlueprintReviewNegativeVectorMatrix/sibling_4290_bytes",
		"TestBlueprintReviewNegativeVectorMatrix/stale_baseline",
		"TestBlueprintReviewNegativeVectorMatrix/stale_blueprint",
		"TestBlueprintReviewNegativeVectorMatrix/task_label_257",
		"TestBlueprintReviewNegativeVectorMatrix/trailing_byte",
		"TestBlueprintReviewNegativeVectorMatrix/uint32_endian_variant",
		"TestBlueprintReviewNegativeVectorMatrix/uint64_endian_variant",
		"TestBlueprintReviewNegativeVectorMatrix/unicode_normalisation",
		"TestBlueprintReviewNegativeVectorMatrix/unicode_task_label",
		"TestBlueprintReviewNegativeVectorMatrix/unknown_top_member",
		"TestBlueprintReviewNegativeVectorMatrix/unknown_nested_finding_member",
		"TestBlueprintReviewNegativeVectorMatrix/verdict_enum_drift",
		"TestBlueprintReviewNegativeVectorMatrix/whitespace",
		"TestBlueprintReviewNegativeVectorMatrix/wrong_kind",
		"TestBlueprintReviewNegativeVectorMatrix/wrong_severity_order",
		"TestBlueprintReviewNegativeVectorMatrix/wrong_same_severity_id_order",
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
		"TestCommandDigestV1ArgvSubstitutionMatrix/empty_argument",
		"TestCommandDigestV1ArgvSubstitutionMatrix/flag_removed",
		"TestCommandDigestV1ArgvSubstitutionMatrix/flag_reordered",
		"TestCommandDigestV1ArgvSubstitutionMatrix/package",
		"TestCommandDigestV1ArgvSubstitutionMatrix/subcommand",
		"TestCommandDigestV1ArgvSubstitutionMatrix/tool",
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
		"TestReleaseRoleEvidenceSizeCaps/ABI_plus_one",
		"TestReleaseRoleEvidenceSizeCaps/exact_ABI",
		"TestReleaseRoleEvidenceSizeCaps/exact_payload",
		"TestReleaseRoleEvidenceSizeCaps/exact_signature",
		"TestReleaseRoleEvidenceSizeCaps/payload_plus_one",
		"TestReleaseRoleEvidenceSizeCaps/signature_plus_one",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/aggregate_plus_one",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/aggregate_plus_one/provenance",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/aggregate_plus_one/set_reports",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/fifth_provenance_temporary",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/fifth_temporary",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/forty-first_report",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/ninth_provenance_authority",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/ninth_provenance_bundle",
		"TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds/ninth_set",
		"TestReleaseCanonicalStoresReconcileTypedTempsAndKeepFutureAndGCInactive/floor_acceptance",
		"TestReleaseCanonicalStoresReconcileTypedTempsAndKeepFutureAndGCInactive/inner_validation",
		"TestReleaseCanonicalStoresReconcileTypedTempsAndKeepFutureAndGCInactive/outer_validation",
		"TestReleaseCanonicalStoresReconcileTypedTempsAndKeepFutureAndGCInactive/reference_aware_gc",
		"TestReleaseCanonicalStoresRejectDescriptorTypeCanonicalAndCapDrift/exact_caps",
		"TestReleaseCanonicalStoresRejectDescriptorTypeCanonicalAndCapDrift/retained_descriptor",
		"TestReleaseCanonicalStoresRejectDescriptorTypeCanonicalAndCapDrift/unsafe_category",
		"TestReleaseCanonicalStoresRejectDescriptorTypeCanonicalAndCapDrift/wrong_type_and_noncanonical",
		"TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema/ancestry",
		"TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema/artifact_set",
		"TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema/authority",
		"TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema/build_report",
		"TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema/bundle",
		"TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema/CU_report_set",
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
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/FIFO",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/group-readable_file",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/group-searchable_directory",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/hard-linked_file",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/manifest_substitution",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/missing_file",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/nested_directory",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/payload_substitution",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/socket",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/symlink",
		"TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles/unknown_file",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedFloorAndRejectsSubstitution/bundle_digest",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedFloorAndRejectsSubstitution/report_source",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedFloorAndRejectsSubstitution/role_payload",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedFloorAndRejectsSubstitution/signature",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry/ABI_substitution",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry/CU_missing",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry/ancestry_signature",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry/missing_ancestry",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry/role_swap",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry/same_source",
		"TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry/wrong_merge_base",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/build_argv",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/build_cardinality",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/build_result",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/CU_cardinality",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/CU_invocation",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/CU_manifest",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/CU_result",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/environment",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/repository",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/role_ABI",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/role_cardinality",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/role_payload",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/role_signature",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/source_commit",
		"TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution/source_tree",
		"TestVerifyReleaseBuildAuthorityV1RejectsSelfAuthorisedBundle/digest",
		"TestVerifyReleaseBuildAuthorityV1RejectsSelfAuthorisedBundle/key",
	)
	proxySelection.FullTestIDs = append(proxySelection.FullTestIDs,
		"TestReleaseCanonicalStoresRefuseAggregatePlusOneWithValidTempsBeforePromotion/provenance_aggregate",
		"TestReleaseCanonicalStoresRefuseAggregatePlusOneWithValidTempsBeforePromotion/set_report_aggregate",
		"TestReleaseCanonicalStoresRefuseValidOverCapacityTempsBeforePromotion/forty-first_report",
		"TestReleaseCanonicalStoresRefuseValidOverCapacityTempsBeforePromotion/ninth_set",
		"TestReleaseCanonicalStoresRejectMissingRequiredSignedMembers/ancestry",
		"TestReleaseCanonicalStoresRejectMissingRequiredSignedMembers/artifact_set",
		"TestReleaseCanonicalStoresRejectMissingRequiredSignedMembers/build_report",
		"TestReleaseCanonicalStoresRejectMissingRequiredSignedMembers/bundle",
		"TestReleaseCanonicalStoresRejectMissingRequiredSignedMembers/CU_report_set",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/ancestry_lineage",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/artifact_role_order",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/authority_encoding",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/build_role_order",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/build_self_signature",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/bundle_entry_size",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/CU_evidence_digest",
		"TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema/empty_report_roles_must_be_an_array",
		"TestReleaseCanonicalRecoveryRejectsGlobalOverCardinalityBeforeMutation/forty-one_existing_reports_block_unrelated_adopt",
		"TestReleaseCanonicalRecoveryRejectsGlobalOverCardinalityBeforeMutation/nine_existing_authorities_block_unrelated_temp_promotion",
		"TestReleaseCanonicalRecoveryRejectsGlobalOverCardinalityBeforeMutation/nine_existing_bundles_block_unrelated_adopt_and_temp_promotion",
		"TestReleaseCanonicalRecoveryRejectsGlobalOverCardinalityBeforeMutation/nine_existing_sets_block_unrelated_publish",
		"TestReleaseCanonicalRecoveryExcludesStagedInertTempsFromProjection/five_malformed_registered_temps",
		"TestReleaseCanonicalRecoveryExcludesStagedInertTempsFromProjection/oversized_build_report",
		"TestReleaseCanonicalRecoveryExcludesStagedInertTempsFromProjection/unknown_oversized_residue",
		"TestReleaseCanonicalStoresCloseCrossTypeDurabilityBeforeUnrelatedRetry/authorities",
		"TestReleaseCanonicalStoresCloseCrossTypeDurabilityBeforeUnrelatedRetry/bundles",
		"TestReleaseCanonicalStoresCloseCrossTypeDurabilityBeforeUnrelatedRetry/reports",
		"TestReleaseCanonicalStoresCloseCrossTypeDurabilityBeforeUnrelatedRetry/sets",
	)
	sort.Strings(proxySelection.FullTestIDs)
	proxySelection.MinimumPassCount = len(proxySelection.FullTestIDs)
	proxyCUSelection := newCUTestPackage("./internal/tools/proxycu",
		"TestDarwinProcessTrackerCloseReturnsQueuedTrackingError",
		"TestDarwinProcessTrackerFailsClosedOnEnumerationError",
		"TestDarwinProcessTrackerFailsClosedWithoutKillingReusedPID",
		"TestDarwinProcessTrackerFindsChildCreatedDuringContainment",
		"TestNewOSCommandRunnerClosesAmbientGoEnvironment",
		"TestOSCommandRunnerClosesBothStartGateDescriptorsWhenStartFails",
		"TestOSCommandRunnerContainsDetachedClosedPipeChild",
		"TestOSCommandRunnerContainsDoubleForkedSetsidChild",
		"TestOSCommandRunnerContainsPipeHoldingGrandchild",
		"TestOSCommandRunnerContainsSetsidGrandchild",
		"TestRunRejectsAbsentAndUnmanifestedCU",
		"TestShellWrappersRejectArityBeforeGoOrTemporaryWork",
		"TestShellWrappersVerifyFrozenReviewAndSelfTest",
		"TestVerifyTestEventsAcceptsExactRunAndPass",
		"TestVerifyTestEventsRejectsCaseAliasMembersBeforeTypedDecode",
		"TestVerifyTestEventsRejectsCorruptEvidence",
		"TestVerifyTestEventsRejectsDuplicateObjectMembers",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition",
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
		"TestVerifyTestEventsRejectsCaseAliasMembersBeforeTypedDecode/Action_action",
		"TestVerifyTestEventsRejectsCaseAliasMembersBeforeTypedDecode/Package_package",
		"TestVerifyTestEventsRejectsCaseAliasMembersBeforeTypedDecode/Test_test",
		"TestVerifyTestEventsRejectsDuplicateObjectMembers/action",
		"TestVerifyTestEventsRejectsDuplicateObjectMembers/known_output",
		"TestVerifyTestEventsRejectsDuplicateObjectMembers/package",
		"TestVerifyTestEventsRejectsDuplicateObjectMembers/test",
		"TestVerifyTestEventsRejectsDuplicateObjectMembers/unknown",
		"TestShellWrappersRejectArityBeforeGoOrTemporaryWork/scripts_build-proxy-release",
		"TestShellWrappersRejectArityBeforeGoOrTemporaryWork/scripts_verify-blueprint-reviewextra",
		"TestShellWrappersRejectArityBeforeGoOrTemporaryWork/scripts_verify-proxy-cu",
		"TestShellWrappersRejectArityBeforeGoOrTemporaryWork/scripts_verify-proxy-cuCU-0_extra",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/duplicate_package_start",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/missing_package_start",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/package_fail",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/package_output_after_pass",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/package_output_before_start",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/package_pass_before_tests",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/test_cont_before_pause",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/test_fail",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/test_output_after_pass",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/test_output_before_run",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/test_pass_while_paused",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/test_pause_before_run",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/test_run_after_pass",
		"TestVerifyTestEventsRejectsEveryInvalidStateTransition/unselected_output",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/blank_line",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/CRLF_framing",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/concatenated_object",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/duplicate_terminal",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/missing_terminal",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/missing_final_LF",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/package_skip",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/package_substitution",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/padded_object",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/whitespace_line",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/unknown_package_action",
		"TestVerifyTestEventsRejectsFramingAndPackageSubstitution/unknown_test_action",
	)
	sort.Strings(proxyCUSelection.FullTestIDs)
	proxyCUSelection.MinimumPassCount = len(proxyCUSelection.FullTestIDs)
	releaseSelection := newCUTestPackage("./internal/tools/proxyrelease",
		"TestBuildProxyReleaseShellEntryParsesThenRequiresExactSource",
		"TestBuildProxyReleaseShellEntryRejectsMissingManifest",
		"TestParseReleaseBuildManifestAcceptsClosedRequestAndRejectsUnknown",
		"TestReadReleaseBuildManifestRejectsSymlink",
		"TestRunRequiresExactSourceBeforeReleaseWork",
	)
	releaseSelection.FullTestIDs = append(releaseSelection.FullTestIDs,
		"TestRunRequiresExactSourceBeforeReleaseWork/floor",
		"TestRunRequiresExactSourceBeforeReleaseWork/target",
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

func canonicalCU2ManifestV1() ([]byte, error) {
	authSelection := newCUTestPackage("./internal/auth",
		"TestOAuthAcceptedCallbackCanonicalBound",
		"TestOAuthAcceptedCallbackQueryGrammar",
		"TestOAuthAcceptedCallbackQueryRejectsBeforeAcceptance",
	)
	providerSelection := newCUTestPackage("./internal/provider/codex",
		"TestCredentialOwnerCommitReceiptOrdering",
		"TestCredentialOwnerContinuationRecoveryRejectsMismatchBeforeAdoption",
		"TestCredentialOwnerCrashAfterReceiptRecoversOriginalCommit",
		"TestCredentialOwnerRefreshResultEnvelopeMaxAndPlusOne",
		"TestCredentialOwnerWaiterReopensAfterTerminalAndCannotReplay",
		"TestNewCredentialCoordinatorDoesNotCreateCredentialOwnerAuthority",
		"TestNewCredentialCoordinatorWithAuthorityDerivesSeparatePurposeKeys",
		"TestNewCredentialCoordinatorWithAuthorityRejectsCandidate",
		"TestRefreshMutationCapacityExactAndPlusOne",
		"TestRefreshMutationCompleteStableOperationFitsCredentialShare",
		"TestRefreshMutationDurableSelectionPrecedesEffectAndRecovers",
		"TestRefreshMutationRefusesInsufficientStableOperationCapacityBeforePublication",
		"TestRefreshMutationReservationFailurePreventsEffect",
		"TestRefreshMutationReservationPrecedesCredentialEffect",
		"TestRefreshMutationSourceActionMapIsSignedAndOneToOne",
		"TestRefreshRestartAdoptsOwnerContinuationBeforeAnchor",
		"TestRefreshRestartDoesNotReplayAttemptWithoutDurableResult",
		"TestRefreshRestartDoesNotReplayDurablePostExchangeResult",
		"TestRefreshRestartRecoversDurablePostSelectionContinuation",
		"TestRefreshRetainedRecoveryTerminalisesOriginalReservation",
		"TestRefreshRetainsSelectionWhenOwnerCommitFails",
	)
	providerSelection.FullTestIDs = append(providerSelection.FullTestIDs,
		"TestCredentialOwnerRefreshResultEnvelopeMaxAndPlusOne/max",
		"TestCredentialOwnerRefreshResultEnvelopeMaxAndPlusOne/plus_one",
		"TestRefreshMutationRefusesInsufficientStableOperationCapacityBeforePublication/567_files",
		"TestRefreshMutationRefusesInsufficientStableOperationCapacityBeforePublication/near_byte_cap",
		"TestRefreshRestartAdoptsOwnerContinuationBeforeAnchor/continuation_durable",
		"TestRefreshRestartAdoptsOwnerContinuationBeforeAnchor/selected_object_durable",
		"TestRefreshMutationCapacityExactAndPlusOne/OAuth_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/OAuth_result",
		"TestRefreshMutationCapacityExactAndPlusOne/byte",
		"TestRefreshMutationCapacityExactAndPlusOne/credential_byte",
		"TestRefreshMutationCapacityExactAndPlusOne/credential_file",
		"TestRefreshMutationCapacityExactAndPlusOne/decision",
		"TestRefreshMutationCapacityExactAndPlusOne/decision_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/fixed_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/mutation",
		"TestRefreshMutationCapacityExactAndPlusOne/mutation_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/oauth",
		"TestRefreshMutationCapacityExactAndPlusOne/operator_slot",
		"TestRefreshMutationCapacityExactAndPlusOne/outer_base",
		"TestRefreshMutationCapacityExactAndPlusOne/outer_object_byte",
		"TestRefreshMutationCapacityExactAndPlusOne/plan_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/progress",
		"TestRefreshMutationCapacityExactAndPlusOne/reauth_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/selected_lease_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/terminal_lease_delta",
		"TestRefreshMutationCapacityExactAndPlusOne/unit",
		"TestRefreshMutationCapacityExactAndPlusOne/wire_frame",
	)
	sort.Strings(providerSelection.FullTestIDs)
	providerSelection.MinimumPassCount = len(providerSelection.FullTestIDs)
	proxySelection := newCUTestPackage("./internal/proxy",
		"TestCredentialOwnerFilesystemBackendReopensTerminalAuthority",
		"TestInstanceAuthorityExternalReferenceRowsAreClosed",
		"TestInstanceAuthorityStagedActivation",
		"TestOperationCoordinatorChildSelectionMapping",
		"TestOperationCoordinatorCrashAfterObjectReopensWithoutSelectingIt",
		"TestOperationCoordinatorKeyBootstrapRecovery",
		"TestOperationCoordinatorOrdering",
	)
	return CanonicalJSONV1(CUManifestV1{
		SchemaVersion:                    1,
		Kind:                             "construction_unit_verification_manifest_v1",
		BlueprintSHA256:                  frozenBlueprintSHA256,
		ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256,
		ReviewAuthorityBaselineCommit:    frozenReviewBaseline,
		Unit:                             "CU-2",
		RaceCount:                        1,
		Packages:                         []CUTestPackageV1{authSelection, providerSelection, proxySelection},
	})
}

func canonicalCU1ManifestV1() ([]byte, error) {
	cmdSelection := newCUTestPackage("./cmd/cq",
		"TestDarwinProxyInspectionBoundaryHasNoLiveCollectorsInCU1",
		"TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState",
		"TestProxyCommandBuildsTypedArgumentsAndDeadlines",
		"TestProxyCommandClassifiesCandidateReceiptLookupBeforeState",
		"TestProxyCommandClassifiesModelsAndCodexAuxiliaryCatalogues",
		"TestProxyCommandClassifiesRefreshAndOperatorRows",
		"TestProxyCommandClassifiesThirteenOrdinaryRows",
		"TestProxyCommandHelpAnywhereAndIgnoredRefreshTails",
		"TestProxyCommandOperatorArgumentsAreTyped",
		"TestProxyCommandParsesCacheTTL",
		"TestProxyCommandPreservesOrdinaryEndOfFlags",
		"TestProxyCommandUsesExactRefreshReadDeadlines",
		"TestProxyDoctorChecksAreDerivedFromIndependentFacts",
		"TestProxyInspectCollectsIndependentFactsAndNeverSynthesisesSuccess",
		"TestProxyInspectHonoursCancelledContextWithoutCallingCollectors",
		"TestProxyInspectNormalisesUnsafeCollectorFacts",
		"TestProxyInspectPropagatesWriterErrors",
		"TestProxyInspectRendersHumanJSONAndDoctorFacts",
		"TestProxyStatusPreDispatchBoundaryUsesOnlyInjectedCollectors",
		"TestProxyStatusPreDispatchPreservesFrozenBareStatus",
	)
	proxySelection := newCUTestPackage("./internal/proxy",
		"TestProxySnapshotFactJSONPreservesUnknownState",
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology",
		"TestProxySnapshotInspectorSkewIsDescriptive",
		"TestProxySnapshotReconcilesSupportedTopologies",
		"TestProxySnapshotRejectsIdentityMismatchAndDoesNotTrustHealthAlone",
		"TestProxySnapshotRejectsMalformedFactsAndUnknownVocabulary",
	)
	proxySelection.FullTestIDs = append(proxySelection.FullTestIDs,
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology/data_plane_contradictory",
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology/data_plane_not_proven",
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology/desired_listener_mismatch",
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology/desired_manager_mismatch",
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology/listener_not_listening",
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology/process_identity_missing",
		"TestProxySnapshotHealthyRequiresCoherentRunningTopology/service_stopped",
		"TestProxySnapshotReconcilesSupportedTopologies/cq_healthy",
		"TestProxySnapshotReconcilesSupportedTopologies/crash_looping",
		"TestProxySnapshotReconcilesSupportedTopologies/foreign_listener",
		"TestProxySnapshotReconcilesSupportedTopologies/homebrew_legacy",
		"TestProxySnapshotReconcilesSupportedTopologies/manual_legacy",
		"TestProxySnapshotReconcilesSupportedTopologies/required_collector_unavailable",
		"TestProxySnapshotReconcilesSupportedTopologies/stopped",
	)
	sort.Strings(proxySelection.FullTestIDs)
	proxySelection.MinimumPassCount = len(proxySelection.FullTestIDs)
	return CanonicalJSONV1(CUManifestV1{
		SchemaVersion:                    1,
		Kind:                             "construction_unit_verification_manifest_v1",
		BlueprintSHA256:                  frozenBlueprintSHA256,
		ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256,
		ReviewAuthorityBaselineCommit:    frozenReviewBaseline,
		Unit:                             "CU-1",
		RaceCount:                        1,
		Packages:                         []CUTestPackageV1{cmdSelection, proxySelection},
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
	Signature                   string `json:"signature"`
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
	Signature                   string                   `json:"signature"`
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
	Signature                                     string       `json:"signature"`
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
	SetSignature                                  string                  `json:"set_signature"`
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
	BundleSignature                 string                 `json:"bundle_signature"`
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

const (
	releaseSetReportStoreMaxBytes = 64 << 20
	releaseProvenanceMaxBytes     = 3 << 20
)

type releaseCanonicalStoresV1 struct {
	mutationMu  sync.Mutex
	rootFD      *os.File
	mutationFD  *os.File
	root        fsutil.SecureDirectory
	sets        fsutil.SecureDirectory
	reports     fsutil.SecureDirectory
	provenance  fsutil.SecureDirectory
	authorities fsutil.SecureDirectory
	bundles     fsutil.SecureDirectory
	device      uint64
}

var openReleaseSecureDirectoryV1 = func(path string) (fsutil.SecureDirectory, error) {
	return (fsutil.OSFileSystem{}).OpenSecureDirectory(path)
}

type releaseCanonicalStoreClassV1 uint8

const (
	releaseSetClassV1 releaseCanonicalStoreClassV1 = iota
	releaseReportClassV1
	releaseAuthorityClassV1
	releaseBundleClassV1
)

type releaseCanonicalPolicyV1 struct {
	directory fsutil.SecureDirectory
	class     releaseCanonicalStoreClassV1
	domain    string
	tag       string
	maxBytes  int64
	decode    func([]byte) (any, error)
}

type releaseCanonicalStoreCountsV1 struct {
	sets            int
	reports         int
	authorities     int
	bundles         int
	setReportTemps  int
	provenanceTemps int
	setReportBytes  uint64
	provenanceBytes uint64
}

type releaseSecureDirectoryV1 interface {
	fsutil.SecureDirectory
	fsutil.DurableDirectory
}

func openReleaseCanonicalStoresV1(root string) (*releaseCanonicalStoresV1, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("release canonical store root must be exact absolute path")
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open release canonical store root: %w", err)
	}
	if err := validateLocalReleaseFilesystem(rootFD); err != nil {
		_ = unix.Close(rootFD)
		return nil, err
	}
	rootFile := os.NewFile(uintptr(rootFD), root)
	if rootFile == nil {
		_ = unix.Close(rootFD)
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	opener := fsutil.OSFileSystem{}
	rootDirectory, err := openReleaseSecureDirectoryV1(root)
	if err != nil {
		_ = rootFile.Close()
		return nil, fmt.Errorf("retain release canonical store root: %w", err)
	}
	store := &releaseCanonicalStoresV1{rootFD: rootFile, root: rootDirectory}
	fail := func(cause error) (*releaseCanonicalStoresV1, error) {
		_ = store.close()
		return nil, cause
	}
	rootInfo, err := rootDirectory.Stat()
	if err != nil {
		return fail(err)
	}
	rootIdentity, ok := opener.FileIdentity(rootInfo)
	if !ok {
		return fail(fmt.Errorf("release canonical store root identity is unavailable"))
	}
	rawInfo, err := rootFile.Stat()
	if err != nil {
		return fail(err)
	}
	rawIdentity, rawOK := opener.FileIdentity(rawInfo)
	if !rawOK || rawIdentity.Device != rootIdentity.Device || rawIdentity.Inode != rootIdentity.Inode {
		return fail(fmt.Errorf("release canonical store root changed during retained open"))
	}
	store.device = rootIdentity.Device
	lockFD, err := unix.Openat(int(rootFile.Fd()), ".release-canonical-store.lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open release canonical mutation lock: %w", err))
	}
	store.mutationFD = os.NewFile(uintptr(lockFD), ".release-canonical-store.lock")
	if store.mutationFD == nil {
		_ = unix.Close(lockFD)
		return fail(fsutil.ErrSecureCapabilityUnavailable)
	}
	lockInfo, err := store.mutationFD.Stat()
	if err != nil {
		return fail(err)
	}
	lockIdentity, lockIdentityOK := opener.FileIdentity(lockInfo)
	lockOwner, lockOwnerOK := opener.FileOwnerUID(lockInfo)
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 || !lockIdentityOK || lockIdentity.Device != store.device || lockIdentity.Links != 1 || !lockOwnerOK || lockOwner != uint64(os.Geteuid()) {
		return fail(fmt.Errorf("release canonical mutation lock has unsafe metadata"))
	}
	rootRetained, ok := rootDirectory.(releaseSecureDirectoryV1)
	if !ok {
		return fail(fsutil.ErrSecureCapabilityUnavailable)
	}
	store.sets, err = openReleaseCanonicalChildDirectoryV1(rootRetained, "release-sets", store.device)
	if err != nil {
		return fail(err)
	}
	store.reports, err = openReleaseCanonicalChildDirectoryV1(rootRetained, "release-reports", store.device)
	if err != nil {
		return fail(err)
	}
	store.provenance, err = openReleaseCanonicalChildDirectoryV1(rootRetained, "release-provenance", store.device)
	if err != nil {
		return fail(err)
	}
	provenanceRetained, ok := store.provenance.(releaseSecureDirectoryV1)
	if !ok {
		return fail(fsutil.ErrSecureCapabilityUnavailable)
	}
	store.authorities, err = openReleaseCanonicalChildDirectoryV1(provenanceRetained, "authorities", store.device)
	if err != nil {
		return fail(err)
	}
	store.bundles, err = openReleaseCanonicalChildDirectoryV1(provenanceRetained, "bundles", store.device)
	if err != nil {
		return fail(err)
	}
	if err := store.validateReleaseProvenanceRootV1(); err != nil {
		return fail(err)
	}
	return store, nil
}

func openReleaseCanonicalChildDirectoryV1(parent fsutil.DurableDirectory, name string, device uint64) (fsutil.SecureDirectory, error) {
	child, err := parent.OpenDirectory(name)
	if err != nil {
		return nil, fmt.Errorf("retain release canonical directory %q: %w", name, err)
	}
	secure, ok := child.(fsutil.SecureDirectory)
	if !ok {
		_ = child.Close()
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := secure.Stat()
	if err != nil {
		_ = secure.Close()
		return nil, err
	}
	identity, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
	owner, ownerOK := (fsutil.OSFileSystem{}).FileOwnerUID(info)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || identity.Device != device || !ownerOK || owner != uint64(os.Geteuid()) {
		_ = secure.Close()
		return nil, fmt.Errorf("release canonical directory %q has unsafe metadata", name)
	}
	return secure, nil
}

func (store *releaseCanonicalStoresV1) close() error {
	if store == nil {
		return nil
	}
	var result error
	for _, directory := range []fsutil.SecureDirectory{store.bundles, store.authorities, store.provenance, store.reports, store.sets, store.root} {
		if directory != nil {
			result = errors.Join(result, directory.Close())
		}
	}
	if store.mutationFD != nil {
		result = errors.Join(result, store.mutationFD.Close())
	}
	if store.rootFD != nil {
		result = errors.Join(result, store.rootFD.Close())
	}
	return result
}

func (store *releaseCanonicalStoresV1) lockMutationV1() (func() error, error) {
	store.mutationMu.Lock()
	for {
		err := unix.Flock(int(store.mutationFD.Fd()), unix.LOCK_EX)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			store.mutationMu.Unlock()
			return nil, fmt.Errorf("lock release canonical mutation: %w", err)
		}
		break
	}
	return func() error {
		err := unix.Flock(int(store.mutationFD.Fd()), unix.LOCK_UN)
		store.mutationMu.Unlock()
		return err
	}, nil
}

func (store *releaseCanonicalStoresV1) validateReleaseProvenanceRootV1() error {
	reader, ok := store.provenance.(fsutil.SecureDirectoryReader)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return err
	}
	if len(entries) != 2 {
		return fmt.Errorf("release provenance root must contain exactly authorities and bundles")
	}
	names := []string{entries[0].Name(), entries[1].Name()}
	sort.Strings(names)
	if names[0] != "authorities" || names[1] != "bundles" {
		return fmt.Errorf("release provenance root contains residue")
	}
	return nil
}

func (store *releaseCanonicalStoresV1) artifactSetPolicy() releaseCanonicalPolicyV1 {
	return releaseCanonicalPolicyV1{store.sets, releaseSetClassV1, "cq/release-artifact-set/v1\x00", "release-artifact-set-v1", 1 << 20, decodeReleaseCanonicalObjectV1[ReleaseArtifactSetV1]}
}

func (store *releaseCanonicalStoresV1) ancestryPolicy() releaseCanonicalPolicyV1 {
	return releaseCanonicalPolicyV1{store.reports, releaseReportClassV1, "cq/source-ancestry-receipt/v1\x00", "source-ancestry-receipt-v1", 1 << 20, decodeReleaseCanonicalObjectV1[SourceAncestryReceiptV1]}
}

func (store *releaseCanonicalStoresV1) buildReportPolicy() releaseCanonicalPolicyV1 {
	return releaseCanonicalPolicyV1{store.reports, releaseReportClassV1, "cq/release-build-report/v1\x00", "release-build-report-v1", 64 << 10, decodeReleaseCanonicalObjectV1[ReleaseBuildReportV1]}
}

func (store *releaseCanonicalStoresV1) cuReportSetPolicy() releaseCanonicalPolicyV1 {
	return releaseCanonicalPolicyV1{store.reports, releaseReportClassV1, "cq/construction-unit-report-set/v1\x00", "construction-unit-report-set-v1", 64 << 10, decodeReleaseCanonicalObjectV1[ConstructionUnitReportSetV1]}
}

func (store *releaseCanonicalStoresV1) authorityPolicy() releaseCanonicalPolicyV1 {
	return releaseCanonicalPolicyV1{store.authorities, releaseAuthorityClassV1, "cq/release-build-authority/v1\x00", "release-build-authority-v1", 64 << 10, decodeReleaseCanonicalObjectV1[ReleaseBuildAuthorityV1]}
}

func (store *releaseCanonicalStoresV1) bundlePolicy() releaseCanonicalPolicyV1 {
	return releaseCanonicalPolicyV1{store.bundles, releaseBundleClassV1, "cq/release-bundle/v1\x00", "release-bundle-v1", 64 << 10, decodeReleaseCanonicalObjectV1[ReleaseBundleV1]}
}

func (store *releaseCanonicalStoresV1) publishReleaseArtifactSet(value ReleaseArtifactSetV1) (string, error) {
	return publishTypedReleaseCanonicalObjectV1(store, store.artifactSetPolicy(), value)
}

func (store *releaseCanonicalStoresV1) adoptReleaseArtifactSet(reader io.Reader) (ReleaseArtifactSetV1, string, error) {
	return adoptTypedReleaseCanonicalObjectV1[ReleaseArtifactSetV1](store, store.artifactSetPolicy(), reader)
}

func (store *releaseCanonicalStoresV1) publishSourceAncestryReceipt(value SourceAncestryReceiptV1) (string, error) {
	return publishTypedReleaseCanonicalObjectV1(store, store.ancestryPolicy(), value)
}

func (store *releaseCanonicalStoresV1) adoptSourceAncestryReceipt(reader io.Reader) (SourceAncestryReceiptV1, string, error) {
	return adoptTypedReleaseCanonicalObjectV1[SourceAncestryReceiptV1](store, store.ancestryPolicy(), reader)
}

func (store *releaseCanonicalStoresV1) publishReleaseBuildReport(value ReleaseBuildReportV1) (string, error) {
	return publishTypedReleaseCanonicalObjectV1(store, store.buildReportPolicy(), value)
}

func (store *releaseCanonicalStoresV1) adoptReleaseBuildReport(reader io.Reader) (ReleaseBuildReportV1, string, error) {
	return adoptTypedReleaseCanonicalObjectV1[ReleaseBuildReportV1](store, store.buildReportPolicy(), reader)
}

func (store *releaseCanonicalStoresV1) publishConstructionUnitReportSet(value ConstructionUnitReportSetV1) (string, error) {
	return publishTypedReleaseCanonicalObjectV1(store, store.cuReportSetPolicy(), value)
}

func (store *releaseCanonicalStoresV1) adoptConstructionUnitReportSet(reader io.Reader) (ConstructionUnitReportSetV1, string, error) {
	return adoptTypedReleaseCanonicalObjectV1[ConstructionUnitReportSetV1](store, store.cuReportSetPolicy(), reader)
}

func (store *releaseCanonicalStoresV1) publishReleaseBuildAuthority(value ReleaseBuildAuthorityV1) (string, error) {
	return publishTypedReleaseCanonicalObjectV1(store, store.authorityPolicy(), value)
}

func (store *releaseCanonicalStoresV1) adoptReleaseBuildAuthority(reader io.Reader) (ReleaseBuildAuthorityV1, string, error) {
	return adoptTypedReleaseCanonicalObjectV1[ReleaseBuildAuthorityV1](store, store.authorityPolicy(), reader)
}

func (store *releaseCanonicalStoresV1) publishReleaseBundle(value ReleaseBundleV1) (string, error) {
	return publishTypedReleaseCanonicalObjectV1(store, store.bundlePolicy(), value)
}

func (store *releaseCanonicalStoresV1) adoptReleaseBundle(reader io.Reader) (ReleaseBundleV1, string, error) {
	return adoptTypedReleaseCanonicalObjectV1[ReleaseBundleV1](store, store.bundlePolicy(), reader)
}

func (store *releaseCanonicalStoresV1) publishRollbackFloorAcceptanceReceipt() error {
	return fmt.Errorf("feature inactive: rollback floor acceptance receipt schema is not owned by CU-0")
}

func (store *releaseCanonicalStoresV1) publishCandidateReleasePromotionReceipt() error {
	return fmt.Errorf("feature inactive: candidate release promotion receipt schema is not owned by CU-0")
}

func (store *releaseCanonicalStoresV1) publishRollbackFloorValidationReceipt() error {
	return fmt.Errorf("feature inactive: rollback floor validation receipt schema is not owned by CU-0")
}

func (store *releaseCanonicalStoresV1) garbageCollectReferencedObjects() error {
	return fmt.Errorf("feature inactive: reference-aware release provenance GC requires durable authority and history anchors")
}

func publishTypedReleaseCanonicalObjectV1[T any](store *releaseCanonicalStoresV1, policy releaseCanonicalPolicyV1, value T) (string, error) {
	if err := validateCurrentReleaseCanonicalTypeV1(any(value)); err != nil {
		return "", err
	}
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		return "", err
	}
	if int64(len(canonical)) > policy.maxBytes {
		return "", fmt.Errorf("release canonical %s exceeds %d bytes", policy.tag, policy.maxBytes)
	}
	return store.publishReleaseCanonicalBytesV1(policy, canonical)
}

func adoptTypedReleaseCanonicalObjectV1[T any](store *releaseCanonicalStoresV1, policy releaseCanonicalPolicyV1, reader io.Reader) (T, string, error) {
	var zero T
	limited := &io.LimitedReader{R: reader, N: policy.maxBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return zero, "", fmt.Errorf("read release canonical %s: %w", policy.tag, err)
	}
	if limited.N <= 0 {
		return zero, "", fmt.Errorf("release canonical %s exceeds %d bytes", policy.tag, policy.maxBytes)
	}
	decodedAny, err := decodeReleaseCanonicalObjectV1[T](data)
	if err != nil {
		return zero, "", err
	}
	decoded, ok := decodedAny.(T)
	if !ok {
		return zero, "", fmt.Errorf("release canonical %s decoded as wrong type", policy.tag)
	}
	if err := validateCurrentReleaseCanonicalTypeV1(any(decoded)); err != nil {
		return zero, "", err
	}
	digest, err := store.publishReleaseCanonicalBytesV1(policy, data)
	return decoded, digest, err
}

func decodeReleaseCanonicalObjectV1[T any](data []byte) (any, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode release canonical object: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("release canonical object has trailing JSON")
	}
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, data) {
		return nil, fmt.Errorf("release canonical object is not exact canonical JCS")
	}
	return value, nil
}

func validateCurrentReleaseCanonicalTypeV1(value any) error {
	switch object := value.(type) {
	case ReleaseArtifactSetV1:
		return validateIntrinsicReleaseArtifactSetV1(object)
	case SourceAncestryReceiptV1:
		return validateIntrinsicSourceAncestryReceiptV1(object)
	case ReleaseBuildReportV1:
		return validateIntrinsicReleaseBuildReportV1(object)
	case ConstructionUnitReportSetV1:
		return validateIntrinsicConstructionUnitReportSetV1(object)
	case ReleaseBuildAuthorityV1:
		return validateIntrinsicReleaseBuildAuthorityV1(object)
	case ReleaseBundleV1:
		return validateIntrinsicReleaseBundleV1(object)
	default:
		return fmt.Errorf("release canonical object type is unavailable")
	}
}

func validateIntrinsicReleaseBuildAuthorityV1(value ReleaseBuildAuthorityV1) error {
	if value.SchemaVersion != 1 || !validRequiredReleaseStringV1(value.AuthorityID) || !lowerHex64Pattern.MatchString(value.RepositoryIdentityDigest) || !lowerHex64Pattern.MatchString(value.BlueprintSHA256) || !lowerHex64Pattern.MatchString(value.ReviewAttestationAggregateSHA256) || !lowerHex40Pattern.MatchString(value.ReviewAuthorityBaselineCommit) || !lowerHex40Pattern.MatchString(value.LineageRootCommit) || !lowerHex64Pattern.MatchString(value.LineageRootTreeDigest) || !validRequiredReleaseStringV1(value.ToolchainIdentity) {
		return fmt.Errorf("invalid release build authority schema")
	}
	if _, err := decodeEd25519PublicKey(value.Ed25519PublicKey); err != nil {
		return err
	}
	return validateReleaseTimestamp(value.CreatedAt, "authority created_at")
}

func validateIntrinsicSourceAncestryReceiptV1(value SourceAncestryReceiptV1) error {
	if value.SchemaVersion != 1 || value.Kind != "source_ancestry_v1" || !lowerHex64Pattern.MatchString(value.ReleaseBuildAuthorityDigest) || !lowerHex64Pattern.MatchString(value.RepositoryIdentityDigest) || !lowerHex40Pattern.MatchString(value.FloorSourceCommit) || !lowerHex64Pattern.MatchString(value.FloorSourceTreeDigest) || !lowerHex40Pattern.MatchString(value.TargetSourceCommit) || !lowerHex64Pattern.MatchString(value.TargetSourceTreeDigest) || value.TargetSourceCommit == value.FloorSourceCommit || value.MergeBaseCommit != value.FloorSourceCommit || !lowerHex64Pattern.MatchString(value.VerificationCommandDigest) {
		return fmt.Errorf("invalid source ancestry receipt schema")
	}
	if err := validateReleaseTimestamp(value.VerifiedAt, "ancestry verified_at"); err != nil {
		return err
	}
	return verifyIntrinsicReleaseSignatureV1(value.SignerPublicKey, value.Signature, sourceAncestrySignable(value))
}

func validateIntrinsicReleaseBuildReportV1(value ReleaseBuildReportV1) error {
	if value.SchemaVersion != 1 || (value.Kind != "build" && value.Kind != "vet" && value.Kind != "race") || (value.Purpose != "floor" && value.Purpose != "target") || !lowerHex64Pattern.MatchString(value.ReleaseBuildAuthorityDigest) || !lowerHex40Pattern.MatchString(value.SourceCommit) || !lowerHex64Pattern.MatchString(value.SourceTreeDigest) || !validRequiredReleaseStringV1(value.ToolchainIdentity) || !lowerHex64Pattern.MatchString(value.BuildEnvironmentDigest) || !lowerHex64Pattern.MatchString(value.CommandDigest) || value.RoleExecutions == nil || value.Outcome != "passed" || value.ExitCode != 0 || value.RaceEnabled != (value.Kind == "race") || !lowerHex64Pattern.MatchString(value.ExecutionResultDigest) {
		return fmt.Errorf("invalid release build report schema")
	}
	wantRoles := []string(nil)
	if value.Kind == "build" {
		wantRoles = []string{"supervisor", "worker"}
		if value.Purpose == "target" {
			wantRoles = []string{"launcher", "supervisor", "worker"}
		}
	}
	if len(value.RoleExecutions) != len(wantRoles) {
		return fmt.Errorf("invalid release build report role cardinality")
	}
	for index, execution := range value.RoleExecutions {
		if execution.Role != wantRoles[index] || !lowerHex64Pattern.MatchString(execution.BuildCommandDigest) || !lowerHex64Pattern.MatchString(execution.ArtifactPayloadDigest) || !lowerHex64Pattern.MatchString(execution.ArtifactManifestDigest) {
			return fmt.Errorf("invalid release build report role execution")
		}
	}
	if err := validateReleaseTimestamp(value.StartedAt, value.Kind+" started_at"); err != nil {
		return err
	}
	if err := validateReleaseTimestamp(value.EndedAt, value.Kind+" ended_at"); err != nil || value.EndedAt < value.StartedAt {
		return fmt.Errorf("invalid release build report interval")
	}
	return verifyIntrinsicReleaseSignatureV1(value.SignerPublicKey, value.Signature, releaseBuildReportSignable(value))
}

func validateIntrinsicConstructionUnitReportSetV1(value ConstructionUnitReportSetV1) error {
	if value.SchemaVersion != 1 || value.Kind != "construction_unit_report_set_v1" || (value.Purpose != "floor" && value.Purpose != "target") || !lowerHex64Pattern.MatchString(value.ReleaseBuildAuthorityDigest) || !lowerHex64Pattern.MatchString(value.BlueprintSHA256) || !lowerHex64Pattern.MatchString(value.ReviewAttestationAggregateSHA256) || !lowerHex40Pattern.MatchString(value.ReviewAuthorityBaselineCommit) || !lowerHex64Pattern.MatchString(value.LegacyAtomicWriterReachabilityCatalogueDigest) || !lowerHex40Pattern.MatchString(value.SourceCommit) || !lowerHex64Pattern.MatchString(value.SourceTreeDigest) || !validRequiredReleaseStringV1(value.ToolchainIdentity) || !lowerHex64Pattern.MatchString(value.BuildEnvironmentDigest) {
		return fmt.Errorf("invalid construction unit report set schema")
	}
	if err := ValidateCUReportSetV1(value.Purpose, value.Reports); err != nil {
		return err
	}
	for _, report := range value.Reports {
		if !lowerHex64Pattern.MatchString(report.VerificationManifestDigest) || !lowerHex64Pattern.MatchString(report.InvocationDigest) || !lowerHex64Pattern.MatchString(report.ExecutionResultDigest) {
			return fmt.Errorf("invalid construction unit report evidence")
		}
		if err := validateReleaseTimestamp(report.StartedAt, report.CUID+" started_at"); err != nil {
			return err
		}
		if err := validateReleaseTimestamp(report.EndedAt, report.CUID+" ended_at"); err != nil || report.EndedAt < report.StartedAt {
			return fmt.Errorf("invalid construction unit report interval")
		}
	}
	return verifyIntrinsicReleaseSignatureV1(value.SignerPublicKey, value.Signature, cuReportSetSignable(value))
}

func validateIntrinsicReleaseArtifactSetV1(value ReleaseArtifactSetV1) error {
	if value.SchemaVersion != 1 || (value.Purpose != "floor" && value.Purpose != "target") || !lowerHex64Pattern.MatchString(value.ReleaseBuildAuthorityDigest) || !lowerHex40Pattern.MatchString(value.SourceCommit) || !lowerHex64Pattern.MatchString(value.SourceTreeDigest) || !validRequiredReleaseStringV1(value.ToolchainIdentity) || !lowerHex64Pattern.MatchString(value.BuildEnvironmentDigest) || !lowerHex64Pattern.MatchString(value.BuildReportDigest) || !lowerHex64Pattern.MatchString(value.VetReportDigest) || !lowerHex64Pattern.MatchString(value.RaceTestReportDigest) || !lowerHex64Pattern.MatchString(value.ConstructionUnitReportSetDigest) || !lowerHex64Pattern.MatchString(value.LegacyAtomicWriterReachabilityCatalogueDigest) {
		return fmt.Errorf("invalid release artifact set schema")
	}
	wantRoles := []string{"supervisor", "worker"}
	if value.Purpose == "floor" {
		if value.SourceAncestryReceiptDigest != nil || value.LauncherABIDigest != nil || value.RequiredLauncherABIDigest == nil {
			return fmt.Errorf("invalid floor release artifact set nullability")
		}
	} else {
		wantRoles = []string{"launcher", "supervisor", "worker"}
		if value.SourceAncestryReceiptDigest == nil || value.LauncherABIDigest == nil || value.RequiredLauncherABIDigest != nil {
			return fmt.Errorf("invalid target release artifact set nullability")
		}
	}
	for _, digest := range []*string{value.SourceAncestryReceiptDigest, value.LauncherABIDigest, value.RequiredLauncherABIDigest} {
		if digest != nil && !lowerHex64Pattern.MatchString(*digest) {
			return fmt.Errorf("invalid release artifact set optional digest")
		}
	}
	if len(value.Roles) != len(wantRoles) {
		return fmt.Errorf("invalid release artifact set role cardinality")
	}
	for index, role := range value.Roles {
		if role.Role != wantRoles[index] || !lowerHex64Pattern.MatchString(role.ArtifactPayloadDigest) || !lowerHex64Pattern.MatchString(role.ArtifactManifestDigest) {
			return fmt.Errorf("invalid release artifact set role")
		}
	}
	if err := validateReleaseFeatureList(value.SupportedFeatures, "supported_features"); err != nil {
		return err
	}
	if err := validateReleaseFeatureList(value.MinimumFloorFeatures, "minimum_floor_features"); err != nil {
		return err
	}
	return verifyIntrinsicReleaseSignatureV1(value.SignerPublicKey, value.SetSignature, releaseArtifactSetSignable(value))
}

func validateIntrinsicReleaseBundleV1(value ReleaseBundleV1) error {
	if value.SchemaVersion != 1 || (value.Purpose != "floor" && value.Purpose != "target") || !lowerHex64Pattern.MatchString(value.ReleaseBuildAuthorityDigest) || !lowerHex64Pattern.MatchString(value.ReleaseArtifactSetDigest) || !lowerHex64Pattern.MatchString(value.ConstructionUnitReportSetDigest) {
		return fmt.Errorf("invalid release bundle schema")
	}
	if value.Purpose == "floor" {
		if value.SourceAncestryReceiptDigest != nil {
			return fmt.Errorf("invalid floor release bundle nullability")
		}
	} else if value.SourceAncestryReceiptDigest == nil || !lowerHex64Pattern.MatchString(*value.SourceAncestryReceiptDigest) {
		return fmt.Errorf("invalid target release bundle nullability")
	}
	if err := ValidateReleaseBundleEntriesV1(value.Purpose, value.Entries); err != nil {
		return err
	}
	return verifyIntrinsicReleaseSignatureV1(value.SignerPublicKey, value.BundleSignature, releaseBundleSignable(value))
}

func verifyIntrinsicReleaseSignatureV1(encodedPublicKey, encodedSignature string, signable any) error {
	publicKey, err := decodeEd25519PublicKey(encodedPublicKey)
	if err != nil {
		return err
	}
	return verifyReleaseSignature(publicKey, encodedSignature, signable)
}

func validRequiredReleaseStringV1(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func (store *releaseCanonicalStoresV1) publishReleaseCanonicalBytesV1(policy releaseCanonicalPolicyV1, canonical []byte) (string, error) {
	if int64(len(canonical)) > policy.maxBytes {
		return "", fmt.Errorf("release canonical %s exceeds %d bytes", policy.tag, policy.maxBytes)
	}
	digest, err := releaseCanonicalBytesDigestV1(policy.domain, canonical)
	if err != nil {
		return "", err
	}
	unlock, err := store.lockMutationV1()
	if err != nil {
		return "", err
	}
	locked := true
	defer func() {
		if locked {
			_ = unlock()
		}
	}()
	finish := func(result string, resultErr error) (string, error) {
		locked = false
		return result, errors.Join(resultErr, unlock())
	}
	if err := store.validateReleaseProvenanceRootV1(); err != nil {
		return finish("", err)
	}
	if err := store.closeReleaseCanonicalDurabilityV1(); err != nil {
		return finish("", err)
	}
	if err := store.reconcileAllReleaseCanonicalTempsV1(); err != nil {
		return finish("", err)
	}
	finalName := digest + ".json"
	if existing, err := readReleaseCanonicalEntryV1(policy.directory, finalName, policy.maxBytes, store.device); err == nil {
		if !bytes.Equal(existing, canonical) {
			return finish("", fmt.Errorf("release canonical digest collision for %s", digest))
		}
		if err := policy.directory.Sync(); err != nil {
			return finish("", fmt.Errorf("sync existing release canonical directory: %w", err))
		}
		reopened, reopenErr := readReleaseCanonicalEntryV1(policy.directory, finalName, policy.maxBytes, store.device)
		if reopenErr != nil || !bytes.Equal(reopened, canonical) {
			return finish("", errors.Join(fmt.Errorf("reopen existing release canonical object"), reopenErr))
		}
		return finish(digest, nil)
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
		return finish("", err)
	}
	if err := store.checkReleaseCanonicalCapacityV1(policy, uint64(len(canonical))); err != nil {
		return finish("", err)
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return finish("", fmt.Errorf("create release canonical temporary name: %w", err))
	}
	tempName := fmt.Sprintf(".cq-%s-%s-%x.tmp", policy.tag, digest, nonce)
	temporary, err := policy.directory.CreateExclusive(tempName, 0o600)
	if err != nil {
		return finish("", fmt.Errorf("create release canonical temporary: %w", err))
	}
	tempLive := true
	cleanup := func() error {
		if tempLive {
			return errors.Join(policy.directory.Remove(tempName), policy.directory.Sync())
		}
		return nil
	}
	if err := writeAllReleaseCanonicalV1(temporary, canonical); err != nil {
		return finish("", errors.Join(err, temporary.Close(), cleanup()))
	}
	if err := temporary.Sync(); err != nil {
		return finish("", errors.Join(fmt.Errorf("sync release canonical temporary: %w", err), temporary.Close(), cleanup()))
	}
	if err := temporary.Close(); err != nil {
		return finish("", errors.Join(fmt.Errorf("close release canonical temporary: %w", err), cleanup()))
	}
	if err := policy.directory.RenameNoReplace(tempName, finalName); err != nil {
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, unix.EEXIST) {
			return finish("", errors.Join(fmt.Errorf("publish release canonical object: %w", err), cleanup()))
		}
		if syncErr := policy.directory.Sync(); syncErr != nil {
			return finish("", errors.Join(fmt.Errorf("sync colliding release canonical directory: %w", syncErr), cleanup()))
		}
		existing, readErr := readReleaseCanonicalEntryV1(policy.directory, finalName, policy.maxBytes, store.device)
		if readErr != nil || !bytes.Equal(existing, canonical) {
			return finish("", errors.Join(fmt.Errorf("release canonical digest collision for %s", digest), readErr, cleanup()))
		}
		if cleanupErr := cleanup(); cleanupErr != nil {
			return finish("", cleanupErr)
		}
		return finish(digest, nil)
	}
	tempLive = false
	if err := policy.directory.Sync(); err != nil {
		return finish("", fmt.Errorf("sync release canonical directory: %w", err))
	}
	reopened, err := readReleaseCanonicalEntryV1(policy.directory, finalName, policy.maxBytes, store.device)
	if err != nil || !bytes.Equal(reopened, canonical) {
		return finish("", errors.Join(fmt.Errorf("reopen published release canonical object"), err))
	}
	return finish(digest, nil)
}

func (store *releaseCanonicalStoresV1) closeReleaseCanonicalDurabilityV1() error {
	for _, leaf := range []struct {
		name      string
		directory fsutil.SecureDirectory
	}{
		{name: "release-sets", directory: store.sets},
		{name: "release-reports", directory: store.reports},
		{name: "release-provenance/authorities", directory: store.authorities},
		{name: "release-provenance/bundles", directory: store.bundles},
	} {
		if err := leaf.directory.Sync(); err != nil {
			return fmt.Errorf("sync retained release canonical directory %q: %w", leaf.name, err)
		}
		info, err := leaf.directory.Stat()
		if err != nil {
			return fmt.Errorf("stat retained release canonical directory %q: %w", leaf.name, err)
		}
		identity, identityOK := (fsutil.OSFileSystem{}).FileIdentity(info)
		owner, ownerOK := (fsutil.OSFileSystem{}).FileOwnerUID(info)
		if !info.IsDir() || info.Mode().Perm() != 0o700 || !identityOK || identity.Device != store.device || !ownerOK || owner != uint64(os.Geteuid()) {
			return fmt.Errorf("retained release canonical directory %q has unsafe metadata", leaf.name)
		}
	}
	return nil
}

func writeAllReleaseCanonicalV1(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return fmt.Errorf("write release canonical temporary: %w", err)
		}
		if count <= 0 || count > len(data) {
			return io.ErrShortWrite
		}
		data = data[count:]
	}
	return nil
}

func readReleaseCanonicalEntryV1(directory fsutil.SecureDirectory, name string, limit int64, device uint64) ([]byte, error) {
	file, err := directory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	identity, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
	owner, ownerOK := (fsutil.OSFileSystem{}).FileOwnerUID(info)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || identity.Device != device || identity.Links != 1 || !ownerOK || owner != uint64(os.Geteuid()) {
		return nil, fmt.Errorf("release canonical entry %q has unsafe descriptor metadata", name)
	}
	limited := &io.LimitedReader{R: file, N: limit + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("release canonical entry %q exceeds %d bytes", name, limit)
	}
	return data, nil
}

type releaseCanonicalTempV1 struct {
	policy    releaseCanonicalPolicyV1
	name      string
	finalName string
	data      []byte
}

type releaseCanonicalTempInspectionV1 struct {
	valid []releaseCanonicalTempV1
	inert []releaseCanonicalTempV1
}

func (store *releaseCanonicalStoresV1) reconcileAllReleaseCanonicalTempsV1() error {
	policies := []releaseCanonicalPolicyV1{
		store.artifactSetPolicy(), store.ancestryPolicy(), store.buildReportPolicy(),
		store.cuReportSetPolicy(), store.authorityPolicy(), store.bundlePolicy(),
	}
	var valid []releaseCanonicalTempV1
	var inert []releaseCanonicalTempV1
	for _, policy := range policies {
		inspection, err := store.inspectReleaseCanonicalTempsV1(policy)
		if err != nil {
			return err
		}
		valid = append(valid, inspection.valid...)
		inert = append(inert, inspection.inert...)
	}
	counts, err := store.scanReleaseCanonicalStoresV1ExcludingInert(inert)
	if err != nil {
		return err
	}
	if err := store.validateReleaseCanonicalRecoveryCapacityV1(counts, valid); err != nil {
		return err
	}
	for _, temporary := range inert {
		if cleanupErr := errors.Join(temporary.policy.directory.Remove(temporary.name), temporary.policy.directory.Sync()); cleanupErr != nil {
			return fmt.Errorf("remove inert release canonical temporary: %w", cleanupErr)
		}
	}
	for _, temporary := range valid {
		if err := store.promoteReleaseCanonicalTempV1(temporary); err != nil {
			return err
		}
	}
	return nil
}

func (store *releaseCanonicalStoresV1) inspectReleaseCanonicalTempsV1(policy releaseCanonicalPolicyV1) (releaseCanonicalTempInspectionV1, error) {
	reader, ok := policy.directory.(fsutil.SecureDirectoryReader)
	if !ok {
		return releaseCanonicalTempInspectionV1{}, fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return releaseCanonicalTempInspectionV1{}, err
	}
	var inspection releaseCanonicalTempInspectionV1
	prefix := ".cq-" + policy.tag + "-"
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		middle := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".tmp")
		parts := strings.Split(middle, "-")
		validName := len(parts) == 2 && isLowerHex(parts[0], 64) && isLowerHex(parts[1], 16)
		data, readErr := readReleaseCanonicalEntryV1(policy.directory, name, policy.maxBytes, store.device)
		validObject := false
		if validName && readErr == nil {
			decoded, decodeErr := policy.decode(data)
			if decodeErr == nil && validateCurrentReleaseCanonicalTypeV1(decoded) == nil {
				digest, digestErr := releaseCanonicalBytesDigestV1(policy.domain, data)
				validObject = digestErr == nil && digest == parts[0]
			}
		}
		if !validObject {
			inspection.inert = append(inspection.inert, releaseCanonicalTempV1{policy: policy, name: name})
			continue
		}
		inspection.valid = append(inspection.valid, releaseCanonicalTempV1{policy: policy, name: name, finalName: parts[0] + ".json", data: data})
	}
	return inspection, nil
}

func (store *releaseCanonicalStoresV1) promoteReleaseCanonicalTempV1(temporary releaseCanonicalTempV1) error {
	policy := temporary.policy
	if err := policy.directory.RenameNoReplace(temporary.name, temporary.finalName); err != nil {
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, unix.EEXIST) {
			return err
		}
		if syncErr := policy.directory.Sync(); syncErr != nil {
			return syncErr
		}
		existing, existingErr := readReleaseCanonicalEntryV1(policy.directory, temporary.finalName, policy.maxBytes, store.device)
		if existingErr != nil || !bytes.Equal(existing, temporary.data) {
			return errors.Join(fmt.Errorf("release canonical temporary collision for %s", strings.TrimSuffix(temporary.finalName, ".json")), existingErr)
		}
		return errors.Join(policy.directory.Remove(temporary.name), policy.directory.Sync())
	}
	if err := policy.directory.Sync(); err != nil {
		return err
	}
	if reopened, err := readReleaseCanonicalEntryV1(policy.directory, temporary.finalName, policy.maxBytes, store.device); err != nil || !bytes.Equal(reopened, temporary.data) {
		return errors.Join(fmt.Errorf("reopen reconciled release canonical object"), err)
	}
	return nil
}

func releaseCanonicalBytesDigestV1(domain string, canonical []byte) (string, error) {
	if uint64(len(canonical)) > uint64(^uint32(0)) {
		return "", fmt.Errorf("release canonical object length exceeds uint32")
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, domain)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(canonical)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func isLowerHex(value string, length int) bool {
	return len(value) == length && strings.Trim(value, "0123456789abcdef") == ""
}

func (store *releaseCanonicalStoresV1) checkReleaseCanonicalCapacityV1(policy releaseCanonicalPolicyV1, add uint64) error {
	counts, err := store.scanReleaseCanonicalStoresV1()
	if err != nil {
		return err
	}
	return validateReleaseCanonicalCapacityV1(policy.class, counts, add)
}

func validateReleaseCanonicalCapacityV1(class releaseCanonicalStoreClassV1, counts releaseCanonicalStoreCountsV1, add uint64) error {
	switch class {
	case releaseSetClassV1:
		if counts.sets >= 8 {
			return fmt.Errorf("release canonical store refuses ninth set")
		}
		if counts.setReportTemps >= 4 {
			return fmt.Errorf("release canonical store refuses fifth temporary")
		}
		if counts.setReportBytes+add > releaseSetReportStoreMaxBytes {
			return fmt.Errorf("release canonical set/report aggregate exceeds %d bytes", releaseSetReportStoreMaxBytes)
		}
	case releaseReportClassV1:
		if counts.reports >= 40 {
			return fmt.Errorf("release canonical store refuses forty-first report")
		}
		if counts.setReportTemps >= 4 {
			return fmt.Errorf("release canonical store refuses fifth temporary")
		}
		if counts.setReportBytes+add > releaseSetReportStoreMaxBytes {
			return fmt.Errorf("release canonical set/report aggregate exceeds %d bytes", releaseSetReportStoreMaxBytes)
		}
	case releaseAuthorityClassV1:
		if counts.authorities >= 8 {
			return fmt.Errorf("release canonical provenance refuses ninth authority")
		}
		if counts.provenanceTemps >= 4 {
			return fmt.Errorf("release canonical provenance refuses fifth temporary")
		}
		if counts.provenanceBytes+add > releaseProvenanceMaxBytes {
			return fmt.Errorf("release canonical provenance aggregate exceeds %d bytes", releaseProvenanceMaxBytes)
		}
	case releaseBundleClassV1:
		if counts.bundles >= 8 {
			return fmt.Errorf("release canonical provenance refuses ninth bundle")
		}
		if counts.provenanceTemps >= 4 {
			return fmt.Errorf("release canonical provenance refuses fifth temporary")
		}
		if counts.provenanceBytes+add > releaseProvenanceMaxBytes {
			return fmt.Errorf("release canonical provenance aggregate exceeds %d bytes", releaseProvenanceMaxBytes)
		}
	}
	return nil
}

func (store *releaseCanonicalStoresV1) validateReleaseCanonicalRecoveryCapacityV1(counts releaseCanonicalStoreCountsV1, temps []releaseCanonicalTempV1) error {
	if counts.sets > 8 {
		return fmt.Errorf("release canonical store refuses ninth set")
	}
	if counts.reports > 40 {
		return fmt.Errorf("release canonical store refuses forty-first report")
	}
	if counts.authorities > 8 {
		return fmt.Errorf("release canonical provenance refuses ninth authority")
	}
	if counts.bundles > 8 {
		return fmt.Errorf("release canonical provenance refuses ninth bundle")
	}
	if counts.setReportTemps > 4 {
		return fmt.Errorf("release canonical store refuses fifth temporary")
	}
	if counts.provenanceTemps > 4 {
		return fmt.Errorf("release canonical provenance refuses fifth temporary")
	}
	if counts.setReportBytes > releaseSetReportStoreMaxBytes {
		return fmt.Errorf("release canonical set/report aggregate exceeds %d bytes", releaseSetReportStoreMaxBytes)
	}
	if counts.provenanceBytes > releaseProvenanceMaxBytes {
		return fmt.Errorf("release canonical provenance aggregate exceeds %d bytes", releaseProvenanceMaxBytes)
	}
	seen := make(map[string]struct{}, len(temps))
	for _, temporary := range temps {
		key := fmt.Sprintf("%d:%s", temporary.policy.class, temporary.finalName)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if existing, err := readReleaseCanonicalEntryV1(temporary.policy.directory, temporary.finalName, temporary.policy.maxBytes, store.device); err == nil {
			if !bytes.Equal(existing, temporary.data) {
				return fmt.Errorf("release canonical temporary collision for %s", strings.TrimSuffix(temporary.finalName, ".json"))
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
			return err
		}
		switch temporary.policy.class {
		case releaseSetClassV1:
			counts.sets++
			if counts.sets > 8 {
				return fmt.Errorf("release canonical store refuses ninth set")
			}
		case releaseReportClassV1:
			counts.reports++
			if counts.reports > 40 {
				return fmt.Errorf("release canonical store refuses forty-first report")
			}
		case releaseAuthorityClassV1:
			counts.authorities++
			if counts.authorities > 8 {
				return fmt.Errorf("release canonical provenance refuses ninth authority")
			}
		case releaseBundleClassV1:
			counts.bundles++
			if counts.bundles > 8 {
				return fmt.Errorf("release canonical provenance refuses ninth bundle")
			}
		}
	}
	return nil
}

func (store *releaseCanonicalStoresV1) scanReleaseCanonicalStoresV1() (releaseCanonicalStoreCountsV1, error) {
	return store.scanReleaseCanonicalStoresV1ExcludingInert(nil)
}

func (store *releaseCanonicalStoresV1) scanReleaseCanonicalStoresV1ExcludingInert(inert []releaseCanonicalTempV1) (releaseCanonicalStoreCountsV1, error) {
	var counts releaseCanonicalStoreCountsV1
	inertKeys := make(map[string]struct{}, len(inert))
	for _, temporary := range inert {
		inertKeys[fmt.Sprintf("%d:%s", temporary.policy.class, temporary.name)] = struct{}{}
	}
	for _, source := range []struct {
		directory fsutil.SecureDirectory
		class     releaseCanonicalStoreClassV1
		maxObject int64
	}{
		{store.sets, releaseSetClassV1, 1 << 20},
		{store.reports, releaseReportClassV1, 1 << 20},
		{store.authorities, releaseAuthorityClassV1, 64 << 10},
		{store.bundles, releaseBundleClassV1, 64 << 10},
	} {
		reader, ok := source.directory.(fsutil.SecureDirectoryReader)
		if !ok {
			return counts, fsutil.ErrSecureCapabilityUnavailable
		}
		entries, err := reader.ReadDir()
		if err != nil {
			return counts, err
		}
		for _, entry := range entries {
			name := entry.Name()
			isFinal := len(name) == 69 && strings.HasSuffix(name, ".json") && isLowerHex(strings.TrimSuffix(name, ".json"), 64)
			tempCap, isTemp := releaseCanonicalTempCapV1(source.class, name)
			_, stagedInert := inertKeys[fmt.Sprintf("%d:%s", source.class, name)]
			if !isFinal && !isTemp {
				return counts, fmt.Errorf("release canonical store contains unknown entry %q", name)
			}
			file, err := source.directory.OpenNoFollow(name)
			if err != nil {
				return counts, err
			}
			info, statErr := file.Stat()
			closeErr := file.Close()
			if statErr != nil {
				return counts, statErr
			}
			if closeErr != nil {
				return counts, closeErr
			}
			identity, identityOK := (fsutil.OSFileSystem{}).FileIdentity(info)
			owner, ownerOK := (fsutil.OSFileSystem{}).FileOwnerUID(info)
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !identityOK || identity.Device != store.device || identity.Links != 1 || !ownerOK || owner != uint64(os.Geteuid()) || info.Size() < 0 {
				return counts, fmt.Errorf("release canonical entry %q has unsafe metadata", name)
			}
			size := uint64(info.Size())
			if isTemp && !stagedInert && info.Size() > tempCap {
				return counts, fmt.Errorf("release canonical temporary %q exceeds type cap", name)
			}
			if isFinal {
				data, err := readReleaseCanonicalEntryV1(source.directory, name, source.maxObject, store.device)
				if err != nil {
					return counts, err
				}
				if err := validateStoredReleaseCanonicalObjectV1(source.class, name, data); err != nil {
					return counts, err
				}
			}
			if stagedInert {
				continue
			}
			switch source.class {
			case releaseSetClassV1:
				counts.setReportBytes += size
				if isTemp {
					counts.setReportTemps++
				} else {
					counts.sets++
				}
			case releaseReportClassV1:
				counts.setReportBytes += size
				if isTemp {
					counts.setReportTemps++
				} else {
					counts.reports++
				}
			case releaseAuthorityClassV1:
				counts.provenanceBytes += size
				if isTemp {
					counts.provenanceTemps++
				} else {
					counts.authorities++
				}
			case releaseBundleClassV1:
				counts.provenanceBytes += size
				if isTemp {
					counts.provenanceTemps++
				} else {
					counts.bundles++
				}
			}
		}
	}
	return counts, nil
}

func releaseCanonicalTempCapV1(class releaseCanonicalStoreClassV1, name string) (int64, bool) {
	var tags map[string]int64
	switch class {
	case releaseSetClassV1:
		tags = map[string]int64{"release-artifact-set-v1": 1 << 20}
	case releaseReportClassV1:
		tags = map[string]int64{"source-ancestry-receipt-v1": 1 << 20, "release-build-report-v1": 64 << 10, "construction-unit-report-set-v1": 64 << 10}
	case releaseAuthorityClassV1:
		tags = map[string]int64{"release-build-authority-v1": 64 << 10}
	case releaseBundleClassV1:
		tags = map[string]int64{"release-bundle-v1": 64 << 10}
	}
	for tag, cap := range tags {
		prefix := ".cq-" + tag + "-"
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		middle := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".tmp")
		parts := strings.Split(middle, "-")
		return cap, len(parts) == 2 && isLowerHex(parts[0], 64) && isLowerHex(parts[1], 16)
	}
	return 0, false
}

func validateStoredReleaseCanonicalObjectV1(class releaseCanonicalStoreClassV1, name string, data []byte) error {
	type candidate struct {
		domain string
		cap    int
		decode func([]byte) (any, error)
	}
	var candidates []candidate
	switch class {
	case releaseSetClassV1:
		candidates = []candidate{{"cq/release-artifact-set/v1\x00", 1 << 20, decodeReleaseCanonicalObjectV1[ReleaseArtifactSetV1]}}
	case releaseReportClassV1:
		candidates = []candidate{
			{"cq/source-ancestry-receipt/v1\x00", 1 << 20, decodeReleaseCanonicalObjectV1[SourceAncestryReceiptV1]},
			{"cq/release-build-report/v1\x00", 64 << 10, decodeReleaseCanonicalObjectV1[ReleaseBuildReportV1]},
			{"cq/construction-unit-report-set/v1\x00", 64 << 10, decodeReleaseCanonicalObjectV1[ConstructionUnitReportSetV1]},
		}
	case releaseAuthorityClassV1:
		candidates = []candidate{{"cq/release-build-authority/v1\x00", 64 << 10, decodeReleaseCanonicalObjectV1[ReleaseBuildAuthorityV1]}}
	case releaseBundleClassV1:
		candidates = []candidate{{"cq/release-bundle/v1\x00", 64 << 10, decodeReleaseCanonicalObjectV1[ReleaseBundleV1]}}
	}
	wantDigest := strings.TrimSuffix(name, ".json")
	for _, candidate := range candidates {
		if len(data) > candidate.cap {
			continue
		}
		decoded, err := candidate.decode(data)
		if err != nil || validateCurrentReleaseCanonicalTypeV1(decoded) != nil {
			continue
		}
		digest, err := releaseCanonicalBytesDigestV1(candidate.domain, data)
		if err == nil && digest == wantDigest {
			return nil
		}
	}
	return fmt.Errorf("release canonical entry %q is not an exact current typed object", name)
}

// ReleaseAuthorityPinV1 is supplied by the caller's trust store, never by the
// release bundle being verified.
type ReleaseAuthorityPinV1 struct {
	Digest           string
	Ed25519PublicKey string
}

// ReleaseCommandEvidenceV1 retains the literal inputs needed to recompute a
// command and its execution-result digest. It is caller-retained evidence, not
// a digest copied from a signed report.
type ReleaseCommandEvidenceV1 struct {
	Purpose           string
	WorkingDirectory  string
	Argv              []string
	ExitCode          int32
	TerminationReason string
	Stdout            []byte
	Stderr            []byte
}

type ReleaseCUEvidenceV1 struct {
	CUID          string
	ManifestBytes []byte
	Command       ReleaseCommandEvidenceV1
}

type ReleaseRoleEvidenceV1 struct {
	Role             string
	Command          ReleaseCommandEvidenceV1
	Payload          []byte
	CodeSignature    []byte
	LauncherABIBytes []byte
	PrivateABIBytes  []byte
}

// ReleaseVerificationEvidenceV1 is the complete retained, unhashed evidence
// required to verify a signed release equality graph independently.
type ReleaseVerificationEvidenceV1 struct {
	RepositoryRemote  []byte
	SourceCommit      string
	SourceTreeListing []byte
	BuildEnvironment  []BuildEnvironmentEntryV1
	BuildReports      []ReleaseCommandEvidenceV1
	ConstructionUnits []ReleaseCUEvidenceV1
	Ancestry          *ReleaseCommandEvidenceV1
	Roles             []ReleaseRoleEvidenceV1
}

func VerifyReleaseBuildAuthorityV1(authority ReleaseBuildAuthorityV1, expected ReleaseAuthorityPinV1) error {
	if !lowerHex64Pattern.MatchString(expected.Digest) {
		return fmt.Errorf("expected release authority digest is invalid")
	}
	if _, err := decodeEd25519PublicKey(expected.Ed25519PublicKey); err != nil {
		return fmt.Errorf("expected release authority key is invalid: %w", err)
	}
	if authority.SchemaVersion != 1 || authority.AuthorityID == "" || !lowerHex64Pattern.MatchString(authority.RepositoryIdentityDigest) || !lowerHex40Pattern.MatchString(authority.LineageRootCommit) || !lowerHex64Pattern.MatchString(authority.LineageRootTreeDigest) {
		return fmt.Errorf("release authority has invalid V1 identity")
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
	digest, err := releaseObjectDigestV1("cq/release-build-authority/v1\x00", authority)
	if err != nil {
		return err
	}
	if digest != expected.Digest || authority.Ed25519PublicKey != expected.Ed25519PublicKey {
		return fmt.Errorf("release authority is not externally approved")
	}
	return nil
}

// VerifyReleaseGraphAgainstApprovedAuthorityV1 verifies a graph only against
// the authority digest and key explicitly approved by its caller. The pin is
// never discovered from the graph or retained evidence.
func VerifyReleaseGraphAgainstApprovedAuthorityV1(graph ReleaseGraphV1, expected ReleaseAuthorityPinV1, evidence ReleaseVerificationEvidenceV1) error {
	if err := verifyAvailableConstructionUnitEvidenceV1(graph.CUReportSet.Reports, evidence.ConstructionUnits); err != nil {
		return err
	}
	if err := verifyAvailableReleaseReachabilityCatalogueV1(); err != nil {
		return err
	}
	return verifyReleaseGraphStructureAgainstApprovedAuthorityV1(graph, expected, evidence)
}

func verifyAvailableReleaseReachabilityCatalogueV1() error {
	return fmt.Errorf("feature inactive: legacy writer reachability catalogue requires CU-2 source regeneration")
}

// verifyReleaseGraphStructureAgainstApprovedAuthorityV1 validates schema and
// equality mechanics for checked-in fixtures. It is not release acceptance:
// only VerifyReleaseGraphAgainstApprovedAuthorityV1 applies CU availability.
func verifyReleaseGraphStructureAgainstApprovedAuthorityV1(graph ReleaseGraphV1, expected ReleaseAuthorityPinV1, evidence ReleaseVerificationEvidenceV1) error {
	if err := VerifyReleaseBuildAuthorityV1(graph.Authority, expected); err != nil {
		return err
	}
	if err := verifyReleaseEvidenceV1(graph, evidence); err != nil {
		return err
	}
	return verifyReleaseGraphV1(graph)
}

func verifyAvailableConstructionUnitEvidenceV1(reports []CUReportV1, evidence []ReleaseCUEvidenceV1) error {
	if len(reports) != len(evidence) {
		return fmt.Errorf("retained CU evidence cardinality mismatch")
	}
	for index, report := range reports {
		canonical, err := CanonicalCUManifestV1(report.CUID)
		if err != nil {
			return fmt.Errorf("feature inactive: %s verification manifest is unavailable", report.CUID)
		}
		retained := evidence[index]
		if retained.CUID != report.CUID || !bytes.Equal(retained.ManifestBytes, canonical) {
			return fmt.Errorf("retained CU manifest %q is not the checked-in canonical manifest", report.CUID)
		}
		digest := framedBytesDigestV1("cq/construction-unit-verification-manifest/v1\x00", canonical)
		if digest != report.VerificationManifestDigest {
			return fmt.Errorf("retained CU manifest %q digest mismatch", report.CUID)
		}
		wantArgv := []string{"./scripts/verify-proxy-cu", report.CUID}
		if !slices.Equal(retained.Command.Argv, wantArgv) {
			return fmt.Errorf("retained CU invocation %q is not exact", report.CUID)
		}
	}
	return nil
}

func verifyReleaseEvidenceV1(graph ReleaseGraphV1, evidence ReleaseVerificationEvidenceV1) error {
	set := graph.ArtifactSet
	if len(evidence.RepositoryRemote) == 0 {
		return fmt.Errorf("release verification evidence has no repository identity bytes")
	}
	repositoryDigest := framedBytesDigestV1("cq/repository-identity/v1\x00", bytes.TrimSpace(evidence.RepositoryRemote))
	if repositoryDigest != graph.Authority.RepositoryIdentityDigest {
		return fmt.Errorf("retained repository identity does not match release authority")
	}
	if evidence.SourceCommit != set.SourceCommit || sha256BytesHex(evidence.SourceTreeListing) != set.SourceTreeDigest {
		return fmt.Errorf("retained source commit or tree listing does not match release graph")
	}
	environmentDigest, err := BuildEnvironmentDigestV1(evidence.BuildEnvironment)
	if err != nil {
		return fmt.Errorf("verify retained build environment: %w", err)
	}
	if environmentDigest != set.BuildEnvironmentDigest {
		return fmt.Errorf("retained build environment does not match release graph")
	}
	reports := []ReleaseBuildReportV1{graph.BuildReport, graph.VetReport, graph.RaceReport}
	kinds := []string{"build", "vet", "race"}
	if len(evidence.BuildReports) != len(reports) {
		return fmt.Errorf("retained build evidence cardinality mismatch")
	}
	goTool := ""
	for index, report := range reports {
		command := evidence.BuildReports[index]
		if err := validateReleaseBuildArgvV1(kinds[index], command.Argv); err != nil {
			return fmt.Errorf("verify retained %s evidence: %w", kinds[index], err)
		}
		if index == 0 {
			goTool = command.Argv[0]
		} else if command.Argv[0] != goTool {
			return fmt.Errorf("retained release reports use different Go tools")
		}
		if err := verifyRetainedCommand(command, "release-"+set.Purpose+"-"+kinds[index], report.CommandDigest, report.ExecutionResultDigest); err != nil {
			return fmt.Errorf("verify retained %s evidence: %w", kinds[index], err)
		}
		if command.ExitCode != report.ExitCode || command.ExitCode != 0 || command.TerminationReason != "exited" || report.Outcome != "passed" {
			return fmt.Errorf("retained %s result is not the accepted passed execution", kinds[index])
		}
	}

	if len(evidence.ConstructionUnits) != len(graph.CUReportSet.Reports) {
		return fmt.Errorf("retained CU evidence cardinality mismatch")
	}
	for index, report := range graph.CUReportSet.Reports {
		cu := evidence.ConstructionUnits[index]
		if cu.CUID != report.CUID || len(cu.ManifestBytes) == 0 || len(cu.ManifestBytes) > maxCUManifestBytes {
			return fmt.Errorf("retained CU evidence %d has invalid identity or manifest size", index)
		}
		manifest, err := ParseCUManifestV1(bytes.NewReader(cu.ManifestBytes))
		if err != nil || manifest.Unit != cu.CUID {
			return fmt.Errorf("retained CU manifest %q is invalid", cu.CUID)
		}
		manifestDigest := framedBytesDigestV1("cq/construction-unit-verification-manifest/v1\x00", cu.ManifestBytes)
		if manifestDigest != report.VerificationManifestDigest {
			return fmt.Errorf("retained CU manifest %q digest mismatch", cu.CUID)
		}
		if err := verifyRetainedCommand(cu.Command, "verify-"+cu.CUID, report.InvocationDigest, report.ExecutionResultDigest); err != nil {
			return fmt.Errorf("verify retained CU %q evidence: %w", cu.CUID, err)
		}
		if cu.Command.ExitCode != report.ExitCode || cu.Command.ExitCode != 0 || cu.Command.TerminationReason != "exited" || !report.RaceEnabled || report.Outcome != "passed" {
			return fmt.Errorf("retained CU %q result is not the accepted passed race execution", cu.CUID)
		}
	}

	if set.Purpose == "floor" {
		if evidence.Ancestry != nil {
			return fmt.Errorf("floor release has retained ancestry evidence")
		}
	} else {
		if graph.Ancestry == nil || evidence.Ancestry == nil {
			return fmt.Errorf("target release lacks retained ancestry evidence")
		}
		commandDigest, resultDigest, err := retainedCommandDigests(*evidence.Ancestry, "ancestry")
		if err != nil {
			return fmt.Errorf("verify retained ancestry evidence: %w", err)
		}
		wantArgv := []string{"/usr/bin/git", "merge-base", graph.Ancestry.FloorSourceCommit, graph.Ancestry.TargetSourceCommit}
		wantStdout := []byte(graph.Ancestry.MergeBaseCommit + "\n")
		if commandDigest != graph.Ancestry.VerificationCommandDigest || !slices.Equal(evidence.Ancestry.Argv, wantArgv) || !bytes.Equal(evidence.Ancestry.Stdout, wantStdout) || len(evidence.Ancestry.Stderr) != 0 || evidence.Ancestry.ExitCode != 0 || evidence.Ancestry.TerminationReason != "exited" || resultDigest == "" {
			return fmt.Errorf("retained ancestry command does not match successful receipt")
		}
	}

	if len(evidence.Roles) != len(graph.ArtifactManifests) {
		return fmt.Errorf("retained role evidence cardinality mismatch")
	}
	for index, manifest := range graph.ArtifactManifests {
		role := evidence.Roles[index]
		if role.Role != manifest.Role {
			return fmt.Errorf("retained role evidence %d does not match %q", index, manifest.Role)
		}
		if err := validateReleaseRoleEvidenceSizesV1(len(role.Payload), len(role.CodeSignature), len(role.LauncherABIBytes), len(role.PrivateABIBytes)); err != nil {
			return fmt.Errorf("retained role %q: %w", role.Role, err)
		}
		if len(role.Command.Argv) == 0 || role.Command.Argv[0] != goTool {
			return fmt.Errorf("retained role %q does not use the release report Go tool", role.Role)
		}
		commandDigest, resultDigest, err := retainedCommandDigests(role.Command, "role-"+role.Role+"-build")
		if err != nil {
			return fmt.Errorf("verify retained role %q command: %w", role.Role, err)
		}
		if commandDigest != manifest.BuildCommandDigest || role.Command.ExitCode != 0 || role.Command.TerminationReason != "exited" || resultDigest == "" {
			return fmt.Errorf("retained role %q command does not match successful manifest build", role.Role)
		}
		if sha256BytesHex(role.Payload) != manifest.ArtifactPayloadDigest || sha256BytesHex(role.CodeSignature) != manifest.CodeSignatureDigest {
			return fmt.Errorf("retained role %q payload or code signature mismatch", role.Role)
		}
		if err := verifyOptionalRawDigest(role.LauncherABIBytes, manifest.LauncherABIDigest, role.Role+" launcher ABI"); err != nil {
			return err
		}
		if err := verifyOptionalRawDigest(role.PrivateABIBytes, manifest.PrivateABIDigest, role.Role+" private ABI"); err != nil {
			return err
		}
	}
	return verifyReleaseBundleEntrySizesFromEvidenceV1(graph, evidence)
}

func verifyReleaseBundleEntrySizesFromEvidenceV1(graph ReleaseGraphV1, evidence ReleaseVerificationEvidenceV1) error {
	expected := make(map[string]uint64, len(graph.Bundle.Entries))
	addCanonical := func(path string, value any) error {
		canonical, err := CanonicalJSONV1(value)
		if err != nil {
			return err
		}
		expected[path] = uint64(len(canonical))
		return nil
	}
	objects := []struct {
		path  string
		value any
	}{
		{"release-build-authority.json", graph.Authority},
		{"release-artifact-set.json", graph.ArtifactSet},
		{"reports/build.json", graph.BuildReport},
		{"reports/vet.json", graph.VetReport},
		{"reports/race.json", graph.RaceReport},
		{"reports/construction-units.json", graph.CUReportSet},
	}
	if graph.Ancestry != nil {
		objects = append(objects, struct {
			path  string
			value any
		}{"source-ancestry.json", *graph.Ancestry})
	}
	for _, object := range objects {
		if err := addCanonical(object.path, object.value); err != nil {
			return fmt.Errorf("canonicalise release entry %q: %w", object.path, err)
		}
	}
	for index, manifest := range graph.ArtifactManifests {
		if err := addCanonical("manifests/"+manifest.Role+".json", manifest); err != nil {
			return fmt.Errorf("canonicalise role manifest %q: %w", manifest.Role, err)
		}
		expected["payloads/"+manifest.Role] = uint64(len(evidence.Roles[index].Payload))
	}
	for _, entry := range graph.Bundle.Entries {
		want, ok := expected[entry.RelativePath]
		if !ok || entry.Size != want {
			return fmt.Errorf("release bundle entry %q raw size mismatch", entry.RelativePath)
		}
		delete(expected, entry.RelativePath)
	}
	if len(expected) != 0 {
		return fmt.Errorf("release bundle omits retained raw evidence entries")
	}
	return nil
}

func validateReleaseBuildArgvV1(kind string, argv []string) error {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) || filepath.Clean(argv[0]) != argv[0] || filepath.Base(argv[0]) != "go" {
		return fmt.Errorf("release build tool must be an exact absolute Go executable")
	}
	var want []string
	switch kind {
	case "build":
		want = []string{argv[0], "build", "./..."}
	case "vet":
		want = []string{argv[0], "vet", "./..."}
	case "race":
		want = []string{argv[0], "test", "-race", "-count=1", "./..."}
	default:
		return fmt.Errorf("unknown release build command kind %q", kind)
	}
	if !slices.Equal(argv, want) {
		return fmt.Errorf("release %s argv is not exact", kind)
	}
	return nil
}

func validateReleaseRoleEvidenceSizesV1(payload, codeSignature, launcherABI, privateABI int) error {
	if payload < 0 || payload > 268435456 {
		return fmt.Errorf("payload exceeds 268435456 bytes")
	}
	if codeSignature < 0 || codeSignature > 1<<20 {
		return fmt.Errorf("code signature exceeds 1 MiB")
	}
	if launcherABI < 0 || launcherABI > 1<<20 || privateABI < 0 || privateABI > 1<<20 {
		return fmt.Errorf("ABI evidence exceeds 1 MiB")
	}
	return nil
}

func verifyRetainedCommand(evidence ReleaseCommandEvidenceV1, purpose, commandDigest, resultDigest string) error {
	gotCommand, gotResult, err := retainedCommandDigests(evidence, purpose)
	if err != nil {
		return err
	}
	if gotCommand != commandDigest || gotResult != resultDigest {
		return fmt.Errorf("retained command or execution-result digest mismatch")
	}
	return nil
}

func retainedCommandDigests(evidence ReleaseCommandEvidenceV1, purpose string) (string, string, error) {
	if evidence.Purpose != purpose {
		return "", "", fmt.Errorf("command purpose is %q, want %q", evidence.Purpose, purpose)
	}
	commandDigest, err := CommandDigestV1(evidence.Purpose, evidence.WorkingDirectory, evidence.Argv)
	if err != nil {
		return "", "", err
	}
	resultDigest, err := ExecutionResultDigestV1(evidence.ExitCode, evidence.TerminationReason, evidence.Stdout, evidence.Stderr)
	if err != nil {
		return "", "", err
	}
	return commandDigest, resultDigest, nil
}

func verifyOptionalRawDigest(data []byte, expected *string, field string) error {
	if expected == nil {
		if len(data) != 0 {
			return fmt.Errorf("retained %s bytes exist for null digest", field)
		}
		return nil
	}
	if len(data) == 0 || sha256BytesHex(data) != *expected {
		return fmt.Errorf("retained %s bytes do not match digest", field)
	}
	return nil
}

func sha256BytesHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func framedBytesDigestV1(domain string, data []byte) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, domain)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func VerifyPairedReleaseGraphsAgainstApprovedAuthorityV1(floor, target ReleaseGraphV1, expected ReleaseAuthorityPinV1, floorEvidence, targetEvidence ReleaseVerificationEvidenceV1) error {
	if floor.Bundle.Purpose != "floor" || target.Bundle.Purpose != "target" {
		return fmt.Errorf("release pair must contain floor then target")
	}
	if err := VerifyReleaseGraphAgainstApprovedAuthorityV1(floor, expected, floorEvidence); err != nil {
		return fmt.Errorf("verify floor release: %w", err)
	}
	if err := VerifyReleaseGraphAgainstApprovedAuthorityV1(target, expected, targetEvidence); err != nil {
		return fmt.Errorf("verify target release: %w", err)
	}
	if floor.ArtifactSet.BuildEnvironmentDigest != target.ArtifactSet.BuildEnvironmentDigest ||
		floor.ArtifactSet.ToolchainIdentity != target.ArtifactSet.ToolchainIdentity ||
		floor.ArtifactSet.LegacyAtomicWriterReachabilityCatalogueDigest != target.ArtifactSet.LegacyAtomicWriterReachabilityCatalogueDigest {
		return fmt.Errorf("release pair environment, toolchain, or reachability mismatch")
	}
	if target.Ancestry == nil || target.Ancestry.FloorSourceCommit != floor.ArtifactSet.SourceCommit ||
		target.Ancestry.FloorSourceTreeDigest != floor.ArtifactSet.SourceTreeDigest ||
		floor.ArtifactSet.SourceCommit != floor.Authority.LineageRootCommit ||
		floor.ArtifactSet.SourceTreeDigest != floor.Authority.LineageRootTreeDigest {
		return fmt.Errorf("release pair ancestry does not bind the accepted floor")
	}
	return nil
}

func verifyReleaseGraphV1(graph ReleaseGraphV1) error {
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

func VerifyReleaseBundleDirectoryAgainstApprovedAuthorityV1(root string, graph ReleaseGraphV1, expected ReleaseAuthorityPinV1, evidence ReleaseVerificationEvidenceV1) (resultErr error) {
	if err := VerifyReleaseGraphAgainstApprovedAuthorityV1(graph, expected, evidence); err != nil {
		return err
	}
	return verifyReleaseBundleDirectoryStructureV1(root, graph)
}

func verifyReleaseBundleDirectoryStructureV1(root string, graph ReleaseGraphV1) (resultErr error) {
	expectedFiles := make(map[string]ReleaseBundleEntryV1, len(graph.Bundle.Entries)+1)
	for _, entry := range graph.Bundle.Entries {
		expectedFiles[entry.RelativePath] = entry
	}
	files, descendants, err := openReleaseBundleTree(root, expectedFiles)
	if err != nil {
		return err
	}
	defer func() {
		for _, file := range files {
			if err := file.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close retained release file: %w", err))
			}
		}
	}()
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
		limit := int64(64 << 10)
		if relative == "release-artifact-set.json" || relative == "source-ancestry.json" {
			limit = 1 << 20
		}
		data, err := readRetainedReleaseFile(files[relative], limit)
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
		size, digest, err := hashRetainedReleaseFile(files[relative], 268435456)
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

func openReleaseBundleTree(root string, expectedFiles map[string]ReleaseBundleEntryV1) (map[string]*os.File, int, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open release bundle root without following links: %w", err)
	}
	rootFile := os.NewFile(uintptr(rootFD), "release-bundle-root")
	defer rootFile.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return nil, 0, err
	}
	if err := validateReleaseDescriptor(rootStat, int64(rootStat.Dev), true, "bundle root"); err != nil {
		return nil, 0, err
	}
	if err := validateLocalReleaseFilesystem(rootFD); err != nil {
		return nil, 0, err
	}
	expectedDirectories := map[string]bool{"manifests": true, "payloads": true, "reports": true}
	files := make(map[string]*os.File, len(expectedFiles)+1)
	closeFiles := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	descendants := 0
	var aggregateBytes uint64
	for {
		rootEntries, readErr := rootFile.ReadDir(1)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			closeFiles()
			return nil, 0, readErr
		}
		entry := rootEntries[0]
		name := entry.Name()
		fd, err := unix.Openat(rootFD, name, releaseDescendantOpenFlagsV1(), 0)
		if err != nil {
			closeFiles()
			return nil, 0, fmt.Errorf("open release descendant %q: %w", name, err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			closeFiles()
			return nil, 0, err
		}
		isDirectory := stat.Mode&unix.S_IFMT == unix.S_IFDIR
		if err := validateReleaseDescriptor(stat, int64(rootStat.Dev), isDirectory, name); err != nil {
			_ = unix.Close(fd)
			closeFiles()
			return nil, 0, err
		}
		descendants++
		if descendants > 17 {
			_ = unix.Close(fd)
			closeFiles()
			return nil, 0, fmt.Errorf("release bundle exceeds 17 descendants")
		}
		if isDirectory {
			if !expectedDirectories[name] {
				_ = unix.Close(fd)
				closeFiles()
				return nil, 0, fmt.Errorf("release bundle contains unknown directory %q", name)
			}
			directory := os.NewFile(uintptr(fd), name)
			for {
				entries, childReadErr := directory.ReadDir(1)
				if errors.Is(childReadErr, io.EOF) {
					break
				}
				if childReadErr != nil {
					_ = directory.Close()
					closeFiles()
					return nil, 0, childReadErr
				}
				child := entries[0]
				if descendants == 17 {
					_ = directory.Close()
					closeFiles()
					return nil, 0, fmt.Errorf("release bundle exceeds 17 descendants")
				}
				relative := name + "/" + child.Name()
				childFD, openErr := unix.Openat(fd, child.Name(), releaseDescendantOpenFlagsV1(), 0)
				if openErr != nil {
					_ = directory.Close()
					closeFiles()
					return nil, 0, fmt.Errorf("open release descendant %q: %w", relative, openErr)
				}
				var childStat unix.Stat_t
				if statErr := unix.Fstat(childFD, &childStat); statErr != nil {
					_ = unix.Close(childFD)
					_ = directory.Close()
					closeFiles()
					return nil, 0, statErr
				}
				if err := validateReleaseDescriptor(childStat, int64(rootStat.Dev), false, relative); err != nil {
					_ = unix.Close(childFD)
					_ = directory.Close()
					closeFiles()
					return nil, 0, err
				}
				if _, ok := expectedFiles[relative]; !ok {
					_ = unix.Close(childFD)
					_ = directory.Close()
					closeFiles()
					return nil, 0, fmt.Errorf("release bundle contains unknown file %q", relative)
				}
				descendants++
				aggregateBytes += uint64(childStat.Size)
				if aggregateBytes > 1<<30 {
					_ = unix.Close(childFD)
					_ = directory.Close()
					closeFiles()
					return nil, 0, fmt.Errorf("release bundle exceeds 1 GiB")
				}
				files[relative] = os.NewFile(uintptr(childFD), relative)
			}
			_ = directory.Close()
			continue
		}
		if name != "bundle.json" {
			if _, ok := expectedFiles[name]; !ok {
				_ = unix.Close(fd)
				closeFiles()
				return nil, 0, fmt.Errorf("release bundle contains unknown file %q", name)
			}
		}
		aggregateBytes += uint64(stat.Size)
		files[name] = os.NewFile(uintptr(fd), name)
	}
	if descendants > 17 || aggregateBytes > 1<<30 {
		closeFiles()
		return nil, 0, fmt.Errorf("release bundle exceeds physical capacity")
	}
	return files, descendants, nil
}

func releaseDescendantOpenFlagsV1() int {
	return unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
}

func validateReleaseDescriptor(stat unix.Stat_t, rootDevice int64, directory bool, name string) error {
	wantType := uint32(unix.S_IFREG)
	if directory {
		wantType = unix.S_IFDIR
	}
	if uint32(stat.Mode)&unix.S_IFMT != wantType {
		return fmt.Errorf("release bundle descendant %q has invalid type", name)
	}
	if int64(stat.Dev) != rootDevice {
		return fmt.Errorf("release bundle descendant %q crosses a mount", name)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		return fmt.Errorf("release bundle descendant %q is not owner-only", name)
	}
	if !directory && stat.Nlink != 1 {
		return fmt.Errorf("release bundle descendant %q has %d hard links, want one", name, stat.Nlink)
	}
	if stat.Size < 0 {
		return fmt.Errorf("release bundle descendant %q has invalid size", name)
	}
	return nil
}

func validateLocalReleaseFilesystem(fd int) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("release bundle verification is unsupported outside Darwin APFS")
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return err
	}
	field := reflect.ValueOf(stat).FieldByName("Fstypename")
	var filesystem strings.Builder
	for index := 0; field.IsValid() && index < field.Len(); index++ {
		member := field.Index(index)
		var value byte
		if member.Kind() >= reflect.Int && member.Kind() <= reflect.Int64 {
			value = byte(member.Int())
		} else {
			value = byte(member.Uint())
		}
		if value == 0 {
			break
		}
		filesystem.WriteByte(value)
	}
	if filesystem.String() != "apfs" {
		return fmt.Errorf("release bundle must reside on local APFS")
	}
	return nil
}

func readRetainedReleaseFile(file *os.File, limit int64) ([]byte, error) {
	if file == nil {
		return nil, fmt.Errorf("release file is absent")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("read bounded retained file: %v", err)
	}
	return data, nil
}

func hashRetainedReleaseFile(file *os.File, limit uint64) (uint64, string, error) {
	if file == nil {
		return 0, "", fmt.Errorf("release payload is absent")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, int64(limit)+1))
	if err != nil || count > int64(limit) {
		return 0, "", fmt.Errorf("read bounded retained payload: %v", err)
	}
	return uint64(count), hex.EncodeToString(hash.Sum(nil)), nil
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
	limit := 64 << 10
	if domain == "cq/release-artifact-set/v1\x00" || domain == "cq/source-ancestry-receipt/v1\x00" {
		limit = 1 << 20
	}
	if len(canonical) > limit {
		return "", fmt.Errorf("release object exceeds %d bytes", limit)
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

func releaseArtifactSetSignable(value ReleaseArtifactSetV1) any {
	return releaseSignableWithoutFieldV1(value, "set_signature")
}

func releaseBuildReportSignable(value ReleaseBuildReportV1) any {
	return releaseSignableWithoutFieldV1(value, "signature")
}

func cuReportSetSignable(value ConstructionUnitReportSetV1) any {
	return releaseSignableWithoutFieldV1(value, "signature")
}

func sourceAncestrySignable(value SourceAncestryReceiptV1) any {
	return releaseSignableWithoutFieldV1(value, "signature")
}

func releaseBundleSignable(value ReleaseBundleV1) any {
	return releaseSignableWithoutFieldV1(value, "bundle_signature")
}

func releaseSignableWithoutFieldV1(value any, field string) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"invalid": true}
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		return map[string]any{"invalid": true}
	}
	delete(object, field)
	return object
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
			previous := ""
			if index > 0 {
				previous = selection.FullTestIDs[index-1]
			}
			return fmt.Errorf("full test ID %q is invalid, duplicated, or unordered after %q", name, previous)
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
