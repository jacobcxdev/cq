package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestSubscriptionPrimerClaimSurvivesPanicAndRestart(t *testing.T) {
	fsys := fsutil.NewMemFS()
	makeJournal := func(stem, name string) *subscriptionPrimerJournal {
		t.Helper()
		p, err := newSubscriptionPrimerJournal(fsys, "/state", stem, name, func(id string) bool { return id == "account" })
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	now := time.Unix(10_000, 0)
	p := makeJournal("first", "First")
	posts := 0
	_, err := p.withJournal(t.Context(), now, time.Minute, func() (time.Time, error) {
		return p.dispatch(t.Context(), now, "account", 5*time.Hour, time.Minute, func() (string, error) {
			posts++
			data, err := fsys.ReadFile("/state/first.json")
			if err != nil || !strings.Contains(string(data), `"state":"claimed"`) {
				t.Error("dispatch preceded durable claim")
			}
			panic("private transport failure")
		}, func() (int64, error) { t.Error("panic must not verify"); return 0, nil })
	})
	if err == nil || err.Error() != "First primer failed" {
		t.Fatalf("panic was not classified safely: %v", err)
	}
	p = makeJournal("first", "First")
	_, err = p.withJournal(t.Context(), now.Add(time.Minute), time.Minute, func() (time.Time, error) {
		return p.dispatch(t.Context(), now.Add(time.Minute), "account", 5*time.Hour, time.Minute, func() (string, error) {
			posts++
			return "admitted", nil
		}, func() (int64, error) { return now.Add(5 * time.Hour).Unix(), nil })
	})
	if err != nil || posts != 1 {
		t.Fatalf("claimed outcome replayed after restart: posts=%d error=%v", posts, err)
	}
	// A different provider has a distinct journal even with the same public ID.
	other := makeJournal("second", "Second")
	if len(other.snapshot(true).Accounts) != 0 {
		t.Fatal("provider borrowed another provider's claim")
	}
	if p.path == other.path || p.lockPath == other.lockPath {
		t.Fatal("providers share journal or lock")
	}
}

func TestSubscriptionPrimerResponseClassifiesMissingAndUncertainDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		err              error
		nilResponse      bool
		nilBody          bool
	}{
		{name: "nil response", nilResponse: true, want: "ambiguous"},
		{name: "nil body", status: 200, nilBody: true, want: "ambiguous"},
		{name: "transport", nilResponse: true, err: errors.New("private-credential"), want: "ambiguous"},
		{name: "timeout", status: 408, body: `{}`, want: "ambiguous"},
		{name: "server", status: 503, body: `{}`, want: "ambiguous"},
		{name: "rejected", status: 429, body: `{"error":"private-credential"}`, want: "rejected"},
		{name: "malformed", status: 200, body: `private-credential`, want: "ambiguous"},
		{name: "response error", status: 200, body: `{"error":"private-credential"}`, want: "ambiguous"},
		{name: "admitted", status: 200, body: `{"id":"response","status":"completed"}`, want: "admitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response *http.Response
			if !tc.nilResponse {
				response = &http.Response{StatusCode: tc.status}
				if !tc.nilBody {
					response.Body = io.NopCloser(strings.NewReader(tc.body))
				}
			}
			state, err := subscriptionPrimerResponse(response, tc.err, "Test", nil)
			if state != tc.want || state != "admitted" && err == nil || err != nil && strings.Contains(err.Error(), "private-credential") {
				t.Fatalf("unsafe response classification: state=%q error=%v", state, err)
			}
		})
	}
}

func TestSubscriptionPrimerCancelledRunDoesNotEnterCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := runSubscriptionPrimer(ctx, func() time.Duration { return time.Minute }, func(context.Context, time.Time) (time.Time, error) {
		t.Error("cancelled loop entered callback")
		return time.Time{}, nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("loop lost cancellation: %v", err)
	}
}
