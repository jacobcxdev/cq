//go:build !windows

package proxy

import (
	"crypto/sha256"
	"os"
	"strconv"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

func newRuntimePrivateSocketFiles() (*os.File, *os.File, error) {
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(descriptors[0])
	unix.CloseOnExec(descriptors[1])
	firstFile := os.NewFile(uintptr(descriptors[0]), "runtime-supervisor-control")
	secondFile := os.NewFile(uintptr(descriptors[1]), "runtime-worker-control")
	return firstFile, secondFile, nil
}

func RuntimeDescriptorIdentityDigest(file *os.File) ([sha256.Size]byte, error) {
	if file == nil {
		return [sha256.Size]byte{}, ErrRuntimeRoleManifest
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return [sha256.Size]byte{}, err
	}
	canonical := "cq/runtime-descriptor-identity/v1\x00" + strconv.FormatUint(uint64(stat.Dev), 10) + "\x00" + strconv.FormatUint(uint64(stat.Ino), 10) + "\x00" + strconv.FormatUint(uint64(stat.Nlink), 10)
	return sha256.Sum256([]byte(canonical)), nil
}

func RuntimeLifecycleHolder(file *os.File, descriptionID string) (LifecycleHolderProof, error) {
	if file == nil || descriptionID == "" {
		return LifecycleHolderProof{}, ErrLifecycleHolderConflict
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return LifecycleHolderProof{}, err
	}
	if stat.Nlink != 1 {
		return LifecycleHolderProof{}, ErrLifecycleHolderConflict
	}
	return LifecycleHolderProof{
		LockIdentity:  fsutil.SecureFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Links: uint64(stat.Nlink)},
		DescriptionID: descriptionID,
		Mode:          LifecycleShared,
	}, nil
}
