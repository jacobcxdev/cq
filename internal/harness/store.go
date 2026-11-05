package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const maxPolicyBytes = 1 << 20

// Load reads private policy state; a missing file leaves the feature disabled.
func Load(fsys fsutil.FileSystem, path string) (Policy, error) {
	if err := validatePolicyPath(path); err != nil {
		return Policy{}, err
	}
	data, err := fsutil.ReadSecureFile(fsys, path, maxPolicyBytes)
	if errors.Is(err, os.ErrNotExist) {
		return emptyPolicy(), nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("read harness policy: %w", err)
	}
	return decodePolicy(data)
}

// Save replaces policy state under the same lock used by Update.
func Save(fsys fsutil.FileSystem, path string, policy Policy) error {
	if err := validatePolicy(policy); err != nil {
		return err
	}
	_, err := Update(fsys, path, func(current *Policy) error {
		*current = policy
		return nil
	})
	return err
}

// Update applies one locked read-modify-write transaction. Lock contention is
// returned as fsutil.ErrExclusiveLockHeld so callers can retry without losing
// another process's overrides. The callback must not retain the policy pointer.
func Update(fsys fsutil.FileSystem, path string, update func(*Policy) error) (policy Policy, err error) {
	if err := validatePolicyPath(path); err != nil {
		return Policy{}, err
	}
	if update == nil {
		return Policy{}, fmt.Errorf("harness policy update is required")
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return Policy{}, fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(fsutil.SecureDirectoryOpener)
	if !ok {
		return Policy{}, fsutil.ErrSecureCapabilityUnavailable
	}
	dir, name := filepath.Dir(path), filepath.Base(path)
	if err := fsutil.EnsureSecureDirectory(fsys, dir); err != nil {
		return Policy{}, fmt.Errorf("secure harness policy directory: %w", err)
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return Policy{}, fmt.Errorf("open harness policy directory: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	validateDirectory := func() error {
		return fsutil.ValidateSecureDirectoryHandle(inspector, directory, dir)
	}
	if err := validateDirectory(); err != nil {
		return Policy{}, err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, name+".lock")
	if err != nil {
		return Policy{}, fmt.Errorf("lock harness policy: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	data, err := fsutil.ReadSecureFileInDirectory(inspector, directory, name, maxPolicyBytes)
	if errors.Is(err, os.ErrNotExist) {
		policy = emptyPolicy()
	} else if err != nil {
		return Policy{}, fmt.Errorf("read harness policy: %w", err)
	} else if policy, err = decodePolicy(data); err != nil {
		return Policy{}, err
	}
	if err := update(&policy); err != nil {
		return Policy{}, err
	}
	if err := validatePolicy(policy); err != nil {
		return Policy{}, err
	}
	data, err = json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return Policy{}, fmt.Errorf("encode harness policy: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxPolicyBytes {
		return Policy{}, fsutil.ErrSecureFileTooLarge
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(inspector, directory, name, data, validateDirectory); err != nil {
		return Policy{}, fmt.Errorf("write harness policy: %w", err)
	}
	return policy, nil
}

func emptyPolicy() Policy {
	return Policy{Version: 1, Projects: make(map[string]Profile), Chats: make(map[string]Profile)}
}

func decodePolicy(data []byte) (Policy, error) {
	var policy Policy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("decode harness policy: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Policy{}, fmt.Errorf("harness policy contains trailing data")
	}
	if err := validatePolicy(policy); err != nil {
		return Policy{}, err
	}
	if policy.Projects == nil {
		policy.Projects = make(map[string]Profile)
	}
	if policy.Chats == nil {
		policy.Chats = make(map[string]Profile)
	}
	return policy, nil
}

func validatePolicy(policy Policy) error {
	if policy.Version != 1 {
		return fmt.Errorf("unsupported harness policy version %d", policy.Version)
	}
	for project, profile := range policy.Projects {
		if !filepath.IsAbs(project) || filepath.Clean(project) != project || strings.ContainsRune(project, '\x00') {
			return fmt.Errorf("harness project path must be absolute and clean: %q", project)
		}
		if profile != Minimal && profile != Full {
			return fmt.Errorf("invalid harness project profile %q", profile)
		}
	}
	for chat, profile := range policy.Chats {
		if !canonicalChatID(chat) {
			return fmt.Errorf("harness chat ID must be a canonical lower-case UUID: %q", chat)
		}
		if profile != Minimal && profile != Full {
			return fmt.Errorf("invalid harness chat profile %q", profile)
		}
	}
	return nil
}

func validatePolicyPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("harness policy file path must be absolute and clean")
	}
	return nil
}

func canonicalChatID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for index := range id {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if id[index] != '-' {
				return false
			}
		} else if !(id[index] >= '0' && id[index] <= '9' || id[index] >= 'a' && id[index] <= 'f') {
			return false
		}
	}
	return true
}
