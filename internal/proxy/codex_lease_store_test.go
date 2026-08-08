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

func TestCodexJournalRetentionAndLateResumeTombstone(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	store := openTestCodexLeaseStore(t, fsys)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	old := testJournalLease(now.Add(-8 * 24 * time.Hour))
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
	record, _, found := store.Lookup(old.Key, []codex.AccountKey{old.AccountKey})
	if !found || record.State != LeaseExpired {
		t.Fatalf("expired record = %#v, found = %v", record, found)
	}
	activeRecord, _, found := store.Lookup(active.Key, []codex.AccountKey{active.AccountKey})
	if !found || activeRecord.State == LeaseExpired {
		t.Fatalf("active record = %#v, found = %v", activeRecord, found)
	}
	if err := store.Compact(now.Add(8*24*time.Hour), DefaultCodexLeaseRetention); err != nil {
		t.Fatal(err)
	}
	if _, _, found := store.Lookup(old.Key, []codex.AccountKey{old.AccountKey}); found {
		t.Fatal("expired tombstone not compacted")
	}
}

func TestCodexJournalDurableWriteOrderAndPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fsys := &recordingDurableFS{OSFileSystem: fsutil.OSFileSystem{}}
	store, err := OpenCodexLeaseStore(fsys, filepath.Join(dir, "leases.json"), filepath.Join(dir, "leases.key"))
	if err != nil {
		t.Fatal(err)
	}
	fsys.events = nil
	if err := store.CommitLeases([]CodexTurnLease{testJournalLease(time.Now())}, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"mkdir", "chmod-dir", "write", "chmod-file", "sync-file", "rename", "sync-dir"}
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
	events []string
}

func (fsys *recordingDurableFS) MkdirAll(path string, mode os.FileMode) error {
	fsys.events = append(fsys.events, "mkdir")
	return fsys.OSFileSystem.MkdirAll(path, mode)
}

func (fsys *recordingDurableFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	fsys.events = append(fsys.events, "write")
	return fsys.OSFileSystem.WriteFile(path, data, mode)
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
	failWrite bool
}

func (fsys *failingDurableFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	if fsys.failWrite {
		return errors.New("ENOSPC")
	}
	return fsys.MemFS.WriteFile(path, data, mode)
}
