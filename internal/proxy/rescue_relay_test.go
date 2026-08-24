package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type rescueDoerFunc func(*http.Request) (*http.Response, error)

func (do rescueDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return do(request)
}

func testRescueRequest(t *testing.T, method, target string, body []byte) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Host = "127.0.0.1:29280"
	request.Header = http.Header{
		"Authorization":       {"Bearer opaque-token"},
		"User-Agent":          {"codex/0.147.0 (darwin 25.0; arm64) Terminal"},
		"Originator":          {"codex"},
		"Version":             {"0.147.0"},
		"Content-Type":        {"application/json"},
		"Accept":              {"text/event-stream"},
		"Session-Id":          {"session"},
		"Thread-Id":           {"thread"},
		"X-Client-Request-Id": {"request"},
		"X-Codex-Window-Id":   {"window"},
	}
	return request
}

func TestRescueRelayForwardsOpaqueBearerOnce(t *testing.T) {
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if got := request.URL.String(); got != "https://chatgpt.com/backend-api/codex/responses" {
				t.Fatalf("upstream URL = %q", got)
			}
			if got := request.Header.Get("Authorization"); got != "Bearer opaque-token" {
				t.Fatalf("authorization = %q", got)
			}
			if request.GetBody != nil {
				t.Fatal("rescue request unexpectedly replayable")
			}
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := string(body); got != `{"model":"gpt-5"}` {
				t.Fatalf("body = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"request-id"}, "Set-Cookie": {"secret=1"}},
				Body:       io.NopCloser(strings.NewReader("data: done\n\n")),
			}, nil
		}),
		Origin:                 origin,
		LoopbackHost:           "127.0.0.1:29280",
		ForwardingAcknowledged: true,
		Budget:                 NewRescueBudget(time.Now, [sha256.Size]byte{1}),
	}
	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "data: done\n\n" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if got := recorder.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("relayed Set-Cookie = %q", got)
	}
}

func TestRescueRelayForwardsCurrentCodexHeadersWithoutCQAuthentication(t *testing.T) {
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if got := request.Header.Get("Authorization"); got != "Bearer current-upstream-token" {
				t.Fatalf("authorization = %q", got)
			}
			if got := request.Header.Get("X-Codex-New-Transport"); got != "current" {
				t.Fatalf("new transport header = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":               {"text/event-stream"},
					"X-Codex-Secondary-Reset-At": {""},
				},
				Body: io.NopCloser(strings.NewReader("data: done\n\n")),
			}, nil
		}),
		Origin:                 origin,
		LoopbackHost:           "127.0.0.1:29280",
		ForwardingAcknowledged: true,
		Budget:                 NewRescueBudget(time.Now, [sha256.Size]byte{1}),
	}
	request := testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`))
	request.Header.Set("Authorization", "Bearer current-upstream-token")
	request.Header.Set("User-Agent", "codex/0.148.0 (darwin 25.0; arm64) Terminal")
	request.Header.Set("Version", "0.148.0")
	request.Header.Set("X-Codex-New-Transport", "current")
	recorder := httptest.NewRecorder()

	relay.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("response/calls = %d/%d, want 200/1; body %q", recorder.Code, calls.Load(), recorder.Body.String())
	}
	if got := recorder.Header().Values("X-Codex-Secondary-Reset-At"); len(got) != 1 || got[0] != "" {
		t.Fatalf("secondary reset header = %q, want one empty value", got)
	}
}

func TestRescueRelayLetsUpstreamDecideMissingAuthentication(t *testing.T) {
	origin, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"upstream":"unauthorized"}`)),
			}, nil
		}),
		Origin: origin, LoopbackHost: "127.0.0.1:29280", ForwardingAcknowledged: true,
		Budget: NewRescueBudget(time.Now, [sha256.Size]byte{1}),
	}
	request := testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`))
	request.Header.Del("Authorization")
	recorder := httptest.NewRecorder()

	relay.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized || recorder.Body.String() != `{"upstream":"unauthorized"}` || calls.Load() != 1 {
		t.Fatalf("response/calls = %d/%q/%d", recorder.Code, recorder.Body.String(), calls.Load())
	}
}

func TestRescueRelayRefusesRedirectWithoutExposingLocation(t *testing.T) {
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"https://example.invalid/escape"}},
				Body:       io.NopCloser(strings.NewReader("redirect")),
			}, nil
		}),
		Origin:                 origin,
		LoopbackHost:           "127.0.0.1:29280",
		ForwardingAcknowledged: true,
		Budget:                 NewRescueBudget(time.Now, [sha256.Size]byte{1}),
	}
	request := testRescueRequest(t, http.MethodPost, "/responses", []byte(`{}`))
	response := httptest.NewRecorder()
	relay.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Location"); got != "" {
		t.Fatalf("location = %q", got)
	}
	if got := response.Body.String(); !strings.Contains(got, `"code":"rescue_redirect_refused"`) {
		t.Fatalf("body = %q", got)
	}
}

func TestRescueRelayRejectsLocalTokenBeforeBodyAndUpstream(t *testing.T) {
	origin, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("must not run")
		}),
		Origin:                 origin,
		LoopbackHost:           "127.0.0.1:29280",
		ForwardingAcknowledged: true,
		DenyBearer:             func(bearer []byte) bool { return string(bearer) == "local-token" },
		Budget:                 NewRescueBudget(time.Now, [sha256.Size]byte{2}),
	}
	request := testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`))
	request.Header.Set("Authorization", "Bearer local-token")
	request.Body = panicOnReadCloser{}
	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "rescue_local_token_refused") {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestRescueRelayRouteAndQueryCatalogue(t *testing.T) {
	origin, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}),
		Origin: origin, LoopbackHost: "127.0.0.1:29280", ForwardingAcknowledged: true,
		Budget: NewRescueBudget(time.Now, [sha256.Size]byte{3}),
	}

	cases := []struct {
		name   string
		method string
		target string
		status int
		code   string
	}{
		{name: "models exact", method: http.MethodGet, target: "/models?client_version=0.147.0", status: http.StatusOK},
		{name: "models missing query", method: http.MethodGet, target: "/models", status: http.StatusOK},
		{name: "models encoded query", method: http.MethodGet, target: "/models?client_version=0%2E147%2E0", status: http.StatusOK},
		{name: "v1 models unsupported", method: http.MethodGet, target: "/v1/models?client_version=0.147.0", status: http.StatusNotFound, code: "rescue_route_unsupported"},
		{name: "compact wrong method", method: http.MethodGet, target: "/responses/compact", status: http.StatusMethodNotAllowed, code: "rescue_method_unsupported"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := testRescueRequest(t, test.method, test.target, nil)
			if test.method == http.MethodGet {
				request.Header.Del("Content-Type")
				request.Header.Del("Accept")
				request.Header.Del("Session-Id")
				request.Header.Del("Thread-Id")
				request.Header.Del("X-Client-Request-Id")
				request.Header.Del("X-Codex-Window-Id")
			}
			recorder := httptest.NewRecorder()
			relay.ServeHTTP(recorder, request)
			if recorder.Code != test.status || (test.code != "" && !strings.Contains(recorder.Body.String(), test.code)) {
				t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
			}
		})
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestRescueBudgetPreservesOwnerCapacity(t *testing.T) {
	budget := NewRescueBudget(time.Now, [sha256.Size]byte{4})
	releases := make([]func(), 0, 8)
	for index := 0; index < 6; index++ {
		release, err := budget.AcquireHTTP(context.Background(), RescueIngressUnverified, []byte("bearer-"+string(rune('a'+index))))
		if err != nil {
			t.Fatalf("unverified %d: %v", index, err)
		}
		releases = append(releases, release)
	}
	if release, err := budget.AcquireHTTP(context.Background(), RescueIngressUnverified, []byte("bearer-over")); !errors.Is(err, ErrRescueCapacity) || release != nil {
		t.Fatalf("seventh unverified = release:%v err:%v", release != nil, err)
	}
	for index := 0; index < 2; index++ {
		release, err := budget.AcquireHTTP(context.Background(), RescueIngressOwnerPermitted, nil)
		if err != nil {
			t.Fatalf("owner %d: %v", index, err)
		}
		releases = append(releases, release)
	}
	if release, err := budget.AcquireHTTP(context.Background(), RescueIngressOwnerPermitted, nil); !errors.Is(err, ErrRescueCapacity) || release != nil {
		t.Fatalf("ninth total = release:%v err:%v", release != nil, err)
	}
	for _, release := range releases {
		release()
	}
}

func TestRescueRelayPropagatesCancellationWithoutRetry(t *testing.T) {
	origin, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	var calls atomic.Int32
	started := make(chan struct{})
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
		Origin: origin, LoopbackHost: "127.0.0.1:29280", ForwardingAcknowledged: true,
		Budget: NewRescueBudget(time.Now, [sha256.Size]byte{5}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`)).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.ServeHTTP(recorder, request)
	}()
	<-started
	cancel()
	<-done
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "rescue_upstream_unavailable") {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestRescueRelayEnforcesBodyLimitBeforeRead(t *testing.T) {
	origin, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.ContentLength != rescueRequestBodyLimit {
				t.Fatalf("content length = %d", request.ContentLength)
			}
			return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
		}),
		Origin: origin, LoopbackHost: "127.0.0.1:29280", ForwardingAcknowledged: true,
		Budget: NewRescueBudget(time.Now, [sha256.Size]byte{8}),
	}
	exact := testRescueRequest(t, http.MethodPost, "/responses", []byte("x"))
	exact.Body = panicOnReadCloser{}
	exact.ContentLength = rescueRequestBodyLimit
	exactRecorder := httptest.NewRecorder()
	relay.ServeHTTP(exactRecorder, exact)
	if exactRecorder.Code != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("exact response = %d calls=%d", exactRecorder.Code, calls.Load())
	}

	over := testRescueRequest(t, http.MethodPost, "/responses", []byte("x"))
	over.Body = panicOnReadCloser{}
	over.ContentLength = rescueRequestBodyLimit + 1
	overRecorder := httptest.NewRecorder()
	relay.ServeHTTP(overRecorder, over)
	if overRecorder.Code != http.StatusRequestEntityTooLarge || calls.Load() != 1 {
		t.Fatalf("over response = %d calls=%d", overRecorder.Code, calls.Load())
	}
}

func TestRescueBudgetGlobalBurstExactAndPlusOne(t *testing.T) {
	budget := NewRescueBudget(func() time.Time { return time.Unix(1, 0) }, [sha256.Size]byte{9})
	for index := 0; index < 32; index++ {
		release, err := budget.AcquireHTTP(context.Background(), RescueIngressUnverified, []byte(fmt.Sprintf("bearer-%d", index)))
		if err != nil {
			t.Fatalf("attempt %d: %v", index, err)
		}
		release()
	}
	if release, err := budget.AcquireHTTP(context.Background(), RescueIngressUnverified, []byte("bearer-over")); !errors.Is(err, ErrRescueCapacity) || release != nil {
		t.Fatalf("plus one = release:%v err:%v", release != nil, err)
	}
}

type panicOnReadCloser struct{}

func (panicOnReadCloser) Read([]byte) (int, error) { panic("body read before admission") }
func (panicOnReadCloser) Close() error             { return nil }
