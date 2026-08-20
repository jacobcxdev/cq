//go:build windows

package proxy

import "crypto/sha256"

type runtimeExecutableIdentity struct {
	device uint64
	inode  uint64
}

func runtimeExecutableDigest(path string) ([sha256.Size]byte, runtimeExecutableIdentity, error) {
	_ = path
	return [sha256.Size]byte{}, runtimeExecutableIdentity{}, ErrRuntimeRoleUnavailable
}
