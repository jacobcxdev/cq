package proxy

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexWSFrameBrokerBindsAccountAcrossSampling(t *testing.T) {
	broker := newCodexWSFrameBrokerForTest(t)

	initial, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, codexWSBrokerFrame("thread", "turn-a", "gpt-5.4"))
	if err != nil {
		t.Fatalf("classify initial frame: %v", err)
	}
	if initial.Kind != CodexWSFrameInitial || initial.AccountKey != codex.AccountKey("account-a") || initial.Request.Model != "gpt-5.4" {
		t.Fatalf("unexpected initial decision: %+v", initial)
	}
	if initial.RotationIntent != nil {
		t.Fatal("initial frame unexpectedly created rotation intent")
	}

	if initial.AttemptGeneration != 1 {
		t.Fatalf("initial attempt generation = %d, want 1", initial.AttemptGeneration)
	}
	if err := broker.DrainAttempt(11, 17, initial.AttemptGeneration); err != nil {
		t.Fatalf("drain initial attempt: %v", err)
	}
	repeated, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, codexWSBrokerFrame("thread", "turn-a", "gpt-5.4"))
	if err != nil {
		t.Fatalf("classify repeated sampling: %v", err)
	}
	if repeated.Kind != CodexWSFrameSameTurn || repeated.AccountKey != initial.AccountKey || repeated.AttemptGeneration != 2 {
		t.Fatalf("same-turn sampling changed socket binding: %+v", repeated)
	}
	if err := broker.DrainAttempt(11, 17, initial.AttemptGeneration); !errors.Is(err, ErrCodexWSStaleGeneration) {
		t.Fatalf("late attempt drain error = %v, want %v", err, ErrCodexWSStaleGeneration)
	}
}

func TestCodexWSFrameBrokerArmsFullNewTurnAfterDrain(t *testing.T) {
	broker := newCodexWSFrameBrokerForTest(t)
	initial := codexWSBrokerFrame("thread", "turn-a", "gpt-5.4")
	successor := codexWSBrokerFrame("thread", "turn-b", "gpt-5.4-mini")
	initialDecision, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, initial)
	if err != nil {
		t.Fatalf("classify initial frame: %v", err)
	}

	if _, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, successor); !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("active successor error = %v, want %v", err, ErrCodexConcurrentTurn)
	}
	if _, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, codexWSBrokerFrame("other-thread", "turn-b", "gpt-5.4")); !errors.Is(err, ErrCodexWSInvalidFrame) {
		t.Fatalf("changed-lane error = %v, want %v", err, ErrCodexWSInvalidFrame)
	}
	if err := broker.DrainAttempt(11, 99, initialDecision.AttemptGeneration); !errors.Is(err, ErrCodexWSStaleGeneration) {
		t.Fatalf("late drain error = %v, want %v", err, ErrCodexWSStaleGeneration)
	}
	if _, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, successor); !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("late drain changed active state: %v", err)
	}
	if err := broker.DrainAttempt(11, 17, initialDecision.AttemptGeneration); err != nil {
		t.Fatalf("drain initial attempt: %v", err)
	}

	decision, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, successor)
	if err != nil {
		t.Fatalf("classify successor: %v", err)
	}
	if decision.Kind != CodexWSFrameRequireResync || decision.AccountKey != codex.AccountKey("account-a") {
		t.Fatalf("unexpected successor decision: %+v", decision)
	}
	intent := decision.RotationIntent
	if intent == nil || !intent.FullNewTurn || intent.Key.Turn != "turn-b" || intent.DownstreamGeneration != 11 || intent.UpstreamGeneration != 17 {
		t.Fatalf("unexpected successor intent: %+v", intent)
	}
	if _, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, initial); !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("late predecessor error = %v, want %v", err, ErrCodexStaleTurn)
	}
	if _, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, successor); !errors.Is(err, ErrCodexWSResyncRequired) {
		t.Fatalf("repeated successor error = %v, want %v", err, ErrCodexWSResyncRequired)
	}
}

func TestCodexWSFrameBrokerFailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		generation  uint64
		messageType int
		frame       []byte
		want        error
	}{
		{name: "late socket generation", generation: 10, messageType: websocket.TextMessage, frame: codexWSBrokerFrame("thread", "turn-a", "gpt-5.4"), want: ErrCodexWSStaleGeneration},
		{name: "binary message", generation: 11, messageType: websocket.BinaryMessage, frame: codexWSBrokerFrame("thread", "turn-a", "gpt-5.4"), want: ErrCodexWSInvalidFrame},
		{name: "malformed JSON", generation: 11, messageType: websocket.TextMessage, frame: []byte(`{"type":`), want: ErrCodexWSInvalidFrame},
		{name: "wrong frame type", generation: 11, messageType: websocket.TextMessage, frame: []byte(`{"type":"response.cancel","model":"gpt-5.4"}`), want: ErrCodexWSInvalidFrame},
		{name: "missing model", generation: 11, messageType: websocket.TextMessage, frame: codexWSBrokerFrame("thread", "turn-a", ""), want: ErrCodexWSHandshakeUnsupported},
		{name: "model mismatch", generation: 11, messageType: websocket.TextMessage, frame: codexWSBrokerFrame("thread", "turn-a", "gpt-5.4-mini"), want: ErrCodexWSHandshakeUnsupported},
		{name: "turn mismatch", generation: 11, messageType: websocket.TextMessage, frame: codexWSBrokerFrame("thread", "turn-b", "gpt-5.4"), want: ErrCodexWSHandshakeUnsupported},
		{name: "duplicate model authority", generation: 11, messageType: websocket.TextMessage, frame: []byte(`{"type":"response.create","model":"gpt-5.4","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn-a","request_kind":"turn"}}}`), want: ErrCodexWSInvalidFrame},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broker := newCodexWSFrameBrokerForTest(t)
			if _, err := broker.ClassifyResponseCreate(test.generation, test.messageType, test.frame); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCodexWSFrameBrokerSerialisesDrainAndSuccessor(t *testing.T) {
	for range 100 {
		broker := newCodexWSFrameBrokerForTest(t)
		initial, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, codexWSBrokerFrame("thread", "turn-a", "gpt-5.4"))
		if err != nil {
			t.Fatalf("classify initial frame: %v", err)
		}

		start := make(chan struct{})
		errorsSeen := make(chan error, 2)
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			errorsSeen <- broker.DrainAttempt(11, 17, initial.AttemptGeneration)
		}()
		go func() {
			defer group.Done()
			<-start
			_, err := broker.ClassifyResponseCreate(11, websocket.TextMessage, codexWSBrokerFrame("thread", "turn-b", "gpt-5.4"))
			errorsSeen <- err
		}()
		close(start)
		group.Wait()
		close(errorsSeen)

		for err := range errorsSeen {
			if err != nil && !errors.Is(err, ErrCodexConcurrentTurn) {
				t.Fatalf("unexpected concurrent result: %v", err)
			}
		}
	}
}

func newCodexWSFrameBrokerForTest(t *testing.T) *CodexWSFrameBroker {
	t.Helper()
	broker, err := NewCodexWSFrameBroker(CodexWSFrameBrokerConfig{
		Handshake: CodexWSHandshake{
			Metadata: CodexTurnMetadata{SessionID: "session", ThreadID: "thread", TurnID: "turn-a", RequestKind: CodexRequestTurn},
			Model:    "gpt-5.4",
		},
		AccountKey:           codex.AccountKey("account-a"),
		ModeEpoch:            7,
		DownstreamGeneration: 11,
		UpstreamGeneration:   17,
		ClientBuild:          "codex-test",
		RetryBudget:          1,
	})
	if err != nil {
		t.Fatalf("create broker: %v", err)
	}
	return broker
}

func codexWSBrokerFrame(thread, turn, model string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":%q,"turn_id":%q,"request_kind":"turn"}}}`, model, thread, turn))
}
