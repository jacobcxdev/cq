//go:build darwin

package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

type deltaNativeReadDirectory struct {
	*deltaCountingDirectory
	readBytes *int64
	opens     *int
}

func (d *deltaNativeReadDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	f, err := d.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	*d.opens++
	return &deltaNativeReadFile{SecureReadFile: f, readBytes: d.readBytes}, nil
}

type deltaNativeReadFile struct {
	fsutil.SecureReadFile
	readBytes *int64
}

func (f *deltaNativeReadFile) Read(data []byte) (int, error) {
	n, err := f.SecureReadFile.Read(data)
	*f.readBytes += int64(n)
	return n, err
}

func TestCodexLeaseSnapshotWriteWorkload(t *testing.T) {
	if os.Getenv("CQ_TEST_WRITE_WORKLOAD") != "1" {
		t.Skip("set CQ_TEST_WRITE_WORKLOAD=1 for bounded native write qualification")
	}
	library, err := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(library)
	var rusage func(int32, int32, unsafe.Pointer) int32
	purego.RegisterLibFunc(&rusage, library, "proc_pid_rusage")
	diskBytes := func() uint64 {
		var usage [20]uint64
		if rusage(int32(os.Getpid()), 2, unsafe.Pointer(&usage)) != 0 {
			t.Fatal("proc_pid_rusage failed")
		}
		return usage[19]
	}
	seed, _, clock := openSizedCodexDeltaCoordinator(t, 878)
	seedStore := seed.Store()
	nearFiles, nearHead, nearCount := codexLeaseNearBudgetImage(t, seedStore)
	t.Logf("near_budget_fixture deltas=%d files=%d logical_bytes=%d", nearCount, len(nearFiles), len(seedStore.journalBytes))
	type result struct {
		written int64
		disk    uint64
		read    int64
		opens   int
		wall    time.Duration
		cpu     time.Duration
	}
	results := map[string]result{}
	for _, mode := range []string{"full", "incremental", "near_budget"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			canonical := seedStore.journalBytes
			if mode == "near_budget" {
				canonical = nearHead
				for name, data := range nearFiles {
					if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(root, "leases.key"), seedStore.key, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "leases.json"), canonical, 0o600); err != nil {
				t.Fatal(err)
			}
			options := CodexContinuityOpenOptions{FS: fsutil.OSFileSystem{}, KeyPath: filepath.Join(root, "leases.key"), JournalPath: filepath.Join(root, "leases.json"), Policy: CodexLeasePolicy{Retention: 7 * 24 * time.Hour, Now: func() time.Time { return *clock }}, Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}}}
			coordinator, err := OpenCodexContinuityCoordinator(options, codexLeaseV2CASTestOwner{})
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Close()
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("native-write-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "full" {
				if err := store.persistLegacyV2Locked(*store.v2, bytes.Clone(store.journalBytes), store.revalidateV2InstalledLocked); err != nil {
					t.Fatal(err)
				}
				store.delta = nil
				if err := store.pruneDeltaOrphansLocked(); err != nil {
					t.Fatal(err)
				}
			}
			directory := store.directory
			written := new(int64)
			read := new(int64)
			opens := new(int)
			store.directory = &deltaNativeReadDirectory{deltaCountingDirectory: &deltaCountingDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), written: written}, readBytes: read, opens: opens}
			fence, err := handle.refreshMutationFence()
			if err != nil {
				t.Fatal(err)
			}
			desired := codexLeaseRuntimeMutationRecord(handle.record)
			desired.State = LeaseBoundActive
			desired.AttemptRefs++
			desired.ResponseObserverRefs++
			for index := range desired.Attempts {
				if desired.Attempts[index].Generation == desired.CurrentAttemptGeneration {
					desired.Attempts[index].State = CodexAttemptStreaming
				}
			}
			*written = 0
			*read = 0
			*opens = 0
			beforeDisk := diskBytes()
			var beforeCPU, afterCPU unix.Rusage
			if err := unix.Getrusage(unix.RUSAGE_SELF, &beforeCPU); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			store.mu.Lock()
			next, _, err := store.applyCodexLaneMutationLocked(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}})
			if err == nil {
				var data []byte
				next, data, err = store.prepareV2Envelope(next)
				if err == nil {
					if mode == "full" {
						err = store.persistLegacyV2Locked(next, data, store.revalidateV2InstalledLocked)
					} else {
						err = store.persistPreparedV2Locked(next, data, store.revalidateV2InstalledLocked)
					}
				}
			}
			store.mu.Unlock()
			wall := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			if err := unix.Getrusage(unix.RUSAGE_SELF, &afterCPU); err != nil {
				t.Fatal(err)
			}
			cpu := time.Duration(afterCPU.Utime.Nano() + afterCPU.Stime.Nano() - beforeCPU.Utime.Nano() - beforeCPU.Stime.Nano())
			result := result{written: *written, disk: diskBytes() - beforeDisk, read: *read, opens: *opens, wall: wall, cpu: cpu}
			results[mode] = result
			references := 0
			if store.delta != nil {
				references = len(store.delta.refs)
			}

			var retained, allocated int64
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil {
					t.Fatal(err)
				}
				retained += info.Size()
				allocated += info.Sys().(*syscall.Stat_t).Blocks * 512
			}
			if len(entries) != references+2 {
				t.Fatalf("unreferenced retained files: entries=%d references=%d", len(entries), references)
			}
			t.Logf("mutations=1 submitted_bytes=%d disk_write_bytes=%d read_bytes=%d opens=%d retained_logical_bytes=%d retained_file_bytes=%d allocated_file_bytes=%d references=%d wall=%s cpu=%s", result.written, result.disk, result.read, result.opens, len(store.journalBytes), retained, allocated, references, result.wall, result.cpu)
			if mode != "full" && result.written > codexLeaseSegmentMaxBytes+codexLeaseHeadMaxBytes {
				t.Fatal("ordinary transition rewrote retained history")
			}
			if mode == "near_budget" && references > 256 {
				t.Fatalf("unbounded segment references: %d", references)
			}
			identity := handle.identity
			if err := coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenCodexContinuityCoordinator(options, codexLeaseV2CASTestOwner{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			record := findCodexLeaseV2CASTestRecord(t, reopened.Store().v2.Records, identity)
			if !record.EverAdmitted || record.State != LeaseOrphaned || !record.SocketLineageExtinct {
				t.Fatal("admission did not survive recovery")
			}
		})
	}
	for _, mode := range []string{"incremental", "near_budget"} {
		before, after := results["full"], results[mode]
		if after.written*50 >= before.written {
			t.Fatalf("%s did not reduce writes by at least98%%", mode)
		}
		t.Logf("%s disk_reduction=%.3f%% wall_ratio=%.3f cpu_ratio=%.3f", mode, 100*(1-float64(after.disk)/float64(before.disk)), float64(after.wall)/float64(before.wall), float64(after.cpu)/float64(before.cpu))
	}
}
