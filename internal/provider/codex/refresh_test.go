package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

func TestFetchUsageRetriedRoutingCookie(t *testing.T) {
	const usage = `{"plan_type":"pro","account_id":"acct-123"}`
	calls := 0
	client := providerDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://chatgpt.com/backend-api/wham/usage" || req.Method != http.MethodGet {
			t.Fatal("usage retry changed destination or method")
		}
		if req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("ChatGPT-Account-Id") != "acct-123" {
			t.Fatal("usage retry changed credential or account")
		}
		if calls == 1 {
			if req.Header.Get("Cookie") != "" {
				t.Fatal("first request contained a cookie")
			}
			return &http.Response{StatusCode: http.StatusNotFound, Request: req,
				Header: http.Header{"Set-Cookie": {
					"__oailb=route-a; Path=/; Secure; HttpOnly",
					"session=private-session; Path=/; Secure",
					"__cf_bm=challenge; Path=/; Secure",
				}}, Body: io.NopCloser(strings.NewReader(`{"detail":"Not Found"}`))}, nil
		}
		if calls != 2 || req.Header.Get("Cookie") != "__oailb=route-a" {
			t.Fatal("retry did not retain only the routing cookie")
		}
		return &http.Response{StatusCode: http.StatusOK, Request: req, Body: io.NopCloser(strings.NewReader(usage))}, nil
	})
	body, status, err := fetchUsage(context.Background(), client, "test-token", "acct-123")
	if err != nil || status != http.StatusOK || string(body) != usage || calls != 2 {
		t.Fatalf("fetchUsage = %q, %d, %v, calls=%d", body, status, err, calls)
	}
}

func TestFetchUsageAccountIDHeader(t *testing.T) {
	var gotAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &urlRewriter{client: srv.Client(), baseURL: srv.URL}
	_, _, err := fetchUsage(context.Background(), client, "test-token", "acct-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAccountID != "acct-123" {
		t.Errorf("ChatGPT-Account-Id = %q, want acct-123", gotAccountID)
	}
}

func TestFetchUsageNoAccountIDHeader(t *testing.T) {
	var gotAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &urlRewriter{client: srv.Client(), baseURL: srv.URL}
	_, _, err := fetchUsage(context.Background(), client, "test-token", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAccountID != "" {
		t.Errorf("ChatGPT-Account-Id = %q, want empty (not set when accountID is empty)", gotAccountID)
	}
}

func TestFetchUsageRoutingCookieBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      int
		cookie      string
		responseURL string
		wantCalls   int
	}{
		{"success", 200, "__oailb=route; Path=/; Secure", "", 1},
		{"auth rejection", 401, "__oailb=route; Path=/; Secure", "", 1},
		{"forbidden", 403, "__oailb=route; Path=/; Secure", "", 1},
		{"rate limited", 429, "__oailb=route; Path=/; Secure", "", 1},
		{"server error", 503, "__oailb=route; Path=/; Secure", "", 1},
		{"no cookie", 404, "", "", 1},
		{"session cookie", 404, "session=private; Path=/; Secure", "", 1},
		{"other infrastructure cookie", 404, "__cf_bm=challenge; Path=/; Secure", "", 1},
		{"empty cookie", 404, "__oailb=; Path=/; Secure", "", 1},
		{"deleted cookie", 404, "__oailb=route; Path=/; Secure; Max-Age=0", "", 1},
		{"expired cookie", 404, "__oailb=route; Path=/; Secure; Expires=Thu, 01 Jan 1970 00:00:00 GMT", "", 1},
		{"foreign domain", 404, "__oailb=route; Domain=other.invalid; Path=/; Secure", "", 1},
		{"unrelated path", 404, "__oailb=route; Path=/unrelated; Secure", "", 1},
		{"redirected host", 404, "__oailb=route; Domain=chatgpt.com; Path=/; Secure", "https://other.invalid/backend-api/wham/usage", 1},
		{"redirected subdomain", 404, "__oailb=route; Domain=chatgpt.com; Path=/; Secure", "https://other.chatgpt.com/backend-api/wham/usage", 1},
		{"redirected scheme", 404, "__oailb=route; Path=/", "http://chatgpt.com/backend-api/wham/usage", 1},
		{"repeated not found", 404, "__oailb=route; Path=/; Secure", "", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := providerDoerFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls > tt.wantCalls {
					t.Fatal("unexpected retry")
				}
				responseRequest := req
				if tt.responseURL != "" {
					responseRequest = req.Clone(req.Context())
					responseRequest.URL, _ = url.Parse(tt.responseURL)
				}
				return &http.Response{StatusCode: tt.status, Request: responseRequest, Header: http.Header{"Set-Cookie": {tt.cookie}}, Body: io.NopCloser(strings.NewReader("upstream response"))}, nil
			})
			body, status, err := fetchUsage(context.Background(), client, "test-token", "acct-123")
			if err != nil || status != tt.status || string(body) != "upstream response" || calls != tt.wantCalls {
				t.Fatalf("body=%q status=%d err=%v calls=%d", body, status, err, calls)
			}
		})
	}
}

func TestFetchUsageHonouredRoutingCookieDeletion(t *testing.T) {
	calls := 0
	client := providerDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNotFound, Request: req,
			Header: http.Header{"Set-Cookie": {
				"__oailb=route; Path=/; Secure",
				"__oailb=; Path=/; Secure; Max-Age=0",
			}}, Body: io.NopCloser(strings.NewReader("not found"))}, nil
	})
	_, status, err := fetchUsage(context.Background(), client, "test-token", "acct-123")
	if err != nil || status != http.StatusNotFound || calls != 1 {
		t.Fatalf("status=%d err=%v calls=%d", status, err, calls)
	}
}

func TestFetchUsageDidNotRedirectRoutingRetry(t *testing.T) {
	calls := 0
	client := httputil.NewClientWithTransport(time.Second, "test", usageRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusNotFound, Request: req,
				Header: http.Header{"Set-Cookie": {"__oailb=route; Path=/backend-api; Secure"}},
				Body:   io.NopCloser(strings.NewReader("not found"))}, nil
		}
		if calls != 2 || req.Header.Get("Cookie") != "__oailb=route" || req.URL.Path != "/backend-api/wham/usage" {
			t.Fatal("routing retry left its cookie scope")
		}
		return &http.Response{StatusCode: http.StatusFound, Request: req, Header: http.Header{"Location": {"https://chatgpt.com/unrelated"}},
			Body: io.NopCloser(strings.NewReader("redirect"))}, nil
	}))
	body, status, err := fetchUsage(context.Background(), client, "test-token", "acct-123")
	if err != nil || status != http.StatusFound || string(body) != "redirect" || calls != 2 {
		t.Fatalf("body=%q status=%d err=%v calls=%d", body, status, err, calls)
	}
}

type usageRoundTripper func(*http.Request) (*http.Response, error)

func (f usageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type usageCloseReader struct {
	io.Reader
	closed int
}

func (r *usageCloseReader) Close() error { r.closed++; return nil }

func TestFetchUsageRoutingRetryIsolationAndBodyClosure(t *testing.T) {
	first := &usageCloseReader{Reader: strings.NewReader("not found")}
	second := &usageCloseReader{Reader: strings.NewReader("usage")}
	calls := 0
	client := providerDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return &http.Response{StatusCode: 404, Request: req, Header: http.Header{"Set-Cookie": {"__oailb=route; Path=/; Secure"}}, Body: first}, nil
		case 2:
			if first.closed != 1 {
				t.Fatal("first body was not closed before retry")
			}
			return &http.Response{StatusCode: 200, Request: req, Body: second}, nil
		default:
			if req.Header.Get("Cookie") != "" {
				t.Fatal("routing cookie leaked between fetches")
			}
			return &http.Response{StatusCode: 200, Request: req, Body: io.NopCloser(strings.NewReader("usage"))}, nil
		}
	})
	for _, account := range []string{"acct-123", "other-account", "acct-123"} {
		_, status, err := fetchUsage(context.Background(), client, "test-token", account)
		if err != nil || status != 200 {
			t.Fatalf("status=%d err=%v", status, err)
		}
	}
	if first.closed != 1 || second.closed != 1 || calls != 4 {
		t.Fatalf("closed=%d,%d calls=%d", first.closed, second.closed, calls)
	}
}

func TestFetchUsageRoutingRetryFailures(t *testing.T) {
	for _, failure := range []string{"transport", "body limit", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			sentinel := errors.New("selected egress failed")
			client := providerDoerFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Context() != ctx {
					t.Fatal("request context changed")
				}
				if calls == 1 {
					if failure == "cancelled" {
						cancel()
					}
					return &http.Response{StatusCode: 404, Request: req, Header: http.Header{"Set-Cookie": {"__oailb=route; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader("not found"))}, nil
				}
				if calls > 2 {
					t.Fatal("retry limit exceeded")
				}
				if failure == "body limit" {
					return &http.Response{StatusCode: 200, Request: req, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", httputil.MaxResponseBody+1)))}, nil
				}
				if failure == "cancelled" {
					return nil, req.Context().Err()
				}
				return nil, sentinel
			})
			_, _, err := fetchUsage(ctx, client, "test-token", "acct-123")
			want := sentinel
			if failure == "body limit" {
				want = httputil.ErrBodyTooLarge
			}
			if failure == "cancelled" {
				want = context.Canceled
			}
			if !errors.Is(err, want) || calls != 2 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
