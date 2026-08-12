//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

func credentialEndpointMaintenanceJournalPath(path string) string { return path + ".maintenance.json" }

func credentialEndpointMaintenanceRollbackPath(path string) string {
	return path + ".maintenance.rollback.json"
}

func legacyCredentialEndpointIdentityOwnerIsCurrent(uid uint64) bool {
	return uid == uint64(os.Geteuid())
}

func InspectLegacyCredentialEndpoint(ctx context.Context, path string) (LegacyCredentialEndpointSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	directoryPath, finalName, err := validateCredentialEndpointPath(path)
	if err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.ValidateSecureDirectory(fsys, directoryPath); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	directory, err := fsys.OpenSecureDirectory(directoryPath)
	if err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	defer directory.Close()
	directoryFD, err := openCredentialEndpointDirectory(directoryPath)
	if err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	defer unix.Close(directoryFD)

	directoryProof, err := inspectLegacyCredentialDirectory(fsys, directory, directoryFD, directoryPath)
	if err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	artifactNames := []string{
		filepath.Base(credentialEndpointLockPath(path)),
		filepath.Base(credentialEndpointSidecarPath(path)),
		filepath.Base(credentialEndpointMaintenanceJournalPath(path)),
		filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
	}
	if err := requireLegacyCredentialArtifactsAbsent(directoryFD, artifactNames); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	socketProof, err := inspectLegacyCredentialSocket(directoryFD, finalName)
	if err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}

	dialer := net.Dialer{Timeout: credentialEndpointDialTimeout}
	conn, probeErr := dialer.DialContext(ctx, "unix", path)
	if conn != nil {
		_ = conn.Close()
		return LegacyCredentialEndpointSnapshot{}, ErrLegacyCredentialEndpointNotRefused
	}
	if !errors.Is(probeErr, syscall.ECONNREFUSED) {
		return LegacyCredentialEndpointSnapshot{}, errors.Join(ErrLegacyCredentialEndpointNotRefused, probeErr)
	}
	if err := ctx.Err(); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}

	finalDirectoryProof, err := inspectLegacyCredentialDirectory(fsys, directory, directoryFD, directoryPath)
	if err != nil || finalDirectoryProof != directoryProof {
		return LegacyCredentialEndpointSnapshot{}, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := requireLegacyCredentialArtifactsAbsent(directoryFD, artifactNames); err != nil {
		return LegacyCredentialEndpointSnapshot{}, err
	}
	finalSocketProof, err := inspectLegacyCredentialSocket(directoryFD, finalName)
	if err != nil || finalSocketProof != socketProof {
		return LegacyCredentialEndpointSnapshot{}, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return LegacyCredentialEndpointSnapshot{
		Version:   legacyCredentialEndpointSnapshotVersion,
		Path:      path,
		State:     LegacyCredentialEndpointRefused,
		Directory: directoryProof,
		Socket:    socketProof,
	}, nil
}

func inspectLegacyCredentialDirectory(fsys fsutil.OSFileSystem, directory fsutil.SecureDirectory, directoryFD int, path string) (LegacyCredentialEndpointIdentity, error) {
	pathInfo, err := fsys.Lstat(path)
	if err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	descriptorInfo, err := directory.Stat()
	if err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	pathIdentity, pathOK := fsys.FileIdentity(pathInfo)
	descriptorIdentity, descriptorOK := fsys.FileIdentity(descriptorInfo)
	if !pathOK || !descriptorOK || pathIdentity.Device != descriptorIdentity.Device || pathIdentity.Inode != descriptorIdentity.Inode {
		return LegacyCredentialEndpointIdentity{}, ErrCredentialEndpointIdentityChanged
	}
	var stat unix.Stat_t
	if err := unix.Fstat(directoryFD, &stat); err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	proof := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "directory", Mode: uint32(stat.Mode) & 0o777,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || proof.UID != fsys.EffectiveUID() || proof.Mode != 0o700 || proof.Device != pathIdentity.Device || proof.Inode != pathIdentity.Inode {
		return LegacyCredentialEndpointIdentity{}, fmt.Errorf("unsafe legacy credential endpoint directory")
	}
	return proof, nil
}

func inspectLegacyCredentialSocket(directoryFD int, name string) (LegacyCredentialEndpointIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return LegacyCredentialEndpointIdentity{}, errors.Join(ErrLegacyCredentialEndpointNotRefused, err)
	}
	proof := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "socket", Mode: uint32(stat.Mode) & 0o777,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || proof.UID != uint64(os.Geteuid()) || proof.Mode != 0o600 || proof.Links != 1 {
		return LegacyCredentialEndpointIdentity{}, ErrLegacyCredentialEndpointNotRefused
	}
	return proof, nil
}

func requireLegacyCredentialArtifactsAbsent(directoryFD int, names []string) error {
	for _, name := range names {
		var stat unix.Stat_t
		err := unix.Fstatat(directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: %s exists", ErrLegacyCredentialEndpointArtifacts, name)
	}
	return nil
}
