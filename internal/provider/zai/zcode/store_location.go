package zcode

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type ownerControlledCQFileSystem interface {
	fsutil.FileSystem
	fsutil.SecurePathInspector
	OpenOwnerControlledCQDirectory(string) (fsutil.SecureDirectory, error)
}

// OpenSessionStore uses a CQ-owned key. Discovery never creates credential state.
func OpenSessionStore(fsys fsutil.FileSystem, configDir string, create bool) (SessionStore, error) {
	if fsys == nil || configDir == "" {
		return SessionStore{}, errors.New("ZCode session store unavailable")
	}
	store := SessionStore{FS: fsys, Path: filepath.Join(configDir, "zcode-accounts.json")}
	keyPath := filepath.Join(configDir, "zcode-accounts.key")
	if ownerFS, ok := fsys.(interface {
		ownerControlledCQFileSystem
		IsDirectCQDirectory(string) bool
	}); ok && ownerFS.IsDirectCQDirectory(configDir) {
		directory, err := ownerFS.OpenOwnerControlledCQDirectory(configDir)
		if err == nil {
			store.ownerControlled = true
			opened, openErr := openOwnerControlledSessionStore(ownerFS, directory, configDir, store, create)
			return opened, errors.Join(openErr, directory.Close())
		}
		if !errors.Is(err, os.ErrNotExist) {
			return SessionStore{}, errors.New("open ZCode account directory")
		}
		if !create {
			return SessionStore{}, ErrNotConfigured
		}
	}
	if !create {
		if _, err := fsys.ReadFile(store.Path); errors.Is(err, os.ErrNotExist) {
			return SessionStore{}, ErrNotConfigured
		}
	}
	if create {
		if err := fsutil.EnsureSecureDirectory(fsys, configDir); err != nil {
			return SessionStore{}, errors.New("create ZCode account directory")
		}
		lock, err := fsutil.AcquireExclusiveLock(fsys, filepath.Join(configDir, "zcode-accounts-key.lock"))
		if err != nil {
			return SessionStore{}, errors.New("lock ZCode account encryption key")
		}
		defer lock.Close()
	}
	key, err := fsys.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) && create {
		if _, readErr := fsys.ReadFile(store.Path); !errors.Is(readErr, os.ErrNotExist) {
			return SessionStore{}, errors.New("ZCode account encryption key unavailable")
		}
		material := make([]byte, 32)
		if _, err := rand.Read(material); err != nil {
			return SessionStore{}, errors.New("create ZCode account encryption key")
		}
		key = []byte(base64.RawURLEncoding.EncodeToString(material))
		clear(material)
		if err := fsutil.SecureAtomicWrite(fsys, keyPath, append(key, '\n')); err != nil {
			return SessionStore{}, errors.New("save ZCode account encryption key")
		}
	} else if err != nil {
		return SessionStore{}, errors.New("read ZCode account encryption key")
	}
	return sessionStoreWithKey(store, key)
}

func openOwnerControlledSessionStore(fsys ownerControlledCQFileSystem, directory fsutil.SecureDirectory, configDir string, store SessionStore, create bool) (SessionStore, error) {
	read := func(name string, limit int64) ([]byte, error) {
		data, _, err := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(fsys, directory, configDir, name, limit)
		return data, err
	}
	if !create {
		if _, err := read("zcode-accounts.json", 1<<20); errors.Is(err, os.ErrNotExist) {
			return SessionStore{}, ErrNotConfigured
		} else if err != nil {
			return SessionStore{}, errors.New("read CQ ZCode sessions")
		}
	}
	if create {
		lock, err := fsutil.AcquireExclusiveLockInOwnerControlledDirectory(fsys, directory, configDir, "zcode-accounts-key.lock")
		if err != nil {
			return SessionStore{}, errors.New("lock ZCode account encryption key")
		}
		defer lock.Close()
		info, err := lock.Stat()
		if err != nil {
			return SessionStore{}, errors.New("inspect ZCode account encryption key lock")
		}
		identity, ok := fsys.FileIdentity(info)
		if !ok {
			return SessionStore{}, fsutil.ErrSecureCapabilityUnavailable
		}
		fence := func() error {
			return fsutil.ValidateExclusiveLockHeldInOwnerControlledDirectory(fsys, directory, configDir, "zcode-accounts-key.lock", identity)
		}
		key, err := read("zcode-accounts.key", 128)
		if errors.Is(err, os.ErrNotExist) {
			if _, readErr := read("zcode-accounts.json", 1<<20); !errors.Is(readErr, os.ErrNotExist) {
				return SessionStore{}, errors.New("ZCode account encryption key unavailable")
			}
			material := make([]byte, 32)
			if _, err := rand.Read(material); err != nil {
				return SessionStore{}, errors.New("create ZCode account encryption key")
			}
			key = []byte(base64.RawURLEncoding.EncodeToString(material))
			clear(material)
			if err := fsutil.SecureAtomicWriteInOwnerControlledDirectoryChecked(fsys, directory, configDir, "zcode-accounts.key", append(key, '\n'), fence); err != nil {
				return SessionStore{}, errors.New("save ZCode account encryption key")
			}
		} else if err != nil {
			return SessionStore{}, errors.New("read ZCode account encryption key")
		}
		return sessionStoreWithKey(store, key)
	}
	key, err := read("zcode-accounts.key", 128)
	if err != nil {
		return SessionStore{}, errors.New("read ZCode account encryption key")
	}
	return sessionStoreWithKey(store, key)
}

func sessionStoreWithKey(store SessionStore, key []byte) (SessionStore, error) {
	secret := strings.TrimSpace(string(key))
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(decoded) != 32 {
		return SessionStore{}, errors.New("invalid ZCode account encryption key")
	}
	clear(decoded)
	store.CipherSecret = secret
	return store, nil
}
