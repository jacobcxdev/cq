package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

const grokPrimerTestID = grok.AccountID("grok_0123456789abcdef0123456789abcdef")

type grokPrimerFake struct {
	badAccount bool
	realClock  bool
	badQuota   bool
	now        time.Time
	period     time.Duration
	reset      time.Time
	used       float64
	posts      int
	modelReads int
	postErr    error
	noModel    bool
	fs         *fsutil.MemFS
}

func (f *grokPrimerFake) Accounts(context.Context) ([]grok.Account, error) {
	accounts := []grok.Account{{ID: grokPrimerTestID, UserID: "user", Plan: "SuperGrok"}}
	if f.badAccount {
		accounts = append(accounts, grok.Account{ID: "grok_ffffffffffffffffffffffffffffffff", UserID: "broken"})
	}
	return accounts, nil
}
func (f *grokPrimerFake) Do(_ context.Context, id grok.AccountID, req *http.Request) (*http.Response, error) {
	if id != grokPrimerTestID {
		return nil, errors.New("wrong account")
	}
	if f.realClock {
		f.now = time.Now().Truncate(time.Second)
	}
	body := "{}"
	switch req.URL.Path {
	case "/v1/billing":
		reset := f.reset
		if reset.IsZero() {
			reset = f.now.Add(f.period)
		}
		body = fmt.Sprintf(`{"config":{"creditUsagePercent":%g,"currentPeriod":{"start":%q,"end":%q}}}`, f.used, reset.Add(-f.period).Format(time.RFC3339), reset.Format(time.RFC3339))
	case "/v1/settings":
		body = `{"subscription_tier_display":"SuperGrok"}`
	case "/v1/models":
		f.modelReads++
		body = `{"data":[{"id":"grok-fast","apiBackend":"responses","reasoningEfforts":["low"]}]}`
		if f.noModel {
			body = `{"data":[]}`
		}
	case "/v1/responses":
		if req.Method != http.MethodPost {
			return nil, errors.New("wrong method")
		}
		data, err := f.fs.ReadFile("/state/grok-window-primer.json")
		if err != nil || !strings.Contains(string(data), `"claimed"`) {
			return nil, errors.New("dispatch before durable claim")
		}
		var request struct {
			Model string `json:"model"`
			Limit int    `json:"max_output_tokens"`
			Store bool   `json:"store"`
		}
		if json.NewDecoder(req.Body).Decode(&request) != nil || request.Model != "grok-fast" || request.Limit != 16 || request.Store {
			return nil, errors.New("invalid primer request")
		}
		f.posts++
		if f.postErr != nil {
			return nil, f.postErr
		}
		f.used = 0.01
		f.reset = f.now.Add(f.period)
		body = `{"id":"primer-response","status":"completed"}`
	default:
		return nil, errors.New("unexpected subscription path")
	}
	if f.badQuota && req.URL.Path == "/v1/billing" {
		body = `{"config":{}}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func newGrokPrimerFake(t *testing.T, period time.Duration) (*GrokPrimer, *grokPrimerFake) {
	t.Helper()
	f := &grokPrimerFake{now: time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC), period: period, fs: fsutil.NewMemFS()}
	p, err := NewGrokPrimer(f.fs, "/state", f)
	if err != nil {
		t.Fatal(err)
	}
	return p, f
}
func TestGrokPrimerSkipsScheduledAndUsedWindows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		period time.Duration
		fixed  bool
		used   float64
	}{
		{"weekly", 7 * 24 * time.Hour, false, 0}, {"active5h", 5 * time.Hour, true, 0}, {"fractionalUse", 5 * time.Hour, false, 0.001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := newGrokPrimerFake(t, tc.period)
			f.used = tc.used
			if tc.fixed {
				f.reset = f.now.Add(tc.period)
			}
			for range 2 {
				if _, err := p.RunOnce(t.Context(), f.now); err != nil {
					t.Fatal(err)
				}
				f.now = f.now.Add(5 * time.Second)
			}
			if f.posts != 0 || f.modelReads != 0 {
				t.Fatalf("non-dormant quota caused requests: posts=%d models=%d", f.posts, f.modelReads)
			}
		})
	}
}
func TestGrokPrimerRequiresMovingResetAndClaimsBeforeSend(t *testing.T) {
	p, f := newGrokPrimerFake(t, 5*time.Hour)
	if _, err := p.RunOnce(t.Context(), f.now); err != nil || f.posts != 0 {
		t.Fatal("first observation sent request", err)
	}
	f.now = f.now.Add(5 * time.Second)
	if _, err := p.RunOnce(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 {
		t.Fatalf("posts=%d", f.posts)
	}
	if p.Health(true).Accounts[string(grokPrimerTestID)].State != "verified" {
		t.Fatal("activation not verified")
	}
	// Restart with moving dormant observations cannot replay the persisted claim.
	f.used = 0
	f.reset = time.Time{}
	reopened, err := NewGrokPrimer(f.fs, "/state", f)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.now = f.now.Add(5 * time.Second)
		if _, err := reopened.RunOnce(t.Context(), f.now); err != nil {
			t.Fatal(err)
		}
	}
	if f.posts != 1 {
		t.Fatal("restarted worker replayed priming")
	}
}
func TestGrokPrimerMissingModelAndAmbiguousDelivery(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			p, f := newGrokPrimerFake(t, 5*time.Hour)
			f.noModel = missing
			if !missing {
				f.postErr = errors.New("private upstream detail")
			}
			_, _ = p.RunOnce(t.Context(), f.now)
			f.now = f.now.Add(5 * time.Second)
			_, err := p.RunOnce(t.Context(), f.now)
			if err == nil {
				t.Fatal("failure not reported")
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("upstream detail leaked")
			}
			want := 1
			if missing {
				want = 0
			}
			if f.posts != want {
				t.Fatalf("posts=%d", f.posts)
			}
			for range 2 {
				f.now = f.now.Add(5 * time.Second)
				_, _ = p.RunOnce(t.Context(), f.now)
			}
			if f.posts != want {
				t.Fatal("ambiguous dispatch repeated")
			}
		})
	}
}

func TestGrokPrimerManualNowConfirmsDormancy(t *testing.T) {
	p, f := newGrokPrimerFake(t, 5*time.Hour)
	f.realClock = true
	if err := p.PrimeNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 {
		t.Fatalf("manual priming posts=%d", f.posts)
	}
}
func TestGrokPrimerConcurrentWorkersDoNotRepeatDispatch(t *testing.T) {
	p, f := newGrokPrimerFake(t, 5*time.Hour)
	other, err := NewGrokPrimer(f.fs, "/state", f)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.RunOnce(t.Context(), f.now)
	_, _ = other.RunOnce(t.Context(), f.now)
	f.now = f.now.Add(5 * time.Second)
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	for _, worker := range []*GrokPrimer{p, other} {
		workers.Add(1)
		go func() { defer workers.Done(); _, err := worker.RunOnce(t.Context(), f.now); failures <- err }()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.posts != 1 {
		t.Fatalf("concurrent priming posts=%d", f.posts)
	}
}
func TestGrokPrimerUnknownQuotaDoesNotDispatch(t *testing.T) {
	p, f := newGrokPrimerFake(t, 5*time.Hour)
	f.badQuota = true
	for range 2 {
		if _, err := p.RunOnce(t.Context(), f.now); err == nil {
			t.Fatal("unknown quota accepted")
		}
		f.now = f.now.Add(5 * time.Second)
	}
	if f.posts != 0 || f.modelReads != 0 {
		t.Fatal("unknown quota triggered inference")
	}
}

func TestGrokPrimerManualFailureDoesNotSuppressHealthyAccount(t *testing.T) {
	p, f := newGrokPrimerFake(t, 5*time.Hour)
	f.realClock = true
	f.badAccount = true
	if err := p.PrimeNow(t.Context()); err == nil {
		t.Fatal("failed account hidden")
	}
	if f.posts != 1 {
		t.Fatalf("failed account prevented healthy priming: posts=%d", f.posts)
	}
}
