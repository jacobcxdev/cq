package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type blockingPrimerExecutor struct{}

func (*blockingPrimerExecutor) Do(ctx context.Context, _ RouteChoice, _ CandidateAttempt, _ *http.Request) (*http.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type primerCaptureExecutor struct {
	request      *http.Request
	choice       RouteChoice
	body         []byte
	status       int
	stream       string
	responseBody io.ReadCloser
	err          error
	nilResponse  bool
	nilBody      bool
	calls        int
}

func (e *primerCaptureExecutor) Do(_ context.Context, choice RouteChoice, _ CandidateAttempt, req *http.Request) (*http.Response, error) {
	e.calls++
	e.request = req.Clone(req.Context())
	e.choice = choice
	if req.Body != nil {
		e.body, _ = io.ReadAll(req.Body)
	}
	if e.err != nil {
		return nil, e.err
	}
	if e.nilResponse {
		return nil, nil
	}
	status := e.status
	if status == 0 {
		status = http.StatusOK
	}
	var responseBody io.ReadCloser
	if !e.nilBody {
		responseBody = e.responseBody
		if responseBody == nil {
			responseBody = io.NopCloser(strings.NewReader(e.stream))
		}
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: responseBody}, nil
}

type primerReadErrorBody struct {
	closed bool
}

func (*primerReadErrorBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func (b *primerReadErrorBody) Close() error {
	b.closed = true
	return nil
}

type primerTrackingBody struct {
	io.Reader
	closed bool
}

func (b *primerTrackingBody) Close() error {
	b.closed = true
	return nil
}

func primerTestRouter(executor ExplicitAccountExecutor) *CodexRequestRouter {
	account := codex.AccountKey("account-1")
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{{
		Key: account, Identity: codex.AccountIdentity{AccountID: "strong-account-1", UserID: "strong-user-1"}, Routable: true,
		Candidates: []codex.CredentialCandidate{{
			Ref:      codex.CandidateRef{AccountKey: account, CandidateID: "candidate-1"},
			Revision: "revision-1", Source: codex.SourceExternal, AccessExpiresAt: time.Now().Add(time.Hour), Routable: true,
		}},
	}}}
	return &CodexRequestRouter{
		Scope:    &CodexRequestScope{Inventory: staticCredentialInventory{inventory: inventory}},
		Executor: executor,
	}
}

func TestCodexPrimerRequestUsesMinimalAccountPinnedPayload(t *testing.T) {
	executor := &primerCaptureExecutor{stream: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"synthetic\"}}\n\ndata: {\"type\":\"response.completed\"}\n\n"}
	requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/backend-api/codex/responses"}

	result, err := requester.Send(context.Background(), "account-1", "gpt-5.3-codex-spark")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != PrimerRequestAdmitted || result.Code != PrimerRequestCodeLifecycleObserved || executor.calls != 1 || executor.choice.AccountKey != "account-1" || executor.request.Method != http.MethodPost || executor.request.URL.Path != "/backend-api/codex/responses" {
		t.Fatalf("result/request = %+v/%+v calls=%d", result, executor.request, executor.calls)
	}
	var body map[string]any
	if err := json.Unmarshal(executor.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "gpt-5.3-codex-spark" || body["store"] != false || body["stream"] != true {
		t.Fatalf("request body = %s", executor.body)
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 0 || body["previous_response_id"] != nil || body["session_id"] != nil || body["thread_id"] != nil || body["turn_id"] != nil {
		t.Fatalf("unsafe synthetic request body = %s", executor.body)
	}
	metadata, ok := body["client_metadata"].(map[string]any)
	if !ok || metadata["cq.synthetic"] != "window-primer-v1" {
		t.Fatalf("metadata = %#v", body["client_metadata"])
	}
}

func TestCodexPrimerRequestClassifiesPreAdmissionRejection(t *testing.T) {
	executor := &primerCaptureExecutor{status: http.StatusBadRequest, stream: `{"error":"bad request"}`}
	requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}

	result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != PrimerRequestRejected || result.Code != PrimerRequestCodeHTTPPreAdmission || executor.calls != 1 {
		t.Fatalf("result = %+v calls=%d", result, executor.calls)
	}
}

func TestCodexPrimerRequestTreatsServerFailureAsAmbiguous(t *testing.T) {
	executor := &primerCaptureExecutor{status: http.StatusInternalServerError, stream: `{"error":"unavailable"}`}
	requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}

	result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != PrimerRequestAmbiguous {
		t.Fatalf("result = %+v", result)
	}
}

func TestCodexPrimerRequestHTTPClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantState PrimerRequestState
		wantCode  PrimerRequestResultCode
	}{
		{name: "typed 401", status: http.StatusUnauthorized, body: `{"error":"unauthorised"}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeAuthRejected},
		{name: "authentication 403", status: http.StatusForbidden, body: `{"error":{"type":"authentication_error"}}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeAuthRejected},
		{name: "policy 403", status: http.StatusForbidden, body: `{"error":"forbidden"}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "typed hard limit", status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeHardLimit},
		{name: "400", status: http.StatusBadRequest, body: `{}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeHTTPPreAdmission},
		{name: "404", status: http.StatusNotFound, body: `{}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeHTTPPreAdmission},
		{name: "405", status: http.StatusMethodNotAllowed, body: `{}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeHTTPPreAdmission},
		{name: "413", status: http.StatusRequestEntityTooLarge, body: `{}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeHTTPPreAdmission},
		{name: "415", status: http.StatusUnsupportedMediaType, body: `{}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeHTTPPreAdmission},
		{name: "422", status: http.StatusUnprocessableEntity, body: `{}`, wantState: PrimerRequestRejected, wantCode: PrimerRequestCodeHTTPPreAdmission},
		{name: "redirect", status: http.StatusFound, body: `{}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "408", status: http.StatusRequestTimeout, body: `{}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "409", status: http.StatusConflict, body: `{}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "425", status: http.StatusTooEarly, body: `{}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "soft 429", status: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit_exceeded"}}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "500", status: http.StatusInternalServerError, body: `{}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "502", status: http.StatusBadGateway, body: `{}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
		{name: "503", status: http.StatusServiceUnavailable, body: `{}`, wantState: PrimerRequestAmbiguous, wantCode: PrimerRequestCodeHTTPAmbiguous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &primerCaptureExecutor{status: test.status, stream: test.body}
			requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}
			result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
			if err != nil {
				t.Fatal(err)
			}
			if result.State != test.wantState || result.Code != test.wantCode || result.HTTPStatus != test.status {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestCodexPrimerRequestLifecycleEvidenceIsNoReplay(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{name: "created", stream: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"synthetic\"}}\n\n"},
		{name: "completed", stream: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic\"}}\n\n"},
		{name: "failed", stream: "data: {\"type\":\"response.failed\"}\n\n"},
		{name: "incomplete", stream: "data: {\"type\":\"response.incomplete\"}\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &primerCaptureExecutor{stream: test.stream}
			requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}
			result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
			if err != nil {
				t.Fatal(err)
			}
			if result.State != PrimerRequestAdmitted || result.Code != PrimerRequestCodeLifecycleObserved {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestCodexPrimerRequestLatchesLifecycleBeforeLaterStreamFailure(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{
			name:   "malformed event after created",
			stream: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"synthetic\"}}\n\ndata: {not-json}\n\n",
		},
		{
			name:   "truncated event after completed",
			stream: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic\"}}\n\ndata: {\"type\":\"response.output_text.delta\"}",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &primerCaptureExecutor{stream: test.stream}
			requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}
			result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
			if err != nil {
				t.Fatal(err)
			}
			if result.State != PrimerRequestAdmitted || result.Code != PrimerRequestCodeLifecycleObserved {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestCodexPrimerRequestAmbiguousEvidence(t *testing.T) {
	tests := []struct {
		name     string
		executor *primerCaptureExecutor
		wantCode PrimerRequestResultCode
	}{
		{name: "transport error", executor: &primerCaptureExecutor{err: errors.New("transport failed")}, wantCode: PrimerRequestCodeTransportError},
		{name: "nil response", executor: &primerCaptureExecutor{nilResponse: true}, wantCode: PrimerRequestCodeTransportError},
		{name: "nil body", executor: &primerCaptureExecutor{nilBody: true}, wantCode: PrimerRequestCodeTransportError},
		{name: "malformed SSE", executor: &primerCaptureExecutor{stream: "data: {not-json}\n\n"}, wantCode: PrimerRequestCodeSSEMalformed},
		{name: "truncated SSE", executor: &primerCaptureExecutor{stream: "data: {\"type\":\"response.output_text.delta\"}"}, wantCode: PrimerRequestCodeSSETruncated},
		{name: "unknown 2xx", executor: &primerCaptureExecutor{stream: "data: {\"type\":\"future.event\"}\n\n"}, wantCode: PrimerRequestCodeLifecycleMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requester := &CodexPrimerRequester{Router: primerTestRouter(test.executor), ResponsesURL: "https://chatgpt.example/responses"}
			result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
			if err != nil {
				t.Fatal(err)
			}
			if result.State != PrimerRequestAmbiguous || result.Code != test.wantCode {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestCodexPrimerRequestUsesBoundedBodyReader(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   io.ReadCloser
	}{
		{name: "read error", body: &primerReadErrorBody{}},
		{name: "allowlisted response read error", status: http.StatusBadRequest, body: &primerReadErrorBody{}},
		{name: "over limit", body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), httputil.MaxResponseBody+1)))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &primerCaptureExecutor{status: test.status, responseBody: test.body}
			requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}
			result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
			if err != nil {
				t.Fatal(err)
			}
			if result.State != PrimerRequestAmbiguous || result.Code != PrimerRequestCodeResponseReadError {
				t.Fatalf("result = %+v", result)
			}
			if body, ok := test.body.(*primerReadErrorBody); ok && !body.closed {
				t.Fatal("read-error body not closed")
			}
		})
	}
}

func TestCodexPrimerRequestClosesResponseBodies(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "admitted", status: http.StatusOK, body: "data: {\"type\":\"response.created\",\"response\":{}}\n\n"},
		{name: "auth rejected", status: http.StatusUnauthorized, body: `{}`},
		{name: "hard limit", status: http.StatusTooManyRequests, body: `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`},
		{name: "rejected", status: http.StatusBadRequest, body: `{}`},
		{name: "ambiguous", status: http.StatusInternalServerError, body: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &primerTrackingBody{Reader: strings.NewReader(test.body)}
			executor := &primerCaptureExecutor{status: test.status, responseBody: body}
			requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}
			if _, err := requester.Send(context.Background(), "account-1", "gpt-5.4"); err != nil {
				t.Fatal(err)
			}
			if !body.closed {
				t.Fatal("response body not closed")
			}
		})
	}
}

func TestCodexPrimerRequestTreatsSuccessWithoutAdmissionAsAmbiguous(t *testing.T) {
	executor := &primerCaptureExecutor{stream: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"}
	requester := &CodexPrimerRequester{Router: primerTestRouter(executor), ResponsesURL: "https://chatgpt.example/responses"}

	result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != PrimerRequestAmbiguous {
		t.Fatalf("result = %+v", result)
	}
}

func TestCodexPrimerRequestBoundsTotalTime(t *testing.T) {
	requester := &CodexPrimerRequester{
		Router: primerTestRouter(&blockingPrimerExecutor{}), ResponsesURL: "https://chatgpt.example/responses",
		Timeout: time.Millisecond,
	}
	result, err := requester.Send(context.Background(), "account-1", "gpt-5.4")
	if err != nil || result.State != PrimerRequestAmbiguous || result.Code != PrimerRequestCodeTimeout {
		t.Fatalf("timeout result = %+v, %v", result, err)
	}
}
