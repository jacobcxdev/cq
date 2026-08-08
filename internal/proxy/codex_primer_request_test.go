package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type blockingPrimerExecutor struct{}

func (*blockingPrimerExecutor) Do(ctx context.Context, _ RouteChoice, _ CandidateAttempt, _ *http.Request) (*http.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type primerCaptureExecutor struct {
	request *http.Request
	body    []byte
	status  int
	stream  string
	calls   int
}

func (e *primerCaptureExecutor) Do(_ context.Context, _ RouteChoice, _ CandidateAttempt, req *http.Request) (*http.Response, error) {
	e.calls++
	e.request = req.Clone(req.Context())
	if req.Body != nil {
		e.body, _ = io.ReadAll(req.Body)
	}
	status := e.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(e.stream))}, nil
}

func primerTestRouter(executor ExplicitAccountExecutor) *CodexRequestRouter {
	account := codex.AccountKey("account-1")
	inventory := codex.Inventory{Accounts: []codex.LogicalAccount{{
		Key: account, Routable: true,
		Candidates: []codex.CredentialCandidate{{
			Ref:      codex.CandidateRef{AccountKey: account, CandidateID: "candidate-1"},
			Revision: "revision-1", Source: codex.SourceExternal, AccessExpiresAt: time.Now().Add(time.Hour),
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
	if result.State != PrimerRequestAdmitted || executor.calls != 1 || executor.request.Method != http.MethodPost || executor.request.URL.Path != "/backend-api/codex/responses" {
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
	if result.State != PrimerRequestRejected || executor.calls != 1 {
		t.Fatalf("result = %+v calls=%d", result, executor.calls)
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
	if err != nil || result.State != PrimerRequestAmbiguous {
		t.Fatalf("timeout result = %+v, %v", result, err)
	}
}
