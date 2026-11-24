//go:build windows

package codex

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const maxManagedCredentialBytes = 1 << 20

func (s *ManagedStore) nativeReadManaged(path string) ([]byte, bool, error) {
	destination, ok := s.FS.(interface {
		WithPrivateDestination(string) (fsutil.OSFileSystem, error)
	})
	if !ok {
		return nil, false, nil
	}
	dir := filepath.Join(s.Home, ".codex", "accounts")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.EqualFold(filepath.Dir(path), dir) {
		return nil, true, fsutil.ErrUnsafeSecurePath
	}
	scoped, err := destination.WithPrivateDestination(dir)
	if err != nil {
		return nil, true, err
	}
	directory, err := fsutil.OpenOwnerControlledDirectory(scoped, dir)
	if err != nil {
		return nil, true, err
	}
	data, _, readErr := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(scoped, directory, dir, filepath.Base(path), maxManagedCredentialBytes)
	return data, true, errors.Join(readErr, directory.Close())
}

func (s *ManagedStore) nativeDurableReplace(path string, data []byte) (handled bool, result error) {
	destination, ok := s.FS.(interface {
		WithPrivateDestination(string) (fsutil.OSFileSystem, error)
	})
	if !ok {
		return false, nil
	}
	dir := filepath.Dir(path)
	scoped, err := destination.WithPrivateDestination(dir)
	if err != nil {
		return true, &CommitError{Step: "secure directory", Err: err}
	}
	if err := fsutil.EnsureSecureDirectory(scoped, dir); err != nil && !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		return true, &CommitError{Step: "secure directory", Err: err}
	}
	directory, err := fsutil.OpenOwnerControlledDirectory(scoped, dir)
	if err != nil {
		return true, &CommitError{Step: "secure directory", Err: err}
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			result = errors.Join(result, &CommitError{Step: "directory close", Committed: result == nil || errors.Is(result, fsutil.ErrCommitIndeterminate), Err: closeErr})
		}
	}()
	if err := fsutil.SecureAtomicWriteInOwnerControlledDirectoryChecked(scoped, directory, dir, filepath.Base(path), data, nil); err != nil {
		return true, &CommitError{Step: "write", Committed: errors.Is(err, fsutil.ErrCommitIndeterminate), Err: err}
	}
	return true, nil
}

func (s *ManagedStore) recoverManagedPermissions() (result error) {
	destination, ok := s.FS.(interface {
		WithPrivateDestination(string) (fsutil.OSFileSystem, error)
	})
	if !ok {
		return nil
	}
	dir := filepath.Join(s.Home, ".codex", "accounts")
	if _, err := s.FS.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	scoped, err := destination.WithPrivateDestination(dir)
	if err != nil {
		return err
	}
	directory, err := fsutil.OpenOwnerControlledDirectory(scoped, dir)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	reader, ok := directory.(fsutil.SecureDirectoryReader)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		candidateID, ok := managedCredentialFilename(name)
		if !ok || entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := scoped.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0 {
			continue
		}
		file, err := scoped.OpenRetainedRegularFileNoFollow(path, fsutil.RetainedRegularFileReadOnly)
		if err != nil {
			return err
		}
		fileInfo, statErr := file.Stat()
		if statErr == nil {
			statErr = fsutil.ValidateExternalCacheFile(scoped, fileInfo)
		}
		var identity fsutil.SecureFileIdentity
		if statErr == nil {
			var identityOK bool
			identity, identityOK = scoped.FileIdentity(fileInfo)
			if !identityOK {
				statErr = fsutil.ErrUnsafeSecurePath
			}
		}
		var data []byte
		if statErr == nil {
			data, statErr = io.ReadAll(io.LimitReader(file, maxManagedCredentialBytes+1))
			if len(data) > maxManagedCredentialBytes {
				statErr = fsutil.ErrSecureFileTooLarge
			}
		}
		if closeErr := file.Close(); statErr != nil || closeErr != nil {
			return errors.Join(statErr, closeErr)
		}
		record, err := decodeManagedRecord(path, data)
		if err != nil {
			return err
		}
		if record.Metadata.Version != 1 || record.Metadata.CandidateID != candidateID ||
			!validProvenance(record.Metadata.Provenance) || !validRefreshOwnership(record.Metadata.RefreshOwnership) ||
			!validOperationState(record.Metadata.OperationState) {
			return fmt.Errorf("%w: unproved CQ-managed credential", fsutil.ErrUnsafeSecurePath)
		}
		if err := fsutil.SealOwnedExternalCacheFileInDirectory(scoped, directory, dir, name, identity, data); err != nil {
			return err
		}
	}
	return nil
}

func managedCredentialFilename(name string) (CandidateID, bool) {
	if len(name) != len("cand_")+32+len(".auth.json") || !strings.HasPrefix(name, "cand_") || !strings.HasSuffix(name, ".auth.json") {
		return "", false
	}
	id := strings.TrimSuffix(name, ".auth.json")
	_, err := hex.DecodeString(strings.TrimPrefix(id, "cand_"))
	return CandidateID(id), err == nil
}
