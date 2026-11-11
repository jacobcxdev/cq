package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// These failures must keep transport identity even if lifecycle cleanup also
// returns ErrCodexWSInvalidFrame. No raw error, endpoint or credential may log.
func TestCodexWSBrokerDialTransportDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  string
	}{
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}, Err: syscall.ECONNREFUSED}, "connection_refused"},
		{"dns", &net.DNSError{Err: "private-secret", Name: "private.invalid"}, "dns"},
		{"cancelled", context.Canceled, "cancelled"},
		{"deadline", context.DeadlineExceeded, "deadline"},
		{"timeout", os.ErrDeadlineExceeded, "timeout"},
		{"connect", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, "connect"},
		{"reset", syscall.ECONNRESET, "connection_reset"},
		{"pipe", syscall.EPIPE, "broken_pipe"},
		{"eof", io.EOF, "eof"},
		{"truncated", io.ErrUnexpectedEOF, "unexpected_eof"},
		{"tls record", tls.RecordHeaderError{Msg: "private-secret"}, "tls"},
		{"tls certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "tls"},
		{"protocol", websocket.ErrBadHandshake, "protocol"},
		{"unknown", errors.New("private-secret"), "unknown"},
	} {
		for _, prewarm := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prewarm=%t", test.name, prewarm), func(t *testing.T) {
				coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
				planner := &codexWSBrokerPlannerStub{runtime: newCodexLeaseRuntimeTest(t, coordinator), slots: []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}}
				downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}, {err: io.EOF}}}
				if prewarm {
					downstream.reads[0].payload = []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session-a\",\"thread_id\":\"thread-a\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
				}
				cause := &url.Error{Op: "Get", URL: "wss://user:private-secret@private.invalid/responses", Err: fmt.Errorf("private-secret: %w", test.cause)}
				dialer := &codexWSBrokerDialerStub{outcomes: map[codex.AccountKey][]codexWSBrokerDialOutcome{"account-a": {{err: cause}}}}
				broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: dialer, UpstreamURL: "wss://example.invalid/responses", DownstreamGeneration: 41})
				if err != nil {
					t.Fatal(err)
				}
				tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
				writer, err := OpenDiagnosticsWriter(tracePath)
				if err != nil {
					t.Fatal(err)
				}
				ctx, diag := withRouteDiagnostics(context.Background())
				ctx = withCodexTrace(ctx, writer, nil, CodexTraceStart{Transport: "websocket"})
				var serveErr error
				stderr := captureStderr(t, func() { serveErr = broker.Serve(ctx, downstream) })
				event := RouteEvent{Provider: "codex", StatusCode: 101}
				event.applyRouteDiagnostics(diag)
				if err := writer.Write(projectCodexDiagnostics(event)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				traces := readCodexTraceEvents(t, tracePath)
				requireCodexTraceEvent(t, traces, func(e CodexTraceEvent) bool {
					return e.Phase == "upstream_dial" && e.Outcome == "error" && e.Reason == test.want && e.UpstreamStatus == 0
				}, "safe dial classification")
				requireCodexTraceEvent(t, traces, func(e CodexTraceEvent) bool {
					return e.Phase == "terminal" && e.Stage == "upstream_dial" && e.Reason == test.want && e.UpstreamStatus == 0
				}, "transport terminal classification")
				if event.Decision != "broker_failed" || event.Reason != test.want {
					t.Errorf("route summary = %+v; error=%v", event, serveErr)
				}
				raw, err := os.ReadFile(tracePath)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"private-secret", "private.invalid", "192.0.2.1"} {
					if strings.Contains(stderr, secret) || strings.Contains(string(raw), secret) {
						t.Fatalf("private transport detail leaked: %s\n%s", stderr, raw)
					}
				}
			})
		}
	}
}

func TestCodexWSTransportClassificationDoesNotReplaceProviderStatus(t *testing.T) {
	// A real HTTP rejection remains provider evidence, even when Dial reports a
	// generic bad-handshake error. Existing status/body projection must survive.
	ctx, diag := withRouteDiagnostics(context.Background())
	emitCodexWSHandshakePayload(ctx, &http.Response{StatusCode: 403}, []byte(`{"error":{"type":"permission_error","code":"access_denied"}}`), RouteChoice{}, CandidateAttempt{}, websocket.ErrBadHandshake)
	event := RouteEvent{}
	event.applyRouteDiagnostics(diag)
	if event.UpstreamStatus != 403 || event.UpstreamError == nil || event.UpstreamError.ErrorCode != "access_denied" {
		t.Fatalf("provider rejection lost: %+v", event)
	}
}

func TestCodexWSDialTransportDiagnosticsPreservesDomainFailure(t *testing.T) {
	cause := fmt.Errorf("private-secret: %w", codex.ErrCredentialAuthorityUnavailable)
	got := codexWSDialResultError(nil, cause, nil)
	if reason := codexTraceErrorReason(got); reason != "credential_authority_unavailable" {
		t.Fatalf("credential failure relabelled: %s", reason)
	}
	if !errors.Is(got, codex.ErrCredentialAuthorityUnavailable) {
		t.Fatal("credential identity lost")
	}
}

func TestCodexWSBrokerProductionExecutorTransportDiagnostics(t *testing.T) {
	for _, credentialFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("credential_failure=%t", credentialFailure), func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
			attempt := CandidateAttempt{AccountKey: "account-a", Candidate: codex.CandidateRef{AccountKey: "account-a", CandidateID: "candidate-a"}, Revision: "revision-1", Source: codex.SourceSystem, Identity: identity, Ordinal: 1}
			planner := &codexWSBrokerPlannerStub{runtime: newCodexLeaseRuntimeTest(t, coordinator), slots: []CodexLeaseAttemptSlotPlan{{AccountKey: "account-a", CandidateID: "candidate-a", Kind: CodexAttemptSlotDirect}}, frozenAccounts: []CodexFrozenDispatchAccount{{choice: RouteChoice{AccountKey: "account-a", EffectiveModel: "gpt-5.6-sol"}, attempts: []CandidateAttempt{attempt}}}}
			resolver := &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{"revision-1": testExactCredentialMaterial(identity, "private-secret")}}
			if credentialFailure {
				resolver.errors = map[codex.Revision]error{"revision-1": codex.ErrCredentialAuthorityUnavailable}
			}
			executor := NewCodexWebSocketAttemptExecutor(nil, resolver)
			dialCalls := 0
			executor.Dialer.Proxy = nil
			executor.Dialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
				dialCalls++
				return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}, Err: syscall.ECONNREFUSED}
			}
			downstream := &codexWSBrokerConnStub{reads: []codexWSBrokerRead{{messageType: websocket.TextMessage, payload: codexTerminatingWSFrame("turn-a", "")}, {err: io.EOF}}}
			broker, err := newCodexTerminatingWSBroker(codexTerminatingWSBrokerConfig{Plans: planner, Upstream: codexExplicitWSUpstreamDialer{executor: executor}, UpstreamURL: "wss://private.invalid/responses", DownstreamGeneration: 41})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "trace.jsonl")
			writer, err := OpenDiagnosticsWriter(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, diag := withRouteDiagnostics(context.Background())
			ctx = withCodexTrace(ctx, writer, nil, CodexTraceStart{Transport: "websocket"})
			serveErr := broker.Serve(ctx, downstream)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			traces := readCodexTraceEvents(t, path)
			if credentialFailure {
				if dialCalls != 0 || !errors.Is(serveErr, codex.ErrCredentialAuthorityUnavailable) {
					t.Fatalf("credential preflight lost: dialCalls=%d err=%v", dialCalls, serveErr)
				}
				requireCodexTraceEvent(t, traces, func(e CodexTraceEvent) bool {
					return e.Phase == "upstream_dial" && e.Outcome == "error" && e.Reason == "credential_authority_unavailable"
				}, "credential authority classification")
			} else {
				if dialCalls != 1 || !errors.Is(serveErr, syscall.ECONNREFUSED) {
					t.Fatalf("transport identity lost: dialCalls=%d err=%v", dialCalls, serveErr)
				}
				requireCodexTraceEvent(t, traces, func(e CodexTraceEvent) bool {
					return e.Phase == "terminal" && e.Stage == "upstream_dial" && e.Reason == "connection_refused"
				}, "production executor dial classification")
				event := RouteEvent{}
				event.applyRouteDiagnostics(diag)
				if event.Reason != "connection_refused" {
					t.Fatalf("route reason = %s", event.Reason)
				}
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private-secret", "private.invalid", "192.0.2.1"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("transport detail leaked: %s", raw)
				}
			}
		})
	}
}
