package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestReadCodexNativeHTTPRequestAcceptsBodyOverLegacyLimit(t *testing.T) {
	body := codexProtocolRequestBodyAtSize(t, maxRequestBody+1)
	request, err := http.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, err := readCodexNativeHTTPRequest(request)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read = %d bytes, %v; want %d bytes", len(got), err, len(body))
	}
}

func TestServerNativeCodexDurablyAdmitsBeforeStreaming(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	attempt := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
	dispatch := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice:   choice,
			attempts: []CandidateAttempt{attempt},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 8)
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-a",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a"},
		events:       &events,
	}
	planner := &codexNativeHTTPPlannerStub{prepared: CodexPreparedHTTPRequest{
		Dispatch:  dispatch,
		Frozen:    frozen,
		Lifecycle: lifecycle,
	}}
	const responseBody = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-a\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-a\"}}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":       {"text/event-stream"},
			"X-Codex-Turn-State": {"private-turn-state"},
			"X-Multi":            {"first", "second"},
		},
		Body: &codexNativeHTTPEventBody{Reader: strings.NewReader(responseBody), events: &events},
	}
	dispatcher := &codexNativeHTTPDispatcher{
		response: response,
		wantURL:  "https://codex.example/responses?include=usage",
		wantBody: encoded,
		events:   &events,
	}
	handler, err := NewCodexNativeHTTPHandler(
		planner,
		&CodexHTTPRequestSession{Executor: dispatcher},
		"https://codex.example/",
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{CodexNativeHTTP: handler}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses?include=usage", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer private-downstream-token")
	writer := newCodexNativeHTTPOrderWriter(&events)

	server.handleNativeCodex(writer, request)

	if planner.calls != 1 || dispatcher.calls != 1 {
		t.Fatalf("planner/dispatcher calls = %d/%d, want 1/1", planner.calls, dispatcher.calls)
	}
	if !bytes.Equal(planner.encoded, encoded) {
		t.Fatalf("planner body changed: got %q want %q", planner.encoded, encoded)
	}
	if planner.headers.Get("Authorization") != "Bearer private-downstream-token" {
		t.Fatalf("planner headers changed: %#v", planner.headers)
	}
	if writer.status != http.StatusOK || writer.body.String() != responseBody {
		t.Fatalf("response status/body = %d/%q", writer.status, writer.body.String())
	}
	if got := writer.header.Values("X-Multi"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("response headers changed: %#v", writer.header)
	}
	wantEvents := []string{"mark", "send", "admit", "write-header", "close", "complete", "drain"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestServerNativeCodexCompactRelaysExactCompletion(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	attempt := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
	dispatch := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice:   choice,
			attempts: []CandidateAttempt{attempt},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 8)
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-a",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a"},
		events:       &events,
	}
	planner := &codexNativeHTTPPlannerStub{prepared: CodexPreparedHTTPRequest{
		Dispatch:  dispatch,
		Frozen:    frozen,
		Lifecycle: lifecycle,
	}}
	const responseBody = `{"id":"response-a","output":[],"encrypted_content":"private-state"}`
	response := &http.Response{
		StatusCode: http.StatusCreated,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"X-Multi":      {"first", "second"},
		},
		Body: &codexNativeHTTPEventBody{Reader: strings.NewReader(responseBody), events: &events},
	}
	dispatcher := &codexNativeHTTPDispatcher{
		response: response,
		wantURL:  "https://codex.example/responses/compact?include=usage",
		wantBody: encoded,
		events:   &events,
	}
	handler, err := NewCodexNativeHTTPHandler(
		planner,
		&CodexHTTPRequestSession{Executor: dispatcher},
		"https://codex.example/",
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{CodexNativeHTTP: handler}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/responses/compact?include=usage", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer private-downstream-token")
	writer := newCodexNativeHTTPOrderWriter(&events)

	server.handleNativeCodexCompact(writer, request, legacyCodexCompactResponsesPath)

	if planner.calls != 1 || dispatcher.calls != 1 {
		t.Fatalf("planner/dispatcher calls = %d/%d, want 1/1", planner.calls, dispatcher.calls)
	}
	if writer.status != http.StatusCreated || writer.body.String() != responseBody {
		t.Fatalf("response status/body = %d/%q", writer.status, writer.body.String())
	}
	if got := writer.header.Values("X-Multi"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("response headers changed: %#v", writer.header)
	}
	wantEvents := []string{"mark", "send", "admit", "write-header", "close", "complete", "drain"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestCodexNativeHTTPFinalRejectionPassesThroughExactly(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	attempt := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
	dispatch := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice:   choice,
			attempts: []CandidateAttempt{attempt},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 8)
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-a",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a"},
		events:       &events,
	}
	planner := &codexNativeHTTPPlannerStub{prepared: CodexPreparedHTTPRequest{
		Dispatch:  dispatch,
		Frozen:    frozen,
		Lifecycle: lifecycle,
	}}
	const responseBody = `{"error":{"eligible_promo":null,"message":"The usage limit has been reached","plan_type":"pro","resets_at":1786832019,"resets_in_seconds":539708,"type":"usage_limit_reached"}}`
	responseReader := &codexNativeHTTPEventBody{Reader: strings.NewReader(responseBody), events: &events}
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"X-Multi":      {"first", "second"},
		},
		Body: responseReader,
	}
	dispatcher := &codexNativeHTTPDispatcher{
		response: response,
		wantURL:  "https://codex.example/responses",
		wantBody: encoded,
		events:   &events,
	}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{Executor: dispatcher}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	writer := newCodexNativeHTTPOrderWriter(&events)

	handled, _ := handler.TryServe(writer, request, false)

	if !handled || planner.calls != 1 || dispatcher.calls != 1 {
		t.Fatalf("handled/planner/dispatcher = %t/%d/%d, want true/1/1", handled, planner.calls, dispatcher.calls)
	}
	if writer.status != http.StatusTooManyRequests || writer.body.String() != responseBody {
		t.Fatalf("response status/body = %d/%q", writer.status, writer.body.String())
	}
	if got := writer.header.Values("X-Multi"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("response headers changed: %#v", writer.header)
	}
	wantEvents := []string{"mark", "send", "close", "finish", "write-header"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if responseReader.closes != 1 {
		t.Fatalf("provider rejection body closes = %d, want 1", responseReader.closes)
	}
}

func TestCodexNativeHTTPPlanFailureIsClaimedAndPrivate(t *testing.T) {
	const private = "private-request-header-identity"
	planner := &codexNativeHTTPPlannerStub{err: errors.New(private)}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(`{"input":"`+private+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+private)
	events := make([]string, 0, 1)
	writer := newCodexNativeHTTPOrderWriter(&events)

	handled, _ := handler.TryServe(writer, request, false)

	if !handled || planner.calls != 1 {
		t.Fatalf("handled/planner calls = %t/%d, want true/1", handled, planner.calls)
	}
	if writer.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", writer.status, http.StatusServiceUnavailable)
	}
	if strings.Contains(writer.body.String(), private) {
		t.Fatalf("private request or dependency data reached response: %q", writer.body.String())
	}
	if strings.Join(events, ",") != "write-header" {
		t.Fatalf("response commit events = %v, want one write-header", events)
	}
}

func TestCodexNativeHTTPPlanFailureReportsSafeStage(t *testing.T) {
	const private = "private-route-snapshot-error"
	planner := &codexNativeHTTPPlannerStub{err: newCodexHTTPRequestPlanError(CodexHTTPRequestPlanDispatch, fmt.Errorf("%w: %s", ErrCodexLeaseAuthorityMismatch, private))}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	var reported CodexHTTPRequestPlanFailure
	handler.reportPlanFailure = func(failure CodexHTTPRequestPlanFailure) { reported = failure }
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(`{"input":"`+private+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, diagnostics := withRouteDiagnostics(request.Context())
	request = request.WithContext(ctx)
	writer := newCodexNativeHTTPOrderWriter(new([]string))

	handler.TryServe(writer, request, false)

	if reported.Stage != CodexHTTPRequestPlanDispatch || reported.Reason != CodexRequestFailureLeaseAuthorityMismatch {
		t.Fatalf("reported failure = %+v, want dispatch/lease authority mismatch", reported)
	}
	event := RouteEvent{}
	event.applyRouteDiagnostics(diagnostics)
	if event.Decision != "plan_failed" || event.Reason != string(CodexRequestFailureLeaseAuthorityMismatch) {
		t.Fatalf("diagnostics = decision %q reason %q, want plan_failed/lease_authority_mismatch", event.Decision, event.Reason)
	}
	if strings.Contains(writer.body.String(), private) {
		t.Fatalf("private failure reached response: %q", writer.body.String())
	}
}

func TestCodexNativeHTTPPlanFailureRedactsUnknownStage(t *testing.T) {
	planner := &codexNativeHTTPPlannerStub{err: &CodexHTTPRequestPlanError{Code: "private-stage-value"}}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	var reported CodexHTTPRequestPlanFailure
	handler.reportPlanFailure = func(failure CodexHTTPRequestPlanFailure) { reported = failure }
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}

	handler.TryServe(newCodexNativeHTTPOrderWriter(new([]string)), request, false)

	if reported.Stage != codexHTTPRequestPlanUnknown || reported.Reason != CodexRequestFailureUnknown {
		t.Fatalf("reported failure = %+v, want unknown/unknown", reported)
	}
}

func TestCodexNativeHTTPRoundTripFailureReportsSafeTrace(t *testing.T) {
	const private = "private-round-trip-detail"
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	choice := codexHTTPSessionChoice("account-a")
	attempt := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
	attempt.Identity = identity
	dispatch := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice:   choice,
			attempts: []CandidateAttempt{attempt},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 5)
	planner := &codexNativeHTTPPlannerStub{prepared: CodexPreparedHTTPRequest{
		Dispatch: dispatch,
		Frozen:   frozen,
		Lifecycle: &codexHTTPSessionLifecycle{
			account:      "account-a",
			slotAccounts: map[uint32]codex.AccountKey{1: "account-a"},
			events:       &events,
		},
	}}
	executor := &CodexAttemptExecutor{
		Secrets: &testExactSecretResolver{materials: map[codex.Revision]codex.CredentialMaterial{
			"revision-a": testExactCredentialMaterial(identity, "private-access-token"),
		}},
		Transport: &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(request.Context())
			if trace == nil || trace.GotConn == nil || trace.WroteRequest == nil || trace.GotFirstResponseByte == nil {
				t.Fatal("outbound request has no HTTP transport trace")
			}
			trace.GotConn(httptrace.GotConnInfo{Reused: true, WasIdle: true, IdleTime: 2500 * time.Millisecond})
			trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New(private)})
			trace.GotFirstResponseByte()
			return nil, fmt.Errorf("%w: %s", syscall.ECONNRESET, private)
		})},
	}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{Executor: executor}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	writer := newCodexNativeHTTPOrderWriter(&events)

	stderr := captureStderr(t, func() { handler.TryServe(writer, request, false) })

	if writer.status != http.StatusBadGateway || !strings.Contains(writer.body.String(), "Codex upstream request failed") {
		t.Fatalf("status/body = %d/%q, want generic 502", writer.status, writer.body.String())
	}
	wantTrace := "cq: Codex route trace transport=http event=session_failed stage=round_trip reason=connection_reset dispatched=true got_conn=true conn_reused=true conn_was_idle=true idle_ms=2500 wrote_request=false write_error=true got_first_response_byte=true\n"
	if stderr != wantTrace {
		t.Fatalf("trace = %q, want %q", stderr, wantTrace)
	}
	if strings.Contains(stderr, private) || strings.Contains(writer.body.String(), private) {
		t.Fatalf("private transport detail escaped: stderr=%q body=%q", stderr, writer.body.String())
	}
}

func TestServerNativeCodexReportsSafeSessionFailure(t *testing.T) {
	const private = "private-session-failure-detail"
	diagnosticsPath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, err := OpenDiagnosticsWriter(diagnosticsPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = diagnostics.Close() })
	handler, err := NewCodexNativeHTTPHandler(
		&codexNativeHTTPPlannerStub{},
		&codexNativeHTTPSessionStub{err: fmt.Errorf("%s: %w", private, ErrCodexLeaseWriterUnavailable)},
		"https://codex.example",
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{CodexNativeHTTP: handler, Diag: diagnostics}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	writer := newCodexNativeHTTPOrderWriter(new([]string))

	stderr := captureStderr(t, func() { server.handleNativeCodex(writer, request) })

	if writer.status != http.StatusBadGateway || !strings.Contains(writer.body.String(), "Codex upstream request failed") {
		t.Fatalf("status/body = %d/%q, want generic 502", writer.status, writer.body.String())
	}
	if stderr != "cq: Codex route trace transport=http event=session_failed stage=session reason=lease_writer_unavailable\n" {
		t.Fatalf("trace = %q, want safe session failure", stderr)
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	events := readDiagnosticsEvents(t, diagnosticsPath)
	if len(events) != 1 || events[0].Decision != "session_failed" || events[0].Reason != "lease_writer_unavailable" {
		t.Fatalf("diagnostics = %+v, want session_failed/lease_writer_unavailable", events)
	}
	assertDiagnosticsLogDoesNotContain(t, diagnosticsPath, private)
	if strings.Contains(stderr, private) {
		t.Fatalf("private session detail escaped: %q", stderr)
	}
}

func TestServerNativeCodexReportsUnavailableSessionResponse(t *testing.T) {
	diagnosticsPath := filepath.Join(t.TempDir(), "routes.jsonl")
	diagnostics, err := OpenDiagnosticsWriter(diagnosticsPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = diagnostics.Close() })
	handler, err := NewCodexNativeHTTPHandler(
		&codexNativeHTTPPlannerStub{},
		&codexNativeHTTPSessionStub{},
		"https://codex.example",
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{CodexNativeHTTP: handler, Diag: diagnostics}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	writer := newCodexNativeHTTPOrderWriter(new([]string))

	stderr := captureStderr(t, func() { server.handleNativeCodex(writer, request) })

	if writer.status != http.StatusBadGateway || !strings.Contains(writer.body.String(), "Codex upstream response unavailable") {
		t.Fatalf("status/body = %d/%q, want unavailable 502", writer.status, writer.body.String())
	}
	if stderr != "cq: Codex route trace transport=http event=session_failed stage=response_validate reason=response_unavailable\n" {
		t.Fatalf("trace = %q, want safe response validation failure", stderr)
	}
	if err := diagnostics.Close(); err != nil {
		t.Fatal(err)
	}
	events := readDiagnosticsEvents(t, diagnosticsPath)
	if len(events) != 1 || events[0].Decision != "session_failed" || events[0].Reason != "response_unavailable" {
		t.Fatalf("diagnostics = %+v, want session_failed/response_unavailable", events)
	}
}

func TestCodexNativeHTTPCancellationUnblocksRequestBody(t *testing.T) {
	body := newCodexNativeHTTPBlockingBody()
	planner := &codexNativeHTTPPlannerStub{}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		handler.TryServe(newCodexNativeHTTPOrderWriter(new([]string)), request, false)
		close(done)
	}()
	<-body.started
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled native request remained blocked reading its body")
	}
	if planner.calls != 0 {
		t.Fatalf("planner calls = %d, want 0", planner.calls)
	}
	if body.closeCalls() != 1 {
		t.Fatalf("body closes = %d, want 1", body.closeCalls())
	}
}

func TestCodexNativeHTTPCancellationRecoversPanickingBodyClose(t *testing.T) {
	const helper = "CQ_NATIVE_HTTP_PANICKING_CLOSE_HELPER"
	if os.Getenv(helper) == "1" {
		body := newCodexNativeHTTPPanickingCloseBody()
		handler, err := NewCodexNativeHTTPHandler(&codexNativeHTTPPlannerStub{}, &CodexHTTPRequestSession{}, "https://codex.example")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/responses", body)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			handler.TryServe(newCodexNativeHTTPOrderWriter(new([]string)), request, false)
			close(done)
		}()
		<-body.started
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("panicking body close stranded native request")
		}
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestCodexNativeHTTPCancellationRecoversPanickingBodyClose$")
	command.Env = append(os.Environ(), helper+"=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("panicking body close crashed helper: %v\n%s", err, output)
	}
}

func TestCodexNativeHTTPAcceptedReadFailureBecomesIndeterminate(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	attempt := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
	dispatch := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice:   choice,
			attempts: []CandidateAttempt{attempt},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 8)
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-a",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a"},
		events:       &events,
	}
	planner := &codexNativeHTTPPlannerStub{prepared: CodexPreparedHTTPRequest{
		Dispatch:  dispatch,
		Frozen:    frozen,
		Lifecycle: lifecycle,
	}}
	const responseBody = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-a\"}}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       &codexNativeHTTPFailingBody{data: []byte(responseBody), events: &events},
	}
	dispatcher := &codexNativeHTTPDispatcher{
		response: response,
		wantURL:  "https://codex.example/responses",
		wantBody: encoded,
		events:   &events,
	}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{Executor: dispatcher}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	writer := newCodexNativeHTTPOrderWriter(&events)

	handled, _ := handler.TryServe(writer, request, false)

	if !handled || writer.status != http.StatusOK || writer.body.String() != responseBody {
		t.Fatalf("handled/status/body = %t/%d/%q", handled, writer.status, writer.body.String())
	}
	wantEvents := []string{"mark", "send", "admit", "write-header", "close", "indeterminate", "drain"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if strings.Count(strings.Join(events, ","), "write-header") != 1 {
		t.Fatalf("response was committed more than once: %v", events)
	}
}

type codexNativeHTTPPlannerStub struct {
	prepared CodexPreparedHTTPRequest
	err      error
	calls    int
	encoded  []byte
	headers  http.Header
}

type codexNativeHTTPSessionStub struct {
	result CodexHTTPRequestSessionResult
	err    error
}

func (session *codexNativeHTTPSessionStub) Do(
	context.Context,
	*http.Request,
	CodexFrozenDispatchPlan,
	*CodexFrozenRequest,
	CodexHTTPRequestLifecycle,
) (CodexHTTPRequestSessionResult, error) {
	return session.result, session.err
}

func (planner *codexNativeHTTPPlannerStub) Build(_ context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	planner.calls++
	planner.encoded = bytes.Clone(input.Encoded)
	planner.headers = input.Headers.Clone()
	return planner.prepared, planner.err
}

type codexNativeHTTPDispatcher struct {
	response *http.Response
	wantURL  string
	wantBody []byte
	events   *[]string
	calls    int
}

func (dispatcher *codexNativeHTTPDispatcher) DispatchFrozen(
	_ context.Context,
	_ RouteChoice,
	attempt CandidateAttempt,
	request *http.Request,
	markDispatched func(CandidateAttempt) error,
) (*http.Response, CandidateAttempt, bool, error) {
	dispatcher.calls++
	if got := request.URL.String(); got != dispatcher.wantURL {
		return nil, attempt, false, &codexNativeHTTPTestError{message: "upstream URL = " + got}
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, attempt, false, err
	}
	gotBody, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(gotBody, dispatcher.wantBody) {
		return nil, attempt, false, &codexNativeHTTPTestError{message: "frozen request changed"}
	}
	if err := markDispatched(attempt); err != nil {
		return nil, attempt, false, err
	}
	*dispatcher.events = append(*dispatcher.events, "send")
	return dispatcher.response, attempt, true, nil
}

type codexNativeHTTPTestError struct{ message string }

func (err *codexNativeHTTPTestError) Error() string { return err.message }

type codexNativeHTTPEventBody struct {
	io.Reader
	events *[]string
	closes int
}

func (body *codexNativeHTTPEventBody) Close() error {
	body.closes++
	*body.events = append(*body.events, "close")
	return nil
}

type codexNativeHTTPFailingBody struct {
	data   []byte
	sent   bool
	events *[]string
}

func (body *codexNativeHTTPFailingBody) Read(target []byte) (int, error) {
	if !body.sent {
		body.sent = true
		return copy(target, body.data), nil
	}
	return 0, errors.New("private provider read failure")
}

func (body *codexNativeHTTPFailingBody) Close() error {
	*body.events = append(*body.events, "close")
	return nil
}

type codexNativeHTTPBlockingBody struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
}

type codexNativeHTTPPanickingCloseBody struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newCodexNativeHTTPPanickingCloseBody() *codexNativeHTTPPanickingCloseBody {
	return &codexNativeHTTPPanickingCloseBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (body *codexNativeHTTPPanickingCloseBody) Read([]byte) (int, error) {
	body.startOnce.Do(func() { close(body.started) })
	<-body.closed
	return 0, errors.New("request body closed")
}

func (body *codexNativeHTTPPanickingCloseBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	panic("private request body close panic")
}

func newCodexNativeHTTPBlockingBody() *codexNativeHTTPBlockingBody {
	return &codexNativeHTTPBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (body *codexNativeHTTPBlockingBody) Read([]byte) (int, error) {
	body.startOnce.Do(func() { close(body.started) })
	<-body.closed
	return 0, errors.New("request body closed")
}

func (body *codexNativeHTTPBlockingBody) Close() error {
	body.closeOnce.Do(func() {
		body.mu.Lock()
		body.closes++
		body.mu.Unlock()
		close(body.closed)
	})
	return nil
}

func (body *codexNativeHTTPBlockingBody) closeCalls() int {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closes
}

type codexNativeHTTPOrderWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
	events *[]string
}

func newCodexNativeHTTPOrderWriter(events *[]string) *codexNativeHTTPOrderWriter {
	return &codexNativeHTTPOrderWriter{header: make(http.Header), events: events}
}

func (writer *codexNativeHTTPOrderWriter) Header() http.Header { return writer.header }

func (writer *codexNativeHTTPOrderWriter) WriteHeader(status int) {
	writer.status = status
	*writer.events = append(*writer.events, "write-header")
}

func (writer *codexNativeHTTPOrderWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.body.Write(body)
}

func (writer *codexNativeHTTPOrderWriter) Flush() {}
