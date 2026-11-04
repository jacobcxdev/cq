package zcode

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type locationReadOnlyFS struct {
	fsutil.FileSystem
	mutations int
}

func (f *locationReadOnlyFS) WriteFile(string, []byte, os.FileMode) error {
	f.mutations++
	return os.ErrPermission
}
func (f *locationReadOnlyFS) MkdirAll(string, os.FileMode) error {
	f.mutations++
	return os.ErrPermission
}
func (f *locationReadOnlyFS) Rename(string, string) error { f.mutations++; return os.ErrPermission }
func (f *locationReadOnlyFS) Remove(string) error         { f.mutations++; return os.ErrPermission }

func locationFixture() LoginSession {
	return LoginSession{UserID: "location-user", Email: "location@example.com", ZCodeJWT: "zcode-fixture", RawOAuth: "oauth-fixture", BusinessJWT: "business-fixture", DeviceMid: "fixture-device"}
}

func TestOpenSessionStoreDiscoveryNeverCreatedState(t *testing.T) {
	fsys := &locationReadOnlyFS{FileSystem: fsutil.NewMemFS()}
	if _, err := OpenSessionStore(fsys, "/missing/config", false); !errors.Is(err, ErrNotConfigured) {
		t.Fatal("missing login was not reported as unconfigured")
	}
	if fsys.mutations != 0 {
		t.Fatal("discovery attempted a filesystem mutation")
	}
	base := fsutil.NewMemFS()
	if err := base.MkdirAll("/config", 0o700); err != nil {
		t.Fatal(err)
	}
	created, err := OpenSessionStore(base, "/config", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := created.Save(locationFixture(), "fixture", false); err != nil {
		t.Fatal(err)
	}
	fsys = &locationReadOnlyFS{FileSystem: base}
	opened, err := OpenSessionStore(fsys, "/config", false)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := opened.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].DeviceMid != "fixture-device" {
		t.Fatal("read-only discovery lost encrypted device/account state")
	}
	if fsys.mutations != 0 {
		t.Fatal("existing login discovery attempted mutation")
	}
}

func TestOpenSessionStoreCreatedPrivateKeyAndReusedItAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cq")
	fsys := fsutil.OSFileSystem{}
	store, err := OpenSessionStore(fsys, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "zcode-accounts.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(string(key)))
	if err != nil || len(decoded) != 32 {
		t.Fatal("encryption key did not contain 32 random bytes")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("encryption key was not private")
		}
		info, err = os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatal("encryption-key directory was not private")
		}
	}
	if _, err := store.Save(locationFixture(), "fixture", false); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		reopened, err := OpenSessionStore(fsys, dir, create)
		if err != nil {
			t.Fatal(err)
		}
		if reopened.CipherSecret != store.CipherSecret {
			t.Fatal("restart replaced encryption key")
		}
		current, err := os.ReadFile(keyPath)
		if err != nil || string(current) != string(key) {
			t.Fatal("existing key file was rewritten")
		}
		accounts, err := reopened.Accounts()
		if err != nil || len(accounts) != 1 || accounts[0].ZCodeJWT != "zcode-fixture" {
			t.Fatal("restart could not read prior account")
		}
	}
}

func TestOpenSessionStoreMissingKeyNeverReplacedExistingState(t *testing.T) {
	for _, content := range []string{`not json`, `{"schema_version":1,"accounts":[]}`} {
		fsys := fsutil.NewMemFS()
		if err := fsys.MkdirAll("/config", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := fsys.WriteFile("/config/zcode-accounts.json", []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, create := range []bool{false, true} {
			if _, err := OpenSessionStore(fsys, "/config", create); err == nil {
				t.Fatal("existing store accepted missing key")
			}
			if _, err := fsys.ReadFile("/config/zcode-accounts.key"); !os.IsNotExist(err) {
				t.Fatal("missing key was silently replaced")
			}
			data, _ := fsys.ReadFile("/config/zcode-accounts.json")
			if string(data) != content {
				t.Fatal("existing store changed after missing-key failure")
			}
		}
	}
}

func TestOpenSessionStoreCorruptKeyFailedClosed(t *testing.T) {
	for _, value := range []string{"", "private-invalid-key", base64.RawURLEncoding.EncodeToString(make([]byte, 31))} {
		fsys := fsutil.NewMemFS()
		if err := fsys.MkdirAll("/config", 0o700); err != nil {
			t.Fatal(err)
		}
		_ = fsys.WriteFile("/config/zcode-accounts.json", []byte(`{"schema_version":1,"accounts":[]}`), 0o600)
		_ = fsys.WriteFile("/config/zcode-accounts.key", []byte(value), 0o600)
		for _, create := range []bool{false, true} {
			if _, err := OpenSessionStore(fsys, "/config", create); err == nil || strings.Contains(err.Error(), "private-invalid-key") {
				t.Fatal("corrupt key accepted or disclosed")
			}
			data, _ := fsys.ReadFile("/config/zcode-accounts.key")
			if string(data) != value {
				t.Fatal("corrupt key was replaced")
			}
		}
	}
}
