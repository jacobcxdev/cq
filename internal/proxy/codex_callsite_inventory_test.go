package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type inventoryAttemptExecutor struct {
	mu    sync.Mutex
	paths []string
}

func (e *inventoryAttemptExecutor) Do(_ context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request) (*http.Response, error) {
	if choice.AccountKey != "identity" || attempt.AccountKey != "identity" || attempt.Candidate.CandidateID != "candidate" {
		panic("call site bypassed explicit route choice")
	}
	e.mu.Lock()
	firstCall := len(e.paths) == 0
	e.paths = append(e.paths, req.URL.Path)
	e.mu.Unlock()
	body := `{"ok":true}`
	status := http.StatusOK
	header := http.Header{"Content-Type": []string{"application/json"}}
	switch req.URL.Path {
	case "/responses":
		requestBody, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(requestBody)))
		if firstCall {
			header.Set("Content-Type", "text/event-stream")
			body = strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_inventory"}}`,
				`data: {"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
				`data: {"type":"response.content_part.added","part":{"type":"output_text"}}`,
				`data: {"type":"response.output_text.delta","delta":"ok"}`,
				`data: {"type":"response.content_part.done","part":{"type":"output_text"}}`,
				`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
				`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
				`data: [DONE]`,
			}, "\n\n") + "\n"
		}
	case "/responses/input_tokens":
		body = `{"input_tokens":3}`
	case "/responses/compact":
		body = `{"object":"response.compact"}`
	case "/images/generations":
		body = `{"created":1,"data":[]}`
	case "/alpha/search":
		body = `{"data":[]}`
	case "/live":
		status = http.StatusCreated
		header.Set("Location", "/v1/live/rtc_inventory")
		body = "answer-sdp"
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestCodexHTTPCallSitesUseExplicitAccountExecutor(t *testing.T) {
	attempt := CandidateAttempt{
		AccountKey: "identity", Candidate: codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"},
		Revision: "revision", Source: codex.SourceSystem,
		Identity: codex.AccountIdentity{AccountID: "strong-account", UserID: "strong-user"}, Ordinal: 1,
	}
	scope := &queuedRequestScope{plans: []CodexRequestPlan{{
		Choice:   RouteChoice{AccountKey: "identity", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"},
		Attempts: []CandidateAttempt{attempt},
	}}}
	executor := &inventoryAttemptExecutor{}
	server := &Server{
		Config: &Config{
			ClaudeUpstream: "https://anthropic.test",
			CodexUpstream:  "https://upstream.test/backend-api/codex",
			LocalToken:     "local",
		},
		CodexRequests: &CodexRequestRouter{Scope: scope, Executor: executor},
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path        string
		contentType string
		body        string
		localAuth   bool
	}{
		{path: "/v1/messages", contentType: "application/json", body: `{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hello"}]}`, localAuth: true},
		{path: "/v1/messages/count_tokens", contentType: "application/json", body: `{"model":"gpt-5.4","messages":[]}`, localAuth: true},
		{path: "/responses", contentType: "application/json", body: `{"model":"gpt-5.4","input":"hello"}`},
		{path: "/responses/compact", contentType: "application/json", body: `{"model":"gpt-5.4","input":[]}`},
		{path: "/images/generations", contentType: "application/json", body: `{"model":"gpt-image-1","prompt":"test"}`},
		{path: "/alpha/search", contentType: "application/json", body: `{"query":"test"}`},
		{path: "/live", contentType: "application/sdp", body: "offer-sdp"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", test.contentType)
		if test.localAuth {
			request.Header.Set("Authorization", "Bearer local")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	want := "/backend-api/codex/responses,/backend-api/codex/v1/responses/input_tokens,/backend-api/codex/responses,/backend-api/codex/responses/compact,/backend-api/codex/images/generations,/backend-api/codex/alpha/search,/backend-api/codex/realtime/calls"
	if got := strings.Join(executor.paths, ","); got != want {
		t.Fatalf("explicit call sites = %s, want %s", got, want)
	}
}
