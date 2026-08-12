package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestServerNativeCodexUsesInjectedLegacyFallbackWithoutRoutingHandler(t *testing.T) {
	t.Parallel()

	requestBody := []byte(`{"model":"gpt-5","input":"private-request"}`)
	body := &codexLegacyNativeHTTPTestBody{Reader: bytes.NewReader(requestBody)}
	fallback := &codexLegacyNativeHTTPFallbackStub{
		status: http.StatusAccepted,
		header: http.Header{"X-Legacy": {"first", "second"}},
		body:   "legacy-response",
		model:  "gpt-5",
	}
	server := &Server{codexLegacyNativeHTTP: fallback}
	request := httptest.NewRequest(http.MethodPost, "/responses", body)
	response := httptest.NewRecorder()

	server.handleNativeCodex(response, request)

	if fallback.calls != 1 || !bytes.Equal(fallback.requestBody, requestBody) {
		t.Fatalf("fallback calls/body = %d/%q", fallback.calls, fallback.requestBody)
	}
	if body.closes != 1 {
		t.Fatalf("request body closes = %d, want 1", body.closes)
	}
	if response.Code != http.StatusAccepted || response.Body.String() != "legacy-response" {
		t.Fatalf("response status/body = %d/%q", response.Code, response.Body.String())
	}
	if got := response.Header().Values("X-Legacy"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("response headers = %#v", response.Header())
	}
}

func TestServerNativeCodexRetainedDeclineUsesLegacyFallback(t *testing.T) {
	t.Parallel()

	requestBody := frozenRequestBody("gpt-5", CodexRequestTurn, "private-retained-request")
	original := &codexRetainedHTTPTestBody{Reader: bytes.NewReader(requestBody)}
	planner := &codexRetainedHTTPPlannerStub{mutateProbe: true}
	retained := newCodexRetainedHTTPTestHandler(t, planner)
	upstreamBody := &codexLegacyNativeHTTPTestBody{Reader: strings.NewReader("exact-legacy-response")}
	transportCalls := 0
	server := &Server{
		Config:          &Config{CodexUpstream: "https://codex.example"},
		CodexNativeHTTP: retained,
		CodexTransport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			transportCalls++
			got, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(got, requestBody) {
				t.Fatalf("legacy upstream body changed: %q", got)
			}
			if got := request.Header.Get("X-Private-Request"); got != "preserved" {
				t.Fatalf("legacy upstream header = %q, want preserved", got)
			}
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header: http.Header{
					"Content-Type":      {"application/x-cq-sentinel"},
					"X-Legacy-Upstream": {"first", "second"},
				},
				Body: upstreamBody,
			}, nil
		}),
	}
	request := httptest.NewRequest(http.MethodPost, "/responses", original)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Private-Request", "preserved")
	response := httptest.NewRecorder()

	server.handleNativeCodex(response, request)

	if planner.probeCalls != 1 || planner.buildCalls != 0 || transportCalls != 1 {
		t.Fatalf("probe/build/legacy calls = %d/%d/%d", planner.probeCalls, planner.buildCalls, transportCalls)
	}
	if original.closes != 1 || upstreamBody.closes != 1 {
		t.Fatalf("request/upstream body closes = %d/%d, want 1/1", original.closes, upstreamBody.closes)
	}
	if response.Code != http.StatusTooManyRequests || response.Body.String() != "exact-legacy-response" {
		t.Fatalf("response status/body = %d/%q", response.Code, response.Body.String())
	}
	if got := response.Header().Values("X-Legacy-Upstream"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("response headers = %#v", response.Header())
	}
	if strings.Contains(response.Body.String(), "private-retained-request") {
		t.Fatalf("response disclosed private request: %q", response.Body.String())
	}
}

func TestServerNativeCodexEnforcementClaimsWithoutLegacyFallback(t *testing.T) {
	t.Parallel()

	planner := &codexNativeHTTPPlannerStub{err: errors.New("private-enforcement-plan-error")}
	native, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	fallback := &codexLegacyNativeHTTPFallbackStub{}
	server := &Server{
		CodexNativeHTTP:       native,
		codexLegacyNativeHTTP: fallback,
	}
	body := &codexLegacyNativeHTTPTestBody{Reader: bytes.NewReader(frozenRequestBody("gpt-5", CodexRequestTurn, "private-request"))}
	request := httptest.NewRequest(http.MethodPost, "/responses", body)
	response := httptest.NewRecorder()

	server.handleNativeCodex(response, request)

	if planner.calls != 1 || fallback.calls != 0 {
		t.Fatalf("enforcement/fallback calls = %d/%d, want 1/0", planner.calls, fallback.calls)
	}
	if body.closes != 1 {
		t.Fatalf("request body closes = %d, want 1", body.closes)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), "private-enforcement-plan-error") {
		t.Fatalf("response disclosed private error: %q", response.Body.String())
	}
}

func TestLegacyNativeHTTPRelaysBodyOverLegacyLimit(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("x", maxRequestBody+1) + `"}`)
	var upstreamBody []byte
	server := &Server{
		Config: &Config{CodexUpstream: "https://codex.example"},
		CodexRequests: testCodexRequestRouter(&fakeCodexSelector{account: &codex.CodexAccount{
			AccessToken: "codex-token",
			AccountID:   "account-a",
		}}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			var err error
			upstreamBody, err = io.ReadAll(request.Body)
			return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{"X-Upstream": {"large"}}, Body: io.NopCloser(strings.NewReader("accepted"))}, err
		})),
	}
	request := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body))
	response := httptest.NewRecorder()

	server.handleNativeCodex(response, request)

	if response.Code != http.StatusCreated || response.Body.String() != "accepted" || response.Header().Get("X-Upstream") != "large" {
		t.Fatalf("response = %d/%q/%q", response.Code, response.Body.String(), response.Header().Get("X-Upstream"))
	}
	if !bytes.Equal(upstreamBody, body) {
		t.Fatalf("upstream body = %d bytes, want %d", len(upstreamBody), len(body))
	}
}

func TestLegacyNativeHTTPRelaysUpstreamPayloadTooLarge(t *testing.T) {
	server := &Server{
		Config: &Config{CodexUpstream: "https://codex.example"},
		CodexRequests: testCodexRequestRouter(&fakeCodexSelector{account: &codex.CodexAccount{
			AccessToken: "codex-token",
			AccountID:   "account-a",
		}}, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusRequestEntityTooLarge,
				Header:     http.Header{"X-Upstream-Limit": {"backend"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"backend limit"}}`)),
			}, nil
		})),
	}
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	response := httptest.NewRecorder()

	server.handleNativeCodex(response, request)

	if response.Code != http.StatusRequestEntityTooLarge || response.Header().Get("X-Upstream-Limit") != "backend" || response.Body.String() != `{"error":{"message":"backend limit"}}` {
		t.Fatalf("response = %d/%q/%q", response.Code, response.Header().Get("X-Upstream-Limit"), response.Body.String())
	}
}

type codexLegacyNativeHTTPFallbackStub struct {
	calls       int
	status      int
	header      http.Header
	body        string
	model       string
	requestBody []byte
}

func (fallback *codexLegacyNativeHTTPFallbackStub) Handle(writer http.ResponseWriter, request *http.Request) string {
	fallback.calls++
	fallback.requestBody, _ = io.ReadAll(request.Body)
	_ = request.Body.Close()
	for key, values := range fallback.header {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	if fallback.status != 0 {
		writer.WriteHeader(fallback.status)
	}
	_, _ = io.WriteString(writer, fallback.body)
	return fallback.model
}

type codexLegacyNativeHTTPTestBody struct {
	io.Reader
	closes int
}

func (body *codexLegacyNativeHTTPTestBody) Close() error {
	body.closes++
	return nil
}
