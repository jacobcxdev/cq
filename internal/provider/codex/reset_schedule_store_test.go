package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/quota"
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

func TestResetAutomationStoreRejectsInvalidPacePhase(t *testing.T) {
	store := NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "state", "schedule.json"))
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	_, err := store.Update(context.Background(), func(state *ResetAutomationState) error {
		state.NextPace = &quota.ResetPacePhase{From: now, Until: now.Add(time.Hour), Percent: -1}
		return nil
	})
	if err == nil {
		t.Fatal("stored negative pace target")
	}
	state, err := store.Load()
	if err != nil || state.Revision != 0 || state.NextPace != nil {
		t.Fatalf("invalid pace changed the saved schedule: %+v, %v", state, err)
	}
}

// These are the exact top-level fields the previous executable accepts.
type resetScheduleV1 struct {
	Version      int                    `json:"version"`
	Revision     uint64                 `json:"revision"`
	Enabled      bool                   `json:"enabled"`
	Mode         ResetAutomationMode    `json:"mode"`
	Entries      []ResetAutomationEntry `json:"entries"`
	LastReplanAt *time.Time             `json:"last_replan_at,omitempty"`
	PlanValid    bool                   `json:"plan_valid"`
	PlanError    string                 `json:"plan_error,omitempty"`
	NextReplanAt *time.Time             `json:"next_replan_at,omitempty"`
}

func readResetScheduleV1(t *testing.T, path string) resetScheduleV1 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state resetScheduleV1
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		t.Fatalf("predecessor rejected updated schedule: %v", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		t.Fatal("predecessor rejected trailing schedule data")
	}
	return state
}

func TestResetAutomationStoreKeepsV1CompatibleMetadataAcrossInterruptedCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "schedule.json")
	fs := &resetScheduleFaultFS{}
	store := NewResetAutomationStore(fs, path)
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
	pace := quota.ResetPacePhase{From: now, Until: now.Add(time.Hour), Percent: 75}
	ctx := context.Background()
	if _, err := store.Update(ctx, func(state *ResetAutomationState) error {
		state.Mode, state.Enabled, state.AutoEnrol = ResetAutomationAdaptive, true, true
		state.NextPace = &pace
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	legacy := readResetScheduleV1(t, path)
	if legacy.Version != 1 || legacy.Revision != 1 || !legacy.Enabled || legacy.Mode != ResetAutomationAdaptive {
		t.Fatalf("unexpected v1 schedule: %+v", legacy)
	}
	restarted := NewResetAutomationStore(fsutil.OSFileSystem{}, path)
	assertCommitted := func() {
		t.Helper()
		state, err := restarted.Load()
		if err != nil || state.Revision != 1 || !state.AutoEnrol || state.NextPace == nil || *state.NextPace != pace {
			t.Fatalf("lost committed schedule metadata: %+v, %v", state, err)
		}
	}
	assertCommitted()

	injected := errors.New("interrupted schedule commit")
	fs.beforeRename = func(name string) error {
		if name == filepath.Base(path) {
			return injected
		}
		return nil
	}
	if _, err := store.Update(ctx, func(state *ResetAutomationState) error {
		state.NextPace.Percent = 80
		return nil
	}); !errors.Is(err, injected) {
		t.Fatalf("interrupted commit = %v", err)
	}
	assertCommitted()

	// The predecessor may write the same revision and fields as the failed
	// update. Its document must still fail to match the prepared metadata.
	legacy.Revision++
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.SecureAtomicWrite(fsutil.OSFileSystem{}, path, data); err != nil {
		t.Fatal(err)
	}
	state, err := restarted.Load()
	if err != nil || state.Revision != 2 || !state.Enabled || state.AutoEnrol || state.NextPace != nil {
		t.Fatalf("predecessor edit inherited schedule metadata: %+v, %v", state, err)
	}
}

type resetScheduleFaultFS struct {
	fsutil.OSFileSystem
	beforeRename func(string) error
}

func (fs *resetScheduleFaultFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	dir, err := fs.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &resetScheduleFaultDirectory{dir, dir.(fsutil.IdentityBoundRenamer), dir.(fsutil.IdentityBoundRemover), fs.beforeRename}, nil
}

type resetScheduleFaultDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	beforeRename func(string) error
}

func (d *resetScheduleFaultDirectory) RenameChecked(oldName, newName string, identity fsutil.SecureFileIdentity) error {
	if d.beforeRename != nil {
		if err := d.beforeRename(newName); err != nil {
			return err
		}
	}
	return d.IdentityBoundRenamer.RenameChecked(oldName, newName, identity)
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
