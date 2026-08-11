package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerEmitDiagnosticsRejectsUnsafeCodexWithoutWriter(t *testing.T) {
	recorder := newCodexDiagnosticsTestCanary(t)
	server := &Server{CodexCanary: recorder}
	unsafeFixture := "raw-private-fixture"
	event := structurallySafeCodexRouteEvent()
	event.Path = unsafeFixture

	server.emitDiagnostics(event)

	if got := recorder.State().SecretLeaks; got != 1 {
		t.Fatalf("secret leaks = %d, want 1", got)
	}
}

func TestServerEmitDiagnosticsProjectsCallerControlledModelBeforeWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	writer, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	recorder := newCodexDiagnosticsTestCanary(t)
	writer.SetCodexCanary(recorder)
	server := &Server{Diag: writer, CodexCanary: recorder}
	unsafeFixture := "gpt-raw-private-fixture"
	event := structurallySafeCodexRouteEvent()
	event.Model = unsafeFixture
	event.Bucket = "model:" + unsafeFixture

	server.emitDiagnostics(event)

	if got := recorder.State().SecretLeaks; got != 0 {
		t.Fatalf("secret leaks = %d, want 0 after safe projection", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || strings.Contains(string(data), unsafeFixture) {
		t.Fatalf("raw model reached diagnostics: %q", data)
	}
	if !strings.Contains(string(data), `"model":"model_family_gpt"`) || !strings.Contains(string(data), `"bucket":"capacity_model_scoped"`) {
		t.Fatalf("projected event = %q", data)
	}
}

func TestServerEmitDiagnosticsUsesWriterCanaryAsCompatibilityFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	writer, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	recorder := newCodexDiagnosticsTestCanary(t)
	writer.SetCodexCanary(recorder)
	server := &Server{Diag: writer}
	event := structurallySafeCodexRouteEvent()
	event.Reason = "raw-private-fixture"

	server.emitDiagnostics(event)

	if got := recorder.State().SecretLeaks; got != 1 {
		t.Fatalf("secret leaks = %d, want 1", got)
	}
}

func TestServerServeDoesNotMintCanaryDayWithoutAdmission(t *testing.T) {
	recorder := newCodexDiagnosticsTestCanary(t)
	listener := listenServingAttestorTestTCP4(t)
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: NewServingAttestor(),
		CodexCanary:     recorder,
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(ctx, listener) }()

	time.Sleep(20 * time.Millisecond)
	if state := recorder.State(); !state.LastObservedAt.IsZero() || state.ConsecutiveCalendarDays != 0 {
		cancel()
		<-serveDone
		t.Fatalf("startup minted canary day evidence: %+v", state)
	}
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("serve error = %v", err)
	}
}
