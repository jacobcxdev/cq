package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexPayloadTraceCapturesExactResponseAndExcludesCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloads, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withCodexTrace(context.Background(), nil, payloads, CodexTraceStart{
		Transport: "http", SessionKey: "codex-session:0123456789ab", SessionSource: "session_id",
	})
	body := []byte{0xff, 0x00, 0x01}
	response := &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"Authorization": {"Bearer secret"},
			"Set-Cookie":    {"session=secret"},
			"X-Request-Id":  {"request-safe"},
		},
		Body: io.NopCloser(bytes.NewReader(body)),
	}
	captureCodexHTTPResponsePayload(ctx, response)
	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, body) {
		t.Fatalf("response body = %x, want %x", read, body)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := payloads.Close(); err != nil {
		t.Fatal(err)
	}

	events := readCodexPayloadEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("payload events = %d, want 1", len(events))
	}
	event := events[0]
	if event.EventType != "codex_payload" || event.TraceID == "" || event.Direction != "upstream_response" || event.StatusCode != http.StatusForbidden || !event.Complete || event.BodyBytes != len(body) || event.BodyEncoding != "base64" {
		t.Fatalf("payload event = %+v", event)
	}
	if event.Headers.Get("Authorization") != "" || event.Headers.Get("Set-Cookie") != "" || event.Headers.Get("X-Request-Id") != "request-safe" {
		t.Fatalf("captured headers = %#v", event.Headers)
	}
}

func TestCodexPayloadTraceMarksEarlyResponseCloseIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloads, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withCodexTrace(context.Background(), nil, payloads, CodexTraceStart{Transport: "http"})
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("partial-response")))}
	captureCodexHTTPResponsePayload(ctx, response)
	buffer := make([]byte, 7)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := payloads.Close(); err != nil {
		t.Fatal(err)
	}
	events := readCodexPayloadEvents(t, path)
	if len(events) != 1 || events[0].Complete || events[0].BodyBytes != len(buffer) {
		t.Fatalf("incomplete payload events = %+v", events)
	}
}

func TestCodexPayloadTraceBoundsStreamingResponseCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	payloads, err := OpenPayloadWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withCodexTrace(context.Background(), nil, payloads, CodexTraceStart{Transport: "http"})
	body := bytes.Repeat([]byte{'x'}, codexPayloadCaptureMaxBytes+1024)
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}
	captureCodexHTTPResponsePayload(ctx, response)
	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, body) {
		t.Fatal("payload capture changed streamed response")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := payloads.Close(); err != nil {
		t.Fatal(err)
	}
	events := readCodexPayloadEvents(t, path)
	if len(events) != 1 || !events[0].Complete || !events[0].Truncated || events[0].BodyBytes != len(body) {
		t.Fatalf("bounded payload event = %+v", events)
	}
	var captured string
	if err := json.Unmarshal(events[0].Body, &captured); err != nil {
		t.Fatal(err)
	}
	if len(captured) != codexPayloadCaptureMaxBytes || captured != string(body[:codexPayloadCaptureMaxBytes]) {
		t.Fatalf("captured payload bytes = %d, want bounded prefix %d", len(captured), codexPayloadCaptureMaxBytes)
	}
}

func readCodexPayloadEvents(t *testing.T, path string) []PayloadEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []PayloadEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), codexPayloadCaptureMaxBytes*2)
	for scanner.Scan() {
		var event PayloadEvent
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
