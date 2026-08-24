package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexJournalHashesComponentsAndResolvesAccount(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	store := openTestCodexLeaseStore(t, fsys)
	lease := testJournalLease(time.Now())
	if err := store.CommitLeases([]CodexTurnLease{lease}, 0); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{lease.Key.Lane.Session, lease.Key.Lane.Thread, lease.Key.Turn, string(lease.AccountKey), lease.ResponseAnchor, lease.TurnState} {
		if strings.Contains(string(data), `"`+raw+`"`) {
			t.Fatalf("journal leaked raw value %q", raw)
		}
	}
	record, account, found := store.Lookup(lease.Key, []codex.AccountKey{"other", lease.AccountKey})
	if !found || account != lease.AccountKey || record.SessionHash == record.ThreadHash || record.ThreadHash == record.TurnHash || record.CorrelationHash == "" || record.TurnStateHash == "" {
		t.Fatalf("record = %#v, account = %q, found = %v", record, account, found)
	}
}

func TestCodexJournalGenerationFenceAndCorruption(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	store := openTestCodexLeaseStore(t, fsys)
	if err := store.CommitLeases([]CodexTurnLease{testJournalLease(time.Now())}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitLeases(nil, 0); err == nil {
		t.Fatal("expected stale generation error")
	}
	data, _ := fsys.ReadFile("/state/leases.json")
	data[len(data)/2] ^= 1
	if err := fsys.WriteFile("/state/leases.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodexLeaseStore(fsys, "/state/leases.json", "/state/leases.key"); err == nil {
		t.Fatal("expected corrupt journal error")
	}
}

func TestCodexJournalKeepsShadowAndAuthorityDistinct(t *testing.T) {
	t.Parallel()
	store := openTestCodexLeaseStore(t, fsutil.NewMemFS())
	shadow := testJournalLease(time.Now())
	shadow.Authoritative = false
	shadow.ModeEpoch = 4
	authority := shadow
	authority.Authoritative = true
	authority.ModeEpoch = 5
	if err := store.CommitLeases([]CodexTurnLease{shadow, authority}, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, found := store.LookupMode(shadow.Key, []codex.AccountKey{shadow.AccountKey}, 4, true); found {
		t.Fatal("shadow record matched authoritative lookup")
	}
	if record, _, found := store.LookupMode(authority.Key, []codex.AccountKey{authority.AccountKey}, 5, true); !found || !record.Authoritative {
		t.Fatalf("authoritative record = %#v, found = %v", record, found)
	}
}

func TestCodexRuntimeObserverOpensForRetainedAuthorityWhileOff(t *testing.T) {
	runtime := &CodexRoutingRuntime{HTTP: CodexModeStatus{
		Effective:                   CodexRoutingOff,
		ModeEpoch:                   9,
		RetainedAuthoritativeEpochs: []uint64{6},
	}}
	observer, err := OpenCodexRuntimeObserver(runtime, fsutil.NewMemFS())
	if err != nil {
		t.Fatal(err)
	}
	if observer == nil || observer.Store == nil {
		t.Fatal("retained authority store unavailable")
	}
}

func TestCodexJournalKeyLossFailsClosed(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	store := openTestCodexLeaseStore(t, fsys)
	if err := store.CommitLeases([]CodexTurnLease{testJournalLease(time.Now())}, 0); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove("/state/leases.key"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodexLeaseStore(fsys, "/state/leases.json", "/state/leases.key"); err == nil {
		t.Fatal("expected missing key error")
	}
}

func TestOpenCodexLeaseStoreRejectsSymlinkKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.key")
	if err := os.WriteFile(target, make([]byte, codexLeaseHMACKeyBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(state, "leases.key")
	if err := os.Symlink(target, keyPath); err != nil {
		t.Fatal(err)
	}
	store, err := OpenCodexLeaseStore(fsutil.OSFileSystem{}, filepath.Join(state, "leases.json"), keyPath)
	if store != nil || err == nil {
		t.Fatalf("OpenCodexLeaseStore = %v, %v; want symlink rejection", store, err)
	}
}

func TestOpenCodexLeaseStoreRejectsSymlinkJournal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	journalPath := filepath.Join(state, "leases.json")
	keyPath := filepath.Join(state, "leases.key")
	store, err := OpenCodexLeaseStore(fsutil.OSFileSystem{}, journalPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitLeases(nil, 0); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.json")
	if err := os.Rename(journalPath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, journalPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCodexLeaseStore(fsutil.OSFileSystem{}, journalPath, keyPath)
	if reopened != nil || err == nil {
		t.Fatalf("OpenCodexLeaseStore = %v, %v; want symlink rejection", reopened, err)
	}
}

func TestOpenCodexLeaseStoreBoundsJournalRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(state, "leases.key")
	if err := os.WriteFile(keyPath, make([]byte, codexLeaseHMACKeyBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(state, "leases.json")
	if err := os.WriteFile(journalPath, make([]byte, (16<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenCodexLeaseStore(fsutil.OSFileSystem{}, journalPath, keyPath)
	if store != nil || !errors.Is(err, fsutil.ErrSecureFileTooLarge) {
		t.Fatalf("OpenCodexLeaseStore = %v, %v; want ErrSecureFileTooLarge", store, err)
	}
}

func TestCodexJournalRestartOrphansAndExtinguishesSocket(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	store := openTestCodexLeaseStore(t, fsys)
	lease := testJournalLease(time.Now())
	if err := store.CommitLeases([]CodexTurnLease{lease}, 0); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenCodexLeaseStore(fsys, "/state/leases.json", "/state/leases.key")
	if err != nil {
		t.Fatal(err)
	}
	record, account, found := restarted.Lookup(lease.Key, []codex.AccountKey{lease.AccountKey})
	if !found || account != lease.AccountKey || record.State != LeaseOrphaned || record.SocketGeneration != 0 || record.ActiveRefs != 0 {
		t.Fatalf("restored record = %#v", record)
	}
}

func TestCodexJournalRestartCommitsThroughRetainedDirectory(t *testing.T) {
	t.Parallel()
	if _, ok := any(fsutil.OSFileSystem{}).(fsutil.SecurePathInspector); !ok {
		t.Skip("secure path inspection is unavailable on this platform")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	journalPath := filepath.Join(state, "leases.json")
	keyPath := filepath.Join(state, "leases.key")
	store, err := OpenCodexLeaseStore(fsutil.OSFileSystem{}, journalPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitLeases([]CodexTurnLease{testJournalLease(time.Now())}, 0); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	heldState := state + ".held"
	fys := &replaceLeaseDirectoryAfterReadsFS{OSFileSystem: fsutil.OSFileSystem{}, state: state, heldState: heldState}
	if _, err := OpenCodexLeaseStore(fys, journalPath, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement namespace journal error = %v, want not exist", err)
	}
	after, err := os.ReadFile(filepath.Join(heldState, filepath.Base(journalPath)))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(after, before) {
		t.Fatal("retained namespace journal was not updated during restart recovery")
	}
}

func TestCodexJournalRetentionAndLateResumeTombstone(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	store := openTestCodexLeaseStore(t, fsys)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	old := testJournalLease(now.Add(-8 * 24 * time.Hour))
	old.State = LeaseOrphaned
	old.RoutingRefs = 0
	old.ActiveAttempts = 0
	active := old
	active.Key.Turn = "active-turn"
	active.RoutingRefs = 1
	if err := store.CommitLeases([]CodexTurnLease{old, active}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Compact(now, DefaultCodexLeaseRetention); err != nil {
		t.Fatal(err)
	}
	if record, _, found := store.Lookup(old.Key, []codex.AccountKey{old.AccountKey}); found {
		t.Fatalf("expired record = %#v, found = %v", record, found)
	}
	activeRecord, _, found := store.Lookup(active.Key, []codex.AccountKey{active.AccountKey})
	if !found || activeRecord.State == LeaseExpired {
		t.Fatalf("active record = %#v, found = %v", activeRecord, found)
	}
}

func TestCodexJournalDurableWriteOrderAndPermissions(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	fsys := &recordingDurableFS{OSFileSystem: fsutil.OSFileSystem{}}
	store, err := OpenCodexLeaseStore(fsys, filepath.Join(dir, "leases.json"), filepath.Join(dir, "leases.key"))
	if err != nil {
		t.Fatal(err)
	}
	fsys.events = nil
	if err := store.CommitLeases([]CodexTurnLease{testJournalLease(time.Now())}, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "write", "sync-file", "close", "rename", "sync-dir"}
	if !slices.Equal(fsys.events, want) {
		t.Fatalf("events = %v, want %v", fsys.events, want)
	}
	for _, name := range []string{"leases.key", "leases.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o", info.Mode().Perm())
	}
}

func TestDurableAtomicWriteUsesUniqueExclusiveTemporaryFiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	fsys := &recordingDurableFS{OSFileSystem: fsutil.OSFileSystem{}}
	path := filepath.Join(dir, "state.json")
	if err := durableAtomicWrite(fsys, path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := durableAtomicWrite(fsys, path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if len(fsys.writePaths) != 2 {
		t.Fatalf("temporary paths = %v, want two", fsys.writePaths)
	}
	if fsys.writePaths[0] == fsys.writePaths[1] {
		t.Fatalf("temporary paths reused %q", fsys.writePaths[0])
	}
	for _, temporaryPath := range fsys.writePaths {
		if temporaryPath == path+".tmp" {
			t.Fatalf("fixed temporary path used %q", temporaryPath)
		}
		if filepath.Dir(temporaryPath) != dir {
			t.Fatalf("temporary path directory = %q, want %q", filepath.Dir(temporaryPath), dir)
		}
	}
}

func TestCodexJournalENOSPCDoesNotAdvanceGeneration(t *testing.T) {
	t.Parallel()
	fsys := &failingDurableFS{MemFS: fsutil.NewMemFS()}
	store := openTestCodexLeaseStore(t, fsys)
	fsys.failWrite = true
	if err := store.CommitLeases([]CodexTurnLease{testJournalLease(time.Now())}, 0); err == nil {
		t.Fatal("expected ENOSPC")
	}
	if store.Generation() != 0 {
		t.Fatalf("generation = %d", store.Generation())
	}
}

func TestCodexJournalIndeterminateCommitDoesNotAdvanceGeneration(t *testing.T) {
	t.Parallel()
	fsys := &failingDurableFS{MemFS: fsutil.NewMemFS()}
	store := openTestCodexLeaseStore(t, fsys)
	fsys.failSyncDir = true
	err := store.CommitLeases([]CodexTurnLease{testJournalLease(time.Now())}, 0)
	if !errors.Is(err, fsutil.ErrCommitIndeterminate) {
		t.Fatalf("commit error = %v, want ErrCommitIndeterminate", err)
	}
	if store.Generation() != 0 {
		t.Fatalf("generation = %d, want 0", store.Generation())
	}
}

func openTestCodexLeaseStore(t *testing.T, fsys fsutil.DurableFileSystem) *CodexLeaseStore {
	t.Helper()
	store, err := OpenCodexLeaseStore(fsys, "/state/leases.json", "/state/leases.key")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testJournalLease(now time.Time) CodexTurnLease {
	return CodexTurnLease{
		Key:                      testCodexLeaseKey("thread-secret", "turn-secret"),
		State:                    LeaseBoundActive,
		AccountKey:               "account-secret",
		Generation:               4,
		ModeEpoch:                2,
		Authoritative:            true,
		RoutingRefs:              1,
		ActiveAttempts:           1,
		UpstreamSocketGeneration: 23,
		ResponseAnchor:           "response-secret",
		TurnState:                "turn-state-secret",
		HasEncryptedState:        true,
		LastSeen:                 now,
	}
}

type recordingDurableFS struct {
	fsutil.OSFileSystem
	events     []string
	writePaths []string
}

type replaceLeaseDirectoryAfterReadsFS struct {
	fsutil.OSFileSystem
	state     string
	heldState string
	reads     int
}

func (fsys *replaceLeaseDirectoryAfterReadsFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &replaceLeaseDirectoryAfterReads{
		SecureDirectory: directory,
		fsys:            fsys,
	}, nil
}

type replaceLeaseDirectoryAfterReads struct {
	fsutil.SecureDirectory
	fsys *replaceLeaseDirectoryAfterReadsFS
}

func (directory *replaceLeaseDirectoryAfterReads) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *replaceLeaseDirectoryAfterReads) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *replaceLeaseDirectoryAfterReads) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *replaceLeaseDirectoryAfterReads) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	file, err := directory.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	directory.fsys.reads++
	if directory.fsys.reads == 2 {
		if err := os.Rename(directory.fsys.state, directory.fsys.heldState); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := os.Mkdir(directory.fsys.state, 0o700); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func (fsys *recordingDurableFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &recordingSecureDirectory{SecureDirectory: directory, fsys: fsys, path: path}, nil
}

type recordingSecureDirectory struct {
	fsutil.SecureDirectory
	fsys *recordingDurableFS
	path string
}

func (directory *recordingSecureDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	directory.fsys.events = append(directory.fsys.events, "create")
	directory.fsys.writePaths = append(directory.fsys.writePaths, filepath.Join(directory.path, name))
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	return &recordingDurableFile{DurableFile: file, events: &directory.fsys.events}, nil
}

func (directory *recordingSecureDirectory) Rename(oldName, newName string) error {
	directory.fsys.events = append(directory.fsys.events, "rename")
	return directory.SecureDirectory.Rename(oldName, newName)
}

func (directory *recordingSecureDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	directory.fsys.events = append(directory.fsys.events, "rename")
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *recordingSecureDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	directory.fsys.events = append(directory.fsys.events, "rename")
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *recordingSecureDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *recordingSecureDirectory) Sync() error {
	directory.fsys.events = append(directory.fsys.events, "sync-dir")
	return directory.SecureDirectory.Sync()
}

func (fsys *recordingDurableFS) MkdirAll(path string, mode os.FileMode) error {
	fsys.events = append(fsys.events, "mkdir")
	return fsys.OSFileSystem.MkdirAll(path, mode)
}

func (fsys *recordingDurableFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	fsys.events = append(fsys.events, "write")
	fsys.writePaths = append(fsys.writePaths, path)
	return fsys.OSFileSystem.WriteFile(path, data, mode)
}

func (fsys *recordingDurableFS) CreateExclusive(path string, mode os.FileMode) (fsutil.DurableFile, error) {
	fsys.events = append(fsys.events, "create")
	fsys.writePaths = append(fsys.writePaths, path)
	file, err := fsys.OSFileSystem.CreateExclusive(path, mode)
	if err != nil {
		return nil, err
	}
	return &recordingDurableFile{DurableFile: file, events: &fsys.events}, nil
}

func (fsys *recordingDurableFS) Chmod(path string, mode os.FileMode) error {
	if filepath.Ext(path) == ".tmp" {
		fsys.events = append(fsys.events, "chmod-file")
	} else {
		fsys.events = append(fsys.events, "chmod-dir")
	}
	return fsys.OSFileSystem.Chmod(path, mode)
}

func (fsys *recordingDurableFS) SyncFile(path string) error {
	fsys.events = append(fsys.events, "sync-file")
	return fsys.OSFileSystem.SyncFile(path)
}

func (fsys *recordingDurableFS) Rename(oldPath, newPath string) error {
	fsys.events = append(fsys.events, "rename")
	return fsys.OSFileSystem.Rename(oldPath, newPath)
}

func (fsys *recordingDurableFS) SyncDir(path string) error {
	fsys.events = append(fsys.events, "sync-dir")
	return fsys.OSFileSystem.SyncDir(path)
}

type failingDurableFS struct {
	*fsutil.MemFS
	failWrite   bool
	failSyncDir bool
}

func (fsys *failingDurableFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &failingSecureDirectory{SecureDirectory: directory, fsys: fsys}, nil
}

type failingSecureDirectory struct {
	fsutil.SecureDirectory
	fsys *failingDurableFS
}

func (directory *failingSecureDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (directory *failingSecureDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (directory *failingSecureDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *failingSecureDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	file, err := directory.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	return &failingDurableFile{DurableFile: file, failWrite: directory.fsys.failWrite}, nil
}

func (directory *failingSecureDirectory) Sync() error {
	if directory.fsys.failSyncDir {
		return errors.New("injected directory sync failure")
	}
	return directory.SecureDirectory.Sync()
}

func (fsys *failingDurableFS) CreateExclusive(path string, mode os.FileMode) (fsutil.DurableFile, error) {
	file, err := fsys.MemFS.CreateExclusive(path, mode)
	if err != nil {
		return nil, err
	}
	return &failingDurableFile{DurableFile: file, failWrite: fsys.failWrite}, nil
}

func (fsys *failingDurableFS) SyncDir(path string) error {
	if fsys.failSyncDir {
		return errors.New("injected directory sync failure")
	}
	return fsys.MemFS.SyncDir(path)
}

type recordingDurableFile struct {
	fsutil.DurableFile
	events *[]string
}

func (file *recordingDurableFile) Stat() (os.FileInfo, error) {
	inspector, ok := file.DurableFile.(fsutil.DurableFileInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}

func (file *recordingDurableFile) Write(data []byte) (int, error) {
	*file.events = append(*file.events, "write")
	return file.DurableFile.Write(data)
}

func (file *recordingDurableFile) Sync() error {
	*file.events = append(*file.events, "sync-file")
	return file.DurableFile.Sync()
}

func (file *recordingDurableFile) Close() error {
	*file.events = append(*file.events, "close")
	return file.DurableFile.Close()
}

type failingDurableFile struct {
	fsutil.DurableFile
	failWrite bool
}

func (file *failingDurableFile) Stat() (os.FileInfo, error) {
	inspector, ok := file.DurableFile.(fsutil.DurableFileInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}

func (file *failingDurableFile) Write(data []byte) (int, error) {
	if file.failWrite {
		return 0, errors.New("ENOSPC")
	}
	return file.DurableFile.Write(data)
}

func (fsys *failingDurableFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	if fsys.failWrite {
		return errors.New("ENOSPC")
	}
	return fsys.MemFS.WriteFile(path, data, mode)
}
