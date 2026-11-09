package zai

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchExcludedUnverifiedSourceBeforeQuotaRequests(t *testing.T) {
	accounts := []Account{
		{ID: "borrowed", Source: "codexbar", APIKey: "unverified"},
		{ID: "owned", Label: "Configured", APIKey: "owned-key"},
	}
	var requests atomic.Int32
	p := NewAccounts(doerFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "owned-key" {
			t.Error("unverified source reached quota endpoint")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"level":"max","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":25}]}}`))}, nil
	}), func() ([]Account, error) { return accounts, nil })
	results, err := p.Fetch(t.Context(), time.Now())
	if err != nil || len(results) != 1 || results[0].AccountID != "owned" || !results[0].Active || results[0].MinRemainingPct() != 75 || requests.Load() != 1 {
		t.Fatalf("unverified source changed totals or preference: rows=%d requests=%d err=%v", len(results), requests.Load(), err)
	}
}
