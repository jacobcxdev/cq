package harness

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

//go:embed curate-harness/SKILL.md
var curationSkill []byte

const curationSkillMarker = "<!-- cq-owned:curate-harness -->"
const CurationSkillID = "user:curate-harness"

// IsCurationCapability recognises both legacy and stable qualified source IDs.
func IsCurationCapability(capability Capability) bool {
	return capability.Kind == SkillKind && (capability.ID == CurationSkillID || capability.ID == CurationSkillID+"@codex/curate-harness" || capability.ID == CurationSkillID+"@agents/curate-harness")
}

type SkillInstallation struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
}

func CurationSkillPath() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return "", fmt.Errorf("CODEX_HOME must be absolute and clean")
	}
	return filepath.Join(home, "skills", "curate-harness"), nil
}

// InstallCurationSkill preserves unrelated content and repeated installs.
func InstallCurationSkill(fsys fsutil.FileSystem, path string) (result SkillInstallation, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "curate-harness" {
		return result, fmt.Errorf("curation skill directory must be absolute and named curate-harness")
	}
	if err = fsutil.EnsureSecureDirectory(fsys, path); err != nil {
		return result, err
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return result, fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := fsys.(fsutil.SecureDirectoryOpener)
	if !ok {
		return result, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	validate := func() error { return fsutil.ValidateSecureDirectoryHandle(inspector, directory, path) }
	if err = validate(); err != nil {
		return result, err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, "cq-install.lock")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	previous, readErr := fsutil.ReadSecureFileInDirectory(inspector, directory, "SKILL.md", maxPolicyBytes)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return result, readErr
	}
	result.Path = filepath.Join(path, "SKILL.md")
	if readErr == nil {
		if !bytes.Contains(previous, []byte(curationSkillMarker)) {
			return result, fmt.Errorf("existing curation skill is not CQ-owned; left unchanged")
		}
		if bytes.Equal(previous, curationSkill) {
			return result, nil
		}
	}
	check := func() error {
		if err := validate(); err != nil {
			return err
		}
		current, currentErr := fsutil.ReadSecureFileInDirectory(inspector, directory, "SKILL.md", maxPolicyBytes)
		if errors.Is(readErr, os.ErrNotExist) && errors.Is(currentErr, os.ErrNotExist) {
			return nil
		}
		if currentErr != nil || !bytes.Equal(current, previous) {
			return fmt.Errorf("curation skill changed during installation")
		}
		return nil
	}
	if err = fsutil.SecureAtomicWriteInDirectoryChecked(inspector, directory, "SKILL.md", curationSkill, check); err != nil {
		return result, err
	}
	result.Changed = true
	return result, nil
}
