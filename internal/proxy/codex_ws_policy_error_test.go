package proxy

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexTerminatingWSBrokerSurfacesStatuslessCyberPolicy(t *testing.T) {
	t.Parallel()
	for _, admitted := range []bool{false, true} {
		name := "before admission"
		if admitted {
			name = "during compaction"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			planner := &codexWSBrokerPlannerStub{
				runtime: newCodexLeaseRuntimeTest(t, coordinator),
				slots: []CodexLeaseAttemptSlotPlan{
					{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect},
					{AccountKey: "account-b", CandidateID: "candidate-b", Kind: CodexAttemptSlotDirect},
				},
			}
			denied := []byte(`{"type":"error","error":{"message":"This content was flagged for possible cybersecurity risk.","type":"invalid_request","param":null,"code":"cyber_policy"},"response_id":"response-a","sequence_number":9}`)
			reads := []codexWSBrokerRead{}
			if admitted {
				reads = append(reads,
					codexWSBrokerRead{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.created","response":{"id":"response-a"}}`)},
					codexWSBrokerRead{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.compaction.compacting","sequence_number":8}`)},
				)
			}
			reads = append(reads, codexWSBrokerRead{messageType: websocket.TextMessage, payload: denied})
			downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{
				{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")},
				{err: io.EOF},
			}}
			dialer := &codexWSBrokerDialerStub{connections: map[codex.AccountKey][]websocketRelayConn{
				"account-a": {&codexWSBrokerConnStub{reads: reads}},
				"account-b": {&codexWSBrokerConnStub{}},
			}}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{
				Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Serve(context.Background(), downstream); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(dialer.accounts, []codex.AccountKey{"account-a"}) {
				t.Fatalf("policy rejection triggered account failover: %v", dialer.accounts)
			}
			writes := downstream.writtenPayloads()
			if len(writes) != len(reads) {
				t.Fatalf("downstream frames = %d, want %d", len(writes), len(reads))
			}
			for i := 0; i < len(reads)-1; i++ {
				if !reflect.DeepEqual(writes[i], reads[i].payload) {
					t.Fatalf("progress frame changed: %s", writes[i])
				}
			}
			var got, want any
			if err := json.Unmarshal(writes[len(writes)-1], &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(`{"type":"response.failed","response":{"id":"response-a","status":"failed","error":{"message":"This content was flagged for possible cybersecurity risk.","type":"invalid_request","param":null,"code":"cyber_policy"}},"sequence_number":9}`), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("client terminal frame = %s, want response.failed preserving provider error", writes[len(writes)-1])
			}
		})
	}
}

func TestCodexWSResponseWriterPreservesOtherFrames(t *testing.T) {
	t.Parallel()
	for _, frame := range []string{
		`{"type":"error","status":400,"error":{"code":"cyber_policy","message":"denied"}}`,
		`{"type":"error","status_code":403,"error":{"code":"cyber_policy","message":"denied"}}`,
		`{"type":"error","status":null,"error":{"code":"cyber_policy"}}`,
		`{"type":"error","status":400,"status_code":403,"error":{"code":"cyber_policy"}}`,
		`{"type":"error","error":{"code":"usage_limit_reached","message":"exhausted"}}`,
		`{"type":"response.failed","response":{"error":{"code":"cyber_policy"}}}`,
		`{"type":"error","error":{"code":42}}`,
		`{"type":"error","error":{"code":"cyber_policy","code":"other"}}`,
		`{"type":"error","error":{"code":"cyber_policy"}`,
	} {
		t.Run(frame, func(t *testing.T) {
			t.Parallel()
			downstream := &codexWSBrokerConnStub{}
			broker := &codexTerminatingWSBroker{config: codexTerminatingWSBrokerConfig{ResponseIdleTimeout: time.Second}}
			if err := broker.writeResponseMessage(context.Background(), downstream, websocket.TextMessage, []byte(frame), CodexSSEError); err != nil {
				t.Fatal(err)
			}
			if got := downstream.writtenPayloads(); !reflect.DeepEqual(got, [][]byte{[]byte(frame)}) {
				t.Fatalf("unrelated or invalid frame changed: %q", got)
			}
		})
	}
}
