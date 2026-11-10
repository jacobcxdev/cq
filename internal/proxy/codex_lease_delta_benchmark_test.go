package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// BenchmarkCodexLeaseIncrementalCommit measures the complete lane commit path
// without native filesystem traffic. NearBudget retains an authenticated chain
// at approximately 90% of the checkpoint budget, rather than timing compaction.
func BenchmarkCodexLeaseIncrementalCommit(b *testing.B) {
	for _, nearBudget := range []bool{false, true} {
		name := "Short"
		if nearBudget {
			name = "NearBudget"
		}
		b.Run(name, func(b *testing.B) {
			seed, envelope := codexProxyBenchmarkLargeJournal(b, 2_000)
			prepared, encoded, err := seed.prepareV2Envelope(envelope)
			if err != nil {
				b.Fatal(err)
			}
			seed.v2, seed.journalBytes = &prepared, encoded
			seed.journalName = "leases.json"
			fsys := fsutil.NewMemFS()
			if err := fsys.MkdirAll("/state", 0o700); err != nil {
				b.Fatal(err)
			}
			if err := fsys.WriteFile("/state/leases.key", seed.key, 0o600); err != nil {
				b.Fatal(err)
			}
			if err := fsys.WriteFile("/state/leases.json", encoded, 0o600); err != nil {
				b.Fatal(err)
			}
			options := testCodexContinuityOptions(fsys)
			options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
			coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
			if err != nil {
				b.Fatal(err)
			}
			store := coordinator.Store()
			if nearBudget {
				encoded = codexLeaseBenchmarkNearBudget(b, store, fsys)
				if err := coordinator.Close(); err != nil {
					b.Fatal(err)
				}
				if err := fsys.WriteFile("/state/leases.json", encoded, 0o600); err != nil {
					b.Fatal(err)
				}
				coordinator, err = OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
				if err != nil {
					b.Fatal(err)
				}
				store = coordinator.Store()
				if store.delta == nil || len(store.delta.refs) < 2 {
					b.Fatal("near-budget setup lost its retained chain")
				}
			}
			defer coordinator.Close()
			record := store.v2.Records[0]
			fence, err := buildCodexLeasePostFence(*store.v2, record.Identity().LaneDigest, []CodexLeaseRecordFence{codexLeaseFenceForRestoredRecord(record)}, nil)
			if err != nil {
				b.Fatal(err)
			}
			desired := codexLeaseRuntimeMutationRecord(record)
			journalBytes := len(store.journalBytes)
			references := len(store.delta.refs)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				fence, err = store.CommitLane(fence, CodexLaneMutation{UpsertRecords: []CodexJournalRecordV2{desired}})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			// Encoded buffers owned by the open store, excluding the decoded
			// object graph, MemFS copies, Go heap overhead and process RSS.
			ownedBufferBytes := cap(store.journalBytes) + cap(store.key) + cap(store.legacyArchiveBytes)
			if store.delta != nil {
				ownedBufferBytes += cap(store.delta.headBytes)
				for _, reference := range store.delta.refs {
					ownedBufferBytes += cap(reference.data)
				}
			}
			b.ReportMetric(float64(journalBytes)/(1<<20), "journal-MiB")
			b.ReportMetric(float64(references), "references")
			b.ReportMetric(float64(ownedBufferBytes), "owned-buffer-bytes")
		})
	}
}

func codexLeaseBenchmarkNearBudget(b *testing.B, store *CodexLeaseStore, fsys *fsutil.MemFS) []byte {
	b.Helper()
	checkpoint := codexLeaseSHA256(store.journalBytes)
	if err := fsys.WriteFile("/state/"+store.storageName("checkpoint", checkpoint), store.journalBytes, 0o600); err != nil {
		b.Fatal(err)
	}
	metadata := *store.v2
	metadata.Lanes, metadata.Records, metadata.MAC = nil, nil, ""
	segment := codexLeaseSegment{Version: 1}
	total, count := 0, 0
	var encoded []byte
	for total < len(store.journalBytes)*9/10 {
		metadata.Generation = store.v2.Generation + uint64(count) + 1
		delta := codexLeaseDelta{Version: 1, Before: metadata.Generation - 1, Metadata: metadata}
		signed, err := store.signDelta(delta)
		if err != nil {
			b.Fatal(err)
		}
		if err := json.Unmarshal(signed, &delta); err != nil {
			b.Fatal(err)
		}
		segment.Deltas = append(segment.Deltas, delta)
		total += len(signed)
		count++
		if len(segment.Deltas) < 48 && total < len(store.journalBytes)*9/10 {
			continue
		}
		encoded, err = store.signSegment(segment)
		if err != nil {
			b.Fatal(err)
		}
		if len(encoded) > codexLeaseSegmentMaxBytes {
			b.Fatalf("benchmark segment exceeded budget: %d", len(encoded))
		}
		digest := codexLeaseSHA256(encoded)
		if err := fsys.WriteFile("/state/"+store.storageName("segment", digest), encoded, 0o600); err != nil {
			b.Fatal(err)
		}
		segment = codexLeaseSegment{Version: 1, Previous: digest}
	}
	final := cloneCodexLeaseV2Envelope(*store.v2)
	final.Generation += uint64(count)
	final, _, err := store.prepareV2Envelope(final)
	if err != nil {
		b.Fatal(err)
	}
	head, err := store.signStorageHead(codexLeaseStorageHead{Version: codexLeaseStorageVersion, Generation: final.Generation, Checkpoint: checkpoint, Tail: codexLeaseSHA256(encoded), DeltaBytes: uint64(total), StateMAC: final.MAC})
	if err != nil {
		b.Fatal(err)
	}
	return bytes.Clone(head)
}
