package main

import (
	"testing"
)

func TestProxyTraceIncludesOversizedRecordMarker(t *testing.T) {
	raw := []byte(`{"time":"2026-11-10T18:00:00Z","event_type":"diagnostics_record_truncated","original_event_type":"codex_trace","trace_id":"trace:test","session_key":"session:test","truncated":true,"record_bytes":70000000,"record_prefix":"prefix"}`)
	got, ok := parseProxyTraceRecord(raw, proxyTraceFilter{traceID: "trace:test", session: map[string]struct{}{"session:test": {}}})
	if !ok || got.Phase != "record_truncated" || got.Reason != "record_too_large" {
		t.Fatalf("oversized trace marker hidden: %#v %v", got, ok)
	}
	if _, ok := parseProxyTraceRecord(raw, proxyTraceFilter{payload: true}); ok {
		t.Fatal("route marker entered payload stream")
	}
}
