//go:build windows

package proxy

import (
	"crypto/sha256"
	"os"
)

func newRuntimePrivateSocketFiles() (*os.File, *os.File, error) {
	return nil, nil, ErrRuntimeRoleUnavailable
}

func RuntimeDescriptorIdentityDigest(file *os.File) ([sha256.Size]byte, error) {
	_ = file
	return [sha256.Size]byte{}, ErrRuntimeRoleUnavailable
}

func RuntimeLifecycleHolder(file *os.File, descriptionID string) (LifecycleHolderProof, error) {
	_ = file
	_ = descriptionID
	return LifecycleHolderProof{}, ErrRuntimeRoleUnavailable
}
