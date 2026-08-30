package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidateInheritedExclusiveLockFile proves file is the strict named lock
// descriptor and carries the capability exported by its exclusive lock owner.
func ValidateInheritedExclusiveLockFile(fsys FileSystem, path string, file *os.File) error {
	if fsys == nil || file == nil || path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrSecureCapabilityUnavailable
	}
	inspector, ok := fsys.(SecurePathInspector)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(SecureDirectoryOpener)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	inheritedInfo, err := inspectInheritedExclusiveLockFile(file)
	if err != nil {
		return fmt.Errorf("inspect inherited lock descriptor: %w", err)
	}
	if err := validateSecureRegularInfo(inspector, inheritedInfo); err != nil {
		return fmt.Errorf("validate inherited lock descriptor: %w", err)
	}
	if err := validateInheritedExclusiveLockHeld(file); err != nil {
		return fmt.Errorf("validate inherited lock ownership: %w", err)
	}
	expected, ok := inspector.FileIdentity(inheritedInfo)
	if !ok {
		return fmt.Errorf("%w: inherited lock identity", ErrUnsafeSecurePath)
	}
	directoryPath := filepath.Dir(path)
	if err := ValidateSecureDirectory(fsys, directoryPath); err != nil {
		return fmt.Errorf("validate inherited lock directory: %w", err)
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return fmt.Errorf("open inherited lock directory: %w", err)
	}
	defer directory.Close()
	if err := ValidateExclusiveLockHeldInDirectory(inspector, directory, filepath.Base(path), expected); err != nil {
		return fmt.Errorf("validate inherited installer lock: %w", err)
	}
	return nil
}
