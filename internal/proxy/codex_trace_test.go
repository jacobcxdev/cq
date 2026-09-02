package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexTraceHTTPOutcomeTreatsWebSocketUpgradeAsSuccess(t *testing.T) {
	if got := codexTraceHTTPOutcome(http.StatusSwitchingProtocols); got != "success" {
		t.Fatalf("WebSocket upgrade outcome = %q, want success", got)
	}
}

func TestCodexTraceWritesOrderedCausalEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	w, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withCodexTrace(context.Background(), w, nil, CodexTraceStart{
		Transport:     "http",
		ConnectionID:  "connection:test",
		SessionKey:    "codex-session:0123456789ab",
		SessionSource: "session_id",
		ThreadKey:     "codex-thread:abcdef012345",
	})
	emitCodexTrace(ctx, CodexTraceEvent{Phase: "ingress", Outcome: "accepted"})
	emitCodexTrace(ctx, CodexTraceEvent{
		Phase:       "dispatch",
		Outcome:     "selected",
		AccountHint: "account:0123456789ab",
		Attempt:     1,
		Lease: &CodexTraceLeaseState{
			Classification:    "bound",
			JournalGeneration: 42,
		},
	})
	emitCodexTrace(ctx, CodexTraceEvent{Phase: "terminal", Outcome: "success", StatusCode: 200})
	traceID, connectionID := codexTraceIDs(ctx)
	if traceID == "" || connectionID != "connection:test" {
		t.Fatalf("trace ids = %q, %q", traceID, connectionID)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []CodexTraceEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event CodexTraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for index, event := range events {
		if event.EventType != "codex_trace" || event.TraceID != traceID || event.Sequence != uint64(index+1) {
			t.Fatalf("event[%d] = %+v", index, event)
		}
		if event.SessionKey != "codex-session:0123456789ab" || event.ThreadKey != "codex-thread:abcdef012345" || event.Transport != "http" {
			t.Fatalf("event[%d] missing trace identity: %+v", index, event)
		}
	}
	if events[1].Lease == nil || events[1].Lease.JournalGeneration != 42 {
		t.Fatalf("dispatch lease = %+v", events[1].Lease)
	}
}

func TestJSONLWriterRotatesAndRetainsBoundedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	w, err := openJSONLWriterWithLimits(path, 180, 2)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 12; sequence++ {
		if err := w.encode(CodexTraceEvent{
			Time: time.Unix(int64(sequence), 0).UTC(), EventType: "codex_trace", TraceID: "trace:test",
			Sequence: uint64(sequence), Transport: "http", Phase: "terminal", Outcome: "success",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".1", path + ".2"} {
		if _, err := os.Stat(candidate); err != nil {
			t.Fatalf("missing retained log %s: %v", candidate, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected over-retained log: %v", err)
	}
}

func TestCodexTraceChildGetsNewTraceAndKeepsConnection(t *testing.T) {
	parent := withCodexTrace(context.Background(), nil, nil, CodexTraceStart{
		Transport: "websocket", ConnectionID: "connection:test",
	})
	child := withCodexTraceSpan(parent, CodexTraceStart{
		Transport: "websocket", SessionKey: "ws-session:0123456789ab", SessionSource: "ws:thread_id",
	})
	parentTrace, parentConnection := codexTraceIDs(parent)
	childTrace, childConnection := codexTraceIDs(child)
	if parentTrace == "" || childTrace == "" || parentTrace == childTrace {
		t.Fatalf("parent trace = %q, child trace = %q", parentTrace, childTrace)
	}
	if parentConnection != "connection:test" || childConnection != parentConnection {
		t.Fatalf("connections = %q, %q", parentConnection, childConnection)
	}
}

func TestRouteSummaryJoinsWebSocketFrameTrace(t *testing.T) {
	ctx := withCodexTrace(context.Background(), nil, nil, CodexTraceStart{
		Transport: "websocket", ConnectionID: "connection:test",
	})
	frame := &routeDiagnostics{}
	frame.applyCodexTrace(ctx)
	event := RouteEvent{}
	event.applyRouteDiagnostics(frame)
	traceID, connectionID := codexTraceIDs(ctx)
	if event.TraceID != traceID || event.ConnectionID != connectionID {
		t.Fatalf("route trace ids = %q/%q, want %q/%q", event.TraceID, event.ConnectionID, traceID, connectionID)
	}
}

func readCodexTraceEvents(t *testing.T, path string) []CodexTraceEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []CodexTraceEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var envelope struct {
			EventType string `json:"event_type"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.EventType != "codex_trace" {
			continue
		}
		var event CodexTraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func requireCodexTraceEvent(t *testing.T, events []CodexTraceEvent, match func(CodexTraceEvent) bool, description string) {
	t.Helper()
	for _, event := range events {
		if match(event) {
			return
		}
	}
	available := make([]string, 0, len(events))
	for _, event := range events {
		available = append(available, event.Phase+"/"+event.Stage+"/"+event.Outcome+"/"+event.Reason)
	}
	t.Fatalf("missing trace event %s; available: %s", description, strings.Join(available, ", "))
}
