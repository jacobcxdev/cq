package opencodego

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

type doUsage func(*http.Request) (*http.Response, error)

func (f doUsage) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestUsageReadsReportedWindows(t *testing.T) {
	client := doUsage(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != UsageURL || r.Method != http.MethodGet ||
			r.Header.Get("Authorization") != "Bearer workspace-key" || r.Header.Get("X-OpenCode-Org-Id") != "wrk_123" {
			t.Error("usage request used the wrong endpoint or workspace")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"usage":{
			"rolling":{"status":"ok","percent":17.5,"resetsAt":"2026-11-18T16:01:02Z"},
			"weekly":{"status":"rate-limited","percent":99.7,"resetsAt":"2026-11-22T01:02:03Z"},
			"monthly":{"status":"ok","percent":0}
		}}`))}, nil
	})
	got := Usage(context.Background(), client, "workspace-key", "wrk_123")
	want := []quota.SharedBudgetWindow{
		{Name: "5h", RemainingPct: 82.5, ResetAtUnix: time.Date(2026, 11, 18, 16, 1, 2, 0, time.UTC).Unix()},
		{Name: "7d", RemainingPct: 0, ResetAtUnix: time.Date(2026, 11, 22, 1, 2, 3, 0, time.UTC).Unix()},
		{Name: "1mo", RemainingPct: 100},
	}
	if len(got) != len(want) {
		t.Fatalf("windows = %+v", got)
	}
	for i, window := range got {
		if window != want[i] {
			t.Errorf("window %d = %+v, want %+v", i, window, want[i])
		}
	}
}

func TestUsageDoesNotInventWindows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", 403, `{"usage":{"rolling":{"status":"ok","percent":0}}}`},
		{"malformed", 200, `{"usage":`},
		{"unknown", 200, `{"usage":{"rolling":{"status":"ok"},"weekly":{"status":"ok","percent":103},"monthly":{"status":"unknown","percent":0}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := doUsage(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			if got := Usage(context.Background(), client, "workspace-key", ""); len(got) != 0 {
				t.Fatalf("unexpected windows %+v", got)
			}
		})
	}
	if got := Usage(context.Background(), nil, "", ""); len(got) != 0 {
		t.Fatalf("unexpected windows %+v", got)
	}
}
