package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func testAuthority(t *testing.T, exchange RefreshExchange) *SessionAuthority {
	t.Helper()
	return NewAuthority(Store{FS: fsutil.OSFileSystem{}, ConfigDir: privateTestDir(t)}, nil, exchange)
}
func loginFixture(user string) Login {
	return Login{Account: Account{UserID: user, Email: user + "@example.test", Plan: "SuperGrok"}, AccessToken: "access-private", RefreshToken: "refresh-private", ExpiresAt: time.Now().Add(time.Hour)}
}
func TestStoreLoginPreferenceAndSecretPrivacy(t *testing.T) {
	a := testAuthority(t, nil)
	first, err := a.SaveLogin(context.Background(), loginFixture("one"), "first", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.SaveLogin(context.Background(), loginFixture("two"), "second", false)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 2 || accounts[0].ID != first.ID || !accounts[0].Active {
		t.Fatalf("%+v %v", accounts, err)
	}
	if err := a.SetPreferred(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	accounts, _ = a.Accounts(context.Background())
	if accounts[0].ID != second.ID {
		t.Fatal(accounts)
	}
	data, err := os.ReadFile(filepath.Join(a.Store.ConfigDir, "grok-accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "access-private") || strings.Contains(string(data), "refresh-private") {
		t.Fatal("plaintext credential persisted")
	}
	data, _ = json.Marshal(accounts)
	if strings.Contains(string(data), "private") {
		t.Fatal("public accounts leak token")
	}
	if err := a.Remove(context.Background(), string(first.ID)); err != nil {
		t.Fatal(err)
	}
	accounts, _ = a.Accounts(context.Background())
	if len(accounts) != 1 || accounts[0].ID != second.ID {
		t.Fatal(accounts)
	}
}
func TestStoreDiscoveryDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	a := NewAuthority(Store{FS: fsutil.OSFileSystem{}, ConfigDir: root}, nil, nil)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("%v %v", accounts, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery created state")
	}
}
func TestStoreDiscoveryExistingDirectoryWithoutAccounts(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/config", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/config/proxy.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAuthority(Store{FS: fsys, ConfigDir: "/config"}, nil, nil)
	accounts, err := a.Accounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("unconfigured discovery = %v, %v", accounts, err)
	}
	info, err := fsys.Stat("/config")
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("discovery changed directory policy: %v, %v", info, err)
	}
	entries, err := fsys.ReadDir("/config")
	if err != nil || len(entries) != 1 || entries[0].Name() != "proxy.json" {
		t.Fatalf("discovery changed state: %v, %v", entries, err)
	}
}

type storeMetadataErrorFS struct {
	*fsutil.MemFS
	err error
}

func (fsys storeMetadataErrorFS) Lstat(string) (os.FileInfo, error) {
	return nil, fsys.err
}

type storeBasicFS struct{ fsutil.FileSystem }

func TestStoreDiscoveryRejectsMetadataErrors(t *testing.T) {
	for _, cause := range []error{os.ErrPermission, fsutil.ErrUnsafeSecurePath} {
		t.Run(cause.Error(), func(t *testing.T) {
			fsys := storeMetadataErrorFS{MemFS: fsutil.NewMemFS(), err: cause}
			store := Store{FS: fsys, ConfigDir: "/config"}
			if _, err := store.readFile(storeFilename, 1024); !errors.Is(err, cause) {
				t.Fatalf("metadata error = %v, want %v", err, cause)
			}
		})
	}
	store := Store{FS: storeBasicFS{fsutil.NewMemFS()}, ConfigDir: "/config"}
	if _, err := store.readFile(storeFilename, 1024); !errors.Is(err, fsutil.ErrSecureCapabilityUnavailable) {
		t.Fatalf("missing no-follow capability = %v", err)
	}
}

func TestStoreDiscoveryStillRejectsUnsafePresentFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o700, 0o755} {
		t.Run(fmt.Sprintf("directory-%04o", mode), func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if err := fsys.MkdirAll("/config", mode); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile("/config/"+storeFilename, []byte("untrusted"), 0o644); err != nil {
				t.Fatal(err)
			}
			store := Store{FS: fsys, ConfigDir: "/config"}
			if _, err := store.readFile(storeFilename, 1024); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
				t.Fatalf("unsafe present file error = %v", err)
			}
		})
	}
}

func TestStoreDiscoveryRejectsSymlinkFile(t *testing.T) {
	root := privateTestDir(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untrusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, storeFilename)); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	store := Store{FS: fsutil.OSFileSystem{}, ConfigDir: root}
	if data, err := store.readFile(storeFilename, 1024); err == nil || len(data) != 0 {
		t.Fatalf("symlinked credential file was read: %q, %v", data, err)
	}
}

func TestStoreRetainedReadKeepsDirectoryAuthority(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/config", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/config/"+storeFilename, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/config")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	store := Store{FS: storeBasicFS{fsys}, ConfigDir: "/config", directory: directory, inspector: fsys, fence: func() error { return nil }}
	data, err := store.readFile(storeFilename, 1024)
	if err != nil || string(data) != "retained" {
		t.Fatalf("retained-directory read = %q, %v", data, err)
	}
}

func TestRefreshConcurrentAuthoritiesExchangeOnce(t *testing.T) {
	var calls atomic.Int32
	exchange := func(context.Context, string) (TokenSet, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return TokenSet{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	a := testAuthority(t, exchange)
	login := loginFixture("one")
	login.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	b := NewAuthority(a.Store, nil, exchange)
	var wg sync.WaitGroup
	for _, authority := range []*SessionAuthority{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := authority.Refresh(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("exchanges=%d", calls.Load())
	}
}
func TestRefreshUncertainOutcomeNeverReplaysAfterRestart(t *testing.T) {
	calls := 0
	exchange := func(context.Context, string) (TokenSet, error) {
		calls++
		return TokenSet{}, errors.New("connection lost")
	}
	a := testAuthority(t, exchange)
	login := loginFixture("one")
	login.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("expected uncertain refresh")
	}
	b := NewAuthority(a.Store, nil, exchange)
	if err := b.Refresh(context.Background()); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("%v", err)
	}
	if calls != 1 {
		t.Fatalf("exchanges=%d", calls)
	}
}
func TestRefreshRejectsChangedIdentity(t *testing.T) {
	a := testAuthority(t, func(context.Context, string) (TokenSet, error) {
		return TokenSet{AccessToken: "new", UserID: "another", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	login := loginFixture("one")
	login.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("changed identity accepted")
	}
}

func TestStoreRetainedNamespaceRejectsReplacement(t *testing.T) {
	a := testAuthority(t, nil)
	if _, err := a.SaveLogin(context.Background(), loginFixture("one"), "", true); err != nil {
		t.Fatal(err)
	}
	lock, err := a.Store.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	records, err := lock.store.read()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(a.Store.ConfigDir, a.Store.ConfigDir+"-moved"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(a.Store.ConfigDir+"-moved", a.Store.ConfigDir)
	if err := os.Mkdir(a.Store.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(a.Store.ConfigDir)
	if err := lock.store.write(records); err == nil {
		t.Fatal("wrote through replaced namespace")
	}
}

type failRenameFS struct {
	fsutil.OSFileSystem
	fail  atomic.Bool
	after atomic.Int32
}
type failRenameDirectory struct {
	fsutil.SecureDirectory
	fs *failRenameFS
}

func (f *failRenameFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	d, err := f.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &failRenameDirectory{SecureDirectory: d, fs: f}, nil
}
func (d *failRenameDirectory) Rename(old, new string) error {
	if d.fs.fail.Swap(false) || d.fs.after.Load() > 0 && d.fs.after.Add(-1) == 0 {
		return errors.New("injected rename failure")
	}
	return d.SecureDirectory.Rename(old, new)
}
func TestRefreshRetainsResultAfterPersistenceFailure(t *testing.T) {
	fs := &failRenameFS{}
	calls := 0
	a := NewAuthority(Store{FS: fs, ConfigDir: privateTestDir(t)}, nil, func(context.Context, string) (TokenSet, error) {
		calls++
		fs.fail.Store(true)
		return TokenSet{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	login := loginFixture("one")
	login.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("expected persistence failure")
	}
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := a.Store.read()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || records.Accounts[0].Tokens.Access != "new-access" || records.Accounts[0].Operation != nil {
		t.Fatalf("calls=%d records=%+v", calls, records)
	}
}
func (d *failRenameDirectory) ProbeExclusiveLockHeld(name string, mode os.FileMode) (os.FileInfo, error) {
	return d.SecureDirectory.(fsutil.ExclusiveLockHeldProber).ProbeExclusiveLockHeld(name, mode)
}
func (d *failRenameDirectory) RenameChecked(old, new string, id fsutil.SecureFileIdentity) error {
	if d.fs.fail.Swap(false) || d.fs.after.Load() > 0 && d.fs.after.Add(-1) == 0 {
		return errors.New("injected rename failure")
	}
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(old, new, id)
}
func (d *failRenameDirectory) RenameNoReplaceChecked(old, new string, id fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(old, new, id)
}
func (d *failRenameDirectory) RemoveChecked(name string, id fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, id)
}

func TestRefreshRetainsResultAfterCommitFailure(t *testing.T) {
	fs := &failRenameFS{}
	calls := 0
	a := NewAuthority(Store{FS: fs, ConfigDir: privateTestDir(t)}, nil, func(context.Context, string) (TokenSet, error) {
		calls++
		fs.after.Store(2)
		return TokenSet{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	login := loginFixture("one")
	login.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("expected commit failure")
	}
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := a.Store.read()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || records.Accounts[0].Tokens.Access != "new-access" || records.Accounts[0].Operation != nil {
		t.Fatalf("calls=%d access=%q pending=%v", calls, records.Accounts[0].Tokens.Access, records.Accounts[0].Operation != nil)
	}
}

func TestRefreshRestartCommitsDurableResultWithoutExchange(t *testing.T) {
	fs := &failRenameFS{}
	calls := 0
	exchange := func(context.Context, string) (TokenSet, error) {
		calls++
		fs.after.Store(2)
		return TokenSet{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	a := NewAuthority(Store{FS: fs, ConfigDir: privateTestDir(t)}, nil, exchange)
	login := loginFixture("one")
	login.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("expected commit failure")
	}
	restarted := NewAuthority(a.Store, nil, exchange)
	if err := restarted.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := restarted.Store.read()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || records.Accounts[0].Tokens.Access != "new-access" || records.Accounts[0].Operation != nil {
		t.Fatal("durable rotation result not recovered")
	}
}

func TestFreshLoginGenerationWinsOverRetainedRefresh(t *testing.T) {
	fs := &failRenameFS{}
	calls := 0
	exchange := func(context.Context, string) (TokenSet, error) {
		calls++
		if calls == 1 {
			fs.fail.Store(true)
		}
		return TokenSet{AccessToken: fmt.Sprintf("access-%d", calls), RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	a := NewAuthority(Store{FS: fs, ConfigDir: privateTestDir(t)}, nil, exchange)
	login := loginFixture("one")
	login.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("expected persistence failure")
	}
	other := NewAuthority(a.Store, nil, exchange)
	if _, err := other.SaveLogin(context.Background(), login, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("retained response superseded new login generation")
	}
}
