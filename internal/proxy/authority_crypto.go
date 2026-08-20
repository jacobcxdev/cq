package proxy

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
)

var ErrAuthorityCryptoInput = errors.New("invalid authority cryptographic input")

// FramedSHA256Hex hashes a domain followed by length-framed byte strings.
func FramedSHA256Hex(domain string, values ...[]byte) (string, error) {
	digest, err := framedHash(sha256.New(), domain, values...)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest), nil
}

// FramedHMACSHA256Hex authenticates a domain followed by length-framed byte strings.
func FramedHMACSHA256Hex(key []byte, domain string, values ...[]byte) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("%w: empty MAC key", ErrAuthorityCryptoInput)
	}
	digest, err := framedHash(hmac.New(sha256.New, key), domain, values...)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest), nil
}

// VerifyFramedHMACSHA256Hex accepts only canonical lower-hex MACs and compares
// decoded bytes in constant time.
func VerifyFramedHMACSHA256Hex(key []byte, domain, expected string, values ...[]byte) bool {
	if len(key) == 0 || len(expected) != sha256.Size*2 || expected != strings.ToLower(expected) {
		return false
	}
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil {
		return false
	}
	actual, err := framedHash(hmac.New(sha256.New, key), domain, values...)
	if err != nil {
		return false
	}
	return hmac.Equal(actual, expectedBytes)
}

// DeriveAuthorityKey derives a purpose-specific key with empty HKDF salt.
// Purpose strings are ASCII protocol labels without a NUL terminator.
func DeriveAuthorityKey(root []byte, purpose string, length int) ([]byte, error) {
	if len(root) == 0 || purpose == "" || strings.ContainsRune(purpose, '\x00') || length <= 0 {
		return nil, fmt.Errorf("%w: HKDF parameters", ErrAuthorityCryptoInput)
	}
	for _, character := range purpose {
		if character < 0x20 || character > 0x7e {
			return nil, fmt.Errorf("%w: HKDF purpose", ErrAuthorityCryptoInput)
		}
	}
	return hkdf.Key(sha256.New, root, nil, purpose, length)
}

func framedHash(digest hash.Hash, domain string, values ...[]byte) ([]byte, error) {
	if domain == "" || domain[len(domain)-1] != 0 {
		return nil, fmt.Errorf("%w: domain must end in NUL", ErrAuthorityCryptoInput)
	}
	for index := 0; index < len(domain)-1; index++ {
		if domain[index] < 0x20 || domain[index] > 0x7e {
			return nil, fmt.Errorf("%w: domain must be printable ASCII", ErrAuthorityCryptoInput)
		}
	}
	if _, err := digest.Write([]byte(domain)); err != nil {
		return nil, err
	}
	var length [4]byte
	for _, value := range values {
		if uint64(len(value)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("%w: framed value too large", ErrAuthorityCryptoInput)
		}
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		if _, err := digest.Write(length[:]); err != nil {
			return nil, err
		}
		if _, err := digest.Write(value); err != nil {
			return nil, err
		}
	}
	return digest.Sum(nil), nil
}
