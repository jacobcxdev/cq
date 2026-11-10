package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCodexLeaseMutationPublishesIncrementalHead(t *testing.T) {
	coordinator, fsys, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	plan := codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
	handle, err := runtimeLease.BeginRequest(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Version    int    `json:"version"`
		Checkpoint string `json:"checkpoint"`
		Generation uint64 `json:"generation"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatal(err)
	}
	if head.Version != 4 || head.Checkpoint == "" || head.Generation != coordinator.Store().Generation() {
		t.Fatalf("mutation persisted full snapshot: version=%d checkpoint=%q", head.Version, head.Checkpoint)
	}
	if bytes.Contains(data, []byte(`"records"`)) {
		t.Fatal("head contains retained history")
	}
	if _, err := handle.AdmitHTTP2xx(); err != nil {
		t.Fatal(err)
	}
}

type deltaCountingDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	written *int64
}

func (d *deltaCountingDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	f, err := d.SecureDirectory.CreateExclusive(name, mode)
	if err != nil {
		return nil, err
	}
	return &deltaCountingFile{DurableFile: f, written: d.written}, nil
}

type deltaCountingFile struct {
	fsutil.DurableFile
	written *int64
}

func (f *deltaCountingFile) Write(data []byte) (int, error) {
	n, err := f.DurableFile.Write(data)
	*f.written += int64(n)
	return n, err
}
func (f *deltaCountingFile) Stat() (os.FileInfo, error) {
	return f.DurableFile.(fsutil.DurableFileInspector).Stat()
}

func countCodexDeltaWrites(store *CodexLeaseStore) *int64 {
	written := new(int64)
	directory := store.directory
	store.directory = &deltaCountingDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), written: written}
	return written
}

func openSizedCodexDeltaCoordinator(t *testing.T, lanes int) (*CodexContinuityCoordinator, *fsutil.MemFS, *time.Time) {
	t.Helper()
	payload, key, now := codexLeaseWriteWorkloadFixtureSize(t, lanes)
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.json", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	clock := now
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{FS: fsys, KeyPath: "/state/leases.key", JournalPath: "/state/leases.json", Policy: CodexLeasePolicy{Retention: 7 * 24 * time.Hour, Now: func() time.Time { return clock }}, Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}}}, codexLeaseV2CASTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator, fsys, &clock
}

func TestCodexLeaseDeltaWriteCostDoesNotScaleWithRetainedHistory(t *testing.T) {
	var smallBytes int64
	for _, lanes := range []int{2, 878} {
		t.Run(fmt.Sprintf("lanes_%d", lanes), func(t *testing.T) {
			coordinator, _, _ := openSizedCodexDeltaCoordinator(t, lanes)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			store := coordinator.Store()
			plan := codexLeaseRuntimeTestPlan("workload-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
			handle, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			written := countCodexDeltaWrites(store)
			generation := store.Generation()
			retained := len(store.journalBytes)
			handle, err = handle.AdmitHTTP2xx()
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = handle.Drain(); err != nil {
				t.Fatal(err)
			}
			t.Logf("retained_bytes=%d mutations=%d submitted_bytes=%d", retained, store.Generation()-generation, *written)
			if *written > 64<<10 {
				t.Fatalf("three mutations exceeded bounded write budget: %d", *written)
			}
			if lanes == 2 {
				smallBytes = *written
			} else if *written > smallBytes+4096 {
				t.Fatalf("writes grew with history: small=%d large=%d", smallBytes, *written)
			}
		})
	}
}

func TestCodexLeaseDeltaCheckpointBudgetAndRecovery(t *testing.T) {
	coordinator, fsys, now := openSizedCodexDeltaCoordinator(t, 2)
	store := coordinator.Store()
	written := countCodexDeltaWrites(store)
	initial := len(store.journalBytes)
	generation := store.Generation()
	checkpoints := 0
	previous := store.delta.head.Checkpoint
	for range 100 {
		store.mu.Lock()
		err := store.commitV2Locked(store.v2.Generation, *store.v2)
		store.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if store.delta.head.Checkpoint != previous {
			checkpoints++
			previous = store.delta.head.Checkpoint
		}
	}
	if checkpoints < 1 {
		t.Fatal("byte budget never checkpointed")
	}
	if *written > 100*(codexLeaseSegmentMaxBytes+codexLeaseHeadMaxBytes)+int64(initial) {
		t.Fatalf("checkpoint write budget grew with history: %d", *written)
	}
	if len(store.delta.refs) > initial/200+2 {
		t.Fatalf("unbounded checkpoint references: %d", len(store.delta.refs))
	}
	want := bytes.Clone(store.journalBytes)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	if reopened.Store().Generation() != generation+100 || !bytes.Equal(want, reopened.Store().journalBytes) {
		t.Fatal("recovery changed quiescent committed authority")
	}
	entries, err := fsys.ReadDir("/state")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(reopened.Store().delta.refs)+2 {
		t.Fatalf("unreferenced journal files remain: entries=%d references=%d", len(entries), len(reopened.Store().delta.refs))
	}
	t.Logf("checkpoint_count=%d submitted_bytes=%d retained_bytes=%d", checkpoints, *written, len(want))
}

type deltaFaultDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	journal       string
	stage         string
	armed         bool
	headPublished bool
}

func (d *deltaFaultDirectory) RenameChecked(oldName, newName string, identity fsutil.SecureFileIdentity) error {
	if d.armed && newName == d.journal && d.stage == "before_head" {
		d.armed = false
		return io.ErrUnexpectedEOF
	}
	err := d.IdentityBoundRenamer.RenameChecked(oldName, newName, identity)
	if err == nil && newName == d.journal {
		d.headPublished = true
		if d.armed && d.stage == "after_head" {
			d.armed = false
			return &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: "injected post-head rename", Err: io.ErrUnexpectedEOF}
		}
	}
	return err
}
func (d *deltaFaultDirectory) Sync() error {
	if d.armed && ((d.stage == "candidate_sync" && !d.headPublished) || (d.stage == "head_sync" && d.headPublished)) {
		d.armed = false
		return io.ErrUnexpectedEOF
	}
	return d.SecureDirectory.Sync()
}

func TestCodexLeaseDeltaCrashPublicationBoundary(t *testing.T) {
	for _, stage := range []string{"candidate_sync", "before_head", "after_head", "head_sync"} {
		t.Run(stage, func(t *testing.T) {
			coordinator, fsys, now := openSizedCodexDeltaCoordinator(t, 2)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			plan := codexLeaseRuntimeTestPlan("crash-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
			handle, err := runtimeLease.BeginRequest(plan)
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(store.journalBytes)
			generation := store.Generation()
			directory := store.directory
			fault := &deltaFaultDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), journal: store.journalName, stage: stage, armed: true}
			store.directory = fault
			_, err = handle.AdmitHTTP2xx()
			committed := stage == "after_head" || stage == "head_sync"
			if committed {
				if !errors.Is(err, fsutil.ErrCommitIndeterminate) || store.poisoned == nil {
					t.Fatalf("head uncertainty not poisoned: %v", err)
				}
			} else if !errors.Is(err, fsutil.ErrCommitNotCommitted) || store.poisoned != nil {
				t.Fatalf("unpublished candidate changed authority: %v poisoned=%v", err, store.poisoned)
			}
			if generation != store.Generation() || !bytes.Equal(before, store.journalBytes) {
				t.Fatal("failed call installed memory authority")
			}
			if err := coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			restored := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
			record := findCodexLeaseV2CASTestRecord(t, restored.Store().v2.Records, handle.identity)
			if record.EverAdmitted != committed {
				t.Fatalf("crash recovery admitted=%v want %v", record.EverAdmitted, committed)
			}
			if !record.SocketLineageExtinct || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 {
				t.Fatal("crash recovery retained active references")
			}
			entries, err := fsys.ReadDir("/state")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(restored.Store().delta.refs)+2 {
				t.Fatalf("candidate leak after recovery: entries=%d refs=%d", len(entries), len(restored.Store().delta.refs))
			}
		})
	}
}

func TestCodexLeaseDeltaRejectsCorruptionAndMissingAuthority(t *testing.T) {
	for _, target := range []string{"head", "checkpoint", "delta", "truncated_delta", "missing_delta", "signed_wrong_generation"} {
		t.Run(target, func(t *testing.T) {
			coordinator, fsys, now := openSizedCodexDeltaCoordinator(t, 2)
			store := coordinator.Store()
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("tamper-turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			if err != nil {
				t.Fatal(err)
			}
			handle, err = handle.MarkDispatched()
			if err != nil {
				t.Fatal(err)
			}
			name := store.journalName
			switch target {
			case "checkpoint":
				name = store.storageName("checkpoint", store.delta.head.Checkpoint)
			case "delta", "truncated_delta", "missing_delta":
				name = store.storageName("segment", store.delta.head.Tail)
			}
			path := "/state/" + name
			data := fsysFileBytes(t, fsys, path)
			switch target {
			case "missing_delta":
				err = fsys.Remove(path)
			case "signed_wrong_generation":
				head := store.delta.head
				head.Generation++
				data, err = store.signStorageHead(head)
				if err == nil {
					err = fsys.WriteFile(path, data, 0o600)
				}
			case "truncated_delta":
				err = fsys.WriteFile(path, data[:len(data)/2], 0o600)
			default:
				data = bytes.Clone(data)
				data[len(data)/2] ^= 1
				err = fsys.WriteFile(path, data, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := handle.AdmitHTTP2xx(); !errors.Is(err, ErrCodexLeaseTrustLost) && !errors.Is(err, ErrCodexLeaseStorePoisoned) {
				t.Fatalf("acknowledged corrupted durable authority: %v", err)
			}
			if err := coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{FS: fsys, KeyPath: "/state/leases.key", JournalPath: "/state/leases.json", Policy: CodexLeasePolicy{Retention: 24 * time.Hour, Now: func() time.Time { return *now }}, Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{9}}}, codexLeaseV2CASTestOwner{})
			if !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("recovered corrupt journal: %v", err)
			}
		})
	}
}

func TestCodexLeaseDeltaLegacyReaderCannotLoadStaleCheckpoint(t *testing.T) {
	coordinator, fsys, _ := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("turn", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
	data := fsysFileBytes(t, fsys, "/state/leases.json")
	var old codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(data, &old); err == nil {
		t.Fatal("legacy reader accepted incremental authority as full snapshot")
	}
	// No raw account/session/evidence enters incremental storage.
	entries, err := fsys.ReadDir("/state")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".key") {
			continue
		}
		data := fsysFileBytes(t, fsys, "/state/"+entry.Name())
		for _, private := range []string{"account-a", "candidate-a", "runtime-session", "runtime-thread"} {
			if bytes.Contains(data, []byte(private)) {
				t.Fatalf("raw identity in %s", entry.Name())
			}
		}
	}
}

func TestCodexLeaseDeltaFailedHeadDoesNotAccumulateCandidates(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(fmt.Sprintf("migrated_%v", migrated), func(t *testing.T) {
			coordinator, fsys, _ := openCodexLeaseRuntimeTestCoordinator(t)
			runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
			store := coordinator.Store()
			if migrated {
				completeCodexLeaseRuntimeTurn(t, runtimeLease, codexLeaseRuntimeTestPlan("seed", []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}))
			}
			directory := store.directory
			fault := &deltaFaultDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), journal: store.journalName, stage: "before_head"}
			store.directory = fault
			initial, err := fsys.ReadDir("/state")
			if err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(store.journalBytes)
			generation := store.Generation()
			for index := 0; index < 12; index++ {
				fault.armed = true
				plan := codexLeaseRuntimeTestPlan(fmt.Sprintf("failed-%d", index), []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}})
				plan.Key.Lane.Session = fmt.Sprintf("failed-session-%d", index)
				if _, err := runtimeLease.BeginRequest(plan); !errors.Is(err, fsutil.ErrCommitNotCommitted) {
					t.Fatalf("unexpected failure: %v", err)
				}
				entries, err := fsys.ReadDir("/state")
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != len(initial) {
					t.Fatalf("failed publication leaked candidates: before=%d after=%d", len(initial), len(entries))
				}
			}
			if store.Generation() != generation || !bytes.Equal(before, store.journalBytes) {
				t.Fatal("failed attempts advanced authority")
			}
		})
	}
}

type replacingDeltaScanDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	replace func() error
}

func (d *replacingDeltaScanDirectory) ReadDir() ([]os.DirEntry, error) {
	entries, err := d.SecureDirectory.(fsutil.SecureDirectoryReader).ReadDir()
	if err != nil {
		return nil, err
	}
	if err := d.replace(); err != nil {
		return nil, err
	}
	return entries, nil
}

func TestCodexLeaseDeltaCleanupFencesInstalledAuthorityReplacement(t *testing.T) {
	coordinator, fsys, _ := openSizedCodexDeltaCoordinator(t, 2)
	store := coordinator.Store()
	next := cloneCodexLeaseV2Envelope(*store.v2)
	next.Generation++
	_, data, err := store.prepareV2Envelope(next)
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := store.writeStorageReference("checkpoint", data, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	headData, err := store.signStorageHead(codexLeaseStorageHead{Version: codexLeaseStorageVersion, Generation: next.Generation, Checkpoint: digest, StateMAC: next.MAC})
	if err != nil {
		t.Fatal(err)
	}
	directory := store.directory
	store.directory = &replacingDeltaScanDirectory{SecureDirectory: directory, IdentityBoundRenamer: directory.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: directory.(fsutil.IdentityBoundRemover), replace: func() error { return fsys.WriteFile("/state/leases.json", headData, 0o600) }}
	if err := store.pruneDeltaOrphansLocked(); !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("cleanup crossed replacement authority: %v", err)
	}
	retained, err := fsys.ReadFile("/state/" + store.storageName("checkpoint", digest))
	if err != nil || !bytes.Equal(retained, data) {
		t.Fatalf("cleanup deleted replacement head dependency: %v", err)
	}
}

func TestCodexLeaseDeltaStartupReclaimsUnpublishedMigration(t *testing.T) {
	coordinator, fsys, now := openCodexLeaseRuntimeTestCoordinator(t)
	store := coordinator.Store()
	_, _, err := store.writeStorageReference("checkpoint", store.journalBytes, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if store.delta != nil {
		t.Fatal("fixture unexpectedly migrated")
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	restored := reopenCodexLeaseRuntimeTestCoordinator(t, fsys, now)
	if restored.Store().delta != nil {
		t.Fatal("orphan replayed without published head")
	}
	entries, err := fsys.ReadDir("/state")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unpublished migration candidate retained: %d entries", len(entries))
	}
}

// Build a valid near-budget chain in memory, sealing each segment once. This
// avoids thousands of native fixture writes while exercising the same signed
// format and replay invariants as individual administrative generation commits.
func codexLeaseNearBudgetImage(t *testing.T, store *CodexLeaseStore) (map[string][]byte, []byte, int) {
	t.Helper()
	files := map[string][]byte{}
	checkpoint := codexLeaseSHA256(store.journalBytes)
	files[store.storageName("checkpoint", checkpoint)] = bytes.Clone(store.journalBytes)
	segment := codexLeaseSegment{Version: 1}
	var encoded []byte
	total := 0
	count := 0
	for total < len(store.journalBytes)*9/10 {
		metadata := *store.v2
		metadata.Lanes = nil
		metadata.Records = nil
		metadata.MAC = ""
		metadata.Generation = store.v2.Generation + uint64(count) + 1
		delta := codexLeaseDelta{Version: 1, Before: metadata.Generation - 1, Metadata: metadata}
		signed, err := store.signDelta(delta)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(signed, &delta); err != nil {
			t.Fatal(err)
		}
		joined := segment
		joined.Deltas = append(append([]codexLeaseDelta(nil), segment.Deltas...), delta)
		next, err := store.signSegment(joined)
		if err != nil {
			t.Fatal(err)
		}
		if len(next) > codexLeaseSegmentMaxBytes && len(segment.Deltas) > 0 {
			digest := codexLeaseSHA256(encoded)
			files[store.storageName("segment", digest)] = encoded
			segment = codexLeaseSegment{Version: 1, Previous: digest, Deltas: []codexLeaseDelta{delta}}
			encoded, err = store.signSegment(segment)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			segment = joined
			encoded = next
		}
		total += len(signed)
		count++
	}
	tail := codexLeaseSHA256(encoded)
	files[store.storageName("segment", tail)] = encoded
	final := cloneCodexLeaseV2Envelope(*store.v2)
	final.Generation += uint64(count)
	final, _, err := store.prepareV2Envelope(final)
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.signStorageHead(codexLeaseStorageHead{Version: codexLeaseStorageVersion, Generation: final.Generation, Checkpoint: checkpoint, Tail: tail, DeltaBytes: uint64(total), StateMAC: final.MAC})
	if err != nil {
		t.Fatal(err)
	}
	return files, head, count
}

func readCodexLeaseLogicalTestBytes(t *testing.T, fsys fsutil.DurableFileSystem, path, keyPath string) []byte {
	t.Helper()
	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCodexLeaseVersion(data)
	if err != nil {
		t.Fatal(err)
	}
	if version != codexLeaseStorageVersion {
		return data
	}
	key, err := fsys.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	directory, err := fsys.(fsutil.SecureDirectoryOpener).OpenSecureDirectory(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	reader := &CodexLeaseStore{key: key, inspector: fsys.(fsutil.SecurePathInspector), directory: directory, journalName: filepath.Base(path)}
	logical, err := reader.loadDeltaStorageLocked(data)
	if err != nil {
		t.Fatal(err)
	}
	return logical
}
