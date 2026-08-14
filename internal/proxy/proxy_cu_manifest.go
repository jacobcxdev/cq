package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxCUManifestBytes = 64 << 10
const maxCUCaptureStreamBytes = 8 << 20

var (
	cuIDPattern       = regexp.MustCompile(`^CU-[0-9]$`)
	cuPackagePattern  = regexp.MustCompile(`^\./[A-Za-z0-9_./-]+$`)
	cuTopTestPattern  = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
	cuFullTestPattern = regexp.MustCompile(`^Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_.-]+)*$`)
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
		"TestManualHelpTextDocumentsEachCommandPath",
		"TestRootHelpShowsFullCLISurface",
		"TestRunModelsHelpDoesNotRefresh",
		"TestRunProxyCodexDefaultHelpDoesNotCreateConfig",
	)
	cmdSelection.FullTestIDs = []string{
		"TestGlobalHelpAndVersionDoNotCreateHomeOrXDGState",
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
		"TestRootHelpShowsFullCLISurface",
		"TestRunModelsHelpDoesNotRefresh",
		"TestRunProxyCodexDefaultHelpDoesNotCreateConfig",
	}
	cmdSelection.MinimumPassCount = len(cmdSelection.FullTestIDs)
	proxySelection := newCUTestPackage("./internal/proxy",
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming",
		"TestCanonicalCUManifestV1ProvidesPinnedCU0Selection",
		"TestCanonicalJSONV1RejectsNonFiniteNumber",
		"TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping",
		"TestExecutionResultDigestV1MatchesLiteralFraming",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture",
		"TestNewCUReportV1TreatsForgedReportOutputAsCapturedBytes",
		"TestParseBlueprintReviewResultAcceptsCleanAndNotClean",
		"TestParseBlueprintReviewResultRejectsFindingOrderAndOversizeInput",
		"TestParseBlueprintReviewSiblingAcceptsOneByteStreaming",
		"TestParseBlueprintReviewSiblingRejectsRecordDigestCorruption",
		"TestParseCUManifestAcceptsClosedCU0Selection",
		"TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount",
		"TestParseCUManifestRejectsUnknownMemberBeforeDispatch",
		"TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality",
		"TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality",
		"TestVerifyBlueprintReviewAcceptsFrozenRound44",
		"TestVerifyBlueprintReviewRejectsSymlinkAuthorityFiles",
	)
	proxySelection.FullTestIDs = append(proxySelection.FullTestIDs,
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming/extra",
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming/missing",
		"TestBuildEnvironmentDigestV1MatchesLiteralFraming/reordered",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/nonzero",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/oversize",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/race_disabled",
		"TestNewCUReportV1RejectsNonPassedAndOversizeCapture/signal",
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
	)
	sort.Strings(proxySelection.FullTestIDs)
	proxySelection.MinimumPassCount = len(proxySelection.FullTestIDs)
	proxyCUSelection := newCUTestPackage("./internal/tools/proxycu",
		"TestRunRejectsAbsentAndUnmanifestedCU",
		"TestShellWrappersVerifyFrozenReviewAndSelfTest",
		"TestVerifyTestEventsAcceptsExactRunAndPass",
		"TestVerifyTestEventsRejectsCorruptEvidence",
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
	)
	sort.Strings(proxyCUSelection.FullTestIDs)
	proxyCUSelection.MinimumPassCount = len(proxyCUSelection.FullTestIDs)
	selections := []CUTestPackageV1{
		cmdSelection,
		proxySelection,
		proxyCUSelection,
		newCUTestPackage("./internal/tools/proxyrelease",
			"TestBuildProxyReleaseShellEntryRejectsMissingManifest",
			"TestCaptureConstructionUnitBuildsReportOnlyFromCompletedCapture",
			"TestCaptureConstructionUnitRecoversRunnerPanic",
			"TestParseReleaseBuildManifestAcceptsClosedRequestAndRejectsUnknown",
			"TestReadReleaseBuildManifestRejectsSymlink",
		),
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
		if !utf8.ValidString(argument) {
			return "", fmt.Errorf("command argument is invalid UTF-8")
		}
		if err := writeUint32FramedString(hash, argument); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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
