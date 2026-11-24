//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type privateDestinationBoundary struct {
	anchor   string
	identity SecureFileIdentity
}

func (boundary privateDestinationBoundary) ResolveSecureBoundary(path string, purpose secureBoundaryPurpose) (secureBoundarySelection, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return secureBoundarySelection{}, err
	}
	if !windowsPathWithin(boundary.anchor, clean) {
		return secureBoundarySelection{}, fmt.Errorf("%w: path outside private destination", ErrUnsafeSecurePath)
	}
	switch purpose {
	case secureBoundaryCQPrivate, secureBoundaryExternalDirectory, secureBoundaryExternalFile:
		return secureBoundarySelection{AnchorPath: boundary.anchor, PostAnchorPrivate: purpose == secureBoundaryCQPrivate}, nil
	default:
		return secureBoundarySelection{}, ErrSecureCapabilityUnavailable
	}
}

func (boundary privateDestinationBoundary) SecureBoundaryIdentity() (SecureFileIdentity, bool) {
	return boundary.identity, boundary.identity != (SecureFileIdentity{})
}

// NewPrivateDestinationFileSystem pins an owner-controlled ancestor of a private destination.
// Only descendants of that ancestor can be used by the returned filesystem.
func NewPrivateDestinationFileSystem(path string) (OSFileSystem, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil {
		return OSFileSystem{}, err
	}
	inspection := OSFileSystem{}
	anchor := filepath.Dir(clean)
	for {
		info, err := inspection.Lstat(anchor)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return OSFileSystem{}, fmt.Errorf("%w: private destination ancestor type", ErrUnsafeSecurePath)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return OSFileSystem{}, err
		}
		parent := filepath.Dir(anchor)
		if parent == anchor {
			return OSFileSystem{}, fmt.Errorf("%w: no private destination ancestor", ErrUnsafeSecurePath)
		}
		anchor = parent
	}
	directory, err := OpenOwnerControlledDirectory(inspection, anchor)
	if err != nil {
		return OSFileSystem{}, err
	}
	info, statErr := directory.Stat()
	closeErr := directory.Close()
	if statErr != nil || closeErr != nil {
		return OSFileSystem{}, errors.Join(statErr, closeErr)
	}
	identity, ok := inspection.FileIdentity(info)
	if !ok || identity == (SecureFileIdentity{}) {
		return OSFileSystem{}, fmt.Errorf("%w: private destination ancestor identity", ErrUnsafeSecurePath)
	}
	return OSFileSystem{secureBoundaryResolver: privateDestinationBoundary{anchor: anchor, identity: identity}}, nil
}

// WithPrivateDestination scopes the default filesystem to an owner-controlled
// destination without replacing an existing value-scoped boundary.
func (fsys OSFileSystem) WithPrivateDestination(path string) (OSFileSystem, error) {
	if fsys.secureBoundaryResolver != nil {
		return fsys, nil
	}
	return NewPrivateDestinationFileSystem(path)
}
