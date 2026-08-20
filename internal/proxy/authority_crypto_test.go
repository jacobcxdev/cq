package proxy

import (
	"encoding/hex"
	"testing"
)

func TestAuthorityCryptoFramesDomainsAndLengths(t *testing.T) {
	first, err := FramedSHA256Hex("cq/test/v1\x00", []byte("ab"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := FramedSHA256Hex("cq/test/v1\x00", []byte("a"), []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different framed payloads produced the same digest")
	}
	if len(first) != 64 {
		t.Fatalf("digest length = %d, want 64", len(first))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("digest is not lower hexadecimal: %q", first)
	}
	if _, err := FramedSHA256Hex("cq/test/v1", []byte("ab")); err == nil {
		t.Fatal("domain without NUL terminator accepted")
	}
	if _, err := FramedSHA256Hex("cq/test\x00/v1\x00", []byte("ab")); err == nil {
		t.Fatal("domain with interior NUL accepted")
	}
}

func TestAuthorityCryptoMACUsesConstantTimeVerifierContract(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	mac, err := FramedHMACSHA256Hex(key, "cq/test/mac/v1\x00", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyFramedHMACSHA256Hex(key, "cq/test/mac/v1\x00", mac, []byte("payload")) {
		t.Fatal("valid MAC rejected")
	}
	if mac[0] == '0' {
		mac = "1" + mac[1:]
	} else {
		mac = "0" + mac[1:]
	}
	if VerifyFramedHMACSHA256Hex(key, "cq/test/mac/v1\x00", mac, []byte("payload")) {
		t.Fatal("modified MAC accepted")
	}
	if VerifyFramedHMACSHA256Hex(key, "cq/test/mac/v1\x00", "ABC", []byte("payload")) {
		t.Fatal("non-canonical MAC accepted")
	}
	if VerifyFramedHMACSHA256Hex(nil, "cq/test/mac/v1\x00", mac, []byte("payload")) {
		t.Fatal("empty MAC key accepted")
	}
}

func TestAuthorityCryptoDerivesPurposeSeparatedKeys(t *testing.T) {
	root := []byte("01234567890123456789012345678901")
	first, err := DeriveAuthorityKey(root, "cq/test/key-a/v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveAuthorityKey(root, "cq/test/key-b/v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("different HKDF purposes produced the same key")
	}
	if _, err := DeriveAuthorityKey(root, "cq/test/key-a/v1\x00", 32); err == nil {
		t.Fatal("NUL-terminated HKDF info accepted")
	}
}
