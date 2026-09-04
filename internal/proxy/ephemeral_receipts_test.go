package proxy

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
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
			if err := closeStore(); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{
				filepath.Join(directory, test.prefix+expiredID+".json"), corruptPath,
			} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("pruned receipt %q still exists: %v", filepath.Base(path), err)
				}
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expired receipts remaining = %d", len(entries))
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
