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

// OpenSessionStore uses a CQ-owned key. Discovery never creates credential state.
func OpenSessionStore(fsys fsutil.FileSystem, configDir string, create bool) (SessionStore, error) {
	if fsys == nil || configDir == "" {
		return SessionStore{}, errors.New("ZCode session store unavailable")
	}
	store := SessionStore{FS: fsys, Path: filepath.Join(configDir, "zcode-accounts.json")}
	keyPath := filepath.Join(configDir, "zcode-accounts.key")
	if !create {
		if _, err := fsys.ReadFile(store.Path); errors.Is(err, os.ErrNotExist) {
			return SessionStore{}, ErrNotConfigured
		}
	}
	if create {
		if err := fsys.MkdirAll(configDir, 0o700); err != nil {
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
	secret := strings.TrimSpace(string(key))
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(decoded) != 32 {
		return SessionStore{}, errors.New("invalid ZCode account encryption key")
	}
	clear(decoded)
	store.CipherSecret = secret
	return store, nil
}
