package proxy

import "fmt"

// DigestReleaseBuildAuthority returns the canonical authority digest after
// validating the closed V1 authority shape. Trust still comes from an external
// ReleaseAuthorityPinV1 supplied to VerifyMinimalReleaseBuildAuthority.
func DigestReleaseBuildAuthority(authority ReleaseBuildAuthorityV1) (string, error) {
	digest, err := releaseObjectDigestV1("cq/release-build-authority/v1\x00", authority)
	if err != nil {
		return "", err
	}
	if err := VerifyReleaseBuildAuthorityV1(authority, ReleaseAuthorityPinV1{Digest: digest, Ed25519PublicKey: authority.Ed25519PublicKey}); err != nil {
		return "", fmt.Errorf("validate release build authority: %w", err)
	}
	return digest, nil
}

func VerifyMinimalReleaseBuildAuthority(authority ReleaseBuildAuthorityV1, expected ReleaseAuthorityPinV1) error {
	return VerifyReleaseBuildAuthorityV1(authority, expected)
}
