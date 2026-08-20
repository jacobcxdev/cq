package proxy

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func TestReleaseBundleBuildsSignedExactRollbackFloorAndAcceptance(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x61}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	authority := minimalReleaseAuthorityForTest(publicKey)
	authorityDigest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	input := rollbackFloorInputForTest(authorityDigest, authority)
	bundle, receipt, err := BuildRollbackFloorBundle(authority, input, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	pin := ReleaseAuthorityPinV1{Digest: authorityDigest, Ed25519PublicKey: authority.Ed25519PublicKey}
	if err := VerifyRollbackFloorBundle(authority, bundle, receipt, pin); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Roles) != 2 || bundle.Roles[0].Role != "supervisor" || bundle.Roles[1].Role != "worker" {
		t.Fatalf("roles = %#v", bundle.Roles)
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("target"), []byte("ancestry"), []byte("launcher_payload")} {
		if bytes.Contains(body, forbidden) {
			t.Fatalf("acceptance receipt contains forbidden field: %s", body)
		}
	}
}

func TestReleaseBundleRejectsRoleSourceToolchainAndReportSubstitution(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x62}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	authority := minimalReleaseAuthorityForTest(publicKey)
	authorityDigest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	base := rollbackFloorInputForTest(authorityDigest, authority)
	tests := map[string]func(*RollbackFloorBuildInputV1){
		"launcher role": func(input *RollbackFloorBuildInputV1) { input.Roles[0].Role = "launcher" },
		"source": func(input *RollbackFloorBuildInputV1) {
			input.SourceCommit = "1111111111111111111111111111111111111111"
		},
		"toolchain": func(input *RollbackFloorBuildInputV1) { input.ToolchainIdentity = "go1.26.2 darwin/arm64" },
		"report":    func(input *RollbackFloorBuildInputV1) { input.VetReportDigest = "not-a-digest" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := base
			input.Roles = append([]RollbackFloorRoleV1(nil), base.Roles...)
			mutate(&input)
			if _, _, err := BuildRollbackFloorBundle(authority, input, privateKey); err == nil {
				t.Fatal("BuildRollbackFloorBundle accepted substituted authority")
			}
		})
	}
}

func minimalReleaseAuthorityForTest(publicKey ed25519.PublicKey) ReleaseBuildAuthorityV1 {
	return ReleaseBuildAuthorityV1{
		SchemaVersion: 1, AuthorityID: "floor-authority", Ed25519PublicKey: hex.EncodeToString(publicKey),
		RepositoryIdentityDigest: digestBytes([]byte("repository")), BlueprintSHA256: frozenBlueprintSHA256,
		ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256, ReviewAuthorityBaselineCommit: frozenReviewBaseline,
		LineageRootCommit: "0123456789abcdef0123456789abcdef01234567", LineageRootTreeDigest: digestBytes([]byte("tree")),
		ToolchainIdentity: "go1.26.1 darwin/arm64", CreatedAt: "2026-08-20T10:00:00Z",
	}
}

func rollbackFloorInputForTest(authorityDigest string, authority ReleaseBuildAuthorityV1) RollbackFloorBuildInputV1 {
	return RollbackFloorBuildInputV1{
		SchemaVersion: 1, AcceptanceRunID: "floor-run", ReleaseBuildAuthorityDigest: authorityDigest,
		SourceCommit: authority.LineageRootCommit, SourceTreeDigest: authority.LineageRootTreeDigest, ToolchainIdentity: authority.ToolchainIdentity,
		Roles: []RollbackFloorRoleV1{
			{Role: "worker", ArtifactDigest: digestBytes([]byte("worker")), ManifestDigest: digestBytes([]byte("worker-manifest"))},
			{Role: "supervisor", ArtifactDigest: digestBytes([]byte("supervisor")), ManifestDigest: digestBytes([]byte("supervisor-manifest"))},
		},
		BuildReportDigest: digestBytes([]byte("build")), VetReportDigest: digestBytes([]byte("vet")), RaceTestReportDigest: digestBytes([]byte("race")), ConstructionUnitReportSetDigest: digestBytes([]byte("cu0-cu8")),
		LegacyAtomicWriterReachabilityCatalogueDigest: digestBytes([]byte("reachability")),
		ConstructionUnits:        []string{"CU-0", "CU-1", "CU-2", "CU-3", "CU-4", "CU-5", "CU-6", "CU-7", "CU-8"},
		ReleaseArtifactSetDigest: digestBytes([]byte("artifact-set")), RequiredLauncherABIDigest: digestBytes([]byte("launcher-abi")),
		RequiredFeatures: []string{"capability_routing_v1", "proxy_v1"}, InstalledCorpusDigest: digestBytes([]byte("installed")), RecoveryCorpusDigest: digestBytes([]byte("recovery")), RollbackCorpusDigest: digestBytes([]byte("rollback")),
		StartedAt: time.Unix(1_700_000_000, 0).UTC(), EndedAt: time.Unix(1_700_000_001, 0).UTC(), Nonce: "0123456789abcdef0123456789abcdef",
	}
}
