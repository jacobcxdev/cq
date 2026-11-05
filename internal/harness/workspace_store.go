package harness

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LoadForContext attaches ancestor portable files without changing user namespaces.
func LoadForContext(fsys fsutil.FileSystem, userPath, cwd string) (Policy, error) {
	policy, err := Load(fsys, userPath)
	if err != nil {
		return Policy{}, err
	}
	if policy.Version != 2 || cwd == "" {
		return policy, nil
	}
	if !filepath.IsAbs(cwd) || strings.ContainsRune(cwd, '\x00') {
		return Policy{}, fmt.Errorf("harness context directory must be absolute")
	}
	var ancestors []string
	for path := filepath.Clean(cwd); ; path = filepath.Dir(path) {
		ancestors = append(ancestors, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		path := filepath.Join(ancestors[i], ".cq", "harness.json")
		workspace, err := LoadWorkspace(fsys, path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Policy{}, fmt.Errorf("workspace policy %s: %w", path, err)
		}
		if err := validateScope(workspace.Rules, mergedDefinitions(policy.Profiles, workspace.Profiles), true); err != nil {
			return Policy{}, fmt.Errorf("workspace policy %s: %w", path, err)
		}
		policy.workspaceFiles = append(policy.workspaceFiles, workspaceLayer{path: path, policy: workspace})
	}
	return policy, nil
}

// LoadWorkspace reads a portable user-owned workspace policy.
func LoadWorkspace(fsys fsutil.FileSystem, path string) (WorkspacePolicy, error) {
	if err := validatePolicyPath(path); err != nil {
		return WorkspacePolicy{}, err
	}
	if err := validateWorkspaceLocation(fsys, filepath.Dir(path)); err != nil {
		return WorkspacePolicy{}, err
	}
	directory, err := fsutil.OpenOwnerControlledDirectory(fsys, filepath.Dir(path))
	if err != nil {
		return WorkspacePolicy{}, err
	}
	defer directory.Close()
	inspector := fsys.(fsutil.SecurePathInspector)
	data, _, err := readWorkspaceFile(inspector, directory, filepath.Base(path))
	if err != nil {
		return WorkspacePolicy{}, err
	}
	if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, directory, filepath.Dir(path)); err != nil {
		return WorkspacePolicy{}, err
	}
	return decodeWorkspace(data)
}

// UpdateWorkspace serialises one portable policy read-modify-write transaction.
func UpdateWorkspace(fsys fsutil.FileSystem, path string, update func(*WorkspacePolicy) error) (policy WorkspacePolicy, err error) {
	if err := validatePolicyPath(path); err != nil {
		return WorkspacePolicy{}, err
	}
	if update == nil {
		return WorkspacePolicy{}, fmt.Errorf("workspace update is required")
	}
	dir, name := filepath.Dir(path), filepath.Base(path)
	if err := validateWorkspaceLocation(fsys, dir); errors.Is(err, os.ErrNotExist) {
		if err := validateWorkspaceLocation(fsys, filepath.Dir(dir)); err != nil {
			return WorkspacePolicy{}, err
		}
		if err := fsutil.EnsureSecureDirectory(fsys, dir); err != nil {
			return WorkspacePolicy{}, err
		}
	} else if err != nil {
		return WorkspacePolicy{}, err
	}
	directory, err := fsutil.OpenOwnerControlledDirectory(fsys, dir)
	if err != nil {
		return WorkspacePolicy{}, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	inspector := fsys.(fsutil.SecurePathInspector)
	lease, err := directory.OpenExclusiveLock(name+".lock", 0o600)
	if err != nil {
		return WorkspacePolicy{}, err
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	info, err := lease.Stat()
	if err != nil {
		return WorkspacePolicy{}, err
	}
	if err := fsutil.ValidateRetainedExternalImportFile(inspector, info); err != nil {
		return WorkspacePolicy{}, err
	}
	if info.Mode().Perm() != 0o600 {
		return WorkspacePolicy{}, fsutil.ErrUnsafeSecurePath
	}
	_, namedIdentity, err := readWorkspaceFile(inspector, directory, name+".lock")
	if err != nil {
		return WorkspacePolicy{}, err
	}
	heldIdentity, ok := inspector.FileIdentity(info)
	if !ok || !fsutil.SameSecureObject(heldIdentity, namedIdentity) {
		return WorkspacePolicy{}, fsutil.ErrUnsafeSecurePath
	}
	before, previous, readErr := readWorkspaceFile(inspector, directory, name)
	existed := readErr == nil
	if errors.Is(readErr, os.ErrNotExist) {
		policy = WorkspacePolicy{Version: 2, Profiles: make(map[string]Definition)}
	} else if readErr != nil {
		return WorkspacePolicy{}, readErr
	} else if policy, err = decodeWorkspace(before); err != nil {
		return WorkspacePolicy{}, err
	}
	if err := update(&policy); err != nil {
		return WorkspacePolicy{}, err
	}
	if err := validateWorkspace(policy); err != nil {
		return WorkspacePolicy{}, err
	}
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return WorkspacePolicy{}, err
	}
	data = append(data, '\n')
	if len(data) > maxPolicyBytes {
		return WorkspacePolicy{}, fsutil.ErrSecureFileTooLarge
	}
	if err := writeWorkspaceFile(fsys, inspector, directory, dir, name, data, previous, existed); err != nil {
		return WorkspacePolicy{}, err
	}
	return policy, nil
}

func writeWorkspaceFile(fsys fsutil.FileSystem, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, dir, name string, data []byte, previous fsutil.SecureFileIdentity, existed bool) (result error) {
	published := false
	defer func() {
		if result != nil {
			outcome := fsutil.CommitNotCommitted
			if published || errors.Is(result, fsutil.ErrCommitIndeterminate) {
				outcome = fsutil.CommitIndeterminate
			}
			result = &fsutil.CommitError{Outcome: outcome, Op: "write portable harness policy", Err: result}
		}
	}()
	renamer, ok := directory.(fsutil.IdentityBoundRenamer)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	remover, ok := directory.(fsutil.IdentityBoundRemover)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	if err := validateWorkspaceLocation(fsys, dir); err != nil {
		return err
	}
	if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, directory, dir); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := "." + name + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := directory.CreateExclusive(temporary, 0o644)
	if err != nil {
		return err
	}
	fileInspector, ok := file.(fsutil.DurableFileInspector)
	if !ok {
		file.Close()
		return fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := fileInspector.Stat()
	if err != nil {
		file.Close()
		return err
	}
	if err := fsutil.ValidateRetainedExternalImportFile(inspector, info); err != nil {
		file.Close()
		return err
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok {
		file.Close()
		return fsutil.ErrUnsafeSecurePath
	}
	defer func() {
		if !published {
			if cleanupErr := remover.RemoveChecked(temporary, identity); !errors.Is(cleanupErr, os.ErrNotExist) {
				result = errors.Join(result, cleanupErr)
			}
		}
	}()
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := validateWorkspaceLocation(fsys, dir); err != nil {
		return err
	}
	if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, directory, dir); err != nil {
		return err
	}
	_, sourceIdentity, err := readWorkspaceFile(inspector, directory, temporary)
	if err != nil {
		return err
	}
	if !fsutil.SameSecureObject(identity, sourceIdentity) {
		return fsutil.ErrUnsafeSecurePath
	}
	_, current, err := readWorkspaceFile(inspector, directory, name)
	if existed {
		if err != nil {
			return err
		}
		if !fsutil.SameSecureObject(previous, current) {
			return fsutil.ErrUnsafeSecurePath
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fsutil.ErrUnsafeSecurePath
	}
	if existed {
		err = renamer.RenameChecked(temporary, name, identity)
	} else {
		err = renamer.RenameNoReplaceChecked(temporary, name, identity)
	}
	if err != nil {
		return err
	}
	published = true
	if err := directory.Sync(); err != nil {
		return err
	}
	if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, directory, dir); err != nil {
		return err
	}
	installed, installedIdentity, err := readWorkspaceFile(inspector, directory, name)
	if err != nil {
		return err
	}
	if !bytes.Equal(installed, data) || !fsutil.SameSecureObject(identity, installedIdentity) {
		return fsutil.ErrUnsafeSecurePath
	}
	return nil
}

func decodeWorkspace(data []byte) (WorkspacePolicy, error) {
	var policy WorkspacePolicy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return WorkspacePolicy{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return WorkspacePolicy{}, fmt.Errorf("workspace policy contains trailing data")
	}
	if err := validateWorkspace(policy); err != nil {
		return WorkspacePolicy{}, err
	}
	if policy.Profiles == nil {
		policy.Profiles = make(map[string]Definition)
	}
	return policy, nil
}

func validateWorkspace(policy WorkspacePolicy) error {
	if policy.Version != 2 {
		return fmt.Errorf("unsupported workspace harness version %d", policy.Version)
	}
	if err := validateDefinitions(policy.Profiles); err != nil {
		return err
	}
	if err := validateScope(policy.Rules, policy.Profiles, false); err != nil {
		return err
	}
	check := func(changes Changes) error {
		for id := range changes.Skills {
			if filepath.IsAbs(id) || len(id) > 2 && id[1] == ':' && (id[2] == '/' || id[2] == '\\') {
				return fmt.Errorf("portable skill rules require stable IDs, not absolute paths")
			}
		}
		return nil
	}
	for _, definition := range policy.Profiles {
		if err := check(definition.Changes); err != nil {
			return err
		}
	}
	if err := check(policy.Rules.All.Changes); err != nil {
		return err
	}
	for _, binding := range policy.Rules.Models {
		if err := check(binding.Changes); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkspaceLocation(fsys fsutil.FileSystem, path string) error {
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := inspector.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fsutil.ErrUnsafeSecurePath
		}
		if info.Mode().Perm()&0o022 != 0 {
			owner, known := inspector.FileOwnerUID(info)
			if info.Mode()&os.ModeSticky == 0 || !known || owner != 0 {
				return fsutil.ErrUnsafeSecurePath
			}
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return fsutil.ValidateOwnerControlledDirectory(fsys, path)
}

func readWorkspaceFile(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string) (data []byte, identity fsutil.SecureFileIdentity, err error) {
	file, err := directory.OpenNoFollow(name)
	if err != nil {
		return nil, identity, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, identity, err
	}
	if err := fsutil.ValidateRetainedExternalImportFile(inspector, info); err != nil {
		return nil, identity, err
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok {
		return nil, identity, fsutil.ErrUnsafeSecurePath
	}
	data, err = io.ReadAll(io.LimitReader(file, maxPolicyBytes+1))
	if err != nil {
		return nil, identity, err
	}
	if len(data) > maxPolicyBytes {
		return nil, identity, fsutil.ErrSecureFileTooLarge
	}
	named, err := directory.OpenNoFollow(name)
	if err != nil {
		return nil, identity, err
	}
	defer func() { err = errors.Join(err, named.Close()) }()
	namedInfo, err := named.Stat()
	if err != nil {
		return nil, identity, err
	}
	if err := fsutil.ValidateRetainedExternalImportFile(inspector, namedInfo); err != nil {
		return nil, identity, err
	}
	namedIdentity, ok := inspector.FileIdentity(namedInfo)
	if !ok || !fsutil.SameSecureObject(identity, namedIdentity) {
		return nil, identity, fsutil.ErrUnsafeSecurePath
	}
	return data, identity, nil
}
