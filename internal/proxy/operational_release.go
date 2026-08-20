//go:build !windows

package proxy

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
)

type OperationalReleaseRoleV1 struct {
	Role           string `json:"role"`
	ArtifactDigest string `json:"artifact_digest"`
	ByteCount      int64  `json:"byte_count"`
}

type OperationalReleaseBuildInputV1 struct {
	Purpose          string                     `json:"purpose"`
	AuthorityDigest  string                     `json:"authority_digest"`
	SourceCommit     string                     `json:"source_commit"`
	SourceTreeDigest string                     `json:"source_tree_digest"`
	Roles            []OperationalReleaseRoleV1 `json:"roles"`
	BuiltAt          time.Time                  `json:"built_at"`
}

type OperationalReleaseBundleV1 struct {
	SchemaVersion    int                        `json:"schema_version"`
	Kind             string                     `json:"kind"`
	Purpose          string                     `json:"purpose"`
	AuthorityDigest  string                     `json:"authority_digest"`
	SourceCommit     string                     `json:"source_commit"`
	SourceTreeDigest string                     `json:"source_tree_digest"`
	Roles            []OperationalReleaseRoleV1 `json:"roles"`
	BuiltAt          string                     `json:"built_at"`
	SignerPublicKey  string                     `json:"signer_public_key"`
	Signature        string                     `json:"signature"`
	Digest           string                     `json:"digest"`
}

func BuildOperationalReleaseBundleV1(authority ReleaseBuildAuthorityV1, input OperationalReleaseBuildInputV1, privateKey ed25519.PrivateKey) (OperationalReleaseBundleV1, error) {
	authorityDigest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		return OperationalReleaseBundleV1{}, err
	}
	if input.AuthorityDigest != authorityDigest {
		return OperationalReleaseBundleV1{}, fmt.Errorf("operational release authority digest mismatch")
	}
	publicKey, err := decodeEd25519PublicKey(authority.Ed25519PublicKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize || !slices.Equal(privateKey.Public().(ed25519.PublicKey), publicKey) {
		return OperationalReleaseBundleV1{}, fmt.Errorf("operational release signer mismatch")
	}
	if err := validateOperationalReleaseInput(input); err != nil {
		return OperationalReleaseBundleV1{}, err
	}
	bundle := OperationalReleaseBundleV1{
		SchemaVersion:    1,
		Kind:             "operational_release_bundle_v1",
		Purpose:          input.Purpose,
		AuthorityDigest:  authorityDigest,
		SourceCommit:     input.SourceCommit,
		SourceTreeDigest: input.SourceTreeDigest,
		Roles:            append([]OperationalReleaseRoleV1(nil), input.Roles...),
		BuiltAt:          input.BuiltAt.UTC().Format(time.RFC3339),
		SignerPublicKey:  authority.Ed25519PublicKey,
	}
	canonical, err := operationalReleaseSignable(bundle)
	if err != nil {
		return OperationalReleaseBundleV1{}, err
	}
	bundle.Signature = hex.EncodeToString(ed25519.Sign(privateKey, canonical))
	bundle.Digest, err = operationalReleaseDigest(bundle)
	if err != nil {
		return OperationalReleaseBundleV1{}, err
	}
	return bundle, nil
}

func VerifyOperationalReleaseBundleV1(authority ReleaseBuildAuthorityV1, bundle OperationalReleaseBundleV1, expected ReleaseAuthorityPinV1) error {
	if err := VerifyMinimalReleaseBuildAuthority(authority, expected); err != nil {
		return err
	}
	authorityDigest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		return err
	}
	builtAt, err := time.Parse(time.RFC3339, bundle.BuiltAt)
	if err != nil {
		return fmt.Errorf("invalid operational release timestamp")
	}
	input := OperationalReleaseBuildInputV1{
		Purpose:          bundle.Purpose,
		AuthorityDigest:  bundle.AuthorityDigest,
		SourceCommit:     bundle.SourceCommit,
		SourceTreeDigest: bundle.SourceTreeDigest,
		Roles:            bundle.Roles,
		BuiltAt:          builtAt,
	}
	if bundle.SchemaVersion != 1 || bundle.Kind != "operational_release_bundle_v1" || bundle.AuthorityDigest != authorityDigest || bundle.SignerPublicKey != authority.Ed25519PublicKey {
		return fmt.Errorf("invalid operational release identity")
	}
	if err := validateOperationalReleaseInput(input); err != nil {
		return err
	}
	publicKey, err := decodeEd25519PublicKey(authority.Ed25519PublicKey)
	if err != nil {
		return err
	}
	canonical, err := operationalReleaseSignable(bundle)
	if err != nil {
		return err
	}
	signature, err := hex.DecodeString(bundle.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, canonical, signature) {
		return fmt.Errorf("invalid operational release signature")
	}
	digest, err := operationalReleaseDigest(bundle)
	if err != nil {
		return err
	}
	if bundle.Digest != digest {
		return fmt.Errorf("invalid operational release digest")
	}
	return nil
}

func validateOperationalReleaseInput(input OperationalReleaseBuildInputV1) error {
	if !lowerHex64Pattern.MatchString(input.AuthorityDigest) || !lowerHexCommit(input.SourceCommit) || !lowerHex64Pattern.MatchString(input.SourceTreeDigest) || input.BuiltAt.IsZero() || !input.BuiltAt.Equal(input.BuiltAt.UTC()) {
		return fmt.Errorf("invalid operational release input")
	}
	wantRoles := []string{"launcher", "supervisor", "worker"}
	if input.Purpose == "floor" {
		wantRoles = []string{"supervisor", "worker"}
	} else if input.Purpose != "target" {
		return fmt.Errorf("invalid operational release purpose")
	}
	if len(input.Roles) != len(wantRoles) {
		return fmt.Errorf("operational release roles do not match purpose")
	}
	for index, role := range input.Roles {
		if role.Role != wantRoles[index] || !lowerHex64Pattern.MatchString(role.ArtifactDigest) || role.ByteCount <= 0 {
			return fmt.Errorf("invalid operational release role")
		}
	}
	return nil
}

func operationalReleaseSignable(bundle OperationalReleaseBundleV1) ([]byte, error) {
	bundle.Signature = ""
	bundle.Digest = ""
	return CanonicalJSONV1(bundle)
}

func operationalReleaseDigest(bundle OperationalReleaseBundleV1) (string, error) {
	bundle.Digest = ""
	canonical, err := CanonicalJSONV1(bundle)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("cq/operational-release-bundle/v1\x00"))
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}
