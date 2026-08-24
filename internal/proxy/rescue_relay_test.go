package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type rescueRoundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip rescueRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func testRescueRequest(t *testing.T, method, target string, body []byte) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
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

func testRescueOrigin(t *testing.T) *url.URL {
	t.Helper()
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	return origin
}

func TestRescueRelayForwardsOpaqueBearerOnce(t *testing.T) {
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if got := request.URL.String(); got != "https://chatgpt.com/backend-api/codex/responses" {
				t.Fatalf("upstream URL = %q", got)
			}
			if got := request.Header.Get("Authorization"); got != "Bearer opaque-token" {
				t.Fatalf("authorization = %q", got)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(body); got != `{"model":"gpt-5"}` {
				t.Fatalf("body = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"request-id"}},
				Body:       io.NopCloser(strings.NewReader("data: done\n\n")),
			}, nil
		}),
		Origin: testRescueOrigin(t),
	}
	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "data: done\n\n" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestRescueRelayForwardsCurrentCodexHeadersWithoutCQAuthentication(t *testing.T) {
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
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
		Origin: testRescueOrigin(t),
	}
	request := testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`))
	request.Header.Set("Authorization", "Bearer current-upstream-token")
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

func TestRescueRelayDoesNotThrottleRepeatedRequests(t *testing.T) {
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: done\n\n")),
			}, nil
		}),
		Origin: testRescueOrigin(t),
	}

	for index := 0; index < 5; index++ {
		recorder := httptest.NewRecorder()
		relay.ServeHTTP(recorder, testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d response = %d %q", index+1, recorder.Code, recorder.Body.String())
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("upstream calls = %d, want 5", calls.Load())
	}
}

func TestRescueRelayLetsUpstreamDecideMissingAuthentication(t *testing.T) {
	var calls atomic.Int32
	relay := &RescueRelay{
		Transport: rescueRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header: http.Header{
					"Content-Type":     {"application/json"},
					"Www-Authenticate": {`Bearer realm="upstream"`},
				},
				Body: io.NopCloser(strings.NewReader(`{"upstream":"unauthorized"}`)),
			}, nil
		}),
		Origin: testRescueOrigin(t),
	}
	request := testRescueRequest(t, http.MethodPost, "/responses", []byte(`{"model":"gpt-5"}`))
	request.Header.Del("Authorization")
	recorder := httptest.NewRecorder()

	relay.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized || recorder.Body.String() != `{"upstream":"unauthorized"}` || calls.Load() != 1 {
		t.Fatalf("response/calls = %d/%q/%d", recorder.Code, recorder.Body.String(), calls.Load())
	}
	if got := recorder.Header().Get("Www-Authenticate"); got != `Bearer realm="upstream"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}

func TestRescueRelayPropagatesCancellationWithoutDeadlineOrRetry(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	imposedDeadline := make(chan bool, 1)
	relay := &RescueRelay{
		Transport: rescueRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			_, imposed := request.Context().Deadline()
			imposedDeadline <- imposed
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
		Origin: testRescueOrigin(t),
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
	hadDeadline := <-imposedDeadline
	cancel()
	<-done
	if hadDeadline {
		t.Fatal("rescue imposed an upstream request deadline")
	}
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "rescue_upstream_unavailable") {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}
