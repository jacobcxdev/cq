package proxy

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

func codexLeaseWriteWorkloadFixture(t *testing.T) ([]byte, []byte, time.Time) {
	return codexLeaseWriteWorkloadFixtureSize(t, 878)
}

func codexLeaseWriteWorkloadFixtureSize(t *testing.T, laneCount int) ([]byte, []byte, time.Time) {
	coordinator, _, clock := openCodexLeaseRuntimeTestCoordinator(t)
	runtimeLease := newCodexLeaseRuntimeTest(t, coordinator)
	slots := []CodexLeaseAttemptSlotPlan{}
	for index := 0; index < 4; index++ {
		slots = append(slots, CodexLeaseAttemptSlotPlan{AccountKey: "account-a", CandidateID: fmt.Sprintf("candidate-%d", index), Kind: CodexAttemptSlotDirect})
	}
	handle, err := runtimeLease.BeginRequest(codexLeaseRuntimeTestPlan("fixture", slots))
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.MarkDispatched()
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.AdmitHTTP2xxContext(context.Background(), CodexHTTPAdmissionEvidence{TurnState: "synthetic-turn-state", HasTurnState: true})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.ProviderCompleted(CodexHTTPCompletionEvidence{CodexHTTPResponseEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "synthetic-response", HasResponseAnchor: true, HasEncryptedState: true}, EndTurn: true})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = handle.Drain()
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.Store()
	envelope := cloneCodexLeaseV2Envelope(*store.v2)
	templateLane := envelope.Lanes[0]
	templateRecord := handle.record
	envelope.Generation = 10000
	envelope.Lanes = nil
	envelope.Records = nil
	for laneIndex := 0; laneIndex < laneCount; laneIndex++ {
		lane := templateLane
		lane.Generation = 5000
		lane.SessionHash = store.hash("session", fmt.Sprintf("session-%d", laneIndex))
		lane.ThreadHash = store.hash("thread", fmt.Sprintf("thread-%d", laneIndex))
		count := 3
		if laneIndex < 24 {
			count = 4
		}
		for turnIndex := 0; turnIndex < count; turnIndex++ {
			record := cloneCodexJournalRecordV2(templateRecord)
			record.RecordGeneration = 100 + uint64(turnIndex)*10
			record.AdmissionJournalGeneration = record.RecordGeneration
			record.SessionHash = lane.SessionHash
			record.ThreadHash = lane.ThreadHash
			record.TurnHash = store.hash("turn", fmt.Sprintf("turn-%d-%d", laneIndex, turnIndex))
			if turnIndex < count-1 {
				record.State = LeaseSuperseded
			} else {
				lane.CurrentTurnHash = record.TurnHash
				lane.LastTurnHash = record.TurnHash
				lane.LastAdmittedTurnHash = record.TurnHash
				lane.LastAdmissionJournalGeneration = record.AdmissionJournalGeneration
			}
			envelope.Records = append(envelope.Records, record)
		}
		envelope.Lanes = append(envelope.Lanes, lane)
	}
	_, payload, err := store.prepareV2Envelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if laneCount == 878 && (len(payload) < 4<<20 || len(payload) > 8<<20) {
		t.Fatalf("unrepresentative journal bytes: %d", len(payload))
	}
	t.Logf("fixture lanes=%d records=%d bytes=%d", len(envelope.Lanes), len(envelope.Records), len(payload))
	return payload, bytes.Clone(store.key), *clock
}
