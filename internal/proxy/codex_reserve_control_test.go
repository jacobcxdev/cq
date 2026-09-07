package proxy

import (
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/quota"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReserveControlRequiresLocalCaller(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if got := normalCallerPolicy(httptest.NewRequest(method, "/_cq/control/reserve", nil)); got != normalCallerRouteLocal {
			t.Fatalf("%s policy = %v, want local", method, got)
		}
	}
}

func TestReserveControlUnavailable(t *testing.T) {
	handler, err := (&Server{Config: &Config{ClaudeUpstream: "https://example.test"}}).handler()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_cq/control/reserve", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestReserveControlMutationsAndValidation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 10, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	handler, err := (&Server{Config: &Config{ClaudeUpstream: "https://example.test"}, Reserve: reserve}).handler()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"action":"set","window":"7d","percent":2}`, http.StatusOK},
		{`{"action":"disable"}`, http.StatusOK},
		{`{"action":"enable"}`, http.StatusOK},
		{`{"action":"set","window":"7d","percent":2,"account":"other"}`, http.StatusBadRequest},
		{`{"action":"set","window":"7d","percent":2} {}`, http.StatusBadRequest},
		{`{"action":"disable","window":"7d"}`, http.StatusBadRequest},
		{`{"action":"unknown"}`, http.StatusConflict},
		{`{"action":"clear"}`, http.StatusOK},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, RuntimeReservePath, strings.NewReader(tc.body)))
		if response.Code != tc.code {
			t.Fatalf("%s: status %d, body %s", tc.body, response.Code, response.Body.String())
		}
	}
	if reserve.Status().Configured {
		t.Fatal("clear left reserve configured")
	}
}
