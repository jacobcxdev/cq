package proxy

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func TestOperationalReleaseBundleSignsExactRoleArtifacts(t *testing.T) {
	seed := []byte(strings.Repeat("s", ed25519.SeedSize))
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	authority := minimalReleaseAuthorityForTest(publicKey)
	authorityDigest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	input := OperationalReleaseBuildInputV1{
		Purpose: "target", AuthorityDigest: authorityDigest,
		SourceCommit: strings.Repeat("a", 40), SourceTreeDigest: strings.Repeat("b", 64),
		Roles: []OperationalReleaseRoleV1{
			{Role: "launcher", ArtifactDigest: strings.Repeat("1", 64), ByteCount: 101},
			{Role: "supervisor", ArtifactDigest: strings.Repeat("2", 64), ByteCount: 102},
			{Role: "worker", ArtifactDigest: strings.Repeat("3", 64), ByteCount: 103},
		},
		BuiltAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	}
	bundle, err := BuildOperationalReleaseBundleV1(authority, input, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyOperationalReleaseBundleV1(authority, bundle, ReleaseAuthorityPinV1{Digest: authorityDigest, Ed25519PublicKey: authority.Ed25519PublicKey}); err != nil {
		t.Fatal(err)
	}
	bundle.Roles[1].ByteCount++
	if err := VerifyOperationalReleaseBundleV1(authority, bundle, ReleaseAuthorityPinV1{Digest: authorityDigest, Ed25519PublicKey: authority.Ed25519PublicKey}); err == nil {
		t.Fatal("accepted changed role artifact")
	}
}

func TestOperationalReleaseBundleRequiresExactPurposeRoles(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	authority := minimalReleaseAuthorityForTest(privateKey.Public().(ed25519.PublicKey))
	digest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	input := OperationalReleaseBuildInputV1{Purpose: "floor", AuthorityDigest: digest, SourceCommit: strings.Repeat("a", 40), SourceTreeDigest: strings.Repeat("b", 64), BuiltAt: time.Now().UTC(), Roles: []OperationalReleaseRoleV1{{Role: "supervisor", ArtifactDigest: strings.Repeat("1", 64), ByteCount: 1}}}
	if _, err := BuildOperationalReleaseBundleV1(authority, input, privateKey); err == nil {
		t.Fatal("accepted missing floor worker")
	}
}
