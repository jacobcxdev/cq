package proxy

import (
	"reflect"
	"testing"
	"time"
)

func TestCodexLeaseDeltaSparseDiffAllocationBound(t *testing.T) {
	_, before := codexProxyBenchmarkLargeJournal(t, 128)
	canonicaliseCodexLeaseV2(&before)
	after := cloneCodexLeaseV2Envelope(before)
	after.Generation++
	after.Records[0].LastObservedAt = after.Records[0].LastObservedAt.Add(time.Second)
	allocations := testing.AllocsPerRun(1, func() {
		delta := makeCodexLeaseDelta(before, after)
		if len(delta.Records) != 1 {
			t.Fatal("sparse diff lost changed record")
		}
	})
	if allocations > 16 {
		t.Fatalf("sparse diff allocated %.0f objects, want <= 16", allocations)
	}
}

func TestCodexLeaseDeltaDiffMatchesRetainedState(t *testing.T) {
	store, base := codexProxyBenchmarkLargeJournal(t, 4)
	canonicaliseCodexLeaseV2(&base)
	for changes := range 16 {
		before := cloneCodexLeaseV2Envelope(base)
		after := cloneCodexLeaseV2Envelope(base)
		after.Generation++
		if changes&1 != 0 {
			after.Records[0].LastObservedAt = after.Records[0].LastObservedAt.Add(time.Second)
			after.Records[0].AttemptEnvelope.Slots[0].CandidateHash = store.hash("candidate", "changed")
			after.Lanes[0].LastObservedAt = after.Lanes[0].LastObservedAt.Add(time.Second)
		}
		if changes&2 != 0 {
			after.Records = after.Records[1:]
			after.Lanes = after.Lanes[1:]
		}
		if changes&4 != 0 {
			record := cloneCodexJournalRecordV2(base.Records[0])
			record.SessionHash = store.hash("session", "added")
			lane := base.Lanes[0]
			lane.SessionHash = record.SessionHash
			after.Records = append(after.Records, record)
			after.Lanes = append(after.Lanes, lane)
		}
		if changes&8 != 0 {
			after.Records = nil
			after.Lanes = nil
		}
		canonicaliseCodexLeaseV2(&after)
		got := makeCodexLeaseDelta(before, after)
		// Use direct identity lookup as an independent oracle for the ordered
		// diff. Every after-image and deletion must match the original snapshots.
		want := codexLeaseDelta{Version: 1, Before: before.Generation, Metadata: after}
		want.Metadata.Lanes, want.Metadata.Records, want.Metadata.MAC = nil, nil, ""
		for _, lane := range after.Lanes {
			found := false
			for _, old := range before.Lanes {
				if codexJournalLaneDigest(old.SessionHash, old.ThreadHash, old.NamespaceHash) == codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash) {
					found = reflect.DeepEqual(old, lane)
					break
				}
			}
			if !found {
				want.Lanes = append(want.Lanes, lane)
			}
		}
		for _, old := range before.Lanes {
			found := false
			for _, lane := range after.Lanes {
				found = found || codexJournalLaneDigest(old.SessionHash, old.ThreadHash, old.NamespaceHash) == codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
			}
			if !found {
				want.DeleteLanes = append(want.DeleteLanes, codexJournalLaneDigest(old.SessionHash, old.ThreadHash, old.NamespaceHash))
			}
		}
		for _, record := range after.Records {
			found := false
			for _, old := range before.Records {
				if old.Identity() == record.Identity() {
					found = reflect.DeepEqual(old, record)
					break
				}
			}
			if !found {
				want.Records = append(want.Records, record)
			}
		}
		for _, old := range before.Records {
			found := false
			for _, record := range after.Records {
				found = found || old.Identity() == record.Identity()
			}
			if !found {
				want.DeleteRecords = append(want.DeleteRecords, old.Identity())
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("diff changed retained-state semantics for change mask %d", changes)
		}
	}
}

func BenchmarkCodexLeaseSparseDeltaDiff(b *testing.B) {
	_, before := codexProxyBenchmarkLargeJournal(b, 2_000)
	canonicaliseCodexLeaseV2(&before)
	after := cloneCodexLeaseV2Envelope(before)
	after.Generation++
	after.Records[0].LastObservedAt = after.Records[0].LastObservedAt.Add(time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if delta := makeCodexLeaseDelta(before, after); len(delta.Records) != 1 {
			b.Fatal("sparse diff lost changed record")
		}
	}
}
