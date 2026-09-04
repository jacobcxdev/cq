package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexProxyBenchmarkBodyBytes = 1 << 20

func BenchmarkCodexDisabledPayloadTrace(b *testing.B) {
	payload := codexProxyBenchmarkRequest(codexProxyBenchmarkBodyBytes)
	ctx := withCodexTrace(context.Background(), nil, nil, CodexTraceStart{Transport: "websocket"})
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		emitCodexRawTracePayload(ctx, PayloadEvent{}, payload)
	}
}

func BenchmarkCodexRequestReplay(b *testing.B) {
	payload := codexProxyBenchmarkRequest(codexProxyBenchmarkBodyBytes)
	envelope, err := NewCodexRequestEnvelope(payload, payload, http.Header{"Content-Type": {"application/json"}}, "gpt-5.6-sol")
	if err != nil {
		b.Fatal(err)
	}
	defer envelope.Release()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		replay, err := envelope.Replay()
		if err != nil {
			b.Fatal(err)
		}
		replay.Release()
	}
}

func BenchmarkCodexWSInspectAndFreezeUnchanged(b *testing.B) {
	payload := codexProxyBenchmarkRequest(codexProxyBenchmarkBodyBytes)
	choice := RouteChoice{
		AccountKey:      "account",
		RequestedModel:  "gpt-5",
		EffectiveModel:  "gpt-5",
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		inspection, err := InspectCodexNativeRequest(context.Background(), payload, nil)
		if err != nil {
			b.Fatal(err)
		}
		frozen, err := inspection.Freeze(context.Background(), choice, nil, HeadroomModeCache)
		if err != nil {
			b.Fatal(err)
		}
		frozen.Release()
	}
}

func BenchmarkCodexWSUnchangedRequestPreparation(b *testing.B) {
	payload := codexProxyBenchmarkRequest(codexProxyBenchmarkBodyBytes)
	pending, err := newCodexWSPendingFrame(websocket.TextMessage, payload)
	if err != nil {
		b.Fatal(err)
	}
	defer pending.Release()
	inspection, err := InspectCodexNativeRequest(context.Background(), payload, nil)
	if err != nil {
		b.Fatal(err)
	}
	frozen, err := inspection.Freeze(context.Background(), RouteChoice{
		AccountKey:      "account",
		RequestedModel:  "gpt-5",
		EffectiveModel:  "gpt-5",
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
	}, nil, HeadroomModeCache)
	if err != nil {
		b.Fatal(err)
	}
	defer frozen.Release()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		prepared, err := codexWSPreparedPendingFrame(frozen, pending)
		if err != nil {
			b.Fatal(err)
		}
		if prepared != pending {
			prepared.Release()
		}
	}
}

func BenchmarkCodexLeasePrepareLargeJournal(b *testing.B) {
	store, envelope := codexProxyBenchmarkLargeJournal(b, 2_000)
	_, encoded, err := store.prepareV2Envelope(envelope)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := store.prepareV2Envelope(envelope); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(encoded))/(1<<20), "journal-MiB")
}

func BenchmarkCodexLeaseRouteSnapshotLargeJournal(b *testing.B) {
	store, envelope := codexProxyBenchmarkLargeJournal(b, 2_000)
	store.v2 = &envelope
	store.generation = envelope.Generation
	store.modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
	store.owner = testCodexLeaseOwner{}
	coordinator := &CodexContinuityCoordinator{store: store, leases: NewCodexTurnLeaseManager(1, true, nil)}
	key := LeaseKey{
		Lane: LaneKey{Session: "session-0000", Thread: "thread-0000", Namespace: CodexResponsesNamespace},
		Turn: "turn-0000",
	}
	accounts := []codex.AccountKey{"account-0", "account-1"}
	policy := CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := coordinator.LoadRouteSnapshot(context.Background(), key, accounts, policy); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCodexLeaseRetentionLargeJournal(b *testing.B) {
	store, envelope := codexProxyBenchmarkLargeJournal(b, 2_000)
	store.v2 = &envelope
	now := envelope.Records[0].LastObservedAt.Add(8 * 24 * time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := store.compactCodexLeaseV2Locked(now, 7*24*time.Hour); err != nil {
			b.Fatal(err)
		}
	}
}

func codexProxyBenchmarkLargeJournal(b *testing.B, laneCount int) (*CodexLeaseStore, codexLeaseJournalEnvelopeV2) {
	b.Helper()
	store, envelope := codexLeaseV2SchemaFixture(b)
	templateLane := envelope.Lanes[0]
	templateRecord := envelope.Records[0]
	envelope.Lanes = make([]CodexJournalLane, 0, laneCount)
	envelope.Records = make([]CodexJournalRecordV2, 0, laneCount)
	for index := range laneCount {
		suffix := fmt.Sprintf("-%04d", index)
		lane := templateLane
		lane.SessionHash = store.hash("session", "session"+suffix)
		lane.ThreadHash = store.hash("thread", "thread"+suffix)
		lane.NamespaceHash = store.hash("namespace", CodexResponsesNamespace)
		lane.CurrentTurnHash = store.hash("turn", "turn"+suffix)
		lane.LastTurnHash = lane.CurrentTurnHash
		record := cloneCodexJournalRecordV2(templateRecord)
		record.SessionHash = lane.SessionHash
		record.ThreadHash = lane.ThreadHash
		record.NamespaceHash = lane.NamespaceHash
		record.TurnHash = lane.CurrentTurnHash
		record.AccountHash = store.hash("account", fmt.Sprintf("account-%d", index%2))
		for slotIndex := range record.AttemptEnvelope.Slots {
			record.AttemptEnvelope.Slots[slotIndex].AccountHash = record.AccountHash
			record.AttemptEnvelope.Slots[slotIndex].CandidateHash = store.hash("candidate", fmt.Sprintf("candidate-%d%s", slotIndex, suffix))
		}
		codexLeaseV2RefreshPlanDigest(b, store, &record)
		envelope.Lanes = append(envelope.Lanes, lane)
		envelope.Records = append(envelope.Records, record)
	}
	return store, envelope
}

func codexProxyBenchmarkRequest(size int) []byte {
	prefix := []byte(`{"type":"response.create","model":"gpt-5","client_metadata":{"x-codex-turn-metadata":{"session_id":"s","thread_id":"t","turn_id":"u","request_kind":"turn"}},"input":"`)
	suffix := []byte(`"}`)
	payload := make([]byte, 0, size)
	payload = append(payload, prefix...)
	payload = append(payload, bytes.Repeat([]byte{'x'}, size-len(prefix)-len(suffix))...)
	return append(payload, suffix...)
}
