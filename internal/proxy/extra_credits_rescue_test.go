package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type extraCreditRestriction struct {
	blocked bool
	err     error
}

func (p extraCreditRestriction) Restricted() (bool, error) { return p.blocked, p.err }

func TestExtraCreditsRescueCannotBypassPolicy(t *testing.T) {
	origin, _ := url.Parse(DefaultCodexUpstream)
	for _, tc := range []struct {
		name   string
		policy extraCreditRestriction
		status int
	}{
		{"disabled", extraCreditRestriction{blocked: true}, 503},
		{"unreadable", extraCreditRestriction{err: errors.New("unavailable")}, 503},
		{"enabled", extraCreditRestriction{}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			relay := &RescueRelay{Origin: origin, ExtraCredits: tc.policy, Transport: rescueRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})}
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(method, "http://localhost/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
				request.Header.Set("Connection", "Upgrade")
				request.Header.Set("Upgrade", "websocket")
				relay.ServeHTTP(response, request)
				if response.Code != tc.status {
					t.Fatalf("%s status=%d", method, response.Code)
				}
			}
			if tc.status != 200 && calls != 0 {
				t.Fatalf("protected rescue dispatched %d requests", calls)
			}
		})
	}
}
