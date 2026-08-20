package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

var (
	ErrProxyInstanceMismatch      = errors.New("proxy instance identity mismatch")
	ErrProxyInstanceRoleConfusion = errors.New("primary and candidate instance roots conflict")
)

type ProxyInstanceRole string

const (
	ProxyInstancePrimary   ProxyInstanceRole = "primary"
	ProxyInstanceCandidate ProxyInstanceRole = "candidate"
)

type ProxyInstanceIdentity struct {
	ProxyInstanceID string
	Role            ProxyInstanceRole
}

type ProxyInstanceIdentityReader interface {
	ReadProxyInstanceIdentity(context.Context, fsutil.SecureDirectory) (ProxyInstanceIdentity, error)
}

type InstanceFileSystem interface {
	fsutil.SecureDirectoryOpener
	fsutil.SecurePathInspector
}

type instanceContextConfig struct {
	filesystem          InstanceFileSystem
	identityReader      ProxyInstanceIdentityReader
	expectedRole        ProxyInstanceRole
	reservedPrimaryRoot string
}

type InstanceContextOption func(*instanceContextConfig)

func WithInstanceFileSystem(filesystem InstanceFileSystem) InstanceContextOption {
	return func(config *instanceContextConfig) { config.filesystem = filesystem }
}

func WithInstanceIdentityReader(reader ProxyInstanceIdentityReader) InstanceContextOption {
	return func(config *instanceContextConfig) { config.identityReader = reader }
}

func WithExpectedInstanceRole(role ProxyInstanceRole) InstanceContextOption {
	return func(config *instanceContextConfig) { config.expectedRole = role }
}

func WithReservedPrimaryRoot(root string) InstanceContextOption {
	return func(config *instanceContextConfig) { config.reservedPrimaryRoot = root }
}

type ProxyInstanceContext struct {
	Root            string
	Parent          string
	Basename        string
	Identity        ProxyInstanceIdentity
	ParentIdentity  fsutil.SecureFileIdentity
	RootIdentity    fsutil.SecureFileIdentity
	ParentDirectory fsutil.SecureDirectory
	RootDirectory   fsutil.SecureDirectory

	mu     sync.Mutex
	closed bool
}

func OpenInstanceContext(root, expectedID string, options ...InstanceContextOption) (*ProxyInstanceContext, error) {
	config := instanceContextConfig{filesystem: fsutil.OSFileSystem{}}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	if config.filesystem == nil || config.identityReader == nil {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := validateInstanceRoot(root); err != nil {
		return nil, err
	}
	if err := validateProxyInstanceID(expectedID); err != nil {
		return nil, err
	}
	ctx := context.Background()
	instance, err := openInstanceCapabilities(config.filesystem, root)
	if err != nil {
		return nil, err
	}
	identity, err := config.identityReader.ReadProxyInstanceIdentity(ctx, instance.RootDirectory)
	if err != nil {
		_ = instance.Close()
		return nil, err
	}
	if identity.ProxyInstanceID != expectedID {
		_ = instance.Close()
		return nil, ErrProxyInstanceMismatch
	}
	if identity.Role != ProxyInstancePrimary && identity.Role != ProxyInstanceCandidate {
		_ = instance.Close()
		return nil, ErrProxyInstanceMismatch
	}
	if config.expectedRole != "" && identity.Role != config.expectedRole {
		_ = instance.Close()
		return nil, ErrProxyInstanceRoleConfusion
	}
	instance.Identity = identity
	if identity.Role == ProxyInstanceCandidate && config.reservedPrimaryRoot != "" {
		reservedParent, reservedParentIdentity, reservedBasename, err := openInstanceLocator(config.filesystem, config.reservedPrimaryRoot)
		if err != nil {
			_ = instance.Close()
			return nil, err
		}
		conflict := instance.Basename == reservedBasename && instance.ParentIdentity == reservedParentIdentity
		_ = reservedParent.Close()
		if conflict {
			_ = instance.Close()
			return nil, ErrProxyInstanceRoleConfusion
		}
	}
	return instance, nil
}

func openInstanceCapabilities(filesystem InstanceFileSystem, root string) (*ProxyInstanceContext, error) {
	parent, parentIdentity, basename, err := openInstanceLocator(filesystem, root)
	if err != nil {
		return nil, err
	}
	retainedParent, ok := parent.(fsutil.DurableDirectory)
	if !ok {
		_ = parent.Close()
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	openedRoot, err := retainedParent.OpenDirectory(basename)
	if err != nil {
		_ = parent.Close()
		return nil, fmt.Errorf("open proxy instance root: %w", err)
	}
	rootDirectory, ok := openedRoot.(fsutil.SecureDirectory)
	if !ok {
		_ = openedRoot.Close()
		_ = parent.Close()
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	rootIdentity, err := validateInstanceDirectory(filesystem, rootDirectory)
	if err != nil {
		_ = rootDirectory.Close()
		_ = parent.Close()
		return nil, err
	}
	parentIdentityAfter, err := validateInstanceDirectory(filesystem, parent)
	if err != nil || parentIdentityAfter != parentIdentity {
		_ = rootDirectory.Close()
		_ = parent.Close()
		return nil, fmt.Errorf("%w: parent identity drift", fsutil.ErrUnsafeSecurePath)
	}
	return &ProxyInstanceContext{
		Root:            root,
		Parent:          filepath.Dir(root),
		Basename:        basename,
		ParentIdentity:  parentIdentity,
		RootIdentity:    rootIdentity,
		ParentDirectory: parent,
		RootDirectory:   rootDirectory,
	}, nil
}

func openInstanceLocator(filesystem InstanceFileSystem, root string) (fsutil.SecureDirectory, fsutil.SecureFileIdentity, string, error) {
	if err := validateInstanceRoot(root); err != nil {
		return nil, fsutil.SecureFileIdentity{}, "", err
	}
	parentPath := filepath.Dir(root)
	parent, err := filesystem.OpenSecureDirectory(parentPath)
	if err != nil {
		return nil, fsutil.SecureFileIdentity{}, "", fmt.Errorf("open proxy instance parent: %w", err)
	}
	parentIdentity, err := validateInstanceDirectory(filesystem, parent)
	if err != nil {
		_ = parent.Close()
		return nil, fsutil.SecureFileIdentity{}, "", err
	}
	reopenedParent, err := filesystem.OpenSecureDirectory(parentPath)
	if err != nil {
		_ = parent.Close()
		return nil, fsutil.SecureFileIdentity{}, "", fmt.Errorf("reopen proxy instance parent: %w", err)
	}
	reopenedParentIdentity, reopenErr := validateInstanceDirectory(filesystem, reopenedParent)
	closeErr := reopenedParent.Close()
	if reopenErr != nil || closeErr != nil || reopenedParentIdentity != parentIdentity {
		_ = parent.Close()
		return nil, fsutil.SecureFileIdentity{}, "", fmt.Errorf("%w: replaced proxy instance parent", fsutil.ErrUnsafeSecurePath)
	}
	return parent, parentIdentity, filepath.Base(root), nil
}

func (instance *ProxyInstanceContext) Close() error {
	instance.mu.Lock()
	defer instance.mu.Unlock()
	if instance.closed {
		return nil
	}
	instance.closed = true
	return errors.Join(instance.RootDirectory.Close(), instance.ParentDirectory.Close())
}

func validateInstanceRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) || strings.ContainsRune(root, '\x00') {
		return fmt.Errorf("%w: instance root", ErrAuthorityPathGrammar)
	}
	return validateAuthorityEntryName(filepath.Base(root))
}

func validateProxyInstanceID(identifier string) error {
	if identifier == "" || len(identifier) > 256 || strings.ContainsAny(identifier, "/\\\x00") || identifier == "." || identifier == ".." {
		return fmt.Errorf("%w: proxy instance ID", ErrAuthorityPathGrammar)
	}
	return nil
}

func validateInstanceDirectory(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory) (fsutil.SecureFileIdentity, error) {
	info, err := directory.Stat()
	if err != nil {
		return fsutil.SecureFileIdentity{}, err
	}
	owner, ownerOK := inspector.FileOwnerUID(info)
	identity, identityOK := inspector.FileIdentity(info)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !ownerOK || owner != inspector.EffectiveUID() || !identityOK {
		return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	return identity, nil
}
