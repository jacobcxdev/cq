package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	proxyResilienceKeyName     = "authority.key"
	proxyRoutingDirectoryName  = "routing"
	proxyDispatchDirectoryName = "dispatch"
	proxyModeDirectoryName     = "mode"
	proxyRoutingLockName       = "routing-selector.lock"
	proxyModeLockName          = "mode-selector.lock"
)

type ProxyResilienceStateOptions struct {
	FS              fsutil.FileSystem
	Root            string
	Random          io.Reader
	Now             func() time.Time
	SkipRuntimeMode bool
}

type ProxyResilienceState struct {
	Routing         *RoutingPolicyStore
	DispatchPermits *CallerDispatchPermitStore
	RuntimeMode     *DurableRuntimeModeEvidenceStore

	mu          sync.Mutex
	closed      bool
	routingDir  fsutil.SecureDirectory
	modeDir     fsutil.SecureDirectory
	routingLock *SelectorCASLock
	modeLock    *SelectorCASLock
	rescueKey   [sha256.Size]byte
}

// ProxyRescueState opens only rescue-mode authority. Corrupt normal routing or
// dispatch state must not make rescue control unavailable.
type ProxyRescueState struct {
	RuntimeMode *DurableRuntimeModeEvidenceStore
	mu          sync.Mutex
	closed      bool
	directory   fsutil.SecureDirectory
	lock        *SelectorCASLock
	rescueKey   [sha256.Size]byte
}

func OpenProxyRescueState(ctx context.Context, options ProxyResilienceStateOptions) (_ *ProxyRescueState, returnErr error) {
	if err := validateProxyResilienceStateOptions(ctx, options); err != nil {
		return nil, err
	}
	if err := fsutil.ValidateSecureDirectory(options.FS, options.Root); err != nil {
		return nil, err
	}
	opener, ok := options.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	inspector, ok := options.FS.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	root, err := opener.OpenSecureDirectory(options.Root)
	if err != nil {
		return nil, err
	}
	key, _, keyErr := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, root, proxyResilienceKeyName, sha256.Size+1)
	rootCloseErr := root.Close()
	if keyErr != nil || rootCloseErr != nil {
		zeroRuntimeBytes(key)
		return nil, errors.Join(keyErr, rootCloseErr)
	}
	defer zeroRuntimeBytes(key)
	if len(key) != sha256.Size {
		return nil, errors.New("proxy resilience authority key invalid")
	}
	if err := fsutil.ValidateSecureDirectory(options.FS, filepath.Join(options.Root, proxyModeDirectoryName)); err != nil {
		return nil, err
	}
	state := &ProxyRescueState{}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, state.Close())
		}
	}()
	derived, err := DeriveAuthorityKey(key, "cq/proxy-resilience/rescue-fairness/v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	copy(state.rescueKey[:], derived)
	zeroRuntimeBytes(derived)
	state.directory, err = opener.OpenSecureDirectory(filepath.Join(options.Root, proxyModeDirectoryName))
	if err != nil {
		return nil, err
	}
	state.lock, err = AcquireSelectorCASLock(inspector, state.directory, proxyModeLockName)
	if err != nil {
		return nil, err
	}
	modeKey, err := DeriveAuthorityKey(key, "cq/proxy-resilience/runtime-mode/v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	state.RuntimeMode, err = OpenRuntimeModeEvidenceStore(ctx, inspector, state.directory, NewAuthorityObjectPublisher(inspector, options.Random, state.lock), modeKey)
	zeroRuntimeBytes(modeKey)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (state *ProxyRescueState) RescueFairnessKey() [sha256.Size]byte {
	if state == nil {
		return [sha256.Size]byte{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return [sha256.Size]byte{}
	}
	return state.rescueKey
}

func (state *ProxyRescueState) Close() error {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil
	}
	state.closed = true
	zeroRuntimeBytes(state.rescueKey[:])
	var err error
	if state.lock != nil {
		err = errors.Join(err, state.lock.Close())
	}
	if state.directory != nil {
		err = errors.Join(err, state.directory.Close())
	}
	return err
}

func validateProxyResilienceStateOptions(ctx context.Context, options ProxyResilienceStateOptions) error {
	if ctx == nil || options.FS == nil || options.Random == nil || options.Now == nil {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	clean := filepath.Clean(options.Root)
	if options.Root == "" || !filepath.IsAbs(options.Root) || clean != options.Root || clean == string(filepath.Separator) {
		return fmt.Errorf("%w: proxy resilience root", fsutil.ErrUnsafeSecurePath)
	}
	return ctx.Err()
}

func InitialiseProxyResilienceState(ctx context.Context, options ProxyResilienceStateOptions) error {
	if err := validateProxyResilienceStateOptions(ctx, options); err != nil {
		return err
	}
	if err := fsutil.EnsureSecureDirectory(options.FS, options.Root); err != nil {
		return err
	}
	opener, ok := options.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	inspector, ok := options.FS.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	root, err := opener.OpenSecureDirectory(options.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	key, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, root, proxyResilienceKeyName, sha256.Size+1)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, sha256.Size)
		if _, err := io.ReadFull(options.Random, key); err != nil {
			return err
		}
		file, err := root.CreateExclusive(proxyResilienceKeyName, 0o600)
		if err != nil {
			zeroRuntimeBytes(key)
			return err
		}
		_, writeErr := file.Write(key)
		syncErr := file.Sync()
		closeErr := file.Close()
		zeroRuntimeBytes(key)
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return err
		}
		if err := root.Sync(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		defer zeroRuntimeBytes(key)
		if len(key) != sha256.Size {
			return errors.New("proxy resilience authority key invalid")
		}
	}
	directories := []string{proxyRoutingDirectoryName, proxyDispatchDirectoryName}
	if !options.SkipRuntimeMode {
		directories = append(directories, proxyModeDirectoryName)
	}
	for _, name := range directories {
		if err := fsutil.EnsureSecureDirectory(options.FS, filepath.Join(options.Root, name)); err != nil {
			return err
		}
	}
	for _, spec := range []struct{ directory, lock string }{
		{proxyRoutingDirectoryName, proxyRoutingLockName},
		{proxyModeDirectoryName, proxyModeLockName},
	} {
		directory, err := opener.OpenSecureDirectory(filepath.Join(options.Root, spec.directory))
		if err != nil {
			return err
		}
		lock, lockErr := AcquireSelectorCASLock(inspector, directory, spec.lock)
		closeErr := directory.Close()
		if lockErr != nil {
			return errors.Join(lockErr, closeErr)
		}
		if err := errors.Join(lock.Close(), closeErr); err != nil {
			return err
		}
	}
	return nil
}

func OpenProxyResilienceState(ctx context.Context, options ProxyResilienceStateOptions) (_ *ProxyResilienceState, returnErr error) {
	if err := validateProxyResilienceStateOptions(ctx, options); err != nil {
		return nil, err
	}
	if err := fsutil.ValidateSecureDirectory(options.FS, options.Root); err != nil {
		return nil, err
	}
	opener, ok := options.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	inspector, ok := options.FS.(fsutil.SecurePathInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	root, err := opener.OpenSecureDirectory(options.Root)
	if err != nil {
		return nil, err
	}
	key, _, keyErr := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, root, proxyResilienceKeyName, sha256.Size+1)
	rootCloseErr := root.Close()
	if keyErr != nil || rootCloseErr != nil {
		zeroRuntimeBytes(key)
		return nil, errors.Join(keyErr, rootCloseErr)
	}
	defer zeroRuntimeBytes(key)
	if len(key) != sha256.Size {
		return nil, errors.New("proxy resilience authority key invalid")
	}
	rescueKey, err := DeriveAuthorityKey(key, "cq/proxy-resilience/rescue-fairness/v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	defer zeroRuntimeBytes(rescueKey)
	for _, name := range []string{proxyRoutingDirectoryName, proxyDispatchDirectoryName, proxyModeDirectoryName} {
		if err := fsutil.ValidateSecureDirectory(options.FS, filepath.Join(options.Root, name)); err != nil {
			return nil, err
		}
	}
	state := &ProxyResilienceState{}
	copy(state.rescueKey[:], rescueKey)
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, state.Close())
		}
	}()
	state.routingDir, err = opener.OpenSecureDirectory(filepath.Join(options.Root, proxyRoutingDirectoryName))
	if err != nil {
		return nil, err
	}
	state.routingLock, err = AcquireSelectorCASLock(inspector, state.routingDir, proxyRoutingLockName)
	if err != nil {
		return nil, err
	}
	routingKey, err := DeriveAuthorityKey(key, "cq/proxy-resilience/routing-policy/v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	routingPublisher := NewAuthorityObjectPublisher(inspector, options.Random, state.routingLock)
	state.Routing, err = OpenRoutingPolicyStore(ctx, inspector, state.routingDir, routingPublisher, routingKey)
	zeroRuntimeBytes(routingKey)
	if err != nil {
		return nil, err
	}
	dispatchKey, err := DeriveAuthorityKey(key, "cq/proxy-resilience/dispatch-permit/v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	state.DispatchPermits, err = OpenCallerDispatchPermitStore(options.FS, filepath.Join(options.Root, proxyDispatchDirectoryName), dispatchKey, options.Now, options.Random)
	zeroRuntimeBytes(dispatchKey)
	if err != nil {
		return nil, err
	}
	if !options.SkipRuntimeMode {
		state.modeDir, err = opener.OpenSecureDirectory(filepath.Join(options.Root, proxyModeDirectoryName))
		if err != nil {
			return nil, err
		}
		state.modeLock, err = AcquireSelectorCASLock(inspector, state.modeDir, proxyModeLockName)
		if err != nil {
			return nil, err
		}
		modeKey, err := DeriveAuthorityKey(key, "cq/proxy-resilience/runtime-mode/v1", sha256.Size)
		if err != nil {
			return nil, err
		}
		modePublisher := NewAuthorityObjectPublisher(inspector, options.Random, state.modeLock)
		state.RuntimeMode, err = OpenRuntimeModeEvidenceStore(ctx, inspector, state.modeDir, modePublisher, modeKey)
		zeroRuntimeBytes(modeKey)
		if err != nil {
			return nil, err
		}
	}
	return state, nil
}

func (state *ProxyResilienceState) RescueFairnessKey() [sha256.Size]byte {
	if state == nil {
		return [sha256.Size]byte{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return [sha256.Size]byte{}
	}
	return state.rescueKey
}

func (state *ProxyResilienceState) Close() error {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil
	}
	state.closed = true
	zeroRuntimeBytes(state.rescueKey[:])
	var err error
	if state.DispatchPermits != nil {
		err = errors.Join(err, state.DispatchPermits.Close())
	}
	if state.routingLock != nil {
		err = errors.Join(err, state.routingLock.Close())
	}
	if state.modeLock != nil {
		err = errors.Join(err, state.modeLock.Close())
	}
	if state.routingDir != nil {
		err = errors.Join(err, state.routingDir.Close())
	}
	if state.modeDir != nil {
		err = errors.Join(err, state.modeDir.Close())
	}
	return err
}
