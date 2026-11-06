package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestFetchLogicalAccountRetainsKnownPlanBeforeDispatch(t *testing.T) {
	now := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	ref := CandidateRef{AccountKey: "logical", CandidateID: "candidate"}
	for _, tt := range []struct {
		name, plan, wantTier, wantCode, wantMessage string
		routable                                    bool
		candidates                                  []CredentialCandidate
	}{
		{name: "expired external owner", plan: "pro", wantTier: "codex_pro_20x", wantCode: "auth_expired", wantMessage: credentialOwnerRefreshRequiredMessage, routable: true, candidates: []CredentialCandidate{providerCandidate(ref, "revision", SourceExternal, now.Add(-time.Minute))}},
		{name: "CQ refresh required", plan: "prolite", wantTier: "codex_prolite_10x", wantCode: "auth_expired", wantMessage: cqRefreshRequiredMessage, routable: true, candidates: []CredentialCandidate{providerRefreshEligibleCandidate(ref, "revision", now.Add(-time.Minute))}},
		{name: "unroutable business", plan: "self_serve_business_prolite", wantTier: "codex_self_serve_business_prolite_5x", wantCode: "no_token", wantMessage: "no token"},
		{name: "missing access token", plan: "promax", wantTier: "codex_promax_50x", wantCode: "no_token", wantMessage: "no token", routable: true, candidates: []CredentialCandidate{providerCandidate(ref, "revision", SourceExternal, now.Add(time.Hour))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &Provider{client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unavailable logical account reached upstream")
				return nil, errors.New("unexpected upstream request")
			})}
			logical := LogicalAccount{Routable: tt.routable, Candidates: tt.candidates,
				Identity: AccountIdentity{AccountID: "account", UserID: "user", Email: "member@example.com", PlanType: tt.plan},
			}
			got := p.fetchLogicalAccount(context.Background(), logical, nil, nil, nil, now)
			if got.Plan != tt.plan || got.RateLimitTier != tt.wantTier {
				t.Fatalf("plan/tier = %q/%q, want %q/%q", got.Plan, got.RateLimitTier, tt.plan, tt.wantTier)
			}
			if got.Error == nil || got.Error.Code != tt.wantCode || got.Error.Message != tt.wantMessage {
				t.Fatalf("error = %+v, want %q/%q", got.Error, tt.wantCode, tt.wantMessage)
			}
		})
	}
}

func TestFetchLogicalAccountPreservesObservedPlanAndTier(t *testing.T) {
	now := time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct{ plan, tier string }{
		{"self_serve_business_prolite", "observed_7x"},
		{"self_serve_business_usage_based", ""},
	} {
		t.Run(tt.plan, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"plan_type": tt.plan, "rate_limit_tier": tt.tier})
			if err != nil {
				t.Fatal(err)
			}
			p := &Provider{client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			candidate := providerCandidate(CandidateRef{AccountKey: "logical", CandidateID: "candidate"}, "revision", SourceExternal, now.Add(time.Hour))
			candidate.Credential = CodexAccount{AccessToken: "test-token", PlanType: "pro"}
			logical := LogicalAccount{Routable: true, Identity: AccountIdentity{PlanType: "promax"}, Candidates: []CredentialCandidate{candidate}}
			got := p.fetchLogicalAccount(context.Background(), logical, nil, nil, nil, now)
			if got.Error != nil || got.Plan != tt.plan || got.RateLimitTier != tt.tier {
				t.Fatalf("observed metadata overwritten: %+v", got)
			}
		})
	}
}

func TestFetchAccountRetainsPlanOnErrorsAndObservedUsageWins(t *testing.T) {
	oldNow := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = oldNow })
	for _, tt := range []struct {
		name, accountPlan, body, wantPlan, wantTier, wantError string
		status                                                 int
		offline                                                bool
	}{
		{name: "transport error", accountPlan: "prolite", offline: true, wantPlan: "prolite", wantTier: "codex_prolite_10x", wantError: "transport_error"},
		{name: "provider rejection", accountPlan: "promax", status: 401, wantPlan: "promax", wantTier: "codex_promax_50x", wantError: "auth_expired"},
		{name: "observed business plan overrides stored personal plan", accountPlan: "pro", status: 200, body: `{"plan_type":"self_serve_business_prolite"}`, wantPlan: "self_serve_business_prolite", wantTier: "codex_self_serve_business_prolite_5x"},
		{name: "observed tier overrides stored plan", accountPlan: "promax", status: 200, body: `{"plan_type":"pro","rate_limit_tier":"provider_7x"}`, wantPlan: "pro", wantTier: "provider_7x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &Provider{client: providerDoerFunc(func(*http.Request) (*http.Response, error) {
				if tt.offline {
					return nil, errors.New("offline")
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
			})}
			got := p.fetchAccount(context.Background(), CodexAccount{AccessToken: "test-token", PlanType: tt.accountPlan})
			if got.Plan != tt.wantPlan || got.RateLimitTier != tt.wantTier {
				t.Fatalf("plan/tier = %q/%q, want %q/%q", got.Plan, got.RateLimitTier, tt.wantPlan, tt.wantTier)
			}
			if tt.wantError != "" && (got.Error == nil || got.Error.Code != tt.wantError) {
				t.Fatalf("error = %+v, want %q", got.Error, tt.wantError)
			}
		})
	}
}

func TestParseUsageCanonicalProUsageAggregate(t *testing.T) {
	oldNow := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = oldNow })
	var results []quota.Result
	for _, plan := range []string{"prolite", "pro", "promax", "self_serve_business_prolite"} {
		body, err := json.Marshal(map[string]any{
			"plan_type": plan,
			"rate_limit": map[string]any{"primary_window": map[string]any{
				"used_percent": 10, "limit_window_seconds": 18000, "reset_at": 18000,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, parseUsage(body, plan+"@example.com", plan))
	}
	_, summary := aggregate.Compute(results, 1000, "codex", nil)
	if summary == nil || summary.TotalMulti != 85 || summary.Label != "1 × pro 10x + 1 × pro 20x + 1 × pro 50x + 1 × business premium 5x = 85x" {
		t.Fatalf("summary = %+v, want 10x + 20x + 50x + 5x usage = 85x", summary)
	}
}

func TestParseUsagePreservesRawPlanMetadata(t *testing.T) {
	got := parseUsage([]byte(`{"plan_type":"self_serve_business_prolite"}`), "member@example.com", "account")
	if got.Plan != "self_serve_business_prolite" {
		t.Fatalf("plan = %q, want raw upstream plan", got.Plan)
	}
}

func TestParseUsageExplicitRateLimitTierWins(t *testing.T) {
	for _, tt := range []struct{ plan, tier string }{
		{"pro", "codex_pro_7x"},
		{"prolite", "codex_pro_20x"},
		{"self_serve_business_prolite", "custom_business_8x"},
		{"self_serve_business_promax", "custom_business_12x"},
		{"future_plan", "future_tier"},
	} {
		t.Run(tt.plan, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"plan_type": tt.plan, "rate_limit_tier": tt.tier})
			if err != nil {
				t.Fatal(err)
			}
			got := parseUsage(body, "member@example.com", "account")
			if got.RateLimitTier != tt.tier || got.Plan != tt.plan {
				t.Fatalf("plan/tier = %q/%q, want raw %q/%q", got.Plan, got.RateLimitTier, tt.plan, tt.tier)
			}
		})
	}
}

func TestAccountsDiscoverPlanLabels(t *testing.T) {
	for _, tt := range []struct{ plan, label, tier string }{
		{"prolite", "prolite", "codex_prolite_10x"},
		{"pro", "pro", "codex_pro_20x"},
		{"promax", "promax", "codex_promax_50x"},
		{"self_serve_business_prolite", "self_serve_business_prolite", "codex_self_serve_business_prolite_5x"},
		{"self_serve_business_promax", "self_serve_business_promax", ""},
	} {
		t.Run(tt.plan, func(t *testing.T) {
			fs := newFakeFS()
			fs.files["/fake/home/.codex/auth.json"] = codexAuthJSON("access", "account", fakeCodexJWT("member@example.com", "account", "user", tt.plan))
			manager := &Accounts{FS: fs, Now: func() time.Time {
				return time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC)
			}}
			accounts, err := manager.Discover(context.Background())
			if err != nil || len(accounts) != 1 {
				t.Fatalf("accounts = %v, error = %v", accounts, err)
			}
			if got := accounts[0]; got.Label != tt.label || got.RateLimitTier != tt.tier {
				t.Fatalf("label/tier = %q/%q, want %q/%q", got.Label, got.RateLimitTier, tt.label, tt.tier)
			}
		})
	}
}

func TestParseUsagePlanCatalogue(t *testing.T) {
	oldNow := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = oldNow })
	for _, tt := range []struct {
		plan, label, tier string
	}{
		{"free", "free", ""},
		{"go", "go", ""},
		{"plus", "plus", ""},
		{"prolite", "pro", "codex_prolite_10x"},
		{"pro", "pro", "codex_pro_20x"},
		{"promax", "pro", "codex_promax_50x"},
		{"team", "business", ""},
		{"self_serve_business_prolite", "business premium", "codex_self_serve_business_prolite_5x"},
		{"self_serve_business_usage_based", "business", ""},
		{"business", "enterprise", ""},
		{"ent26", "enterprise", ""},
		{"enterprise_cbp_automation", "enterprise (automation)", ""},
		{"enterprise_cbp_usage_based", "enterprise", ""},
		{"enterprise", "enterprise", ""},
		{"hc", "enterprise", ""},
		{"education", "edu", ""},
		{"edu", "edu", ""},
		{"edu_plus", "edu plus", ""},
		{"edu_pro", "edu pro", ""},
		{"sci", "academic researchers", ""},
		{"sci_plus", "academic researchers plus", ""},
		{"guest", "guest", ""},
		{"free_workspace", "free_workspace", ""},
		{"quorum", "quorum", ""},
		{"k12", "k12", ""},
		{"unknown", "unknown", ""},
		{"", "unknown", ""},
		{"future_plan", "future_plan", ""},
		{"self_serve_business_pro", "self_serve_business_pro", ""},
		{"self_serve_business_promax", "self_serve_business_promax", ""},
		{"ProLite", "pro", "codex_prolite_10x"},
	} {
		t.Run(tt.plan, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"plan_type": tt.plan,
				"rate_limit": map[string]any{"primary_window": map[string]any{
					"used_percent": 10, "limit_window_seconds": 604800,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := parseUsage(body, "member@example.com", "account")
			rawPlan := tt.plan
			if rawPlan == "" {
				rawPlan = "unknown"
			}
			if got.Status != quota.StatusOK || got.Plan != rawPlan || got.RateLimitTier != tt.tier {
				t.Fatalf("status/plan/tier = %q/%q/%q, want ok/%q/%q", got.Status, got.Plan, got.RateLimitTier, rawPlan, tt.tier)
			}
			if label := provider.CodexPlanLabel(got.Plan); label != tt.label {
				t.Fatalf("label = %q, want %q", label, tt.label)
			}
		})
	}
}

func TestParseUsagePlanCatalogueRejectsInvalidPlan(t *testing.T) {
	for _, body := range []string{`{"plan_type":42}`, `{"plan_type":"pro","rate_limit_tier":42}`} {
		got := parseUsage([]byte(body), "member@example.com", "account")
		if got.Error == nil || got.Error.Code != "parse_error" || got.Plan != "" || got.RateLimitTier != "" {
			t.Fatalf("invalid plan result = %+v", got)
		}
	}
}

func TestRateLimitTierOctoberIncrease(t *testing.T) {
	for _, tt := range []struct {
		plan, date, want string
	}{
		{"prolite", "2026-10-28T23:59:59Z", "codex_prolite_5x"},
		{"prolite", "2026-10-30T00:00:00Z", "codex_prolite_10x"},
		{"pro", "2026-10-30T00:00:00Z", "codex_pro_20x"},
		{"promax", "2026-10-28T23:59:59Z", "codex_promax_25x"},
		{"promax", "2026-10-30T00:00:00Z", "codex_promax_50x"},
		{"self_serve_business_prolite", "2026-10-30T00:00:00Z", "codex_self_serve_business_prolite_5x"},
	} {
		t.Run(tt.plan+tt.date, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.date)
			if err != nil {
				t.Fatal(err)
			}
			if got := rateLimitTierForPlan(tt.plan, now); got != tt.want {
				t.Fatalf("tier = %q, want %q", got, tt.want)
			}
		})
	}
}
