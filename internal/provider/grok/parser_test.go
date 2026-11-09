package grok

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestParseSubscriptionQuota(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		used      float64
		remaining int
		status    quota.Status
	}{
		{0, 100, quota.StatusOK}, {12.5, 88, quota.StatusOK},
		{99.99, 1, quota.StatusOK}, {100, 0, quota.StatusExhausted}, {105, 0, quota.StatusExhausted},
	} {
		t.Run(fmt.Sprint(tc.used), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"config":{"creditUsagePercent":%v,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-01T00:00:00Z","end":"2026-11-08T00:00:00Z"},"prepaidBalance":{"val":999},"onDemandCap":{"val":300}}}`, tc.used))
			result, err := parseQuota(body, now)
			if err != nil {
				t.Fatal(err)
			}
			window := result.Windows[quota.Window7Day]
			wantExact := max(0, 100-tc.used)
			if result.Status != tc.status || window.RemainingPct != tc.remaining || window.RemainingPctExact == nil || math.Abs(*window.RemainingPctExact-wantExact) > 1e-9 {
				t.Fatalf("unexpected quota: %#v, %#v", result, window)
			}
			if window.ResetAtUnix != time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC).Unix() {
				t.Fatal("wrong reset")
			}
		})
	}
}

func TestParseSubscriptionQuotaRejectsUnknownOrStaleCapacity(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	for _, body := range []string{
		`null`, `{}`, `{"config":null}`, `{"config":{"creditUsagePercent":null}}`,
		`{"config":{"creditUsagePercent":-1,"currentPeriod":{"start":"2026-11-01T00:00:00Z","end":"2026-11-08T00:00:00Z"}}}`,
		`{"config":{"creditUsagePercent":0,"currentPeriod":{"start":"2026-11-08T00:00:00Z","end":"2026-11-01T00:00:00Z"}}}`,
		`{"config":{"creditUsagePercent":0,"currentPeriod":{"start":"2026-10-01T00:00:00Z","end":"2026-11-01T00:00:00Z"}}}`,
		`{"config":{"creditUsagePercent":0,"currentPeriod":{"start":"2026-12-01T00:00:00Z","end":"2026-12-08T00:00:00Z"}}}`,
		`{"config":{"creditUsagePercent":0,"currentPeriod":{"start":"invalid","end":"2026-11-08T00:00:00Z"}}}`,
		`{"config":{"creditUsagePercent":"50","currentPeriod":{"start":"2026-11-01T00:00:00Z","end":"2026-11-08T00:00:00Z"}}}`,
		`{"config":{"prepaidBalance":{"val":999999}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := parseQuota([]byte(body), now); err == nil {
				t.Fatal("unknown quota accepted")
			}
		})
	}
}

func TestParseSubscriptionCalendarPeriodUsesActualBoundaries(t *testing.T) {
	result, err := parseQuota([]byte(`{"config":{"creditUsagePercent":25,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-10-20T12:00:00Z","end":"2026-11-20T12:00:00Z"}}}`), time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Windows["31d"]; !ok {
		t.Fatalf("fabricated monthly period: %#v", result.Windows)
	}
}

func TestParseSubscriptionQuotaOmittedZeroUsage(t *testing.T) {
	// Sanitised live shape: proto3 JSON omits the zero-valued percentage.
	body := []byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-06T17:04:32.756+00:00","end":"2026-11-13T17:04:32.756+00:00"},"isUnifiedBillingUser":true}}`)
	result, err := parseQuota(body, time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	window, ok := result.Windows[quota.Window7Day]
	if !ok || result.Status != quota.StatusOK || window.RemainingPct != 100 || window.RemainingPctExact == nil || *window.RemainingPctExact != 100 || window.ResetAtUnix != time.Date(2026, 11, 13, 17, 4, 32, 0, time.UTC).Unix() {
		t.Fatalf("unexpected omitted-zero result: %#v", result)
	}
}

func TestParseSubscriptionQuotaRejectsMissingUsageWithoutValidPeriod(t *testing.T) {
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	for _, body := range []string{
		`{"config":{}}`,
		`{"config":{"currentPeriod":null}}`,
		`{"config":{"currentPeriod":{}}}`,
		`{"config":{"currentPeriod":{"start":"2026-11-06T17:04:32Z"}}}`,
		`{"config":{"currentPeriod":{"start":"2026-10-06T17:04:32Z","end":"2026-10-13T17:04:32Z"}}}`,
		`{"config":{"creditUsagePercent":null,"currentPeriod":{"start":"2026-11-06T17:04:32Z","end":"2026-11-13T17:04:32Z"}}}`,
		`{"config":{"creditUsagePercent":"0","currentPeriod":{"start":"2026-11-06T17:04:32Z","end":"2026-11-13T17:04:32Z"}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := parseQuota([]byte(body), now); err == nil {
				t.Fatal("invalid or unknown quota accepted")
			}
		})
	}
}
