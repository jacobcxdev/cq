package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyReserveControl(t *testing.T) {
	for _, command := range []string{"set", "status", "windows", "disable", "enable", "clear"} {
		t.Run(command, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/_cq/control/reserve" || r.Header.Get("Authorization") != "Bearer local" {
					t.Errorf("wrong request: %s", r.URL.Path)
				}
				if command == "status" || command == "windows" {
					if r.Method != http.MethodGet {
						t.Errorf("method = %s", r.Method)
					}
				} else {
					var body struct {
						Action  string  `json:"action"`
						Window  string  `json:"window"`
						Percent float64 `json:"percent"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if r.Method != http.MethodPost || body.Action != command {
						t.Errorf("mutation = %#v", body)
					}
					if command == "set" && (body.Window != "7d" || body.Percent != 2) {
						t.Errorf("set = %#v", body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"configured":false,"windows":{}}`))
			}))
			defer server.Close()
			port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
			args := []string{command, "--json"}
			if command == "set" {
				args = append(args, "--window", "7d", "--percent", "2")
			}
			var output bytes.Buffer
			err := runProxyReserveWithDependencies(context.Background(), args, &output, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local"}, nil }, Doer: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(output.Bytes()) {
				t.Fatalf("invalid JSON: %s", output.String())
			}
		})
	}
}

func TestProxyReserveRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{nil, {"set"}, {"set", "--window", "7d", "--percent", "NaN"}, {"set", "--window", "7d", "--percent", "101"}, {"set", "--window", "7d", "--percent", "0"}, {"disable", "--window", "7d"}, {"wat"}, {"status", "--json", "--json"}} {
		if err := runProxyReserveWithDependencies(context.Background(), args, &bytes.Buffer{}, proxyPolicyDependencies{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestProxyReserveReportsUnavailableService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	err := runProxyReserveWithDependencies(context.Background(), []string{"status"}, &bytes.Buffer{}, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "local"}, nil }, Doer: server.Client()})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("error = %v", err)
	}
}

func TestProxyReserveLegacyOutputAndErrors(t *testing.T) {
	for _, tc := range []struct{ action, body, want string }{
		{"status", `{"configured":false,"windows":{}}`, "System account reserve: not configured.\n"},
		{"windows", `{"windows":{}}`, "No system-account quota windows available yet.\n"},
		{"windows", `{"windows":{"7d":{},"5h":{}}}`, "5h\n7d\n"},
		{"status", `{"configured":true,"window":"7d","percent":2,"enabled":false,"email":"a@example.com","reason":"disabled_until_reset"}`, "System account reserve: 2% of 7d (disabled until reset)\nAccount: a@example.com\nAvailability: disabled_until_reset\n"},
	} {
		var output bytes.Buffer
		err := runProxyReserveWithDependencies(context.Background(), []string{tc.action}, &output, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})})
		if err != nil || output.String() != tc.want {
			t.Fatalf("legacy %s output=%q error=%v", tc.action, &output, err)
		}
	}
	for _, code := range []string{"", "reserve_evidence_required", "routing_io_failed", "unknown-private-code"} {
		var output bytes.Buffer
		err := runProxyReserveWithDependencies(context.Background(), []string{"disable"}, &output, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 409, Header: http.Header{"X-Cq-Reserve-Error": []string{code}}, Body: io.NopCloser(strings.NewReader("reserve control rejected\n"))}, nil
		})})
		if err == nil || err.Error() != "proxy reserve control failed: HTTP 409" || output.Len() != 0 {
			t.Fatalf("legacy error changed: %v output=%q", err, &output)
		}
	}
}

type reserveErrorBody struct {
	reader        io.Reader
	reads, closes int
}

func newReserveErrorBody(oversized bool) *reserveErrorBody {
	body := &reserveErrorBody{}
	if oversized {
		body.reader = strings.NewReader(strings.Repeat("x", (1<<20)+1))
	}
	return body
}
func (b *reserveErrorBody) Read(p []byte) (int, error) {
	b.reads++
	if b.reader == nil {
		return 0, errors.New("private body failure")
	}
	return b.reader.Read(p)
}
func (b *reserveErrorBody) Close() error { b.closes++; return nil }

func TestProxyReserveErrorStatusBeforeBody(t *testing.T) {
	for _, status := range []int{401, 403, 409} {
		for _, oversized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/oversized=%t", status, oversized), func(t *testing.T) {
				body := newReserveErrorBody(oversized)
				var output bytes.Buffer
				err := runProxyReserveWithDependencies(context.Background(), []string{"disable"}, &output, proxyPolicyDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "synthetic"}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: http.Header{"X-Cq-Reserve-Error": []string{"reserve_evidence_required"}}, Body: body}, nil
				})})
				if err == nil || err.Error() != fmt.Sprintf("proxy reserve control failed: HTTP %d", status) || output.Len() != 0 {
					t.Errorf("legacy error=%v output=%q", err, &output)
				}
				if body.reads != 0 || body.closes != 1 {
					t.Errorf("error body reads=%d closes=%d", body.reads, body.closes)
				}
			})
		}
	}
}
