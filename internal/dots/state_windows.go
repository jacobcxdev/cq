//go:build windows

package dots

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

func windowsStateFS(path string) (fsutil.OSFileSystem, error) {
	return fsutil.NewPrivateDestinationFileSystem(path)
}

func windowsPrivateDirectory(path string) error {
	fsys, err := windowsStateFS(path)
	if err != nil {
		return err
	}
	return fsutil.ValidateSecureDirectory(fsys, path)
}

func windowsEnsurePrivateDirectory(path string) error {
	fsys, err := windowsStateFS(path)
	if err != nil {
		return err
	}
	return fsutil.EnsureSecureDirectory(fsys, path)
}

func windowsValidatePrivateFile(path string) error {
	fsys, err := windowsStateFS(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := fsutil.ValidateSecureDirectory(fsys, filepath.Dir(path)); err != nil {
		return err
	}
	return fsutil.ValidateSecureRegularFile(fsys, path)
}

func windowsSaveJSON(path string, value any) error {
	if err := PrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("private state could not be encoded")
	}
	fsys, err := windowsStateFS(filepath.Dir(path))
	if err != nil {
		return err
	}
	return fsutil.SecureAtomicWrite(fsys, path, append(data, '\n'))
}

func windowsReadPrivate(path, root string, exactMode bool) ([]byte, error) {
	if !cleanAbsolute(path) || root != "" && (!cleanAbsolute(root) || !within(root, path)) {
		return nil, errors.New("invalid declared credential path")
	}
	if exactMode {
		fsys, err := windowsStateFS(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		return fsutil.ReadSecureFile(fsys, path, 1<<20)
	}
	fsys := fsutil.OSFileSystem{}
	file, err := fsys.OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() > 1<<20 {
		return nil, errors.New("private state exceeds size limit")
	}
	if err := fsutil.ValidateExternalCredentialFile(fsys, before); err != nil {
		return nil, err
	}
	data, err := httputil.ReadBody(file)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := fsutil.ValidateExternalCredentialFile(fsys, after); err != nil || revision(before) != revision(after) {
		return nil, errors.New("credential revision changed during read")
	}
	return data, nil
}

func readSystemAuth(path string) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(filepath.Clean(path), filepath.Join(home, ".codex", "auth.json")) || !cleanAbsolute(path) {
		return windowsReadPrivate(path, "", false)
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.ValidateExternalCredentialDirectory(fsys, filepath.Dir(path)); err != nil {
		return nil, err
	}
	named, err := fsys.Lstat(path)
	if err != nil {
		return nil, err
	}
	validateInfo := func(info os.FileInfo) (fsutil.SecureFileIdentity, error) {
		if info.Size() > 1<<20 {
			return fsutil.SecureFileIdentity{}, fsutil.ErrSecureFileTooLarge
		}
		if err := fsutil.ValidateExternalCacheFile(fsys, info); err != nil {
			return fsutil.SecureFileIdentity{}, err
		}
		identity, ok := fsys.FileIdentity(info)
		if !ok || identity.Links != 1 {
			return fsutil.SecureFileIdentity{}, fsutil.ErrUnsafeSecurePath
		}
		return identity, nil
	}
	namedID, err := validateInfo(named)
	if err != nil {
		return nil, err
	}
	file, err := fsys.OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	validateOpened := func(info os.FileInfo) (fsutil.SecureFileIdentity, error) {
		identity, err := validateInfo(info)
		if err != nil {
			return fsutil.SecureFileIdentity{}, err
		}
		if fsutil.ValidateExternalCredentialFile(fsys, info) != nil {
			if err := fsys.ValidateCodexSystemCredential(file); err != nil {
				return fsutil.SecureFileIdentity{}, err
			}
		}
		return identity, nil
	}
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	openedID, err := validateOpened(before)
	if err != nil {
		return nil, err
	}
	if !fsutil.SameSecureObject(namedID, openedID) {
		return nil, fsutil.ErrUnsafeSecurePath
	}
	data, err := httputil.ReadBody(file)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	afterID, err := validateOpened(after)
	if err != nil {
		return nil, err
	}
	if afterID != openedID || revision(before) != revision(after) {
		return nil, fsutil.ErrUnsafeSecurePath
	}
	return data, nil
}

func windowsMigratePrivatePort(path string) error {
	data, err := windowsReadPrivate(path, "", true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	port, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if parseErr != nil || port < 1 || port > 65535 || len(data) > 8 {
		return errors.New("private proxy port invalid")
	}
	return nil
}
