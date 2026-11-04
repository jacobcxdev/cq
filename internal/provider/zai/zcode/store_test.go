package zcode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func storeTestSession(id, token string) LoginSession {
	return LoginSession{UserID: id, Email: id + "@example.com", Name: id, ZCodeJWT: "zcode-" + token, RawOAuth: "oauth-" + token, BusinessJWT: "business-" + token, RefreshToken: "refresh-" + token}
}

func TestSessionStoreKeptStableAccountsAndEncryptedSecrets(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store := SessionStore{FS: fsys, Path: "/config/zcode.json", CipherSecret: "local-store-key"}
	first, err := store.Save(storeTestSession("first", "one"), "primary", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(storeTestSession("second", "two"), "secondary", true)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := store.Save(storeTestSession("first", "rotated"), "", false)
	if err != nil || rotated.ID != first.ID || rotated.Label != "primary" {
		t.Fatal("refresh changed stable identity/label")
	}
	accounts, err := store.Accounts()
	if err != nil || len(accounts) != 2 || accounts[0].ID != second.ID || accounts[1].ZCodeJWT != "zcode-rotated" {
		t.Fatal("account order or rotation lost")
	}
	data, _ := fsys.ReadFile(store.Path)
	for _, token := range []string{"zcode-one", "oauth-one", "business-two", "refresh-two", "zcode-rotated", store.CipherSecret} {
		if strings.Contains(string(data), token) {
			t.Fatal("credential persisted without encryption")
		}
	}
	public, _ := json.Marshal(accounts)
	if strings.Contains(string(public), "zcode-rotated") {
		t.Fatal("public account output leaked credential")
	}
	if err := store.SetActive("primary"); err != nil {
		t.Fatal(err)
	}
	accounts, _ = store.Accounts()
	if accounts[0].ID != first.ID {
		t.Fatal("preferred account not selected")
	}
	if err := store.Remove(second.ID); err != nil {
		t.Fatal(err)
	}
	accounts, _ = store.Accounts()
	if len(accounts) != 1 || accounts[0].ID != first.ID {
		t.Fatal("remove lost another account")
	}
	if _, err := fsys.ReadFile("/config/zai.json"); !os.IsNotExist(err) {
		t.Fatal("legacy API-key state touched")
	}
}

func TestSessionStoreRejectedAmbiguousOrMalformedStateWithoutOverwrite(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store := SessionStore{FS: fsys, Path: "/config/zcode.json", CipherSecret: "local-store-key"}
	_, _ = store.Save(storeTestSession("first", "one"), "same", false)
	_, _ = store.Save(storeTestSession("second", "two"), "same", false)
	before, _ := fsys.ReadFile(store.Path)
	if err := store.SetActive("same"); err == nil {
		t.Fatal("ambiguous label selected")
	}
	after, _ := fsys.ReadFile(store.Path)
	if string(before) != string(after) {
		t.Fatal("failed selection changed store")
	}
	wrong := store
	wrong.CipherSecret = "wrong-secret"
	if _, err := wrong.Accounts(); err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatal("wrong key accepted or leaked")
	}
	_ = fsys.WriteFile(store.Path, []byte(`{"schema_version":1,"accounts":[{"credentials":"private-secret"}]}`), 0o600)
	if _, err := store.Save(storeTestSession("third", "three"), "", false); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("corrupt store overwritten or leaked")
	}
}

func TestSessionStoreUsedPrivateAtomicFile(t *testing.T) {
	fsys := fsutil.OSFileSystem{}
	path := t.TempDir() + "/private/zcode.json"
	store := SessionStore{FS: fsys, Path: path, CipherSecret: "local-key"}
	if _, err := store.Save(storeTestSession("first", "one"), "", false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("session file not private")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || runtime.GOOS != "windows" && parent.Mode().Perm() != 0o700 {
		t.Fatal("session directory not private")
	}
}

func TestSessionStoreBoundCiphertextToAccount(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store := SessionStore{FS: fsys, Path: "/config/zcode.json", CipherSecret: "local-store-key"}
	_, _ = store.Save(storeTestSession("first", "one"), "", false)
	_, _ = store.Save(storeTestSession("second", "two"), "", false)
	data, _ := fsys.ReadFile(store.Path)
	var file loginSessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	file.Accounts[0].Credentials, file.Accounts[1].Credentials = file.Accounts[1].Credentials, file.Accounts[0].Credentials
	data, _ = json.Marshal(file)
	_ = fsys.WriteFile(store.Path, data, 0o600)
	if _, err := store.Accounts(); err == nil {
		t.Fatal("cross-account ciphertext was accepted")
	}
}
