//go:build windows

package proxy

import (
	"github.com/jacobcxdev/cq/internal/fsutil"
)

func (backend *ProductionLifecycleBackend) retainLifecycleDescription(directory fsutil.SecureDirectory, name string, mode LifecycleLockMode, file lifecycleDescriptionFile, afterLock func() error) (LifecycleLockDescription, error) {
	_ = directory
	_ = name
	_ = mode
	_ = afterLock
	_ = file.Close()
	return nil, fsutil.ErrSecureCapabilityUnavailable
}
