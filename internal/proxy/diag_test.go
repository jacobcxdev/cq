package proxy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticsWriterCreatesAndAppendsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")

	w, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	if err := w.Write(RouteEvent{
		Time:       time.Unix(1, 0).UTC(),
		Method:     "POST",
		Path:       "/v1/messages",
		Provider:   "claude",
		RouteKind:  "anthropic_messages",
		Model:      "claude-sonnet",
		StatusCode: 200,
		LatencyMS:  12,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w, err = OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("reopen diagnostics writer: %v", err)
	}
	if err := w.Write(RouteEvent{
		Time:       time.Unix(2, 0).UTC(),
		Method:     "POST",
		Path:       "/responses",
		Provider:   "codex",
		RouteKind:  "codex_native",
		Model:      "gpt-5.4",
		StatusCode: 201,
		LatencyMS:  34,
	}); err != nil {
		t.Fatalf("append Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("append Close: %v", err)
	}

	events := readDiagnosticsEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Path != "/v1/messages" || events[1].Path != "/responses" {
		t.Fatalf("events paths = %q, %q", events[0].Path, events[1].Path)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat diagnostics log: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %#o, want 0600", got)
		}
	}
}

func TestDiagnosticsWriterNilSafe(t *testing.T) {
	var w *DiagnosticsWriter
	if err := w.Write(RouteEvent{Path: "/v1/messages"}); err != nil {
		t.Fatalf("nil Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestDiagnosticsWriterConcurrentWritesProduceValidJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	w, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}

	const count = 64
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := w.Write(RouteEvent{
				Time:       time.Unix(int64(i), 0).UTC(),
				Method:     "POST",
				Path:       "/v1/messages",
				Provider:   "claude",
				StatusCode: 200,
			}); err != nil {
				t.Errorf("Write(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readDiagnosticsEvents(t, path)
	if len(events) != count {
		t.Fatalf("events = %d, want %d", len(events), count)
	}
	for i, ev := range events {
		if ev.Method != "POST" || ev.Path != "/v1/messages" || ev.Provider != "claude" {
			t.Fatalf("event %d = %+v", i, ev)
		}
	}
}

func TestDiagnosticsWriterCloseSafeAndStopsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	w, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatalf("OpenDiagnosticsWriter: %v", err)
	}
	if err := w.Write(RouteEvent{Time: time.Unix(1, 0).UTC(), Method: "GET", Path: "/health", Provider: "proxy"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := w.Write(RouteEvent{Time: time.Unix(2, 0).UTC(), Method: "GET", Path: "/health", Provider: "proxy"}); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}

	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events after closed write = %d, want 1", len(events))
	}
}

func readDiagnosticsEvents(t *testing.T, path string) []RouteEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open diagnostics log: %v", err)
	}
	defer f.Close()

	var events []RouteEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event RouteEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid diagnostics JSON line %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan diagnostics log: %v", err)
	}
	return events
}
