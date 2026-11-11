package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestExtraCreditsPolicyPersistsOnlySelectedAccount(t *testing.T) {
	fs := fsutil.NewMemFS()
	policy := &ExtraCreditsPolicy{FS: fs, Path: "/state/codex-extra-credits.json"}
	if allowed, err := policy.Allowed("account-a"); err != nil || !allowed {
		t.Fatalf("default = %v, %v", allowed, err)
	}
	if err := policy.Set("account-a", false); err != nil {
		t.Fatal(err)
	}
	reopened := &ExtraCreditsPolicy{FS: fs, Path: policy.Path}
	for key, want := range map[AccountKey]bool{"account-a": false, "account-b": true} {
		if got, err := reopened.Allowed(key); err != nil || got != want {
			t.Fatalf("%s allowed = %v, %v, want %v", key, got, err, want)
		}
	}
	if restricted, err := reopened.Restricted(); err != nil || !restricted {
		t.Fatalf("restricted = %v, %v", restricted, err)
	}
	if err := reopened.Set("account-a", true); err != nil {
		t.Fatal(err)
	}
	if restricted, err := policy.Restricted(); err != nil || restricted {
		t.Fatalf("enabled restricted = %v, %v", restricted, err)
	}
}

func TestExtraCreditsPolicyRejectsInvalidStateAndKeys(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"version":2,"disabled_accounts":[]}`, `{"version":1,"disabled_accounts":null}`, `{"version":1,"disabled_accounts":[""]}`, `{"version":1,"disabled_accounts":["a","a"]}`, `{"version":1,"disabled_accounts":[]} trailing`, `{"version":1,"disabled_accounts":["a"],"disabled_accounts":[]}`} {
		t.Run(body, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			policy := &ExtraCreditsPolicy{FS: fs, Path: "/state/policy.json"}
			if err := fs.MkdirAll("/state", 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(policy.Path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if allowed, err := policy.Allowed("a"); err == nil || allowed {
				t.Fatalf("malformed allowed = %v, %v", allowed, err)
			}
			if err := policy.Set("a", true); err == nil {
				t.Fatal("overwrote malformed policy")
			}
		})
	}
	policy := &ExtraCreditsPolicy{FS: fsutil.NewMemFS(), Path: "/state/policy.json"}
	for _, key := range []AccountKey{"", " a", "a\n", "a\x00b"} {
		if _, err := policy.Allowed(key); err == nil {
			t.Fatalf("allowed invalid key %q", key)
		}
		if err := policy.Set(key, false); err == nil {
			t.Fatalf("set invalid key %q", key)
		}
	}
}

func TestExtraCreditsPolicyMutationLockPreservesOtherAccounts(t *testing.T) {
	fs := fsutil.NewMemFS()
	policy := &ExtraCreditsPolicy{FS: fs, Path: "/state/policy.json"}
	if err := policy.Set("first", false); err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.AcquireExclusiveLock(fs, policy.Path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Set("second", false); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("competing mutation = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := policy.Set("second", false); err != nil {
		t.Fatal(err)
	}
	if err := policy.Set("first", true); err != nil {
		t.Fatal(err)
	}
	if allowed, err := policy.Allowed("second"); err != nil || allowed {
		t.Fatalf("other account changed = %v, %v", allowed, err)
	}
}

func TestExtraCreditsPolicyNativePermissionsAndSymlinkRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	policy := &ExtraCreditsPolicy{FS: fsutil.OSFileSystem{}, Path: filepath.Join(dir, "policy.json")}
	if err := policy.Set("account", false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(policy.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file permissions: %v, %v", info, err)
	}
	if err := os.Rename(policy.Path, policy.Path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(policy.Path+".saved", policy.Path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if allowed, err := policy.Allowed("account"); err == nil || allowed {
		t.Fatalf("symlink allowed = %v, %v", allowed, err)
	}
	if err := policy.Set("account", true); err == nil {
		t.Fatal("modified symlink policy")
	}
}

func TestExtraCreditsIdentityKeySurvivesManagedAccountAdoption(t *testing.T) {
	identity := AccountIdentity{AccountID: "workspace", UserID: "user", Email: "first@example.com", RecordKey: "legacy"}
	key, err := ExtraCreditsIdentityKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	policy := &ExtraCreditsPolicy{FS: fsutil.NewMemFS(), Path: "/state/policy.json"}
	if err := policy.Set(key, false); err != nil {
		t.Fatal(err)
	}
	for _, logical := range []LogicalAccount{
		{Key: "gen:external", Identity: identity},
		{Key: "acct:managed", Identity: AccountIdentity{AccountID: identity.AccountID, UserID: identity.UserID, Email: "changed@example.com", RecordKey: "new"}},
	} {
		lookup, err := ExtraCreditsIdentityKey(logical.Identity)
		if err != nil || lookup != key {
			t.Fatalf("changed key: %q, %v", lookup, err)
		}
		if allowed, err := policy.Allowed(lookup); err != nil || allowed {
			t.Fatalf("adoption enabled credits: %v, %v", allowed, err)
		}
	}
	for _, invalid := range []AccountIdentity{{}, {AccountID: "workspace"}, {UserID: "user"}} {
		if _, err := ExtraCreditsIdentityKey(invalid); err == nil {
			t.Fatal("accepted weak identity")
		}
	}
	other, err := ExtraCreditsIdentityKey(AccountIdentity{AccountID: "other-workspace", UserID: "user"})
	if err != nil || other == key {
		t.Fatalf("workspace identity collision: %q, %v", other, err)
	}
}
