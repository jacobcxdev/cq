package zai

import (
	"bytes"
	"errors"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestRemoveAPIKeyAccountsKeptOAuthOrderAndSecretStore(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := StoreAccount(fsys, "/config", "first-private", "first", false); err != nil {
		t.Fatal(err)
	}
	first, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("first-user", "one"), "first browser", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StoreAccount(fsys, "/config", "second-private", "second", false); err != nil {
		t.Fatal(err)
	}
	second, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("second-user", "two"), "second browser", true)
	if err != nil {
		t.Fatal(err)
	}
	beforeSecrets, err := fsys.ReadFile("/config/zcode-accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveAPIKeyAccounts(fsys, "/config"); err != nil {
		t.Fatal(err)
	}
	accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].ID != second.ID || accounts[1].ID != first.ID || accounts[0].OAuth == nil || accounts[1].OAuth == nil {
		t.Fatal("cleanup lost OAuth references or preference order")
	}
	afterSecrets, _ := fsys.ReadFile("/config/zcode-accounts.json")
	if !bytes.Equal(beforeSecrets, afterSecrets) {
		t.Fatal("cleanup rewrote encrypted logins")
	}
	firstResult, _ := fsys.ReadFile("/config/zai.json")
	if err := RemoveAPIKeyAccounts(fsys, "/config"); err != nil {
		t.Fatal(err)
	}
	secondResult, _ := fsys.ReadFile("/config/zai.json")
	if !bytes.Equal(firstResult, secondResult) {
		t.Fatal("idempotent cleanup changed account file")
	}
}

func TestRemoveAPIKeyAccountsRequiredUsableSavedBrowserLogin(t *testing.T) {
	for _, corruptLogin := range []bool{false, true} {
		fsys := fsutil.NewMemFS()
		if _, err := StoreAccount(fsys, "/config", "retained-private", "native", true); err != nil {
			t.Fatal(err)
		}
		if corruptLogin {
			if _, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("saved-user", "one"), "browser", false); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile("/config/zcode-accounts.key", []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := fsys.ReadFile("/config/zai.json")
		if err := RemoveAPIKeyAccounts(fsys, "/config"); err == nil {
			t.Fatal("cleanup accepted absent or unreadable browser login")
		}
		after, _ := fsys.ReadFile("/config/zai.json")
		if !bytes.Equal(before, after) {
			t.Fatal("failed cleanup changed accounts")
		}
	}
}

func TestRemoveAPIKeyAccountsFailedAtomicReplacementPreservedBothKinds(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := StoreAccount(fsys, "/config", "retained-private", "native", true); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreZCodeAccount(fsys, "/config", oauthAccountFixture("saved-user", "one"), "browser", false); err != nil {
		t.Fatal(err)
	}
	before, _ := fsys.ReadFile("/config/zai.json")
	secretsBefore, _ := fsys.ReadFile("/config/zcode-accounts.json")
	failing := &bulkCleanupWriteFailureFS{MemFS: fsys}
	if err := RemoveAPIKeyAccounts(failing, "/config"); err == nil || failing.replacements != 1 {
		t.Fatal("cleanup did not report failed atomic replacement")
	}
	after, _ := fsys.ReadFile("/config/zai.json")
	secretsAfter, _ := fsys.ReadFile("/config/zcode-accounts.json")
	if !bytes.Equal(before, after) || !bytes.Equal(secretsBefore, secretsAfter) {
		t.Fatal("failed cleanup changed keys or encrypted logins")
	}
	accounts, err := ReadAccounts(fsys, noEnvironment, "/config")
	if err != nil || len(accounts) != 2 || accounts[0].APIKey != "retained-private" || accounts[1].OAuth == nil {
		t.Fatal("failed cleanup lost existing accounts")
	}
}

type bulkCleanupWriteFailureFS struct {
	*fsutil.MemFS
	replacements int
}
type bulkCleanupWriteFailureDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	owner *bulkCleanupWriteFailureFS
}

func (fsys *bulkCleanupWriteFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &bulkCleanupWriteFailureDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), owner: fsys}, nil
}

func (directory *bulkCleanupWriteFailureDirectory) RenameChecked(string, string, fsutil.SecureFileIdentity) error {
	directory.owner.replacements++
	return errors.New("injected atomic replacement failure")
}
