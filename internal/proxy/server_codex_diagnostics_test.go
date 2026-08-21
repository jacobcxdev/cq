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

func TestServerEmitDiagnosticsAllowsCodexWebSocketBrokerRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.jsonl")
	writer, err := OpenDiagnosticsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	recorder := newCodexDiagnosticsTestCanary(t)
	writer.SetCodexCanary(recorder)
	server := &Server{Diag: writer, CodexCanary: recorder}
	event := structurallySafeCodexRouteEvent()
	event.RouteKind = "codex_websocket_broker"

	server.emitDiagnostics(event)

	if got := recorder.State().SecretLeaks; got != 0 {
		t.Fatalf("secret leaks = %d, want 0", got)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	events := readDiagnosticsEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].RouteKind; got != "codex_websocket_broker" {
		t.Fatalf("route kind = %q, want codex_websocket_broker", got)
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

func TestServerEmitDiagnosticsProjectsRequestedModelClassBeforeWriter(t *testing.T) {
	for _, test := range []struct {
		requestedModel string
		wantClass      string
	}{
		{requestedModel: "gpt-5.6-sol", wantClass: "gpt_5_6_sol"},
		{requestedModel: "gpt-5.6-terra", wantClass: "gpt_5_6_terra"},
		{requestedModel: "gpt-5.6-luna", wantClass: "gpt_5_6_luna"},
	} {
		t.Run(test.requestedModel, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.jsonl")
			writer, err := OpenDiagnosticsWriter(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.Close() })
			recorder := newCodexDiagnosticsTestCanary(t)
			writer.SetCodexCanary(recorder)
			server := &Server{Diag: writer, CodexCanary: recorder}
			event := structurallySafeCodexRouteEvent()
			event.RequestedModelClass = test.requestedModel

			server.emitDiagnostics(event)
			if got := recorder.State().SecretLeaks; got != 0 {
				t.Fatalf("secret leaks = %d, want 0", got)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), test.requestedModel) {
				t.Fatalf("raw requested model reached diagnostics: %q", data)
			}
			if !strings.Contains(string(data), `"requested_model_class":"`+test.wantClass+`"`) {
				t.Fatalf("projected class missing from diagnostics: %q", data)
			}
		})
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
