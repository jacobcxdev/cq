package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestRelayCodexAcceptedHTTPResponseRelaysSSETerminalOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		body         string
		wantKind     string
		wantEndTurn  bool
		wantEvidence CodexHTTPResponseEvidence
	}{
		{
			name:         "completion defaults to end turn",
			body:         "data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-a\",\"encrypted_content\":\"private-state\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-a\"}}\n\ndata: [DONE]\n\n",
			wantKind:     "completed",
			wantEndTurn:  true,
			wantEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-a", HasResponseAnchor: true, HasEncryptedState: true},
		},
		{
			name:         "completion retains continuation",
			body:         "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-b\",\"end_turn\":false}}\n\n",
			wantKind:     "completed",
			wantEndTurn:  false,
			wantEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-b", HasResponseAnchor: true},
		},
		{
			name:         "provider failure",
			body:         "data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-c\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"encrypted_content\":\"private-state\"}}\n\n",
			wantKind:     "failed",
			wantEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-c", HasResponseAnchor: true, HasEncryptedState: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := &codexHTTPObserverLifecycleCalls{}
			lifecycle := &codexHTTPObserverLifecycle{calls: calls, generation: 1}
			body := newCodexHTTPObserverBody([]string{test.body[:1], test.body[1:7], test.body[7:]}, nil, calls)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
					"X-Multi":      []string{"first", "second"},
				},
				Body: body,
			}
			writer := newCodexHTTPObserverWriter()

			if err := relayCodexAcceptedHTTPResponse(context.Background(), writer, response, codexHTTPResponseModeSSE, lifecycle); err != nil {
				t.Fatal(err)
			}
			if got := writer.body.String(); got != test.body {
				t.Fatalf("relayed body = %q, want exact %q", got, test.body)
			}
			if writer.status != http.StatusOK {
				t.Fatalf("status = %d, want %d", writer.status, http.StatusOK)
			}
			if !reflect.DeepEqual(writer.header.Values("X-Multi"), []string{"first", "second"}) {
				t.Fatalf("multi-value header = %#v", writer.header.Values("X-Multi"))
			}
			if writer.flushes == 0 {
				t.Fatal("SSE relay did not flush")
			}

			got := calls.snapshot()
			if got.terminalKind != test.wantKind || got.endTurn != test.wantEndTurn || got.terminalCalls != 1 || got.drainCalls != 1 {
				t.Fatalf("lifecycle calls = %#v", got)
			}
			if got.responseEvidence != test.wantEvidence {
				t.Fatalf("response evidence = %#v, want %#v", got.responseEvidence, test.wantEvidence)
			}
			if !reflect.DeepEqual(got.order, []string{"close", test.wantKind + "@1", "drain@2"}) {
				t.Fatalf("lifecycle order = %#v", got.order)
			}
			if body.closeCalls != 1 {
				t.Fatalf("body close calls = %d, want 1", body.closeCalls)
			}
		})
	}
}

func TestRelayCodexAcceptedHTTPResponsePersistsStreamedTurnStateBeforeRelay(t *testing.T) {
	t.Parallel()
	const turnState = "private-streamed-turn-state"
	bodyBytes := "data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"" + turnState + "\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	calls := &codexHTTPObserverLifecycleCalls{}
	body := newCodexHTTPObserverBody([]string{bodyBytes}, nil, calls)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	writer := newCodexHTTPObserverWriter()

	if err := relayCodexAcceptedHTTPResponse(context.Background(), writer, response, codexHTTPResponseModeSSE, &codexHTTPObserverLifecycle{calls: calls, generation: 1}); err != nil {
		t.Fatal(err)
	}
	if got := writer.body.String(); got != bodyBytes {
		t.Fatalf("relayed body = %q, want exact body", got)
	}
	got := calls.snapshot()
	if got.admissionCalls != 1 || got.admissionEvidence != (CodexHTTPAdmissionEvidence{TurnState: turnState, HasTurnState: true}) {
		t.Fatalf("streamed admission = calls %d evidence %#v", got.admissionCalls, got.admissionEvidence)
	}
	if !reflect.DeepEqual(got.order, []string{"admit@1", "close", "completed@2", "drain@3"}) {
		t.Fatalf("lifecycle order = %#v", got.order)
	}
}

func TestRelayCodexAcceptedHTTPResponseStagesLatestTurnStatePerChunk(t *testing.T) {
	t.Parallel()
	const firstState = "private-first-streamed-state"
	const finalState = "private-final-streamed-state"
	bodyBytes := "data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"" + firstState + "\"}}\n\n" +
		"data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"" + finalState + "\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	calls := &codexHTTPObserverLifecycleCalls{}
	body := newCodexHTTPObserverBody([]string{bodyBytes}, nil, calls)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	writer := newCodexHTTPObserverWriter()

	if err := relayCodexAcceptedHTTPResponse(context.Background(), writer, response, codexHTTPResponseModeSSE, &codexHTTPObserverLifecycle{calls: calls, generation: 1}); err != nil {
		t.Fatal(err)
	}
	if got := writer.body.String(); got != bodyBytes {
		t.Fatalf("relayed body = %q, want exact body", got)
	}
	got := calls.snapshot()
	if got.admissionCalls != 1 || got.admissionEvidence != (CodexHTTPAdmissionEvidence{TurnState: finalState, HasTurnState: true}) {
		t.Fatalf("staged admission = calls %d evidence %#v", got.admissionCalls, got.admissionEvidence)
	}
	if !reflect.DeepEqual(got.order, []string{"admit@1", "close", "completed@2", "drain@3"}) {
		t.Fatalf("lifecycle order = %#v", got.order)
	}
}

func TestRelayCodexAcceptedHTTPResponseWithholdsUnpersistedTurnState(t *testing.T) {
	t.Parallel()
	const turnState = "private-unpersisted-turn-state"
	admissionErr := errors.New("streamed admission failed")
	bodyBytes := "data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"" + turnState + "\"}}\n\n"
	calls := &codexHTTPObserverLifecycleCalls{admissionErr: admissionErr}
	body := newCodexHTTPObserverBody([]string{bodyBytes}, nil, calls)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	writer := newCodexHTTPObserverWriter()

	err := relayCodexAcceptedHTTPResponse(context.Background(), writer, response, codexHTTPResponseModeSSE, &codexHTTPObserverLifecycle{calls: calls, generation: 1})
	if !errors.Is(err, admissionErr) {
		t.Fatalf("relay error = %v, want admission failure", err)
	}
	if writer.body.Len() != 0 {
		t.Fatalf("relayed unpersisted metadata bytes = %q", writer.body.String())
	}
	if strings.Contains(err.Error(), turnState) {
		t.Fatalf("relay error disclosed private turn state: %v", err)
	}
	got := calls.snapshot()
	if got.admissionCalls != 1 || got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
	if !reflect.DeepEqual(got.order, []string{"admit@1", "close", "indeterminate@1", "drain@2"}) {
		t.Fatalf("lifecycle order = %#v", got.order)
	}
}

func TestRelayCodexAcceptedHTTPResponseRelaysCompactCompletion(t *testing.T) {
	t.Parallel()
	const raw = `{"id":"private-response-id","output":[],"encrypted_content":"private-state"}`
	calls := &codexHTTPObserverLifecycleCalls{}
	lifecycle := &codexHTTPObserverLifecycle{calls: calls, generation: 1}
	body := newCodexHTTPObserverBody([]string{raw[:9], raw[9:]}, nil, calls)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	writer := newCodexHTTPObserverWriter()

	if err := relayCodexAcceptedHTTPResponse(context.Background(), writer, response, codexHTTPResponseModeCompact, lifecycle); err != nil {
		t.Fatal(err)
	}
	if got := writer.body.String(); got != raw {
		t.Fatalf("relayed body = %q, want exact %q", got, raw)
	}
	if writer.flushes != 0 {
		t.Fatalf("compact flushes = %d, want 0", writer.flushes)
	}
	got := calls.snapshot()
	if got.terminalKind != "completed" || !got.endTurn || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
	wantEvidence := CodexHTTPResponseEvidence{ResponseAnchor: "private-response-id", HasResponseAnchor: true, HasEncryptedState: true}
	if got.responseEvidence != wantEvidence {
		t.Fatalf("response evidence = %#v, want %#v", got.responseEvidence, wantEvidence)
	}
}

func TestRelayCodexAcceptedHTTPResponseMarksAmbiguousOutcomesIndeterminate(t *testing.T) {
	t.Parallel()
	readFailure := errors.New("private upstream read failure")
	tests := []struct {
		name         string
		mode         codexHTTPResponseMode
		body         string
		readErr      error
		wantEvidence CodexHTTPResponseEvidence
	}{
		{name: "malformed SSE", mode: codexHTTPResponseModeSSE, body: "data: {\n\n"},
		{name: "truncated SSE", mode: codexHTTPResponseModeSSE, body: "data: {\"type\":\"response.completed\",\"response\":{}}\n"},
		{name: "done without typed terminal", mode: codexHTTPResponseModeSSE, body: "data: [DONE]\n\n"},
		{
			name: "read failure after completion", mode: codexHTTPResponseModeSSE,
			body:         "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-before-read-error\",\"encrypted_content\":\"private-state\"}}\n\n",
			readErr:      readFailure,
			wantEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-before-read-error", HasResponseAnchor: true, HasEncryptedState: true},
		},
		{
			name: "conflicting terminal evidence", mode: codexHTTPResponseModeSSE,
			body:         "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-terminal-conflict\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{}}\n\n",
			wantEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-terminal-conflict", HasResponseAnchor: true},
		},
		{
			name: "conflicting response anchors", mode: codexHTTPResponseModeSSE,
			body:         "data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-first\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-second\"}}\n\n",
			wantEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-first", HasResponseAnchor: true},
		},
		{
			name: "failure followed by malformed event", mode: codexHTTPResponseModeSSE,
			body:         "data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-before-malformed\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{}}\n\ndata: {\n\n",
			wantEvidence: CodexHTTPResponseEvidence{ResponseAnchor: "response-before-malformed", HasResponseAnchor: true},
		},
		{name: "malformed compact", mode: codexHTTPResponseModeCompact, body: `{"id":"private-body"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := &codexHTTPObserverLifecycleCalls{}
			lifecycle := &codexHTTPObserverLifecycle{calls: calls, generation: 1}
			body := newCodexHTTPObserverBody([]string{test.body}, test.readErr, calls)
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
			writer := newCodexHTTPObserverWriter()

			err := relayCodexAcceptedHTTPResponse(context.Background(), writer, response, test.mode, lifecycle)
			if err == nil {
				t.Fatal("relay error = nil, want ambiguous outcome")
			}
			if bytes.Contains([]byte(err.Error()), []byte("private-body")) {
				t.Fatalf("relay error disclosed response body: %v", err)
			}
			got := calls.snapshot()
			if got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 {
				t.Fatalf("lifecycle calls = %#v", got)
			}
			if got.responseEvidence != test.wantEvidence {
				t.Fatalf("response evidence = %#v, want %#v", got.responseEvidence, test.wantEvidence)
			}
			if !reflect.DeepEqual(got.order, []string{"close", "indeterminate@1", "drain@2"}) {
				t.Fatalf("lifecycle order = %#v", got.order)
			}
		})
	}
}

func TestRelayCodexAcceptedHTTPResponseTransportUncertaintyDominatesTerminalBytes(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("downstream writer unavailable")
	closeErr := errors.New("upstream body close uncertain")
	tests := []struct {
		name     string
		cancel   bool
		writeErr error
		closeErr error
		wantErr  error
	}{
		{name: "cancelled context", cancel: true, wantErr: context.Canceled},
		{name: "downstream write failure", writeErr: writeErr, wantErr: writeErr},
		{name: "body close failure", closeErr: closeErr, wantErr: closeErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			if test.cancel {
				cancel()
			} else {
				defer cancel()
			}
			calls := &codexHTTPObserverLifecycleCalls{}
			lifecycle := &codexHTTPObserverLifecycle{calls: calls, generation: 1}
			body := newCodexHTTPObserverBody([]string{"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"}, nil, calls)
			body.closeErr = test.closeErr
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
			writer := newCodexHTTPObserverWriter()
			writer.writeErr = test.writeErr

			err := relayCodexAcceptedHTTPResponse(ctx, writer, response, codexHTTPResponseModeSSE, lifecycle)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("relay error = %v, want %v", err, test.wantErr)
			}
			got := calls.snapshot()
			if got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 || got.cleanupCancelled {
				t.Fatalf("lifecycle calls = %#v", got)
			}
			if !reflect.DeepEqual(got.order, []string{"close", "indeterminate@1", "drain@2"}) {
				t.Fatalf("lifecycle order = %#v", got.order)
			}
			if body.closeCalls != 1 {
				t.Fatalf("body close calls = %d, want 1", body.closeCalls)
			}
		})
	}
}

func TestRelayCodexAcceptedHTTPResponseRecoversBodyClosePanicAndDrains(t *testing.T) {
	t.Parallel()
	calls := &codexHTTPObserverLifecycleCalls{}
	body := newCodexHTTPObserverBody([]string{"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"}, nil, calls)
	body.closePanic = true
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}

	err := relayCodexAcceptedHTTPResponse(context.Background(), newCodexHTTPObserverWriter(), response, codexHTTPResponseModeSSE, &codexHTTPObserverLifecycle{calls: calls, generation: 1})
	if err == nil {
		t.Fatal("relay error = nil, want private-safe close uncertainty")
	}
	if strings.Contains(err.Error(), "private response body close panic") {
		t.Fatalf("relay error disclosed body close panic: %v", err)
	}
	got := calls.snapshot()
	if got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
	if !reflect.DeepEqual(got.order, []string{"close", "indeterminate@1", "drain@2"}) {
		t.Fatalf("lifecycle order = %#v", got.order)
	}
}

func TestRelayCodexAcceptedHTTPResponseRecoversBodyReadPanicAndDrains(t *testing.T) {
	t.Parallel()
	calls := &codexHTTPObserverLifecycleCalls{}
	body := newCodexHTTPObserverBody(nil, nil, calls)
	body.readPanic = true
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}

	err := relayCodexAcceptedHTTPResponse(context.Background(), newCodexHTTPObserverWriter(), response, codexHTTPResponseModeSSE, &codexHTTPObserverLifecycle{calls: calls, generation: 1})
	if err == nil {
		t.Fatal("relay error = nil, want private-safe read uncertainty")
	}
	if strings.Contains(err.Error(), "private response body read panic") {
		t.Fatalf("relay error disclosed body read panic: %v", err)
	}
	got := calls.snapshot()
	if got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
	if !reflect.DeepEqual(got.order, []string{"close", "indeterminate@1", "drain@2"}) {
		t.Fatalf("lifecycle order = %#v", got.order)
	}
	if body.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", body.closeCalls)
	}
}

func TestCodexHTTPResponseObserverFinishesOnceAndIgnoresLateBytes(t *testing.T) {
	t.Parallel()
	calls := &codexHTTPObserverLifecycleCalls{}
	lifecycle := &codexHTTPObserverLifecycle{calls: calls, generation: 1}
	observer := newCodexHTTPResponseObserver(context.Background(), codexHTTPResponseModeSSE, lifecycle)
	observer.Observe([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))

	const finishers = 32
	results := make(chan error, finishers)
	var group sync.WaitGroup
	for range finishers {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- observer.Finish(nil)
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("Finish error = %v", err)
		}
	}

	observer.Observe([]byte("data: {\"type\":\"response.failed\",\"response\":{}}\n\n"))
	if err := observer.Finish(errors.New("late private failure")); err != nil {
		t.Fatalf("late Finish changed cached result: %v", err)
	}
	got := calls.snapshot()
	if got.terminalKind != "completed" || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
	observer.mu.Lock()
	retainedEvidence := observer.evidence
	observer.mu.Unlock()
	if retainedEvidence != (codexHTTPObservedResponseEvidence{}) {
		t.Fatalf("observer retained raw response evidence: %#v", retainedEvidence)
	}
}

func TestCodexHTTPResponseObserverDoesNotRetryStaleTerminalCallbacks(t *testing.T) {
	t.Parallel()
	terminalErr := errors.New("stale terminal callback")
	drainErr := errors.New("stale drain callback")
	calls := &codexHTTPObserverLifecycleCalls{terminalErr: terminalErr, drainErr: drainErr}
	lifecycle := &codexHTTPObserverLifecycle{calls: calls, generation: 1}
	body := newCodexHTTPObserverBody([]string{"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"}, nil, calls)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}

	err := relayCodexAcceptedHTTPResponse(context.Background(), newCodexHTTPObserverWriter(), response, codexHTTPResponseModeSSE, lifecycle)
	if !errors.Is(err, terminalErr) || !errors.Is(err, drainErr) {
		t.Fatalf("relay error = %v, want terminal and drain errors", err)
	}
	got := calls.snapshot()
	if got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
	if !reflect.DeepEqual(got.order, []string{"close", "completed@1", "drain@1"}) {
		t.Fatalf("lifecycle order = %#v", got.order)
	}
}

func TestCodexHTTPResponseObserverBoundsAndClearsCompactBytes(t *testing.T) {
	t.Parallel()
	calls := &codexHTTPObserverLifecycleCalls{}
	observer := newCodexHTTPResponseObserver(context.Background(), codexHTTPResponseModeCompact, &codexHTTPObserverLifecycle{calls: calls, generation: 1})
	observer.Observe(bytes.Repeat([]byte{'x'}, codexProtocolMaxBytes))
	observer.Observe([]byte("private-overflow-body"))

	err := observer.Finish(nil)
	if !errors.Is(err, errCodexHTTPResponseCompactTooLarge) {
		t.Fatalf("Finish error = %v, want compact overflow", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("private-overflow-body")) {
		t.Fatalf("Finish error disclosed compact bytes: %v", err)
	}
	observer.mu.Lock()
	retained := len(observer.compact)
	observer.mu.Unlock()
	if retained != 0 {
		t.Fatalf("retained compact bytes = %d, want 0", retained)
	}
	got := calls.snapshot()
	if got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
}

func TestCodexHTTPResponseObserverRejectsOversizedCompactAnchorBeforeCompletion(t *testing.T) {
	t.Parallel()
	anchor := strings.Repeat("a", codexTurnIDMaxBytes+1)
	bodyBytes := `{"id":"` + anchor + `","output":[],"encrypted_content":"private-state"}`
	calls := &codexHTTPObserverLifecycleCalls{}
	body := newCodexHTTPObserverBody([]string{bodyBytes}, nil, calls)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}

	err := relayCodexAcceptedHTTPResponse(context.Background(), newCodexHTTPObserverWriter(), response, codexHTTPResponseModeCompact, &codexHTTPObserverLifecycle{calls: calls, generation: 1})
	if !errors.Is(err, errCodexHTTPResponseMalformed) {
		t.Fatalf("relay error = %v, want privacy-safe malformed error", err)
	}
	got := calls.snapshot()
	if got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
	wantEvidence := CodexHTTPResponseEvidence{HasEncryptedState: true}
	if got.responseEvidence != wantEvidence {
		t.Fatalf("response evidence = %#v, want %#v", got.responseEvidence, wantEvidence)
	}
}

func TestCodexHTTPResponseObserverRedactsProviderControlledCompactParserErrors(t *testing.T) {
	t.Parallel()
	const privateType = "private-provider-type-do-not-log"
	bodyBytes := `{"output":[{"type":"message","role":"assistant","content":[{"type":"` + privateType + `"}]}]}`
	calls := &codexHTTPObserverLifecycleCalls{}
	body := newCodexHTTPObserverBody([]string{bodyBytes}, nil, calls)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}

	err := relayCodexAcceptedHTTPResponse(context.Background(), newCodexHTTPObserverWriter(), response, codexHTTPResponseModeCompact, &codexHTTPObserverLifecycle{calls: calls, generation: 1})
	if !errors.Is(err, errCodexHTTPResponseMalformed) {
		t.Fatalf("relay error = %v, want privacy-safe malformed error", err)
	}
	if strings.Contains(err.Error(), privateType) {
		t.Fatalf("relay error disclosed provider-controlled type: %v", err)
	}
	got := calls.snapshot()
	if got.terminalKind != "indeterminate" || got.terminalCalls != 1 || got.drainCalls != 1 {
		t.Fatalf("lifecycle calls = %#v", got)
	}
}

type codexHTTPObserverLifecycleCalls struct {
	mu                sync.Mutex
	order             []string
	admissionEvidence CodexHTTPAdmissionEvidence
	admissionCalls    int
	terminalKind      string
	endTurn           bool
	responseEvidence  CodexHTTPResponseEvidence
	terminalCalls     int
	drainCalls        int
	cleanupCancelled  bool
	admissionErr      error
	terminalErr       error
	drainErr          error
}

func (calls *codexHTTPObserverLifecycleCalls) append(value string) {
	calls.mu.Lock()
	defer calls.mu.Unlock()
	calls.order = append(calls.order, value)
}

type codexHTTPObserverLifecycleSnapshot struct {
	order             []string
	admissionEvidence CodexHTTPAdmissionEvidence
	admissionCalls    int
	terminalKind      string
	endTurn           bool
	responseEvidence  CodexHTTPResponseEvidence
	terminalCalls     int
	drainCalls        int
	cleanupCancelled  bool
}

func (calls *codexHTTPObserverLifecycleCalls) snapshot() codexHTTPObserverLifecycleSnapshot {
	calls.mu.Lock()
	defer calls.mu.Unlock()
	return codexHTTPObserverLifecycleSnapshot{
		order:             append([]string(nil), calls.order...),
		admissionEvidence: calls.admissionEvidence,
		admissionCalls:    calls.admissionCalls,
		terminalKind:      calls.terminalKind,
		endTurn:           calls.endTurn,
		responseEvidence:  calls.responseEvidence,
		terminalCalls:     calls.terminalCalls,
		drainCalls:        calls.drainCalls,
		cleanupCancelled:  calls.cleanupCancelled,
	}
}

type codexHTTPObserverLifecycle struct {
	calls      *codexHTTPObserverLifecycleCalls
	generation int
}

func (lifecycle *codexHTTPObserverLifecycle) next() CodexHTTPRequestLifecycle {
	return &codexHTTPObserverLifecycle{calls: lifecycle.calls, generation: lifecycle.generation + 1}
}

func (lifecycle *codexHTTPObserverLifecycle) EverAdmitted() bool { return true }

func (lifecycle *codexHTTPObserverLifecycle) AccountKey() codex.AccountKey { return "account" }

func (lifecycle *codexHTTPObserverLifecycle) MarkDispatchedContext(context.Context) (CodexHTTPRequestLifecycle, error) {
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) RejectAndPrepareContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error) {
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) RecordAccountUnavailableContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error) {
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) RecordQuotaExhaustedContext(context.Context, uint32) (CodexHTTPRequestLifecycle, error) {
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) CompleteAccountUnavailableCycleContext(context.Context) (CodexHTTPRequestLifecycle, error) {
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) AbandonBeforeDispatchContext(context.Context) (CodexHTTPRequestLifecycle, error) {
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) FinishRejected() (CodexHTTPRequestLifecycle, error) {
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) IndeterminateContext(ctx context.Context, evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	lifecycle.calls.mu.Lock()
	lifecycle.calls.terminalKind = "indeterminate"
	lifecycle.calls.responseEvidence = evidence
	lifecycle.calls.terminalCalls++
	lifecycle.calls.cleanupCancelled = ctx.Err() != nil
	lifecycle.calls.order = append(lifecycle.calls.order, "indeterminate@"+strconv.Itoa(lifecycle.generation))
	err := lifecycle.calls.terminalErr
	lifecycle.calls.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) Drain() (CodexHTTPRequestLifecycle, error) {
	lifecycle.calls.mu.Lock()
	lifecycle.calls.drainCalls++
	lifecycle.calls.order = append(lifecycle.calls.order, "drain@"+strconv.Itoa(lifecycle.generation))
	err := lifecycle.calls.drainErr
	lifecycle.calls.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) AdmitHTTP2xxContext(_ context.Context, evidence CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error) {
	lifecycle.calls.mu.Lock()
	lifecycle.calls.admissionEvidence = evidence
	lifecycle.calls.admissionCalls++
	lifecycle.calls.order = append(lifecycle.calls.order, "admit@"+strconv.Itoa(lifecycle.generation))
	err := lifecycle.calls.admissionErr
	lifecycle.calls.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) ProviderCompleted(evidence CodexHTTPCompletionEvidence) (CodexHTTPRequestLifecycle, error) {
	lifecycle.calls.mu.Lock()
	lifecycle.calls.terminalKind = "completed"
	lifecycle.calls.endTurn = evidence.EndTurn
	lifecycle.calls.responseEvidence = evidence.CodexHTTPResponseEvidence
	lifecycle.calls.terminalCalls++
	lifecycle.calls.order = append(lifecycle.calls.order, "completed@"+strconv.Itoa(lifecycle.generation))
	err := lifecycle.calls.terminalErr
	lifecycle.calls.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return lifecycle.next(), nil
}

func (lifecycle *codexHTTPObserverLifecycle) ProviderFailed(evidence CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	lifecycle.calls.mu.Lock()
	lifecycle.calls.terminalKind = "failed"
	lifecycle.calls.responseEvidence = evidence
	lifecycle.calls.terminalCalls++
	lifecycle.calls.order = append(lifecycle.calls.order, "failed@"+strconv.Itoa(lifecycle.generation))
	err := lifecycle.calls.terminalErr
	lifecycle.calls.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return lifecycle.next(), nil
}

type codexHTTPObserverBody struct {
	chunks     [][]byte
	readErr    error
	readPanic  bool
	closeErr   error
	closePanic bool
	closeCalls int
	calls      *codexHTTPObserverLifecycleCalls
}

func newCodexHTTPObserverBody(chunks []string, readErr error, calls *codexHTTPObserverLifecycleCalls) *codexHTTPObserverBody {
	body := &codexHTTPObserverBody{readErr: readErr, calls: calls}
	for _, chunk := range chunks {
		body.chunks = append(body.chunks, []byte(chunk))
	}
	return body
}

func (body *codexHTTPObserverBody) Read(buffer []byte) (int, error) {
	if body.readPanic {
		panic("private response body read panic")
	}
	if len(body.chunks) == 0 {
		if body.readErr != nil {
			err := body.readErr
			body.readErr = nil
			return 0, err
		}
		return 0, io.EOF
	}
	read := copy(buffer, body.chunks[0])
	body.chunks[0] = body.chunks[0][read:]
	if len(body.chunks[0]) == 0 {
		body.chunks = body.chunks[1:]
	}
	return read, nil
}

func (body *codexHTTPObserverBody) Close() error {
	body.closeCalls++
	body.calls.append("close")
	if body.closePanic {
		panic("private response body close panic")
	}
	return body.closeErr
}

type codexHTTPObserverWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	flushes  int
	writeErr error
}

func newCodexHTTPObserverWriter() *codexHTTPObserverWriter {
	return &codexHTTPObserverWriter{header: make(http.Header)}
}

func (writer *codexHTTPObserverWriter) Header() http.Header { return writer.header }

func (writer *codexHTTPObserverWriter) WriteHeader(status int) { writer.status = status }

func (writer *codexHTTPObserverWriter) Write(data []byte) (int, error) {
	if writer.writeErr != nil {
		return 0, writer.writeErr
	}
	return writer.body.Write(data)
}

func (writer *codexHTTPObserverWriter) Flush() { writer.flushes++ }
