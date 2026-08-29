//go:build !windows

package proxy

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

func TestReleaseBuildAuthorityRequiresExternalPinnedKeyAndDigest(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x63}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	authority := minimalReleaseAuthorityForTest(publicKey)
	digest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMinimalReleaseBuildAuthority(authority, ReleaseAuthorityPinV1{Digest: digest, Ed25519PublicKey: authority.Ed25519PublicKey}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMinimalReleaseBuildAuthority(authority, ReleaseAuthorityPinV1{Digest: digestBytes([]byte("foreign")), Ed25519PublicKey: authority.Ed25519PublicKey}); err == nil {
		t.Fatal("authority accepted foreign external digest")
	}
}
