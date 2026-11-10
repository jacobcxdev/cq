package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type diagnosticsFlushFile struct {
	calls    []string
	syncErr  error
	closeErr error
	entered  chan struct{}
	release  <-chan struct{}
}

func (f *diagnosticsFlushFile) Write(data []byte) (int, error) { return len(data), nil }
func (f *diagnosticsFlushFile) Sync() error {
	f.calls = append(f.calls, "sync")
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	return f.syncErr
}
func (f *diagnosticsFlushFile) Close() error {
	f.calls = append(f.calls, "close")
	return f.closeErr
}

func TestJSONLWriterFlushesBeforeClosing(t *testing.T) {
	syncFailure := errors.New("sync failed")
	closeFailure := errors.New("close failed")
	for _, test := range []struct {
		name              string
		syncErr, closeErr error
	}{
		{name: "success"},
		{name: "sync failure", syncErr: syncFailure},
		{name: "close failure", closeErr: closeFailure},
		{name: "both failures", syncErr: syncFailure, closeErr: closeFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := &diagnosticsFlushFile{syncErr: test.syncErr, closeErr: test.closeErr}
			writer := &jsonlWriter{file: file}
			err := writer.close()
			if !reflect.DeepEqual(file.calls, []string{"sync", "close"}) {
				t.Fatalf("shutdown operations = %v, want sync then close", file.calls)
			}
			if (err != nil) != (test.syncErr != nil || test.closeErr != nil) || (test.syncErr != nil && !errors.Is(err, test.syncErr)) || (test.closeErr != nil && !errors.Is(err, test.closeErr)) {
				t.Fatalf("close lost flush/close failure: %v", err)
			}
			if err := writer.close(); err != nil || len(file.calls) != 2 {
				t.Fatalf("repeated close = %v, operations %v", err, file.calls)
			}
		})
	}
}

func TestJSONLWriterCloseWaitsForDiskSync(t *testing.T) {
	release := make(chan struct{})
	file := &diagnosticsFlushFile{entered: make(chan struct{}), release: release}
	writer := &jsonlWriter{file: file}
	done := make(chan error, 1)
	go func() { done <- writer.close() }()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	select {
	case <-file.entered:
	case <-time.After(time.Second):
		t.Fatal("close did not reach disk sync")
	}
	select {
	case err := <-done:
		t.Fatalf("close returned before disk sync completed: %v", err)
	default:
	}
	// Cleanup releases the deliberately blocked sync even if an assertion fails.
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestJSONLWriterRotationFlushesBeforeRetainingFile(t *testing.T) {
	for _, failSync := range []bool{false, true} {
		t.Run(fmt.Sprint(failSync), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.jsonl")
			want := []byte("retained evidence\n")
			if err := os.WriteFile(path, want, 0o600); err != nil {
				t.Fatal(err)
			}
			file := &diagnosticsFlushFile{}
			if failSync {
				file.syncErr = errors.New("disk flush failed")
			}
			writer := &jsonlWriter{file: file, path: path, retained: 1}
			defer writer.close()
			err := writer.rotateLocked()
			if !reflect.DeepEqual(file.calls, []string{"sync", "close"}) {
				t.Fatalf("rotation operations = %v, want sync then close", file.calls)
			}
			retainedPath := path + ".1"
			if failSync {
				if !errors.Is(err, file.syncErr) {
					t.Fatalf("rotation hid disk flush failure: %v", err)
				}
				if _, err := os.Stat(retainedPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed flush still rotated evidence")
				}
				if err := writer.encode("later"); !errors.Is(err, file.syncErr) {
					t.Fatalf("failed writer accepted later evidence: %v", err)
				}
				if err := writer.close(); !errors.Is(err, file.syncErr) {
					t.Fatalf("shutdown hid earlier flush failure: %v", err)
				}
				retainedPath = path
			} else if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(retainedPath)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("rotation lost retained evidence: %q, %v", got, err)
			}
		})
	}
}

func TestJSONLWriterCloseReportsDirectorySyncFailure(t *testing.T) {
	file := &diagnosticsFlushFile{}
	writer := &jsonlWriter{file: file, path: filepath.Join(t.TempDir(), "missing", "routes.jsonl")}
	if err := writer.close(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown hid directory flush failure: %v", err)
	}
	if !reflect.DeepEqual(file.calls, []string{"sync", "close"}) {
		t.Fatalf("directory flush failure leaked file: %v", file.calls)
	}
}

func TestJSONLWriterBoundsOversizedPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.jsonl")
	w, err := openJSONLWriterWithLimits(path, 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	event := PayloadEvent{TraceID: "trace:test", Complete: true, BodyBytes: 8192, Body: encodeBody([]byte(strings.Repeat("secret-body", 819)))}
	for range 4 {
		if err := w.encode(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{path, path + ".1", path + ".2"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 1024 {
			t.Fatalf("retained file = %d bytes, limit 1024", len(data))
		}
		var got PayloadEvent
		if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Truncated || got.Complete || got.BodyBytes != event.BodyBytes || got.TraceID != event.TraceID || len(got.Body) == 0 {
			t.Fatalf("oversized event lost explicit prefix/evidence: %#v", got)
		}
	}
}

func TestJSONLWriterBoundsOversizedNonPayloadRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	w, err := openJSONLWriterWithLimits(path, 512, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.encode(map[string]string{"event_type": "codex_trace", "trace_id": "trace:test", "error": strings.Repeat("\x00", 4096)}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 512 {
		t.Fatalf("oversized record = %d bytes, limit 512", len(data))
	}
	var got struct {
		EventType         string `json:"event_type"`
		Truncated         bool   `json:"truncated"`
		Prefix            string `json:"record_prefix"`
		TraceID           string `json:"trace_id"`
		OriginalEventType string `json:"original_event_type"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.EventType != "diagnostics_record_truncated" || !got.Truncated || got.Prefix == "" {
		t.Fatalf("missing truncation evidence: %s", data)
	}
	if got.TraceID != "trace:test" || got.OriginalEventType != "codex_trace" {
		t.Fatal("truncation lost trace identity")
	}
}

func TestJSONLWriterReportsRepeatedRotationFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := openJSONLWriterWithLimits(filepath.Join(dir, "routes.jsonl"), 64, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.encode(strings.Repeat("x", 40)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.encode(strings.Repeat("y", 40)); err == nil {
		t.Fatal("rotation failure hidden")
	}
	if err := w.encode("later"); err == nil {
		t.Fatal("failed writer silently accepted later evidence")
	}
}

func TestBoundedDiagnosticsPayloadPrefixAlwaysUsesJSONString(t *testing.T) {
	event := PayloadEvent{TraceID: "trace:test", BodyBytes: 4096, Body: json.RawMessage(strings.Repeat("1", 4096)), Complete: true}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := boundedDiagnosticsRecord(event, encoded, 512)
	if err != nil {
		t.Fatal(err)
	}
	var got PayloadEvent
	if err := json.Unmarshal(bounded, &got); err != nil {
		t.Fatal(err)
	}
	var prefix string
	if err := json.Unmarshal(got.Body, &prefix); err != nil {
		t.Fatalf("body_encoding=json-prefix needs JSON string, got %s: %v", got.Body, err)
	}
	if prefix == "" || !strings.HasPrefix(string(event.Body), prefix) {
		t.Fatal("prefix lost original encoded bytes")
	}
}
