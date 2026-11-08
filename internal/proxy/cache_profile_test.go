package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

func TestCacheProfilerWebSocketWorksWithoutPayloadDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	headers := make(http.Header)
	headers.Set("Session_id", "private-websocket-session")
	for index, request := range []string{`{"type":"response.create","model":"gpt-6","input":[{"role":"user","content":"test"}]}`, `{"type":"response.create","model":"gpt-6","previous_response_id":"private-response","input":[]}`} {
		ctx := withCacheProfile(context.Background(), p, headers)
		ctx = withCodexTrace(ctx, nil, nil, CodexTraceStart{Transport: "websocket"})
		emitCodexRawTracePayload(ctx, PayloadEvent{Direction: "downstream_request", Complete: true}, []byte(request))
		response := `{"type":"response.completed","response":{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":90},"output_tokens":4}}}`
		if index == 1 {
			response = `{"type":"response.completed","response":{"usage":{"input_tokens":200,"input_tokens_details":{"cached_tokens":20},"output_tokens":4}}}`
		}
		emitCodexRawTracePayload(ctx, PayloadEvent{Direction: "upstream_response", AccountHint: "account:test", Complete: true}, []byte(response))
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("events=%d, want 2", len(events))
	}
	if !events[0].Usage.Known || events[0].Usage.CachedInputTokens != 90 {
		t.Fatalf("usage=%+v", events[0].Usage)
	}
	if events[1].Snapshot.Complete || events[1].Comparison != nil {
		t.Fatalf("delta-only continuation claimed complete: %+v", events[1])
	}
	if events[1].ModelHash == "" || events[0].ModelHash != events[1].ModelHash || events[1].PartitionHash == "" || events[0].PartitionHash != events[1].PartitionHash || events[1].Reason != "unknown_prefix" || !events[1].CachedTokensDecreased {
		t.Fatalf("opaque WebSocket identity/drop lost: %+v", events[1])
	}
}

func TestCacheProfilerHTTPPreservesWireAndRecordsUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	profiler, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer profiler.Close()
	requestBody := `{"model":"private-model-value","instructions":"private-system-value","input":[{"role":"user","content":"private-user-value"}]}`
	responseBody := `{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":70},"output_tokens":5}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != requestBody || r.Header.Get("Authorization") != "Bearer private-credential-value" {
			t.Error("request changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer server.Close()
	client := profiler.WrapClient(server.Client())
	for range 2 {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(requestBody))
		req.Header.Set("Session_id", "private-session-value")
		req.Header.Set("Authorization", "Bearer private-credential-value")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil || string(body) != responseBody {
			t.Fatalf("response changed: %q %v", body, err)
		}
		_ = response.Body.Close()
		_ = response.Body.Close()
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("events=%d, want 2", len(events))
	}
	if events[0].Reason != "cold_start" || events[1].Reason != "stable_prefix" {
		t.Fatalf("reasons=%q,%q", events[0].Reason, events[1].Reason)
	}
	if !events[1].Usage.Known || events[1].Usage.InputTokens != 100 || events[1].Usage.CachedInputTokens != 70 || events[1].Usage.OutputTokens != 5 {
		t.Fatalf("usage=%+v", events[1].Usage)
	}
	if events[1].Comparison == nil || !events[1].Comparison.PrefixStable {
		t.Fatalf("comparison=%+v", events[1].Comparison)
	}
	data, _ := os.ReadFile(path)
	for _, secret := range []string{"private-model-value", "private-system-value", "private-user-value", "private-session-value", "private-credential-value", server.URL} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("private value reached cache log")
		}
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
}

func TestCacheProfilerOpaqueContinuationRetainsIdentityAndObservedDrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			_, _ = io.WriteString(w, `{"usage":{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cached_tokens":90}}}`)
		} else {
			_, _ = io.WriteString(w, `{"usage":{"input_tokens":200,"output_tokens":1,"input_tokens_details":{"cached_tokens":20}}}`)
		}
	}))
	defer server.Close()
	client := p.WrapClient(server.Client())
	for _, body := range []string{`{"model":"synthetic-model","input":[{"role":"user","content":"first"}]}`, `{"model":"synthetic-model","previous_response_id":"opaque-prior","input":[{"role":"user","content":"next"}]}`} {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/responses", strings.NewReader(body))
		req.Header.Set("Session_id", "synthetic-session")
		req.Header.Set("Authorization", "Bearer synthetic-credential")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("events=%d", len(events))
	}
	if events[1].ModelHash == "" || events[0].ModelHash != events[1].ModelHash || events[1].Snapshot.Complete || events[1].Comparison != nil || !events[1].CachedTokensDecreased || events[1].Reason != "unknown_prefix" {
		t.Fatalf("opaque observation=%+v", events[1])
	}
}

func readCacheProfileTestEvents(t *testing.T, path string) []CacheProfileEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []CacheProfileEvent
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event CacheProfileEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestCacheProfilerMessagesStreamCombinesUsageWithoutBuffering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	first := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"cache_read_input_tokens\":4,\"cache_creation_input_tokens\":2,\"output_tokens\":0}}}\n\n"
	last := "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	firstRead := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-firstRead
		_, _ = io.WriteString(w, last)
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"test"}]}`))
	req.Header.Set("X-Session-Id", "test-session")
	response, err := p.WrapClient(server.Client()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(first))
	_, err = io.ReadFull(response.Body, buffer)
	close(firstRead)
	if err != nil || string(buffer) != first {
		t.Fatalf("first streamed bytes changed: %v", err)
	}
	rest, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(rest) != last {
		t.Fatalf("last streamed bytes changed: %v", err)
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	u := events[0].Usage
	if !u.Known || u.InputTokens != 14 || u.CachedInputTokens != 4 || u.CacheWriteInputTokens != 2 || u.OutputTokens != 5 {
		t.Fatalf("usage=%+v", u)
	}
}

type cacheProfileTestTransport func(*http.Request) (*http.Response, error)

func (f cacheProfileTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCacheProfilerSkipsNonInferenceAndPreservesTransportErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	wantErr := errors.New("private error must not reach log")
	transport := p.WrapTransport(cacheProfileTestTransport(func(*http.Request) (*http.Response, error) { return nil, wantErr }))
	for _, endpoint := range []string{"/oauth/token", "/api/monitor/usage", "/v1/messages/count_tokens"} {
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid"+endpoint, strings.NewReader(`{"model":"private-model","input":"private"}`))
		if _, err := transport.RoundTrip(req); err != wantErr {
			t.Fatalf("transport error changed: %v", err)
		}
	}
	if events := readCacheProfileTestEvents(t, path); len(events) != 0 {
		t.Fatalf("events=%d", len(events))
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{"model":"test","input":"test"}`))
	if _, err := transport.RoundTrip(req); err != wantErr {
		t.Fatalf("transport error changed: %v", err)
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 1 || events[0].Outcome != "transport_error" || events[0].Usage.Known {
		t.Fatalf("events=%+v", events)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(wantErr.Error())) {
		t.Fatal("private transport error reached log")
	}
}

func TestCacheProfilerFlagsDropsAndSeparatesPartitionChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cached := []int{90, 60, 40}
	index := 0
	transport := p.WrapTransport(cacheProfileTestTransport(func(r *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"usage": map[string]any{"input_tokens": 100, "output_tokens": 4, "input_tokens_details": map[string]int{"cached_tokens": cached[index]}}})
		index++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}))
	for _, key := range []string{"private-key-one", "private-key-one", "private-key-two"} {
		body, _ := json.Marshal(map[string]any{"model": "test", "input": "test", "prompt_cache_key": key})
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", bytes.NewReader(body))
		req.Header.Set("Session_id", "private-session")
		req.Header.Set("Authorization", "Bearer private-token")
		response, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 3 || !events[1].CachedTokensDecreased || events[2].CachedTokensDecreased || events[2].Reason != "cache_partition_changed" || events[2].Comparison != nil {
		t.Fatalf("events=%+v", events)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("private-key")) {
		t.Fatal("cache key reached log")
	}
}

func TestCacheProfilerRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cache.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if p, err := OpenCacheProfiler(path); err == nil {
		_ = p.Close()
		t.Fatal("symlink accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "untouched" {
		t.Fatal("symlink target changed")
	}
}

func TestCacheProfilerUsageUnknownAndInvalidAreNotZeroHits(t *testing.T) {
	for _, test := range []struct {
		name, body string
		known      bool
	}{
		{"missing cache", `{"usage":{"input_tokens":100,"output_tokens":4}}`, false},
		{"explicit zero", `{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":0},"output_tokens":4}}`, true},
		{"duplicate cache", `{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":10,"cached_tokens":90},"output_tokens":4}}`, false},
		{"impossible cache", `{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":101},"output_tokens":4}}`, false},
		{"truncated json", `{"usage":{"input_tokens":100`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := cacheProfileUsageParser{}
			p.feed([]byte(test.body))
			p.eof()
			if got := p.result(); got.Known != test.known {
				t.Fatalf("usage=%+v", got)
			}
		})
	}
}

func TestCacheProfilerPreservesBodyReadErrorAndEarlyClose(t *testing.T) {
	for _, closeEarly := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "cache.jsonl")
		p, err := OpenCacheProfiler(path)
		if err != nil {
			t.Fatal(err)
		}
		wantErr := errors.New("private reader error")
		body := &cacheProfileErrorBody{err: wantErr}
		attempt := &cacheProfileAttempt{profiler: p, event: CacheProfileEvent{SchemaVersion: 1, EventType: "cache_profile"}, started: time.Now()}
		wrapped := newCacheProfileBody(body, "application/json", attempt)
		if !closeEarly {
			buffer := make([]byte, 100)
			n, err := wrapped.Read(buffer)
			if n != 7 || err != wantErr || string(buffer[:n]) != "partial" {
				t.Fatalf("read=%d %q %v", n, buffer[:n], err)
			}
		}
		if err := wrapped.Close(); err != wantErr {
			t.Fatalf("close error=%v", err)
		}
		_ = p.Close()
		events := readCacheProfileTestEvents(t, path)
		if len(events) != 1 || events[0].Usage.Known || body.closes != 1 {
			t.Fatalf("events=%+v closes=%d", events, body.closes)
		}
	}
}

type cacheProfileErrorBody struct {
	err    error
	closes int
}

func (b *cacheProfileErrorBody) Read(p []byte) (int, error) { return copy(p, "partial"), b.err }
func (b *cacheProfileErrorBody) Close() error               { b.closes++; return b.err }

func TestCacheProfilerUsesNativeThreadRatherThanWindowIdentity(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Codex-Window-Id", "same-private-window")
	if session := cacheProfileRequestSession(headers, []byte(`{"model":"test","input":"test"}`)); session != "" {
		t.Fatalf("window used as chat identity: %q", session)
	}
	first := []byte(`{"model":"test","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"first","turn_id":"turn","request_kind":"turn"}},"input":"test"}`)
	second := bytes.Replace(first, []byte(`"thread_id":"first"`), []byte(`"thread_id":"second"`), 1)
	a, b := cacheProfileRequestSession(headers, first), cacheProfileRequestSession(headers, second)
	if a == "" || b == "" || a == b {
		t.Fatalf("native identities not distinct: %q %q", a, b)
	}
	headers.Set("Session_id", "header-session")
	if got := cacheProfileRequestSession(headers, first); got != a {
		t.Fatalf("native thread lost to broad header: %q", got)
	}
}

func TestCacheProfilerZCodeGatewayKeepsNativeChatCorrelation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var gatewaySessions []string
	upstream := &http.Client{Transport: cacheProfileTestTransport(func(r *http.Request) (*http.Response, error) {
		gatewaySessions = append(gatewaySessions, r.Header.Get("X-Session-Id"))
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("x-codex-turn-metadata")) {
			t.Fatal("test requires native metadata stripped by Gateway projection")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg-local","type":"message","role":"assistant","model":"glm-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"cache_read_input_tokens":4,"output_tokens":2}}`))}, nil
	})}
	server := &Server{ZAIGateway: &zcode.Gateway{HTTP: p.WrapClient(upstream), Version: zcode.ClientVersion}}
	accounts := []zaiprov.Account{{ID: "private-account", Transport: "zcode-start", OAuth: &zcode.LoginSession{ZCodeJWT: "private-start-token"}}}
	native := []byte(`{"model":"glm-5.3","instructions":"stable instructions","client_metadata":{"x-codex-turn-metadata":{"session_id":"native-session","thread_id":"native-thread","turn_id":"native-turn","request_kind":"turn"}},"input":"test"}`)
	headers := make(http.Header)
	headers.Set("X-Codex-Window-Id", "private-window")
	for range 2 {
		ctx, _ := withRouteDiagnostics(context.Background())
		response, err := server.doZAIRequest(ctx, "https://example.invalid/responses", headers, native, accounts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 2 || events[0].SessionHash == "" || events[0].SessionHash != events[1].SessionHash || events[1].Reason != "stable_prefix" || events[1].Route != "zcode-start" {
		t.Fatalf("events=%+v", events)
	}
	if len(gatewaySessions) != 2 || gatewaySessions[0] == gatewaySessions[1] {
		t.Fatalf("provider session behaviour changed: %q", gatewaySessions)
	}
}

func TestCacheProfilerDistinguishesCredentialsFromTrustedAccountChanges(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "cache.jsonl")
		p, err := OpenCacheProfiler(path)
		if err != nil {
			t.Fatal(err)
		}
		transport := p.WrapTransport(cacheProfileTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":80},"output_tokens":4}}`))}, nil
		}))
		for _, identity := range []string{"first", "second"} {
			ctx, _ := withRouteDiagnostics(context.Background())
			if trusted {
				noteRouteAccount(ctx, identity, false)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{"model":"test","input":"test"}`))
			req.Header.Set("Session_id", "session")
			req.Header.Set("Authorization", "Bearer "+identity)
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		_ = p.Close()
		events := readCacheProfileTestEvents(t, path)
		want := "credential_identity_changed"
		if trusted {
			want = "account_changed"
		}
		if len(events) != 2 || events[1].Reason != want || events[1].Comparison != nil {
			t.Fatalf("trusted=%v events=%+v", trusted, events)
		}
	}
}

func TestCacheProfilerRecordsNilResponseWithoutChangingTransportResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	transport := p.WrapTransport(cacheProfileTestTransport(func(*http.Request) (*http.Response, error) { return nil, nil }))
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{"model":"test","input":"test"}`))
	response, err := transport.RoundTrip(req)
	if response != nil || err != nil {
		t.Fatalf("result changed: %v %v", response, err)
	}
	events := readCacheProfileTestEvents(t, path)
	if len(events) != 1 || events[0].Outcome != "empty_response" || events[0].Usage.Known {
		t.Fatalf("events=%+v", events)
	}
}

func TestCacheProfilerClaudeHandlerProfilesBufferedRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	p, err := OpenCacheProfiler(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	requestBody := `{"model":"claude-sonnet-4-5","max_tokens":100,"system":"stable private policy","messages":[{"role":"user","content":"private request"}]}`
	responseBody := `{"id":"msg-local","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"cache_read_input_tokens":40,"cache_creation_input_tokens":0,"output_tokens":4}}`
	selector := &fakeSelector{accounts: []keyring.ClaudeOAuth{{Email: "private@example.invalid", AccessToken: "private-provider-token"}}}
	innerCalls := 0
	inner := p.WrapTransport(cacheProfileTestTransport(func(r *http.Request) (*http.Response, error) {
		innerCalls++
		if r.GetBody == nil {
			t.Error("handler buffering did not provide GetBody")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer private-provider-token" {
			t.Errorf("upstream credential=%q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != requestBody {
			t.Errorf("upstream request changed: %q %v", body, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody)), Request: r}, nil
	}))
	srv := &Server{Config: &Config{LocalToken: "private-local-token"}, Selector: selector, Transport: &TokenTransport{Selector: selector, Inner: inner}, CacheProfiler: p}
	handler := srv.proxyHandler(mustParseURL("https://api.anthropic.com"))
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
		req.GetBody = nil // Real accepted HTTP requests start without replay support.
		req.Header.Set("Authorization", "Bearer private-local-token")
		req.Header.Set("X-Claude-Code-Session-Id", "private-claude-session")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK || response.Body.String() != responseBody {
			t.Fatalf("response changed: %d %q", response.Code, response.Body.String())
		}
	}
	events := readCacheProfileTestEvents(t, path)
	if innerCalls != 2 || len(events) != 2 {
		t.Fatalf("calls/events=%d/%d", innerCalls, len(events))
	}
	if !events[0].Snapshot.Complete || events[1].Reason != "stable_prefix" || events[1].SessionHash == "" || events[1].AccountSource != "route_account" {
		t.Fatalf("events=%+v", events)
	}
	if !events[1].Usage.Known || events[1].Usage.InputTokens != 48 || events[1].Usage.CachedInputTokens != 40 {
		t.Fatalf("usage=%+v", events[1].Usage)
	}
	data, _ := os.ReadFile(path)
	for _, value := range []string{"private-provider-token", "private-local-token", "private@example.invalid", "private-claude-session", "stable private policy", "private request"} {
		if bytes.Contains(data, []byte(value)) {
			t.Fatal("private value reached profiling log")
		}
	}
}
