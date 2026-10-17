// Package dots embeds the isolated Dots Python controller and runtime sources.
package dots

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

//go:embed *.py runtime/*.py
var payload embed.FS

// Sources returns only deployable sources, never test fixtures or private settings.
func Sources() (map[string][]byte, error) {
	result := make(map[string][]byte)
	err := fs.WalkDir(payload, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "test_") || !strings.HasSuffix(path, ".py") {
			return nil
		}
		data, err := payload.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = data
		return nil
	})
	return result, err
}

func owned(info os.FileInfo) bool {
	inspector := fsutil.OSFileSystem{}
	owner, ok := inspector.FileOwnerUID(info)
	return ok && owner == inspector.EffectiveUID()
}

// PrivateRoot validates or creates a private installation root without following a root symlink.
func PrivateRoot(path string, create bool) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("Dots root must be a clean absolute path")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && create {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		// Parent aliases such as macOS /tmp are canonicalised before creating the root.
		parent, resolveErr := filepath.EvalSymlinks(filepath.Dir(path))
		if resolveErr != nil {
			return "", resolveErr
		}
		path = filepath.Join(parent, filepath.Base(path))
		if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
			return "", err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !owned(info) {
		return "", fmt.Errorf("Dots root must be an owned directory with mode 0700, without symlinks")
	}
	return filepath.EvalSymlinks(path)
}

// ValidateInstallation checks existing private controller and runtime paths without writes.
func ValidateInstallation(root string) error {
	if _, err := PrivateRoot(root, false); err != nil {
		return err
	}
	for _, name := range []string{"payload", "payload/runtime", "runtime", "runtime/venv", "runtime/venv/bin"} {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned(info) || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("invalid private Dots directory")
		}
		if (name == "payload" || name == "runtime") && info.Mode().Perm() != 0o700 {
			return fmt.Errorf("Dots controller and runtime directories must be mode 0700")
		}
	}
	for _, name := range []string{"payload/cli.py", "settings.json"} {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("invalid private Dots controller or configuration")
		}
	}
	if err := filepath.WalkDir(filepath.Join(root, "payload"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !owned(info) {
			return fmt.Errorf("unsafe Dots payload entry")
		}
		if entry.IsDir() {
			if info.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("Dots payload directory is writable by another user")
			}
		} else if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || (strings.HasSuffix(path, ".py") && info.Mode().Perm() != 0o600) {
			return fmt.Errorf("Dots payload source must be private and owned")
		}
		return nil
	}); err != nil {
		return err
	}
	// The venv Python executable may be the standard symlink to its base interpreter.
	info, err := os.Stat(filepath.Join(root, "runtime", "venv", "bin", "python"))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("Dots venv interpreter unavailable")
	}
	return nil
}

// Extract atomically replaces the embedded controller payload, never the live runtime.
func Extract(root string) error {
	root, err := PrivateRoot(root, true)
	if err != nil {
		return err
	}
	sources, err := Sources()
	if err != nil {
		return err
	}
	target := filepath.Join(root, "payload")
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !owned(info) {
			return fmt.Errorf("invalid Dots payload directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Validate all existing destinations before making any replacement.
	for name := range sources {
		path := filepath.Join(target, filepath.FromSlash(name))
		for current := filepath.Dir(path); current != target; current = filepath.Dir(current) {
			if info, err := os.Lstat(current); err == nil {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned(info) {
					return fmt.Errorf("invalid Dots payload directory")
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if info, err := os.Lstat(path); err == nil {
			if !info.Mode().IsRegular() || !owned(info) {
				return fmt.Errorf("invalid Dots payload source")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(target, "runtime"), 0o700); err != nil {
		return err
	}
	for name, data := range sources {
		destination := filepath.Join(target, filepath.FromSlash(name))
		temporary, err := os.CreateTemp(filepath.Dir(destination), ".dots-source-*")
		if err != nil {
			return err
		}
		tmp := temporary.Name()
		if _, err = temporary.Write(data); err == nil {
			err = temporary.Sync()
		}
		if closeErr := temporary.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmp, destination)
		}
		os.Remove(tmp)
		if err != nil {
			return err
		}
	}
	return nil
}
