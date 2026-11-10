package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// Block actual durability barriers, rather than a runtime callback. Both the
// coordinator and its owned store must wait until a journal operation finishes
// syncing before they release directory ownership or report shutdown complete.
func TestCodexLeaseDeltaCloseWaitsForDurableMutation(t *testing.T) {
	for _, owner := range []string{"coordinator", "store"} {
		for _, stage := range []string{"candidate_file_sync", "head_directory_sync"} {
			t.Run(owner+"/"+stage, func(t *testing.T) {
				coordinator, fsys, now := openSizedCodexDeltaCoordinator(t, 2)
				store := coordinator.Store()
				expected := store.Generation() + 1
				*now = now.Add(8 * 24 * time.Hour)
				directory := store.directory
				gate := &deltaCloseSyncDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), journal: store.journalName, stage: stage, entered: make(chan struct{}), release: make(chan struct{})}
				store.directory = gate
				t.Cleanup(gate.unblock)
				mutated := make(chan error, 1)
				go func() {
					defer func() {
						if recover() != nil {
							mutated <- errors.New("journal mutation panicked")
						}
					}()
					mutated <- store.Compact(*now, DefaultCodexLeaseRetention)
				}()
				select {
				case <-gate.entered:
				case err := <-mutated:
					t.Fatalf("mutation missed %s: %v", stage, err)
				case <-time.After(2 * time.Second):
					t.Fatalf("mutation never reached %s", stage)
				}
				select {
				case err := <-mutated:
					t.Fatalf("mutation acknowledged before %s finished: %v", stage, err)
				default:
				}
				closed := make(chan error, 1)
				go func() {
					defer func() {
						if recover() != nil {
							closed <- errors.New("journal close panicked")
						}
					}()
					if owner == "store" {
						closed <- store.close()
					} else {
						closed <- coordinator.Close()
					}
				}()
				// A pending or held writer proves Close reached the store lifecycle
				// barrier. No timing-based sleep is needed to infer that Close started.
				deadline := time.Now().Add(2 * time.Second)
				for store.lifecycle.TryRLock() {
					store.lifecycle.RUnlock()
					select {
					case err := <-closed:
						t.Fatalf("Close crossed unfinished %s: %v", stage, err)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatal("Close never reached lifecycle barrier")
					}
					runtime.Gosched()
				}
				select {
				case err := <-closed:
					t.Fatalf("Close crossed unfinished %s: %v", stage, err)
				default:
				}
				select {
				case err := <-mutated:
					t.Fatalf("mutation acknowledged blocked %s: %v", stage, err)
				default:
				}
				gate.unblock()
				select {
				case err := <-mutated:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("mutation did not finish after sync release")
				}
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Close did not finish after durable mutation")
				}
				if gate.candidateSynced.Load() == 0 || gate.headSynced.Load() == 0 {
					t.Fatal("Close returned without candidate and head durability barriers")
				}
				var head codexLeaseStorageHead
				if err := json.Unmarshal(fsysFileBytes(t, fsys, "/state/leases.json"), &head); err != nil {
					t.Fatal(err)
				}
				if head.Generation != expected {
					t.Fatalf("closed durable generation=%d, want acknowledged %d", head.Generation, expected)
				}
				recovered := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
				if recovered.Store().Generation() != expected {
					t.Fatalf("recovered generation=%d, want acknowledged %d", recovered.Store().Generation(), expected)
				}
				if len(recovered.Store().v2.Records) != 0 || len(recovered.Store().v2.Lanes) != 0 {
					t.Fatal("recovery lost acknowledged collection of expired idle history")
				}
			})
		}
	}
}

type deltaCloseSyncDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	journal         string
	stage           string
	entered         chan struct{}
	release         chan struct{}
	once            sync.Once
	releaseOnce     sync.Once
	headPublished   atomic.Bool
	candidateSynced atomic.Int32
	headSynced      atomic.Int32
}

func (d *deltaCloseSyncDirectory) unblock() { d.releaseOnce.Do(func() { close(d.release) }) }
func (d *deltaCloseSyncDirectory) wait()    { d.once.Do(func() { close(d.entered); <-d.release }) }
func (d *deltaCloseSyncDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	file, err := d.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	head := strings.HasPrefix(name, "."+d.journal+".")
	return &deltaCloseSyncFile{DurableFile: file, directory: d, head: head}, nil
}
func (d *deltaCloseSyncDirectory) RenameChecked(oldName, newName string, id fsutil.SecureFileIdentity) error {
	err := d.IdentityBoundRenamer.RenameChecked(oldName, newName, id)
	if err == nil && newName == d.journal {
		d.headPublished.Store(true)
	}
	return err
}
func (d *deltaCloseSyncDirectory) Sync() error {
	head := d.headPublished.Load()
	if head && d.stage == "head_directory_sync" {
		d.wait()
	}
	if err := d.SecureDirectory.Sync(); err != nil {
		return err
	}
	if head {
		d.headSynced.Add(1)
	}
	return nil
}

type deltaCloseSyncFile struct {
	fsutil.DurableFile
	directory *deltaCloseSyncDirectory
	head      bool
}

func (f *deltaCloseSyncFile) Stat() (os.FileInfo, error) {
	return f.DurableFile.(fsutil.DurableFileInspector).Stat()
}
func (f *deltaCloseSyncFile) Sync() error {
	if !f.head && f.directory.stage == "candidate_file_sync" {
		f.directory.wait()
	}
	if err := f.DurableFile.Sync(); err != nil {
		return err
	}
	if !f.head {
		f.directory.candidateSynced.Add(1)
	}
	return nil
}
