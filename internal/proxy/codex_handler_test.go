package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/keyring"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type fakeCodexSelector struct {
	account *codex.CodexAccount
	err     error
}

func (f *fakeCodexSelector) Select(_ context.Context, exclude ...string) (*codex.CodexAccount, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.account == nil {
		return nil, fmt.Errorf("no codex accounts available")
	}
	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}
	if codexAcctExcluded(f.account, excludeSet) {
		return nil, fmt.Errorf("no codex accounts available")
	}
	result := *f.account
	return &result, nil
}

func newCodexTransport(sel CodexSelector) *CodexTokenTransport {
	return &CodexTokenTransport{
		Selector: sel,
		Inner:    http.DefaultTransport,
	}
}

func TestHandleCodex_NonStreaming(t *testing.T) {
	// Fake OpenAI upstream that returns SSE (ChatGPT backend always streams).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth headers.
		if got := r.Header.Get("Authorization"); got != "Bearer codex-tok" {
			t.Errorf("upstream auth = %q, want Bearer codex-tok", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
			t.Errorf("upstream account-id = %q, want acct-123", got)
		}
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %q, want /responses", r.URL.Path)
		}

		// Verify request was translated and has stream=true.
		body, _ := io.ReadAll(r.Body)
		var req openaiResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("upstream body not valid JSON: %v", err)
		}
		if req.Model != "gpt-5.4-mini" {
			t.Errorf("upstream model = %q, want gpt-5.4-mini", req.Model)
		}
		if !req.Stream {
			t.Error("upstream request should have stream=true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			`data: {"type":"response.created","response":{"id":"resp_test"}}`,
			`data: {"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
			`data: {"type":"response.content_part.added","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_text.delta","delta":"Hi there!"}`,
			`data: {"type":"response.content_part.done","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`,
			`data: [DONE]`,
		}
		for _, ev := range events {
			fmt.Fprintln(w, ev)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL,
			LocalToken:     "test-tok",
		},
		CodexTransport: newCodexTransport(&fakeCodexSelector{
			account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct-123",
				Email:       "user@test.com",
			},
		}),
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4-mini","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))

	srv.handleCodex(w, req, []byte(body))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp anthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "message" {
		t.Errorf("type = %q, want message", resp.Type)
	}
	if len(resp.Content) == 0 {
		t.Fatal("no content blocks")
	}
	if resp.Content[0].Text != "Hi there!" {
		t.Errorf("text = %q, want 'Hi there!'", resp.Content[0].Text)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.CacheReadInputTokens == nil || *resp.Usage.CacheReadInputTokens != 2 {
		t.Fatalf("cache_read_input_tokens = %v, want 2", resp.Usage.CacheReadInputTokens)
	}
	if resp.Usage.ReasoningOutputTokens == nil || *resp.Usage.ReasoningOutputTokens != 1 {
		t.Fatalf("reasoning_output_tokens = %v, want 1", resp.Usage.ReasoningOutputTokens)
	}
	if resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 8 {
		t.Fatalf("total_tokens = %v, want 8", resp.Usage.TotalTokens)
	}
}

func TestHandleCodex_Streaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request has stream=true.
		body, _ := io.ReadAll(r.Body)
		var req openaiResponsesRequest
		json.Unmarshal(body, &req)
		if !req.Stream {
			t.Error("upstream request should have stream=true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			`data: {"type":"response.created","response":{"id":"resp_s1"}}`,
			`data: {"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
			`data: {"type":"response.content_part.added","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_text.delta","delta":"Hi"}`,
			`data: {"type":"response.content_part.done","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
			`data: [DONE]`,
		}
		for _, ev := range events {
			fmt.Fprintln(w, ev)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL,
			LocalToken:     "test-tok",
		},
		CodexTransport: newCodexTransport(&fakeCodexSelector{
			account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct-123",
			},
		}),
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4-mini","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))

	srv.handleCodex(w, req, []byte(body))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	result := w.Body.String()
	if !strings.Contains(result, "event: message_start") {
		t.Error("missing message_start")
	}
	if !strings.Contains(result, "event: content_block_delta") {
		t.Error("missing content_block_delta")
	}
	if !strings.Contains(result, "event: message_stop") {
		t.Error("missing message_stop")
	}
}

func TestHandleCodex_NoTransport(t *testing.T) {
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://api.openai.com",
			LocalToken:     "test-tok",
		},
		CodexTransport: nil,
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)

	srv.handleCodex(w, req, []byte(body))

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleCodex_SelectorError(t *testing.T) {
	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  "https://api.openai.com",
			LocalToken:     "test-tok",
		},
		CodexTransport: newCodexTransport(&fakeCodexSelector{err: fmt.Errorf("no accounts")}),
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)

	srv.handleCodex(w, req, []byte(body))

	if w.Code != 502 {
		t.Errorf("status = %d, want 502 (transport error surfaces as bad gateway)", w.Code)
	}
}

func TestHandleCodex_ModelValidationProbe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var req openaiResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if req.Model != "gpt-5.4" {
			t.Fatalf("upstream model = %q, want gpt-5.4", req.Model)
		}
		if !req.Stream {
			t.Fatal("upstream request should have stream=true")
		}
		if req.Instructions == "" {
			t.Fatal("upstream instructions should not be empty")
		}

		var items []openaiInputItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			t.Fatalf("unmarshal input items: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("input items = %d, want 1", len(items))
		}
		if items[0].Type != "message" || items[0].Role != "user" {
			t.Fatalf("input item = %+v, want message/user", items[0])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			`data: {"type":"response.created","response":{"id":"resp_validate"}}`,
			`data: {"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
			`data: {"type":"response.content_part.added","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_text.delta","delta":"ok"}`,
			`data: {"type":"response.content_part.done","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
			`data: [DONE]`,
		}
		for _, ev := range events {
			fmt.Fprintln(w, ev)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL,
			LocalToken:     "test-tok",
		},
		CodexTransport: newCodexTransport(&fakeCodexSelector{
			account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct",
			},
		}),
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4[1m]","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"Hi","cache_control":{"type":"ephemeral"}}]}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))

	srv.handleCodex(w, req, []byte(body))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp anthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}
	if resp.Type != "message" {
		t.Fatalf("response type = %q, want message", resp.Type)
	}
	if resp.Model != "gpt-5.4" {
		t.Fatalf("response model = %q, want gpt-5.4", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "ok" {
		t.Fatalf("unexpected content: %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 1 || resp.Usage.OutputTokens != 1 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
}

func TestHandleCodex_RejectsClaudeModel(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var req openaiResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if req.Model != "claude-haiku-4-5-20251001" {
			t.Fatalf("upstream model = %q, want claude-haiku-4-5-20251001", req.Model)
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"detail":"The 'claude-haiku-4-5-20251001' model is not supported when using Codex with a ChatGPT account."}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL,
			LocalToken:     "test-tok",
		},
		CodexTransport: newCodexTransport(&fakeCodexSelector{
			account: &codex.CodexAccount{
				AccessToken: "tok",
				AccountID:   "acct",
			},
		}),
	}

	w := httptest.NewRecorder()
	body := `{"model":"claude-haiku-4-5-20251001","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)

	srv.handleCodex(w, req, []byte(body))

	if upstreamCalled {
		t.Fatal("codex upstream should not be called for Claude models")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "is not a Codex model") {
		t.Fatalf("response = %s, want local model guard error", w.Body.String())
	}
}

func TestHandleCodex_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer upstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			CodexUpstream:  upstream.URL,
			LocalToken:     "test-tok",
		},
		CodexTransport: newCodexTransport(&fakeCodexSelector{
			account: &codex.CodexAccount{
				AccessToken: "tok",
				AccountID:   "acct",
			},
		}),
	}

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", nil)

	srv.handleCodex(w, req, []byte(body))

	if w.Code != 429 {
		t.Errorf("status = %d, want 429", w.Code)
	}
}

func TestServer_CodexRouting(t *testing.T) {
	// Claude upstream — should NOT be hit for Codex model.
	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("claude upstream should not be called for codex model")
	}))
	defer claudeUpstream.Close()

	// Codex upstream (always returns SSE).
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			`data: {"type":"response.created","response":{"id":"resp_route"}}`,
			`data: {"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
			`data: {"type":"response.content_part.added","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_text.delta","delta":"routed!"}`,
			`data: {"type":"response.content_part.done","part":{"type":"output_text"}}`,
			`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
			`data: [DONE]`,
		}
		for _, ev := range events {
			fmt.Fprintln(w, ev)
			flusher.Flush()
		}
	}))
	defer codexUpstream.Close()

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: claudeUpstream.URL,
			CodexUpstream:  codexUpstream.URL,
			LocalToken:     "tok",
		},
		CodexTransport: newCodexTransport(&fakeCodexSelector{
			account: &codex.CodexAccount{
				AccessToken: "codex-tok",
				AccountID:   "acct",
			},
		}),
	}

	handler := srv.proxyHandler(mustParseURL(claudeUpstream.URL))

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp anthropicResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Content) == 0 || resp.Content[0].Text != "routed!" {
		t.Errorf("unexpected response: %s", w.Body.String())
	}
}

func TestServer_CountTokensAlwaysRoutesToClaude(t *testing.T) {
	claudeHits := 0
	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claudeHits++
		if r.URL.Path != countTokensPath {
			t.Fatalf("upstream path = %q, want %q", r.URL.Path, countTokensPath)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":123}`))
	}))
	defer claudeUpstream.Close()

	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("codex upstream should not be called for count_tokens")
	}))
	defer codexUpstream.Close()

	future := int64(9999999999999)
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{{Email: "a@test.com", AccessToken: "claude-tok", ExpiresAt: future}}}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: claudeUpstream.URL,
			CodexUpstream:  codexUpstream.URL,
			LocalToken:     "tok",
		},
		Transport: &TokenTransport{Selector: sel, Inner: http.DefaultTransport},
		CodexTransport: newCodexTransport(&fakeCodexSelector{
			account: &codex.CodexAccount{AccessToken: "codex-tok", AccountID: "acct"},
		}),
	}

	handler := srv.proxyHandler(mustParseURL(claudeUpstream.URL))

	w := httptest.NewRecorder()
	body := `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", countTokensPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if claudeHits != 1 {
		t.Fatalf("claude hits = %d, want 1", claudeHits)
	}
	if strings.TrimSpace(w.Body.String()) != `{"input_tokens":123}` {
		t.Fatalf("response = %s, want count_tokens payload", w.Body.String())
	}
}

func TestServer_ClaudeModelStillWorks(t *testing.T) {
	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"claude works"}]}`))
	}))
	defer claudeUpstream.Close()

	future := int64(9999999999999)
	sel := &fakeSelector{accounts: []keyring.ClaudeOAuth{
		{Email: "a@test.com", AccessToken: "claude-tok", ExpiresAt: future},
	}}

	srv := &Server{
		Config: &Config{
			ClaudeUpstream: claudeUpstream.URL,
			CodexUpstream:  "https://api.openai.com",
			LocalToken:     "tok",
		},
		Transport: &TokenTransport{Selector: sel, Inner: http.DefaultTransport},
	}

	handler := srv.proxyHandler(mustParseURL(claudeUpstream.URL))

	w := httptest.NewRecorder()
	body := `{"model":"claude-3-opus-20240229","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "claude works") {
		t.Errorf("response = %s, want 'claude works'", w.Body.String())
	}
}

func TestServer_HealthWithCodex(t *testing.T) {
	srv := &Server{
		Config: &Config{
			LocalToken: "tok",
		},
		Discover: func() []keyring.ClaudeOAuth {
			return []keyring.ClaudeOAuth{{Email: "a@test.com"}}
		},
		CodexDiscover: func() []codex.CodexAccount {
			return []codex.CodexAccount{{Email: "b@test.com"}, {Email: "c@test.com"}}
		},
	}

	w := httptest.NewRecorder()
	srv.handleHealth(w, httptest.NewRequest("GET", "/health", nil))

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	accounts := resp["accounts"].(map[string]any)
	if accounts["claude"].(float64) != 1 {
		t.Errorf("claude = %v, want 1", accounts["claude"])
	}
	if accounts["codex"].(float64) != 2 {
		t.Errorf("codex = %v, want 2", accounts["codex"])
	}
}
