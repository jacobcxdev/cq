package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyTraceFollowCheckpointAttachesCurrentBeforeRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	history := proxyTraceTestRecord("history")
	initial := proxyTraceTestRecord("initial")
	if err := os.WriteFile(path+".1", []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	records, follower, err := readProxyTraceRecordsForFollowWithHook(path, proxyTraceFilter{}, func() {
		rotateProxyTraceTestLog(t, path, proxyTraceTestRecord("successor"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	if got, want := proxyTraceIDs(records), []string{"trace:history", "trace:initial", "trace:successor"}; !slices.Equal(got, want) {
		t.Fatalf("trace IDs = %v, want %v", got, want)
	}
}

func TestProxyTraceFollowBridgeKeepsAttachedInodesAcrossRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	initial := proxyTraceTestRecord("initial")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	_, follower, err := readProxyTraceRecordsForFollow(path, proxyTraceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	tail := proxyTraceTestRecord("tail")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(tail); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	rotateProxyTraceTestLog(t, path, proxyTraceTestRecord("successor-one"))
	var output bytes.Buffer
	if err := follower.readAvailableWithHook(path, proxyTraceFilter{}, &output, true, func() {
		rotateProxyTraceTestLog(t, path, proxyTraceTestRecord("successor-two"))
		rotateProxyTraceTestLog(t, path, proxyTraceTestRecord("successor-three"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := follower.ReadAvailable(path, proxyTraceFilter{}, &output, true); err != nil {
		t.Fatal(err)
	}
	if got, want := proxyTraceOutputIDs(t, output.Bytes()), []string{"trace:tail", "trace:successor-one", "trace:successor-two", "trace:successor-three"}; !slices.Equal(got, want) {
		t.Fatalf("trace IDs = %v, want %v", got, want)
	}
}

func TestProxyTraceFollowFirstPollAttachesRetainedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	records, follower, err := readProxyTraceRecordsForFollow(path, proxyTraceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	if len(records) != 0 {
		t.Fatalf("initial records = %+v", records)
	}
	if err := os.WriteFile(path+".1", []byte(proxyTraceTestRecord("retained")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(proxyTraceTestRecord("current")), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := follower.ReadAvailable(path, proxyTraceFilter{}, &output, true); err != nil {
		t.Fatal(err)
	}
	if got, want := proxyTraceOutputIDs(t, output.Bytes()), []string{"trace:retained", "trace:current"}; !slices.Equal(got, want) {
		t.Fatalf("trace IDs = %v, want %v", got, want)
	}
}

func proxyTraceTestRecord(id string) string {
	return `{"time":"2026-09-02T10:00:00Z","event_type":"codex_trace","trace_id":"trace:` + id + `","sequence":1,"transport":"http","phase":"terminal","outcome":"success"}` + "\n"
}

func rotateProxyTraceTestLog(t *testing.T, path, current string) {
	t.Helper()
	for index := 3; index >= 1; index-- {
		from := fmt.Sprintf("%s.%d", path, index)
		to := fmt.Sprintf("%s.%d", path, index+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Rename(path, path+".1"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
}

func proxyTraceIDs(records []proxyTraceRecord) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.TraceID)
	}
	return ids
}

func proxyTraceOutputIDs(t *testing.T, output []byte) []string {
	t.Helper()
	var ids []string
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
		var record proxyTraceRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, record.TraceID)
	}
	return ids
}

func TestProxyTraceFiltersUserFacingThreadSelector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	keys := proxy.CodexTraceSessionKeys("codex://threads/thread-one")
	threadKey := ""
	for _, key := range keys {
		if strings.HasPrefix(key, "codex-thread:") {
			threadKey = key
			break
		}
	}
	if threadKey == "" {
		t.Fatal("thread selector did not derive codex-thread correlation")
	}
	old := `{"time":"2026-09-02T10:00:00Z","event_type":"codex_trace","trace_id":"trace:00000000000000000000000000000001","sequence":1,"session_key":"codex-session:aaaaaaaaaaaa","thread_key":"` + threadKey + `","transport":"http","phase":"ingress","outcome":"accepted"}` + "\n"
	current := `{"time":"2026-09-02T10:00:01Z","event_type":"codex_trace","trace_id":"trace:00000000000000000000000000000001","sequence":2,"session_key":"codex-session:aaaaaaaaaaaa","thread_key":"` + threadKey + `","transport":"http","phase":"terminal","outcome":"error","status_code":503,"reason":"bound_unresolved"}` + "\n" +
		`{"time":"2026-09-02T10:00:02Z","event_type":"codex_trace","trace_id":"trace:00000000000000000000000000000002","sequence":1,"session_key":"codex-session:ffffffffffff","transport":"http","phase":"terminal","outcome":"success"}` + "\n"
	if err := os.WriteFile(path+".1", []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runProxyTraceWith([]string{"--session", "codex://threads/thread-one", "--json"}, &output, proxyTraceDependencies{
		LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{DiagnosticsLog: path}, nil },
		Now:        func() time.Time { return time.Date(2026, 9, 2, 10, 1, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Count(got, `"trace_id"`) != 2 || !strings.Contains(got, `"status_code":503`) || strings.Contains(got, "ffffffffffff") {
		t.Fatalf("output = %s", got)
	}
}

func TestProxyTraceRequiresConfiguredLog(t *testing.T) {
	err := runProxyTraceWith(nil, &bytes.Buffer{}, proxyTraceDependencies{
		LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{}, nil },
		Now:        time.Now,
	})
	if err == nil || !strings.Contains(err.Error(), "diagnostics log is not configured") {
		t.Fatalf("error = %v", err)
	}
}
