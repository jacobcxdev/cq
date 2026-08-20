//go:build !windows

package proxy

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

func (backend *ProductionLifecycleBackend) retainLifecycleDescription(directory fsutil.SecureDirectory, name string, mode LifecycleLockMode, file lifecycleDescriptionFile, afterLock func() error) (LifecycleLockDescription, error) {
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	identity, err := stableAuthorityLockIdentity(backend.inspector, info)
	if err != nil {
		return nil, err
	}
	operation := unix.LOCK_SH | unix.LOCK_NB
	if mode == LifecycleExclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	fd := int(file.Fd())
	if err := unix.Flock(fd, operation); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrLifecycleLockHeld
		}
		return nil, err
	}
	if afterLock != nil {
		if err := afterLock(); err != nil {
			_ = unix.Flock(fd, unix.LOCK_UN)
			return nil, err
		}
	}
	pathHandle, err := directory.OpenNoFollow(name)
	if err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	pathInfo, pathErr := pathHandle.Stat()
	pathCloseErr := pathHandle.Close()
	var pathIdentity fsutil.SecureFileIdentity
	var identityErr error
	if pathErr == nil {
		pathIdentity, identityErr = stableAuthorityLockIdentity(backend.inspector, pathInfo)
	}
	if pathErr != nil || pathCloseErr != nil || identityErr != nil || pathIdentity != identity {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("%w: lifecycle lock path identity", fsutil.ErrUnsafeSecurePath)
	}
	if err := validateAuthorityDirectory(backend.inspector, directory); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	descriptionRandom := make([]byte, 16)
	if _, err := io.ReadFull(backend.random, descriptionRandom); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	closeFile = false
	return &productionLifecycleDescription{file: file, fd: fd, identity: identity, id: hex.EncodeToString(descriptionRandom), mode: mode}, nil
}

type productionLifecycleDescription struct {
	mu       sync.Mutex
	file     lifecycleDescriptionFile
	fd       int
	identity fsutil.SecureFileIdentity
	id       string
	mode     LifecycleLockMode
	closed   bool
}

func (description *productionLifecycleDescription) Identity() fsutil.SecureFileIdentity {
	return description.identity
}
func (description *productionLifecycleDescription) DescriptionID() string { return description.id }
func (description *productionLifecycleDescription) Mode() LifecycleLockMode {
	description.mu.Lock()
	defer description.mu.Unlock()
	return description.mode
}
func (description *productionLifecycleDescription) DowngradeToShared() error {
	description.mu.Lock()
	defer description.mu.Unlock()
	if description.closed || description.mode != LifecycleExclusive {
		return ErrLifecycleLockMode
	}
	if err := unix.Flock(description.fd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return err
	}
	description.mode = LifecycleShared
	return nil
}
func (description *productionLifecycleDescription) Close() error {
	description.mu.Lock()
	defer description.mu.Unlock()
	if description.closed {
		return nil
	}
	description.closed = true
	return errors.Join(unix.Flock(description.fd, unix.LOCK_UN), description.file.Close())
}
