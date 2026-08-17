package proxy

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
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

func TestBuildEnvironmentDigestV1RejectsToolchainAndSecretDrift(t *testing.T) {
	base := []BuildEnvironmentEntryV1{
		{Key: "CGO_ENABLED", Value: "0"}, {Key: "GOAMD64", Value: "v1"},
		{Key: "GOARCH", Value: "amd64"}, {Key: "GOARM", Value: ""},
		{Key: "GOARM64", Value: ""}, {Key: "GOEXPERIMENT", Value: ""},
		{Key: "GOFLAGS", Value: "-trimpath"}, {Key: "GOOS", Value: "linux"},
		{Key: "GOTOOLCHAIN", Value: "go1.26.1"}, {Key: "LC_ALL", Value: "C"},
		{Key: "SOURCE_DATE_EPOCH", Value: "0"}, {Key: "TZ", Value: "UTC"},
	}
	for name, mutate := range map[string]func([]BuildEnvironmentEntryV1){
		"toolchain": func(entries []BuildEnvironmentEntryV1) { entries[8].Value = "go1.26.5" },
		"secret":    func(entries []BuildEnvironmentEntryV1) { entries[6].Value = "-trimpath token=secret" },
		"locale":    func(entries []BuildEnvironmentEntryV1) { entries[9].Value = "en_GB.UTF-8" },
		"timezone":  func(entries []BuildEnvironmentEntryV1) { entries[11].Value = "Europe/London" },
	} {
		t.Run(name, func(t *testing.T) {
			entries := append([]BuildEnvironmentEntryV1(nil), base...)
			mutate(entries)
			if _, err := BuildEnvironmentDigestV1(entries); err == nil {
				t.Fatal("accepted release environment drift")
			}
		})
	}
}

func TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory(t *testing.T) {
	for name, call := range map[string]func() error{
		"purpose": func() error {
			_, err := CommandDigestV1("arbitrary", ".", []string{"go", "test"})
			return err
		},
		"absolute cwd": func() error {
			_, err := CommandDigestV1("release-target-race", "/repo", []string{"go", "test"})
			return err
		},
		"traversal cwd": func() error {
			_, err := CommandDigestV1("release-target-race", "../repo", []string{"go", "test"})
			return err
		},
		"empty argv": func() error {
			_, err := CommandDigestV1("release-target-race", ".", nil)
			return err
		},
		"nul argv": func() error {
			_, err := CommandDigestV1("release-target-race", ".", []string{"go\x00test"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("accepted open command digest input")
			}
		})
	}
	if _, err := CommandDigestV1("role-worker-build", ".", []string{"go", "build", "./cmd/cq"}); err != nil {
		t.Fatal(err)
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

func TestVerifyReleaseGraphV1RejectsIncompleteGraph(t *testing.T) {
	if err := VerifyReleaseGraphV1(ReleaseGraphV1{}); err == nil {
		t.Fatal("accepted an incomplete release graph")
	}
}

func TestVerifyReleaseGraphV1AcceptsSignedFloorAndRejectsSubstitution(t *testing.T) {
	graph := signedFloorReleaseGraph(t)
	if err := VerifyReleaseGraphV1(graph); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ReleaseGraphV1){
		"role payload": func(graph *ReleaseGraphV1) {
			graph.ArtifactSet.Roles[0].ArtifactPayloadDigest = strings.Repeat("f", 64)
		},
		"report source": func(graph *ReleaseGraphV1) {
			graph.RaceReport.SourceCommit = strings.Repeat("f", 40)
		},
		"signature": func(graph *ReleaseGraphV1) {
			graph.CUReportSet.Signature = strings.Repeat("0", 128)
		},
		"bundle digest": func(graph *ReleaseGraphV1) {
			graph.Bundle.ReleaseArtifactSetDigest = strings.Repeat("f", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := graph
			changed.ArtifactSet.Roles = append([]ReleaseArtifactRoleV1(nil), graph.ArtifactSet.Roles...)
			mutate(&changed)
			if err := VerifyReleaseGraphV1(changed); err == nil {
				t.Fatal("accepted substituted release graph")
			}
		})
	}
}

func TestVerifyReleaseGraphV1AcceptsSignedTargetAndRejectsAncestry(t *testing.T) {
	graph := signedTargetReleaseGraph(t)
	if err := VerifyReleaseGraphV1(graph); err != nil {
		t.Fatal(err)
	}
	for name, fixture := range map[string]struct {
		mutate func(*ReleaseGraphV1)
		resign func(*testing.T, *ReleaseGraphV1)
	}{
		"same source": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry.TargetSourceCommit = graph.Ancestry.FloorSourceCommit
		}, resign: resignTargetAncestryChain},
		"wrong merge base": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry.MergeBaseCommit = graph.Ancestry.TargetSourceCommit
		}, resign: resignTargetAncestryChain},
		"ancestry signature": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry.Signature = strings.Repeat("0", 128)
		}},
		"missing ancestry": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry = nil
		}},
		"role swap": {mutate: func(graph *ReleaseGraphV1) {
			graph.ArtifactSet.Roles[0], graph.ArtifactSet.Roles[1] = graph.ArtifactSet.Roles[1], graph.ArtifactSet.Roles[0]
		}, resign: resignTargetSetChain},
		"CU missing": {mutate: func(graph *ReleaseGraphV1) {
			graph.CUReportSet.Reports = graph.CUReportSet.Reports[:9]
		}, resign: resignTargetCUChain},
		"ABI substitution": {mutate: func(graph *ReleaseGraphV1) {
			replacement := strings.Repeat("f", 64)
			graph.ArtifactManifests[1].LauncherABIDigest = &replacement
		}},
	} {
		t.Run(name, func(t *testing.T) {
			changed := graph
			ancestry := *graph.Ancestry
			changed.Ancestry = &ancestry
			changed.ArtifactSet.Roles = append([]ReleaseArtifactRoleV1(nil), graph.ArtifactSet.Roles...)
			changed.ArtifactManifests = append([]ReleaseArtifactManifestV1(nil), graph.ArtifactManifests...)
			changed.CUReportSet.Reports = append([]CUReportV1(nil), graph.CUReportSet.Reports...)
			fixture.mutate(&changed)
			if fixture.resign != nil {
				fixture.resign(t, &changed)
			}
			if err := VerifyReleaseGraphV1(changed); err == nil {
				t.Fatal("accepted invalid target release graph")
			}
		})
	}
}

func resignTargetAncestryChain(t *testing.T, graph *ReleaseGraphV1) {
	t.Helper()
	privateKey := fixedReleasePrivateKey()
	graph.Ancestry.Signature = ""
	graph.Ancestry.Signature = signReleaseObjectForTest(t, *graph.Ancestry, privateKey)
	ancestryDigest := releaseObjectDigestForTest(t, "cq/source-ancestry-receipt/v1\x00", *graph.Ancestry)
	graph.ArtifactSet.SourceAncestryReceiptDigest = &ancestryDigest
	graph.Bundle.SourceAncestryReceiptDigest = &ancestryDigest
	resignTargetSetChain(t, graph)
}

func resignTargetCUChain(t *testing.T, graph *ReleaseGraphV1) {
	t.Helper()
	privateKey := fixedReleasePrivateKey()
	graph.CUReportSet.Signature = ""
	graph.CUReportSet.Signature = signReleaseObjectForTest(t, graph.CUReportSet, privateKey)
	cuDigest := releaseObjectDigestForTest(t, "cq/construction-unit-report-set/v1\x00", graph.CUReportSet)
	graph.ArtifactSet.ConstructionUnitReportSetDigest = cuDigest
	graph.Bundle.ConstructionUnitReportSetDigest = cuDigest
	resignTargetSetChain(t, graph)
}

func resignTargetSetChain(t *testing.T, graph *ReleaseGraphV1) {
	t.Helper()
	privateKey := fixedReleasePrivateKey()
	graph.ArtifactSet.SetSignature = ""
	graph.ArtifactSet.SetSignature = signReleaseObjectForTest(t, graph.ArtifactSet, privateKey)
	setDigest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", graph.ArtifactSet)
	graph.Bundle.ReleaseArtifactSetDigest = setDigest
	graph.Bundle.BundleSignature = ""
	graph.Bundle.BundleSignature = signReleaseObjectForTest(t, graph.Bundle, privateKey)
}

func TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles(t *testing.T) {
	graph := signedFloorReleaseGraph(t)
	t.Run("exact floor", func(t *testing.T) {
		if err := VerifyReleaseBundleDirectoryV1(materialiseFloorReleaseBundle(t, graph), graph); err != nil {
			t.Fatal(err)
		}
	})
	for name, mutate := range map[string]func(*testing.T, string){
		"payload substitution": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "payloads", "worker"), []byte("substituted"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"manifest substitution": func(t *testing.T, root string) {
			path := filepath.Join(root, "manifests", "worker.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"missing file": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "reports", "vet.json")); err != nil {
				t.Fatal(err)
			}
		},
		"unknown file": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "unknown"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, root string) {
			if err := os.Symlink("worker", filepath.Join(root, "payloads", "link")); err != nil {
				t.Fatal(err)
			}
		},
		"nested directory": func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "payloads", "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := materialiseFloorReleaseBundle(t, graph)
			mutate(t, root)
			if err := VerifyReleaseBundleDirectoryV1(root, graph); err == nil {
				t.Fatal("accepted substituted physical bundle")
			}
		})
	}
}

func signedFloorReleaseGraph(t *testing.T) ReleaseGraphV1 {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = 7
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := hex.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	digest := func(value byte) string { return strings.Repeat(string(value), 64) }
	commit := strings.Repeat("a", 40)
	tree := digest('b')
	environmentDigest := digest('c')
	reachabilityDigest := digest('d')
	launcherABI := digest('e')
	privateABI := digest('1')
	authority := ReleaseBuildAuthorityV1{
		SchemaVersion: 1, AuthorityID: "authority-1", Ed25519PublicKey: publicKey,
		RepositoryIdentityDigest: digest('2'), BlueprintSHA256: frozenBlueprintSHA256,
		ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256,
		ReviewAuthorityBaselineCommit:    frozenReviewBaseline, LineageRootCommit: commit,
		LineageRootTreeDigest: tree, ToolchainIdentity: "go1.26.1 darwin/arm64",
		CreatedAt: "2026-08-17T10:00:00Z",
	}
	authorityDigest := releaseObjectDigestForTest(t, "cq/release-build-authority/v1\x00", authority)
	manifests := []ReleaseArtifactManifestV1{
		{SchemaVersion: 1, Role: "supervisor", ReleaseBuildAuthorityDigest: authorityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildCommandDigest: digest('3'), BuildEnvironmentDigest: environmentDigest, Architecture: "darwin/arm64", BuildID: "supervisor-1", SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"}, LauncherABIDigest: &launcherABI, PrivateABIDigest: &privateABI, CodeSignatureDigest: digest('4'), ArtifactPayloadDigest: sha256HexForTest([]byte("supervisor"))},
		{SchemaVersion: 1, Role: "worker", ReleaseBuildAuthorityDigest: authorityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildCommandDigest: digest('6'), BuildEnvironmentDigest: environmentDigest, Architecture: "darwin/arm64", BuildID: "worker-1", SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"}, PrivateABIDigest: &privateABI, CodeSignatureDigest: digest('7'), ArtifactPayloadDigest: sha256HexForTest([]byte("worker"))},
	}
	manifestDigests := []string{
		releaseObjectDigestForTest(t, "cq/release-artifact-manifest/v1\x00", manifests[0]),
		releaseObjectDigestForTest(t, "cq/release-artifact-manifest/v1\x00", manifests[1]),
	}
	roles := []ReleaseArtifactRoleV1{
		{Role: "supervisor", ArtifactPayloadDigest: manifests[0].ArtifactPayloadDigest, ArtifactManifestDigest: manifestDigests[0]},
		{Role: "worker", ArtifactPayloadDigest: manifests[1].ArtifactPayloadDigest, ArtifactManifestDigest: manifestDigests[1]},
	}
	build := ReleaseBuildReportV1{SchemaVersion: 1, Kind: "build", Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildEnvironmentDigest: environmentDigest, CommandDigest: digest('9'), Outcome: "passed", ExitCode: 0, RaceEnabled: false, ExecutionResultDigest: digest('a'), StartedAt: "2026-08-17T10:00:01Z", EndedAt: "2026-08-17T10:00:02Z", SignerPublicKey: publicKey}
	for index, role := range roles {
		build.RoleExecutions = append(build.RoleExecutions, ReleaseRoleExecutionV1{Role: role.Role, BuildCommandDigest: manifests[index].BuildCommandDigest, ArtifactPayloadDigest: role.ArtifactPayloadDigest, ArtifactManifestDigest: role.ArtifactManifestDigest})
	}
	build.Signature = signReleaseObjectForTest(t, build, privateKey)
	vet := build
	vet.Kind, vet.CommandDigest, vet.ExecutionResultDigest, vet.RoleExecutions, vet.StartedAt, vet.EndedAt, vet.Signature = "vet", digest('b'), digest('c'), []ReleaseRoleExecutionV1{}, "2026-08-17T10:00:03Z", "2026-08-17T10:00:04Z", ""
	vet.Signature = signReleaseObjectForTest(t, vet, privateKey)
	race := build
	race.Kind, race.CommandDigest, race.ExecutionResultDigest, race.RoleExecutions, race.RaceEnabled, race.StartedAt, race.EndedAt, race.Signature = "race", digest('d'), digest('e'), []ReleaseRoleExecutionV1{}, true, "2026-08-17T10:00:05Z", "2026-08-17T10:00:06Z", ""
	race.Signature = signReleaseObjectForTest(t, race, privateKey)
	reports := makeCUReports(9)
	for index := range reports {
		reports[index].VerificationManifestDigest = digest('1')
		reports[index].InvocationDigest = digest('2')
		reports[index].ExecutionResultDigest = digest('3')
		reports[index].StartedAt = "2026-08-17T10:00:07Z"
		reports[index].EndedAt = "2026-08-17T10:00:08Z"
	}
	cuSet := ConstructionUnitReportSetV1{SchemaVersion: 1, Kind: "construction_unit_report_set_v1", Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, BlueprintSHA256: frozenBlueprintSHA256, ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256, ReviewAuthorityBaselineCommit: frozenReviewBaseline, LegacyAtomicWriterReachabilityCatalogueDigest: reachabilityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildEnvironmentDigest: environmentDigest, Reports: reports, SignerPublicKey: publicKey}
	cuSet.Signature = signReleaseObjectForTest(t, cuSet, privateKey)
	buildDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", build)
	vetDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", vet)
	raceDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", race)
	cuSetDigest := releaseObjectDigestForTest(t, "cq/construction-unit-report-set/v1\x00", cuSet)
	artifactSet := ReleaseArtifactSetV1{SchemaVersion: 1, Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, SignerPublicKey: publicKey, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildEnvironmentDigest: environmentDigest, BuildReportDigest: buildDigest, VetReportDigest: vetDigest, RaceTestReportDigest: raceDigest, ConstructionUnitReportSetDigest: cuSetDigest, LegacyAtomicWriterReachabilityCatalogueDigest: reachabilityDigest, RequiredLauncherABIDigest: &launcherABI, Roles: roles, SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"}}
	artifactSet.SetSignature = signReleaseObjectForTest(t, artifactSet, privateKey)
	artifactSetDigest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", artifactSet)
	entries := []ReleaseBundleEntryV1{
		{RelativePath: "manifests/supervisor.json", Kind: "file", Digest: manifestDigests[0], Size: canonicalSizeForTest(t, manifests[0])},
		{RelativePath: "manifests/worker.json", Kind: "file", Digest: manifestDigests[1], Size: canonicalSizeForTest(t, manifests[1])},
		{RelativePath: "payloads/supervisor", Kind: "file", Digest: manifests[0].ArtifactPayloadDigest, Size: uint64(len("supervisor"))},
		{RelativePath: "payloads/worker", Kind: "file", Digest: manifests[1].ArtifactPayloadDigest, Size: uint64(len("worker"))},
		{RelativePath: "release-artifact-set.json", Kind: "file", Digest: artifactSetDigest, Size: canonicalSizeForTest(t, artifactSet)},
		{RelativePath: "release-build-authority.json", Kind: "file", Digest: authorityDigest, Size: canonicalSizeForTest(t, authority)},
		{RelativePath: "reports/build.json", Kind: "file", Digest: buildDigest, Size: canonicalSizeForTest(t, build)},
		{RelativePath: "reports/construction-units.json", Kind: "file", Digest: cuSetDigest, Size: canonicalSizeForTest(t, cuSet)},
		{RelativePath: "reports/race.json", Kind: "file", Digest: raceDigest, Size: canonicalSizeForTest(t, race)},
		{RelativePath: "reports/vet.json", Kind: "file", Digest: vetDigest, Size: canonicalSizeForTest(t, vet)},
	}
	bundle := ReleaseBundleV1{SchemaVersion: 1, Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, ReleaseArtifactSetDigest: artifactSetDigest, ConstructionUnitReportSetDigest: cuSetDigest, Entries: entries, SignerPublicKey: publicKey}
	bundle.BundleSignature = signReleaseObjectForTest(t, bundle, privateKey)
	return ReleaseGraphV1{Authority: authority, BuildReport: build, VetReport: vet, RaceReport: race, CUReportSet: cuSet, ArtifactManifests: manifests, ArtifactSet: artifactSet, Bundle: bundle}
}

func signedTargetReleaseGraph(t *testing.T) ReleaseGraphV1 {
	t.Helper()
	floor := signedFloorReleaseGraph(t)
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = 7
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	digest := func(value byte) string { return strings.Repeat(string(value), 64) }
	targetCommit := strings.Repeat("f", 40)
	targetTree := digest('9')
	launcherABI := digest('e')
	privateABI := digest('1')
	authorityDigest := releaseObjectDigestForTest(t, "cq/release-build-authority/v1\x00", floor.Authority)
	launcher := ReleaseArtifactManifestV1{
		SchemaVersion: 1, Role: "launcher", ReleaseBuildAuthorityDigest: authorityDigest,
		SourceCommit: targetCommit, SourceTreeDigest: targetTree, ToolchainIdentity: floor.Authority.ToolchainIdentity,
		BuildCommandDigest: digest('8'), BuildEnvironmentDigest: floor.ArtifactSet.BuildEnvironmentDigest,
		Architecture: "darwin/arm64", BuildID: "launcher-1", SupportedFeatures: []string{"proxy_v1"},
		MinimumFloorFeatures: []string{"proxy_v1"}, LauncherABIDigest: &launcherABI,
		CodeSignatureDigest: digest('5'), ArtifactPayloadDigest: sha256HexForTest([]byte("launcher")),
	}
	manifests := []ReleaseArtifactManifestV1{launcher, floor.ArtifactManifests[0], floor.ArtifactManifests[1]}
	for index := 1; index < len(manifests); index++ {
		manifests[index].SourceCommit = targetCommit
		manifests[index].SourceTreeDigest = targetTree
		manifests[index].BuildID = "target-" + manifests[index].Role
		if manifests[index].Role == "supervisor" {
			manifests[index].LauncherABIDigest = &launcherABI
			manifests[index].PrivateABIDigest = &privateABI
		}
	}
	manifestDigests := make([]string, len(manifests))
	roles := make([]ReleaseArtifactRoleV1, len(manifests))
	for index := range manifests {
		manifestDigests[index] = releaseObjectDigestForTest(t, "cq/release-artifact-manifest/v1\x00", manifests[index])
		roles[index] = ReleaseArtifactRoleV1{Role: manifests[index].Role, ArtifactPayloadDigest: manifests[index].ArtifactPayloadDigest, ArtifactManifestDigest: manifestDigests[index]}
	}

	build := floor.BuildReport
	build.Purpose, build.SourceCommit, build.SourceTreeDigest = "target", targetCommit, targetTree
	build.CommandDigest, build.ExecutionResultDigest, build.Signature = digest('4'), digest('5'), ""
	build.RoleExecutions = make([]ReleaseRoleExecutionV1, len(roles))
	for index := range roles {
		build.RoleExecutions[index] = ReleaseRoleExecutionV1{Role: roles[index].Role, BuildCommandDigest: manifests[index].BuildCommandDigest, ArtifactPayloadDigest: roles[index].ArtifactPayloadDigest, ArtifactManifestDigest: roles[index].ArtifactManifestDigest}
	}
	build.Signature = signReleaseObjectForTest(t, build, privateKey)
	vet := floor.VetReport
	vet.Purpose, vet.SourceCommit, vet.SourceTreeDigest, vet.Signature = "target", targetCommit, targetTree, ""
	vet.CommandDigest, vet.ExecutionResultDigest = digest('6'), digest('7')
	vet.Signature = signReleaseObjectForTest(t, vet, privateKey)
	race := floor.RaceReport
	race.Purpose, race.SourceCommit, race.SourceTreeDigest, race.Signature = "target", targetCommit, targetTree, ""
	race.CommandDigest, race.ExecutionResultDigest = digest('8'), digest('9')
	race.Signature = signReleaseObjectForTest(t, race, privateKey)

	cuSet := floor.CUReportSet
	cuSet.Purpose, cuSet.SourceCommit, cuSet.SourceTreeDigest, cuSet.Signature = "target", targetCommit, targetTree, ""
	cuSet.Reports = makeCUReports(10)
	for index := range cuSet.Reports {
		cuSet.Reports[index].VerificationManifestDigest = digest('1')
		cuSet.Reports[index].InvocationDigest = digest('2')
		cuSet.Reports[index].ExecutionResultDigest = digest('3')
		cuSet.Reports[index].StartedAt = "2026-08-17T10:00:07Z"
		cuSet.Reports[index].EndedAt = "2026-08-17T10:00:08Z"
	}
	cuSet.Signature = signReleaseObjectForTest(t, cuSet, privateKey)

	ancestry := SourceAncestryReceiptV1{
		SchemaVersion: 1, Kind: "source_ancestry_v1", ReleaseBuildAuthorityDigest: authorityDigest,
		RepositoryIdentityDigest: floor.Authority.RepositoryIdentityDigest,
		FloorSourceCommit:        floor.Authority.LineageRootCommit, FloorSourceTreeDigest: floor.Authority.LineageRootTreeDigest,
		TargetSourceCommit: targetCommit, TargetSourceTreeDigest: targetTree, MergeBaseCommit: floor.Authority.LineageRootCommit,
		VerificationCommandDigest: digest('a'), VerifiedAt: "2026-08-17T10:00:09Z",
		SignerPublicKey: floor.Authority.Ed25519PublicKey,
	}
	ancestry.Signature = signReleaseObjectForTest(t, ancestry, privateKey)
	ancestryDigest := releaseObjectDigestForTest(t, "cq/source-ancestry-receipt/v1\x00", ancestry)
	buildDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", build)
	vetDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", vet)
	raceDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", race)
	cuSetDigest := releaseObjectDigestForTest(t, "cq/construction-unit-report-set/v1\x00", cuSet)
	artifactSet := ReleaseArtifactSetV1{
		SchemaVersion: 1, Purpose: "target", ReleaseBuildAuthorityDigest: authorityDigest,
		SignerPublicKey: floor.Authority.Ed25519PublicKey, SourceCommit: targetCommit, SourceTreeDigest: targetTree,
		ToolchainIdentity: floor.Authority.ToolchainIdentity, BuildEnvironmentDigest: floor.ArtifactSet.BuildEnvironmentDigest,
		BuildReportDigest: buildDigest, VetReportDigest: vetDigest, RaceTestReportDigest: raceDigest,
		ConstructionUnitReportSetDigest:               cuSetDigest,
		LegacyAtomicWriterReachabilityCatalogueDigest: floor.ArtifactSet.LegacyAtomicWriterReachabilityCatalogueDigest,
		SourceAncestryReceiptDigest:                   &ancestryDigest, LauncherABIDigest: &launcherABI, Roles: roles,
		SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"},
	}
	artifactSet.SetSignature = signReleaseObjectForTest(t, artifactSet, privateKey)
	artifactSetDigest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", artifactSet)
	entries := []ReleaseBundleEntryV1{
		{RelativePath: "manifests/launcher.json", Kind: "file", Digest: manifestDigests[0], Size: canonicalSizeForTest(t, manifests[0])},
		{RelativePath: "manifests/supervisor.json", Kind: "file", Digest: manifestDigests[1], Size: canonicalSizeForTest(t, manifests[1])},
		{RelativePath: "manifests/worker.json", Kind: "file", Digest: manifestDigests[2], Size: canonicalSizeForTest(t, manifests[2])},
		{RelativePath: "payloads/launcher", Kind: "file", Digest: manifests[0].ArtifactPayloadDigest, Size: uint64(len("launcher"))},
		{RelativePath: "payloads/supervisor", Kind: "file", Digest: manifests[1].ArtifactPayloadDigest, Size: uint64(len("supervisor"))},
		{RelativePath: "payloads/worker", Kind: "file", Digest: manifests[2].ArtifactPayloadDigest, Size: uint64(len("worker"))},
		{RelativePath: "release-artifact-set.json", Kind: "file", Digest: artifactSetDigest, Size: canonicalSizeForTest(t, artifactSet)},
		{RelativePath: "release-build-authority.json", Kind: "file", Digest: authorityDigest, Size: canonicalSizeForTest(t, floor.Authority)},
		{RelativePath: "reports/build.json", Kind: "file", Digest: buildDigest, Size: canonicalSizeForTest(t, build)},
		{RelativePath: "reports/construction-units.json", Kind: "file", Digest: cuSetDigest, Size: canonicalSizeForTest(t, cuSet)},
		{RelativePath: "reports/race.json", Kind: "file", Digest: raceDigest, Size: canonicalSizeForTest(t, race)},
		{RelativePath: "reports/vet.json", Kind: "file", Digest: vetDigest, Size: canonicalSizeForTest(t, vet)},
		{RelativePath: "source-ancestry.json", Kind: "file", Digest: ancestryDigest, Size: canonicalSizeForTest(t, ancestry)},
	}
	bundle := ReleaseBundleV1{SchemaVersion: 1, Purpose: "target", ReleaseBuildAuthorityDigest: authorityDigest, ReleaseArtifactSetDigest: artifactSetDigest, ConstructionUnitReportSetDigest: cuSetDigest, SourceAncestryReceiptDigest: &ancestryDigest, Entries: entries, SignerPublicKey: floor.Authority.Ed25519PublicKey}
	bundle.BundleSignature = signReleaseObjectForTest(t, bundle, privateKey)
	return ReleaseGraphV1{Authority: floor.Authority, Ancestry: &ancestry, BuildReport: build, VetReport: vet, RaceReport: race, CUReportSet: cuSet, ArtifactManifests: manifests, ArtifactSet: artifactSet, Bundle: bundle}
}

func materialiseFloorReleaseBundle(t *testing.T, graph ReleaseGraphV1) string {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"manifests", "payloads", "reports"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
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
	for _, manifest := range graph.ArtifactManifests {
		objects["manifests/"+manifest.Role+".json"] = manifest
	}
	for path, object := range objects {
		data, err := CanonicalJSONV1(object)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for role, data := range map[string][]byte{"supervisor": []byte("supervisor"), "worker": []byte("worker")} {
		if err := os.WriteFile(filepath.Join(root, "payloads", role), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func canonicalSizeForTest(t *testing.T, value any) uint64 {
	t.Helper()
	data, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(len(data))
}

func sha256HexForTest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func signReleaseObjectForTest(t *testing.T, value any, privateKey ed25519.PrivateKey) string {
	t.Helper()
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(ed25519.Sign(privateKey, canonical))
}

func fixedReleasePrivateKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = 7
	}
	return ed25519.NewKeyFromSeed(seed)
}

func releaseObjectDigestForTest(t *testing.T, domain string, value any) string {
	t.Helper()
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	hash.Write([]byte(domain))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(canonical)))
	hash.Write(length[:])
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil))
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
		"./cmd/cq":                      19,
		"./internal/proxy":              142,
		"./internal/tools/proxycu":      24,
		"./internal/tools/proxyrelease": 11,
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
