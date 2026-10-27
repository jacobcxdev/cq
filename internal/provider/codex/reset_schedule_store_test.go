package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestResetAutomationStoreDefaultsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "schedule.json")
	store := NewResetAutomationStore(fsutil.OSFileSystem{}, path)
	state, err := store.Load()
	if err != nil || state.Enabled || state.Mode != ResetAutomationFixed || state.Revision != 0 {
		t.Fatalf("defaults = %+v, %v", state, err)
	}
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	_, err = store.Update(context.Background(), func(s *ResetAutomationState) error {
		s.Enabled = true
		s.Entries = append(s.Entries, ResetAutomationEntry{ID: "entry", AccountKey: "account", CreditID: "credit", At: &now, State: ResetAutomationPending})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err = NewResetAutomationStore(fsutil.OSFileSystem{}, path).Load()
	if err != nil || !state.Enabled || state.Revision != 1 || len(state.Entries) != 1 || state.Entries[0].CreditID != "credit" {
		t.Fatalf("restart = %+v, %v", state, err)
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("permissions = %v, %v", info, err)
	}
}

func TestResetAutomationStoreRejectsCorruptionAndInvalidMutations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "schedule.json")
	store := NewResetAutomationStore(fsutil.OSFileSystem{}, path)
	for _, data := range []string{`{`, `{"version":99}`, `{"version":1,"mode":"spend_everything"}`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update(context.Background(), func(s *ResetAutomationState) error {
		s.Mode = "invalid"
		return nil
	})
	if err == nil {
		t.Fatal("accepted invalid mode")
	}
	state, err := store.Load()
	if err != nil || state.Revision != 0 || state.Enabled {
		t.Fatalf("invalid mutation persisted: %+v, %v", state, err)
	}
}

func TestResetAutomationStoreSerialisesProcessesAndHonoursContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "schedule.json")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := NewResetAutomationStore(fsutil.OSFileSystem{}, path).Update(context.Background(), func(*ResetAutomationState) error { return nil })
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	store := NewResetAutomationStore(fsutil.OSFileSystem{}, path)
	state, err := store.Load()
	if err != nil || state.Revision != 8 {
		t.Fatalf("lost update: %+v, %v", state, err)
	}
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = store.Update(ctx, func(*ResetAutomationState) error { t.Error("mutation ran while locked"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", err)
	}
}

func TestResetAutomationStoreRejectsDuplicateAndUnsafeState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "schedule.json")
	store := NewResetAutomationStore(fsutil.OSFileSystem{}, path)
	at := time.Now().UTC()
	entry := ResetAutomationEntry{ID: "one", AccountKey: "account", CreditID: "credit", At: &at, State: ResetAutomationPending}
	for _, duplicateID := range []bool{false, true} {
		_, err := store.Update(context.Background(), func(state *ResetAutomationState) error {
			other := entry
			if !duplicateID {
				other.ID = "two"
			}
			state.Entries = []ResetAutomationEntry{entry, other}
			return nil
		})
		if err == nil {
			t.Fatal("duplicate accepted")
		}
	}
	if _, err := store.Update(context.Background(), func(state *ResetAutomationState) error { state.Entries = []ResetAutomationEntry{entry}; return nil }); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	} // POSIX modes do not change Windows ACLs.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("insecure schedule accepted")
	}
}
