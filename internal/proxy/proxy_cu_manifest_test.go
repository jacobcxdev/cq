package proxy

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildEnvironmentDigestV1MatchesLiteralFraming(t *testing.T) {
	environment := []BuildEnvironmentEntryV1{
		{Key: "CGO_ENABLED", Value: "0"},
		{Key: "GOAMD64", Value: "v1"},
		{Key: "GOARCH", Value: "amd64"},
		{Key: "GOARM", Value: ""},
		{Key: "GOARM64", Value: ""},
		{Key: "GOEXPERIMENT", Value: ""},
		{Key: "GOFLAGS", Value: "-trimpath"},
		{Key: "GOOS", Value: "linux"},
		{Key: "GOTOOLCHAIN", Value: "go1.26.1"},
		{Key: "LC_ALL", Value: "C"},
		{Key: "SOURCE_DATE_EPOCH", Value: "0"},
		{Key: "TZ", Value: "UTC"},
	}
	got, err := BuildEnvironmentDigestV1(environment)
	if err != nil {
		t.Fatal(err)
	}
	const want = "568514f3afdc4789ac1591a7a649c3d19d522727d8b8a4c21c55a16d14b503f8"
	if got != want {
		t.Fatalf("BuildEnvironmentDigestV1() = %s, want %s", got, want)
	}
	for name, mutate := range map[string]func([]BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1{
		"missing": func(entries []BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1 { return entries[:len(entries)-1] },
		"extra": func(entries []BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1 {
			return append(entries, BuildEnvironmentEntryV1{Key: "TOKEN", Value: "forbidden"})
		},
		"reordered": func(entries []BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1 {
			entries[0], entries[1] = entries[1], entries[0]
			return entries
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyEntries := append([]BuildEnvironmentEntryV1(nil), environment...)
			if _, err := BuildEnvironmentDigestV1(mutate(copyEntries)); err == nil {
				t.Fatal("accepted invalid build environment")
			}
		})
	}
}

func TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality(t *testing.T) {
	floor := makeCUReports(9)
	if err := ValidateCUReportSetV1("floor", floor); err != nil {
		t.Fatal(err)
	}
	target := makeCUReports(10)
	if err := ValidateCUReportSetV1("target", target); err != nil {
		t.Fatal(err)
	}
	for name, reports := range map[string][]CUReportV1{
		"floor missing":  floor[:8],
		"floor plus one": append(append([]CUReportV1(nil), floor...), CUReportV1{SchemaVersion: 1, CUID: "CU-9", Kind: "construction_unit_report_v1", Outcome: "passed", RaceEnabled: true}),
		"target reordered": func() []CUReportV1 {
			copyReports := append([]CUReportV1(nil), target...)
			copyReports[0], copyReports[1] = copyReports[1], copyReports[0]
			return copyReports
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			purpose := "target"
			if strings.HasPrefix(name, "floor") {
				purpose = "floor"
			}
			if err := ValidateCUReportSetV1(purpose, reports); err == nil {
				t.Fatal("accepted invalid CU report set")
			}
		})
	}
}

func TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality(t *testing.T) {
	floor := makeReleaseBundleEntries("floor")
	if len(floor) != 10 {
		t.Fatalf("floor entries = %d", len(floor))
	}
	if err := ValidateReleaseBundleEntriesV1("floor", floor); err != nil {
		t.Fatal(err)
	}
	target := makeReleaseBundleEntries("target")
	if len(target) != 13 {
		t.Fatalf("target entries = %d", len(target))
	}
	if err := ValidateReleaseBundleEntriesV1("target", target); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]ReleaseBundleEntryV1{
		"missing":   target[:12],
		"plus one":  append(append([]ReleaseBundleEntryV1(nil), target...), ReleaseBundleEntryV1{RelativePath: "bundle.json", Kind: "file", Digest: strings.Repeat("1", 64), Size: 1}),
		"directory": append(append([]ReleaseBundleEntryV1(nil), target[:12]...), ReleaseBundleEntryV1{RelativePath: "reports", Kind: "file", Digest: strings.Repeat("1", 64), Size: 1}),
		"reordered": func() []ReleaseBundleEntryV1 {
			copyEntries := append([]ReleaseBundleEntryV1(nil), target...)
			copyEntries[0], copyEntries[1] = copyEntries[1], copyEntries[0]
			return copyEntries
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateReleaseBundleEntriesV1("target", entries); err == nil {
				t.Fatal("accepted invalid release bundle entries")
			}
		})
	}
}

func makeCUReports(count int) []CUReportV1 {
	reports := make([]CUReportV1, count)
	for index := range reports {
		reports[index] = CUReportV1{SchemaVersion: 1, CUID: "CU-" + string(rune('0'+index)), Kind: "construction_unit_report_v1", Outcome: "passed", RaceEnabled: true}
	}
	return reports
}

func makeReleaseBundleEntries(purpose string) []ReleaseBundleEntryV1 {
	paths := []string{
		"manifests/supervisor.json", "manifests/worker.json", "payloads/supervisor", "payloads/worker",
		"release-artifact-set.json", "release-build-authority.json", "reports/build.json", "reports/construction-units.json", "reports/race.json", "reports/vet.json",
	}
	if purpose == "target" {
		paths = append(paths, "manifests/launcher.json", "payloads/launcher", "source-ancestry.json")
	}
	// Validation requires byte ordering, so keep the fixture independent of
	// implementation-owned expected path tables.
	slices.Sort(paths)
	entries := make([]ReleaseBundleEntryV1, len(paths))
	for index, path := range paths {
		entries[index] = ReleaseBundleEntryV1{RelativePath: path, Kind: "file", Digest: strings.Repeat("1", 64), Size: 1}
	}
	return entries
}

func TestParseCUManifestRejectsUnknownMemberBeforeDispatch(t *testing.T) {
	_, err := ParseCUManifestV1(strings.NewReader(`{"schema_version":1,"unit":"CU-0","extra":true}`))
	if err == nil {
		t.Fatal("accepted unknown member")
	}
}

func TestExecutionResultDigestV1MatchesLiteralFraming(t *testing.T) {
	got, err := ExecutionResultDigestV1(0, "exited", []byte("ok\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = "5b5105133bcdac62f609fb3e6d2ca7aaa3d9a5cbcad526b7aef67baf66d05bf1"
	if got != want {
		t.Fatalf("ExecutionResultDigestV1() = %s, want %s", got, want)
	}
}

func TestNewCUReportV1TreatsForgedReportOutputAsCapturedBytes(t *testing.T) {
	started := time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC)
	ended := started.Add(time.Second)
	capture := CUReportCaptureV1{
		CUID:                       "CU-0",
		VerificationManifestDigest: strings.Repeat("1", 64),
		InvocationDigest:           strings.Repeat("2", 64),
		ExitCode:                   0,
		TerminationReason:          "exited",
		RaceEnabled:                true,
		Stdout:                     []byte(`{"kind":"construction_unit_report_v1"}`),
		StartedAt:                  started,
		EndedAt:                    ended,
	}
	report, err := NewCUReportV1(capture)
	if err != nil {
		t.Fatal(err)
	}
	changed := capture
	changed.Stdout = append(append([]byte(nil), capture.Stdout...), '\n')
	changedReport, err := NewCUReportV1(changed)
	if err != nil {
		t.Fatal(err)
	}
	if report.ExecutionResultDigest == changedReport.ExecutionResultDigest {
		t.Fatal("captured output substitution did not change the result digest")
	}
	if report.Kind != "construction_unit_report_v1" || report.Outcome != "passed" {
		t.Fatalf("report = %#v", report)
	}
}

func TestNewCUReportV1RejectsNonPassedAndOversizeCapture(t *testing.T) {
	base := CUReportCaptureV1{
		CUID:                       "CU-0",
		VerificationManifestDigest: strings.Repeat("1", 64),
		InvocationDigest:           strings.Repeat("2", 64),
		RaceEnabled:                true,
		TerminationReason:          "exited",
		StartedAt:                  time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC),
		EndedAt:                    time.Date(2026, 8, 14, 16, 0, 1, 0, time.UTC),
	}
	for name, mutate := range map[string]func(*CUReportCaptureV1){
		"nonzero":       func(value *CUReportCaptureV1) { value.ExitCode = 1 },
		"signal":        func(value *CUReportCaptureV1) { value.TerminationReason = "signalled" },
		"race disabled": func(value *CUReportCaptureV1) { value.RaceEnabled = false },
		"oversize":      func(value *CUReportCaptureV1) { value.Stdout = make([]byte, maxCUCaptureStreamBytes+1) },
	} {
		t.Run(name, func(t *testing.T) {
			capture := base
			mutate(&capture)
			if _, err := NewCUReportV1(capture); err == nil {
				t.Fatal("accepted invalid CU capture")
			}
		})
	}
}

func TestParseCUManifestAcceptsClosedCU0Selection(t *testing.T) {
	input := `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[{"full_test_ids":["TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping"],"minimum_pass_count":1,"package":"./internal/proxy","top_level_tests":["TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping"]}],"race_count":1,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`
	manifest, err := ParseCUManifestV1(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Packages) != 1 || manifest.Packages[0].MinimumPassCount != 1 {
		t.Fatalf("manifest packages = %#v", manifest.Packages)
	}
}

func TestCanonicalCUManifestV1ProvidesPinnedCU0Selection(t *testing.T) {
	data, err := CanonicalCUManifestV1("CU-0")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseCUManifestV1(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Unit != "CU-0" || len(manifest.Packages) != 4 {
		t.Fatalf("CU-0 manifest = %#v", manifest)
	}
	wantCounts := map[string]int{
		"./cmd/cq":                      15,
		"./internal/proxy":              37,
		"./internal/tools/proxycu":      13,
		"./internal/tools/proxyrelease": 5,
	}
	for _, selection := range manifest.Packages {
		if got, want := len(selection.FullTestIDs), wantCounts[selection.Package]; got != want {
			t.Fatalf("CU-0 manifest package %s has %d tests, want %d", selection.Package, got, want)
		}
	}
	if _, err := CanonicalCUManifestV1("CU-1"); err == nil {
		t.Fatal("returned a manifest for absent CU-1")
	}
}

func TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount(t *testing.T) {
	for name, input := range map[string]string{
		"empty selection": `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[],"race_count":1,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`,
		"duplicate test":  `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[{"full_test_ids":["TestOne","TestOne"],"minimum_pass_count":2,"package":"./internal/proxy","top_level_tests":["TestOne"]}],"race_count":1,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`,
		"race count":      `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[{"full_test_ids":["TestOne"],"minimum_pass_count":1,"package":"./internal/proxy","top_level_tests":["TestOne"]}],"race_count":2,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCUManifestV1(strings.NewReader(input)); err == nil {
				t.Fatal("accepted invalid CU manifest")
			}
		})
	}
}
