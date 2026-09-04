package proxy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestEphemeralReceiptStoresPruneExpiredAndCorruptReceipts(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		open   func(string, time.Time) (func() error, error)
	}{
		{
			name: "admission", prefix: "consumed-",
			open: func(path string, _ time.Time) (func() error, error) {
				store, err := OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, path)
				return store.Close, err
			},
		},
		{
			name: "dispatch", prefix: "dispatch-permit-",
			open: func(path string, now time.Time) (func() error, error) {
				store, err := OpenCallerDispatchPermitStore(
					fsutil.OSFileSystem{}, path, bytes.Repeat([]byte{0x41}, 32),
					func() time.Time { return now }, bytes.NewReader(bytes.Repeat([]byte{0x42}, 16)),
				)
				return store.Close, err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			directory := filepath.Join(t.TempDir(), "receipts")
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			expiredID := strings.Repeat("a", 32)
			liveID := strings.Repeat("b", 32)
			corruptID := strings.Repeat("c", 32)
			writeEphemeralReceiptFixture(t, directory, test.prefix, expiredID, now.Add(-2*ephemeralReceiptExpiryGrace), test.name)
			writeEphemeralReceiptFixture(t, directory, test.prefix, liveID, now.Add(time.Hour), test.name)
			corruptPath := filepath.Join(directory, test.prefix+corruptID+".json")
			if err := os.WriteFile(corruptPath, []byte("not-json"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := now.Add(-ephemeralReceiptCorruptMaxAge - time.Hour)
			if err := os.Chtimes(corruptPath, old, old); err != nil {
				t.Fatal(err)
			}
			unrelatedPath := filepath.Join(directory, "operator-note.txt")
			if err := os.WriteFile(unrelatedPath, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}

			closeStore, err := test.open(directory, now)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{
				filepath.Join(directory, test.prefix+expiredID+".json"), corruptPath,
			} {
				waitForEphemeralReceiptCondition(t, func() bool {
					_, err := os.Stat(path)
					return os.IsNotExist(err)
				}, "pruned receipt "+filepath.Base(path))
			}
			if err := closeStore(); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{
				filepath.Join(directory, test.prefix+liveID+".json"), unrelatedPath,
			} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("retained file %q: %v", filepath.Base(path), err)
				}
			}
		})
	}
}

func TestEphemeralReceiptHealthTracksCurrentPruneFailure(t *testing.T) {
	var state proxyEphemeralStateObservability
	state.recordPruneStart(ephemeralReceiptAdmission)
	state.recordPruneStart(ephemeralReceiptDispatch)
	active := state.snapshot()
	if !active.AdmissionActive || !active.DispatchActive {
		t.Fatalf("active prune health = %#v", active)
	}
	state.recordScan(ephemeralReceiptAdmission, 3, 0, os.ErrPermission)
	state.recordScan(ephemeralReceiptDispatch, 4, 0, os.ErrPermission)
	failed := state.snapshot()
	if !failed.AdmissionFailed || !failed.DispatchFailed || failed.PruneErrors != 2 {
		t.Fatalf("failed prune health = %#v", failed)
	}
	state.recordScan(ephemeralReceiptAdmission, 2, 1, nil)
	state.recordScan(ephemeralReceiptDispatch, 3, 1, nil)
	recovered := state.snapshot()
	if recovered.AdmissionFailed || recovered.DispatchFailed || recovered.PruneErrors != 2 || recovered.PrunedReceipts != 2 {
		t.Fatalf("recovered prune health = %#v", recovered)
	}
}

func TestEphemeralReceiptPruneDoesNotSkipDeletedDirectoryBatches(t *testing.T) {
	now := time.Now().UTC()
	directory := filepath.Join(t.TempDir(), "receipts")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2*ephemeralReceiptScanBatch+17; index++ {
		id := fmt.Sprintf("%032x", index)
		writeEphemeralReceiptFixture(t, directory, "consumed-", id, now.Add(-2*ephemeralReceiptExpiryGrace), "admission")
	}
	store, err := OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, directory)
	if err != nil {
		t.Fatal(err)
	}
	waitForEphemeralReceiptCondition(t, func() bool {
		entries, readErr := os.ReadDir(directory)
		return readErr == nil && len(entries) == 0
	}, "all expired receipt batches pruned")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEphemeralReceiptPruneDoesNotBlockStartupOrRequest(t *testing.T) {
	tests := []struct {
		name string
		open func(*blockingEphemeralReceiptFS, string) (func() error, func() error, error)
	}{
		{
			name: "admission",
			open: func(fsys *blockingEphemeralReceiptFS, path string) (func() error, func() error, error) {
				store, err := OpenNormalCallerAdmissionStore(fsys, path)
				return store.Close, func() error {
					return store.Consume(context.Background(), ProviderBranchAdmissionConsumptionV1{
						SchemaVersion:     1,
						AdmissionID:       strings.Repeat("d", 32),
						ConsumptionDigest: "digest",
						MAC:               "mac",
					})
				}, err
			},
		},
		{
			name: "dispatch",
			open: func(fsys *blockingEphemeralReceiptFS, path string) (func() error, func() error, error) {
				store, err := OpenCallerDispatchPermitStore(
					fsys, path, bytes.Repeat([]byte{0x41}, 32), time.Now,
					bytes.NewReader(bytes.Repeat([]byte{0x42}, 16)),
				)
				return store.Close, func() error {
					_, issueErr := store.IssueAndConsume(context.Background(), CallerDispatchPermitRequestV2{
						CallerAdmissionDigest: strings.Repeat("a", 64),
						CallerDomain:          NormalCallerCodex,
						CallerSubjectID:       "caller-safe-id",
						SessionDigest:         strings.Repeat("b", 64),
						PoolID:                testPoolIDA,
						RoutingGeneration:     7,
						AllowedAccounts:       []codex.AccountKey{"account-a"},
						SelectedAccount:       "account-a",
					})
					return issueErr
				}, err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipts")
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			fsys := &blockingEphemeralReceiptFS{started: make(chan struct{}), release: make(chan struct{})}
			type openResult struct {
				close   func() error
				operate func() error
				err     error
			}
			opened := make(chan openResult, 1)
			go func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						opened <- openResult{err: fmt.Errorf("open panic: %v", recovered)}
					}
				}()
				closeStore, operate, err := test.open(fsys, path)
				opened <- openResult{close: closeStore, operate: operate, err: err}
			}()
			var result openResult
			select {
			case result = <-opened:
				if result.err != nil {
					t.Fatal(result.err)
				}
			case <-time.After(2 * time.Second):
				close(fsys.release)
				t.Fatal("store open blocked on receipt prune")
			}
			select {
			case <-fsys.started:
			case <-time.After(2 * time.Second):
				close(fsys.release)
				_ = result.close()
				t.Fatal("background receipt prune did not start")
			}
			operated := make(chan error, 1)
			go func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						operated <- fmt.Errorf("operation panic: %v", recovered)
					}
				}()
				operated <- result.operate()
			}()
			select {
			case err := <-operated:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				close(fsys.release)
				_ = result.close()
				t.Fatal("receipt creation blocked on prune")
			}
			close(fsys.release)
			if err := result.close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type blockingEphemeralReceiptFS struct {
	fsutil.OSFileSystem
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (fsys *blockingEphemeralReceiptFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &blockingEphemeralReceiptDirectory{SecureDirectory: directory, fsys: fsys}, nil
}

type blockingEphemeralReceiptDirectory struct {
	fsutil.SecureDirectory
	fsys *blockingEphemeralReceiptFS
}

func (directory *blockingEphemeralReceiptDirectory) VisitEntries(batchSize int, visit func(os.DirEntry) error) error {
	directory.fsys.once.Do(func() { close(directory.fsys.started) })
	<-directory.fsys.release
	return directory.SecureDirectory.(fsutil.SecureDirectoryVisitor).VisitEntries(batchSize, visit)
}

func (directory *blockingEphemeralReceiptDirectory) RemoveChecked(name string, identity fsutil.SecureFileIdentity) error {
	return directory.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, identity)
}

func waitForEphemeralReceiptCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeEphemeralReceiptFixture(t *testing.T, directory, prefix, id string, validUntil time.Time, kind string) {
	t.Helper()
	document := `{"admission_id":"` + id + `","valid_until":"` + validUntil.Format(time.RFC3339Nano) + `"}`
	if kind == "dispatch" {
		document = `{"permit_id":"` + id + `","valid_until":"` + validUntil.Format(time.RFC3339Nano) + `"}`
	}
	if err := os.WriteFile(filepath.Join(directory, prefix+id+".json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}
