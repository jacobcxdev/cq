package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const authorityObjectIdentityDomain = "cq/stable-object/content/v1\x00"

var (
	ErrAuthorityPriorMismatch = errors.New("authority selector prior mismatch")
	ErrAuthorityPathGrammar   = errors.New("invalid authority path grammar")
	ErrAuthorityCASCapability = errors.New("authority selector CAS capability required")
)

type StableObjectIdentity struct {
	File   fsutil.SecureFileIdentity
	Size   int64
	Digest string
}

type DurableObjectPublisher interface {
	PublishImmutable(context.Context, fsutil.SecureDirectory, string, []byte, fs.FileMode) (StableObjectIdentity, error)
	ReplaceSelectorExactPrior(context.Context, fsutil.SecureDirectory, string, *StableObjectIdentity, []byte) (StableObjectIdentity, error)
}

type AuthorityObjectPublisher struct {
	inspector fsutil.SecurePathInspector
	random    io.Reader
	cas       selectorCASCapability
}

type selectorCASCapability interface {
	AcquireSelectorCAS(context.Context, fsutil.SecurePathInspector, fsutil.SecureDirectory) (func() error, error)
	validateSelectorCAS(fsutil.SecureDirectory) error
	selectorCASCapability()
}

func NewAuthorityObjectPublisher(inspector fsutil.SecurePathInspector, random io.Reader, capabilities ...selectorCASCapability) *AuthorityObjectPublisher {
	var capability selectorCASCapability
	if len(capabilities) == 1 {
		capability = capabilities[0]
	}
	return &AuthorityObjectPublisher{inspector: inspector, random: random, cas: capability}
}

func (publisher *AuthorityObjectPublisher) PublishImmutable(ctx context.Context, directory fsutil.SecureDirectory, name string, body []byte, mode fs.FileMode) (StableObjectIdentity, error) {
	if mode.Perm() != 0o600 || mode&^fs.ModePerm != 0 {
		return StableObjectIdentity{}, fmt.Errorf("%w: immutable object mode", ErrAuthorityPathGrammar)
	}
	return publisher.publish(ctx, directory, name, nil, body, true, nil)
}

func (publisher *AuthorityObjectPublisher) ReplaceSelectorExactPrior(ctx context.Context, directory fsutil.SecureDirectory, name string, prior *StableObjectIdentity, body []byte) (StableObjectIdentity, error) {
	if publisher == nil || publisher.cas == nil {
		return StableObjectIdentity{}, ErrAuthorityCASCapability
	}
	release, err := publisher.cas.AcquireSelectorCAS(ctx, publisher.inspector, directory)
	if err != nil {
		return StableObjectIdentity{}, fmt.Errorf("acquire selector CAS: %w", err)
	}
	identity, publishErr := publisher.publish(ctx, directory, name, prior, body, prior == nil, func() error {
		return publisher.cas.validateSelectorCAS(directory)
	})
	if publishErr != nil {
		publishErr = fmt.Errorf("publish selector: %w", publishErr)
	}
	return identity, errors.Join(publishErr, release())
}

// SelectorCASLock owns the exact exclusive lock description used to serialise
// selector replacement. Copies share one state and therefore one gate.
type SelectorCASLock struct {
	state *selectorCASLockState
}

type selectorCASLockState struct {
	inspector         fsutil.SecurePathInspector
	directoryIdentity fsutil.SecureFileIdentity
	name              string
	identity          fsutil.SecureFileIdentity
	lock              fsutil.ExclusiveLock
	token             chan struct{}
	closed            bool
}

func AcquireSelectorCASLock(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string) (*SelectorCASLock, error) {
	if inspector == nil || directory == nil {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := validateAuthorityEntryName(name); err != nil {
		return nil, err
	}
	directoryIdentity, err := authorityDirectoryIdentity(inspector, directory)
	if err != nil {
		return nil, err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, name)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	identity, err := stableAuthorityLockIdentity(inspector, info)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	state := &selectorCASLockState{inspector: inspector, directoryIdentity: directoryIdentity, name: name, identity: identity, lock: lock, token: make(chan struct{}, 1)}
	state.token <- struct{}{}
	return &SelectorCASLock{state: state}, nil
}

func (*SelectorCASLock) selectorCASCapability() {}

func (lock *SelectorCASLock) sharesDescription(other *SelectorCASLock) bool {
	return lock != nil && other != nil && lock.state != nil && lock.state == other.state
}

func (lock *SelectorCASLock) AcquireSelectorCAS(ctx context.Context, _ fsutil.SecurePathInspector, directory fsutil.SecureDirectory) (func() error, error) {
	if lock == nil || lock.state == nil || directory == nil {
		return nil, ErrAuthorityCASCapability
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock.state.token:
	}
	var releaseOnce sync.Once
	release := func() error {
		releaseOnce.Do(func() { lock.state.token <- struct{}{} })
		return nil
	}
	if lock.state.closed {
		_ = release()
		return nil, ErrAuthorityCASCapability
	}
	if err := lock.validateSelectorCAS(directory); err != nil {
		_ = release()
		return nil, err
	}
	return release, nil
}

func (lock *SelectorCASLock) validateSelectorCAS(directory fsutil.SecureDirectory) error {
	if lock == nil || lock.state == nil || directory == nil {
		return fmt.Errorf("%w: missing lock state", ErrAuthorityCASCapability)
	}
	if lock.state.closed {
		return fmt.Errorf("%w: lock closed", ErrAuthorityCASCapability)
	}
	directoryIdentity, err := authorityDirectoryIdentity(lock.state.inspector, directory)
	if err != nil {
		return fmt.Errorf("%w: directory validation: %v", ErrAuthorityCASCapability, err)
	}
	if !fsutil.SameSecureObject(directoryIdentity, lock.state.directoryIdentity) {
		return fmt.Errorf("%w: directory identity changed", ErrAuthorityCASCapability)
	}
	if err := validateSelectorCASLockPath(lock.state.inspector, directory, lock.state.name, lock.state.identity); err != nil {
		return fmt.Errorf("%w: lock path identity", ErrAuthorityCASCapability)
	}
	return nil
}

func validateSelectorCASLockPath(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string, expected fsutil.SecureFileIdentity) error {
	opened, err := directory.OpenNoFollow(name)
	if err != nil {
		return err
	}
	info, statErr := opened.Stat()
	closeErr := opened.Close()
	if statErr != nil {
		return statErr
	}
	if closeErr != nil {
		return closeErr
	}
	identity, err := stableAuthorityLockIdentity(inspector, info)
	if err != nil {
		return err
	}
	if identity != expected {
		return fsutil.ErrUnsafeSecurePath
	}
	return nil
}

func (lock *SelectorCASLock) Close() error {
	if lock == nil || lock.state == nil {
		return nil
	}
	<-lock.state.token
	defer func() { lock.state.token <- struct{}{} }()
	if lock.state.closed {
		return nil
	}
	lock.state.closed = true
	return lock.state.lock.Close()
}

func (publisher *AuthorityObjectPublisher) publish(ctx context.Context, directory fsutil.SecureDirectory, name string, prior *StableObjectIdentity, body []byte, noReplace bool, validateCAS func() error) (result StableObjectIdentity, resultErr error) {
	if publisher == nil || publisher.inspector == nil || publisher.random == nil || directory == nil {
		return StableObjectIdentity{}, fsutil.ErrSecureCapabilityUnavailable
	}
	renamer, ok := directory.(fsutil.IdentityBoundRenamer)
	if !ok {
		return StableObjectIdentity{}, fsutil.ErrSecureCapabilityUnavailable
	}
	remover, ok := directory.(fsutil.IdentityBoundRemover)
	if !ok {
		return StableObjectIdentity{}, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := validateAuthorityEntryName(name); err != nil {
		return StableObjectIdentity{}, err
	}
	if err := ctx.Err(); err != nil {
		return StableObjectIdentity{}, err
	}
	if err := validateAuthorityDirectory(publisher.inspector, directory); err != nil {
		return StableObjectIdentity{}, err
	}
	if prior != nil {
		if err := publisher.requirePrior(directory, name, *prior); err != nil {
			return StableObjectIdentity{}, err
		}
	} else if noReplace {
		if err := requireAuthorityEntryAbsent(directory, name); err != nil {
			return StableObjectIdentity{}, err
		}
	}

	temporaryName, temporary, err := publisher.createTemporary(directory, name)
	if err != nil {
		return StableObjectIdentity{}, err
	}
	temporaryInspector, ok := temporary.(fsutil.DurableFileInspector)
	if !ok {
		_ = temporary.Close()
		return StableObjectIdentity{}, fsutil.ErrSecureCapabilityUnavailable
	}
	createdInfo, err := temporaryInspector.Stat()
	if err != nil {
		_ = temporary.Close()
		return StableObjectIdentity{}, fmt.Errorf("stat authority temporary: %w", err)
	}
	createdIdentity, err := stableAuthorityLockIdentity(publisher.inspector, createdInfo)
	if err != nil {
		_ = temporary.Close()
		return StableObjectIdentity{}, err
	}
	removeTemporary := true
	defer func() {
		if !removeTemporary {
			return
		}
		cleanupErr := remover.RemoveChecked(temporaryName, createdIdentity)
		if cleanupErr == nil || errors.Is(cleanupErr, os.ErrNotExist) {
			if cleanupErr == nil {
				resultErr = errors.Join(resultErr, directory.Sync())
			}
			return
		}
		result = StableObjectIdentity{}
		resultErr = errors.Join(resultErr, fmt.Errorf("clean authority temporary: %w", cleanupErr))
	}()
	if err := writeAuthorityBody(ctx, temporary, body); err != nil {
		_ = temporary.Close()
		return StableObjectIdentity{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return StableObjectIdentity{}, fmt.Errorf("sync authority temporary: %w", err)
	}
	temporaryInfo, err := temporaryInspector.Stat()
	if err != nil {
		_ = temporary.Close()
		return StableObjectIdentity{}, fmt.Errorf("stat authority temporary: %w", err)
	}
	temporaryIdentity, err := stableAuthorityIdentity(publisher.inspector, temporaryInfo, body)
	if err != nil {
		_ = temporary.Close()
		return StableObjectIdentity{}, err
	}
	if !fsutil.SameSecureObject(temporaryIdentity.File, createdIdentity) {
		_ = temporary.Close()
		return StableObjectIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	if err := temporary.Close(); err != nil {
		return StableObjectIdentity{}, fmt.Errorf("close authority temporary: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return StableObjectIdentity{}, err
	}
	if err := validateAuthorityDirectory(publisher.inspector, directory); err != nil {
		return StableObjectIdentity{}, err
	}
	temporaryBody, temporaryFileIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(publisher.inspector, directory, temporaryName, int64(len(body))+1)
	if err != nil {
		return StableObjectIdentity{}, fmt.Errorf("reopen authority temporary: %w", err)
	}
	if !fsutil.SameSecureObject(temporaryFileIdentity, temporaryIdentity.File) || !bytes.Equal(temporaryBody, body) {
		return StableObjectIdentity{}, fmt.Errorf("%w: temporary identity or content drift", fsutil.ErrUnsafeSecurePath)
	}
	if prior != nil {
		if err := publisher.requirePrior(directory, name, *prior); err != nil {
			return StableObjectIdentity{}, err
		}
	} else if noReplace {
		if err := requireAuthorityEntryAbsent(directory, name); err != nil {
			return StableObjectIdentity{}, err
		}
	}
	if validateCAS != nil {
		if err := validateCAS(); err != nil {
			return StableObjectIdentity{}, err
		}
	}
	if noReplace {
		err = renamer.RenameNoReplaceChecked(temporaryName, name, temporaryIdentity.File)
	} else {
		err = renamer.RenameChecked(temporaryName, name, temporaryIdentity.File)
	}
	if err != nil {
		return StableObjectIdentity{}, fmt.Errorf("publish authority object: %w", err)
	}
	removeTemporary = false
	if err := directory.Sync(); err != nil {
		return StableObjectIdentity{}, fmt.Errorf("sync authority directory: %w", err)
	}
	installed, installedIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(publisher.inspector, directory, name, int64(len(body))+1)
	if err != nil {
		return StableObjectIdentity{}, fmt.Errorf("reopen authority object: %w", err)
	}
	if !fsutil.SameSecureObject(installedIdentity, temporaryIdentity.File) || !bytes.Equal(installed, body) {
		return StableObjectIdentity{}, fmt.Errorf("%w: installed identity or content drift", fsutil.ErrUnsafeSecurePath)
	}
	if err := validateAuthorityDirectory(publisher.inspector, directory); err != nil {
		return StableObjectIdentity{}, err
	}
	return temporaryIdentity, nil
}

func (publisher *AuthorityObjectPublisher) createTemporary(directory fsutil.SecureDirectory, name string) (string, fsutil.DurableFile, error) {
	for attempt := 0; attempt < 8; attempt++ {
		random := make([]byte, 16)
		if _, err := io.ReadFull(publisher.random, random); err != nil {
			return "", nil, fmt.Errorf("sample authority temporary name: %w", err)
		}
		temporaryName := "." + name + "." + hex.EncodeToString(random) + ".tmp"
		file, err := directory.CreateExclusive(temporaryName, 0o600)
		if err == nil {
			return temporaryName, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("create authority temporary: %w", err)
		}
	}
	return "", nil, fmt.Errorf("create authority temporary: %w", os.ErrExist)
}

func (publisher *AuthorityObjectPublisher) requirePrior(directory fsutil.SecureDirectory, name string, prior StableObjectIdentity) error {
	if prior.Size < 0 || prior.Digest == "" || prior.File.Links != 1 {
		return ErrAuthorityPriorMismatch
	}
	body, fileIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(publisher.inspector, directory, name, prior.Size+1)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthorityPriorMismatch, err)
	}
	actual, err := stableAuthorityIdentityFromParts(fileIdentity, int64(len(body)), body)
	if err != nil || actual != prior {
		return ErrAuthorityPriorMismatch
	}
	return nil
}

func validateAuthorityEntryName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") || len(name) > 217 {
		return fmt.Errorf("%w: %q", ErrAuthorityPathGrammar, name)
	}
	return nil
}

func validateAuthorityDirectory(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory) error {
	_, err := authorityDirectoryIdentity(inspector, directory)
	return err
}

func authorityDirectoryIdentity(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory) (fsutil.SecureFileIdentity, error) {
	info, err := directory.Stat()
	if err != nil {
		return fsutil.SecureFileIdentity{}, fmt.Errorf("stat authority directory: %w", err)
	}
	identity, identityOK := inspector.FileIdentity(info)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || fsutil.ValidateSecureOwner(inspector, info) != nil || !identityOK {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	return stableDirectoryObjectIdentity(identity), nil
}

// Directory link counts may change when this transaction creates or removes
// entries. Device, inode, and file ID remain stable object identity.
func stableDirectoryObjectIdentity(identity fsutil.SecureFileIdentity) fsutil.SecureFileIdentity {
	identity.Links = 0
	return identity
}

func requireAuthorityEntryAbsent(directory fsutil.SecureDirectory, name string) error {
	file, err := directory.OpenNoFollow(name)
	if err == nil {
		_ = file.Close()
		return os.ErrExist
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func stableAuthorityIdentity(inspector fsutil.SecurePathInspector, info os.FileInfo, body []byte) (StableObjectIdentity, error) {
	fileIdentity, identityOK := inspector.FileIdentity(info)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || fsutil.ValidateSecureOwner(inspector, info) != nil || !identityOK || fileIdentity.Links != 1 {
		return StableObjectIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	return stableAuthorityIdentityFromParts(fileIdentity, info.Size(), body)
}

func stableAuthorityLockIdentity(inspector fsutil.SecurePathInspector, info os.FileInfo) (fsutil.SecureFileIdentity, error) {
	identity, identityOK := inspector.FileIdentity(info)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || fsutil.ValidateSecureOwner(inspector, info) != nil || !identityOK || identity.Links != 1 {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	return identity, nil
}

func stableAuthorityIdentityFromParts(fileIdentity fsutil.SecureFileIdentity, size int64, body []byte) (StableObjectIdentity, error) {
	digest, err := FramedSHA256Hex(authorityObjectIdentityDomain, body)
	if err != nil {
		return StableObjectIdentity{}, err
	}
	return StableObjectIdentity{File: fileIdentity, Size: size, Digest: digest}, nil
}

func writeAuthorityBody(ctx context.Context, destination io.Writer, body []byte) error {
	for len(body) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := destination.Write(body)
		if err != nil {
			return fmt.Errorf("write authority temporary: %w", err)
		}
		if written <= 0 || written > len(body) {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}
