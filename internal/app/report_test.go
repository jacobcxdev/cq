package app

import (
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestBuildReportSingleProvider(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Codex: {{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 1000 + 9000},
		}}},
	}
	r := buildReport(now, []provider.ID{provider.Codex}, fetched, nil)
	if len(r.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(r.Providers))
	}
	if r.Providers[0].Aggregate != nil {
		t.Fatal("single-account provider should not have aggregate")
	}
}

func TestBuildReportClaudeAggregate(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {
			{Status: quota.StatusOK, RateLimitTier: "default_claude_max_20x",
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: 1000 + 9000},
				}},
			{Status: quota.StatusOK, RateLimitTier: "default_claude_max_20x",
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 1000 + 9000},
				}},
		},
	}
	r := buildReport(now, []provider.ID{provider.Claude}, fetched, nil)
	if r.Providers[0].Aggregate == nil {
		t.Fatal("Claude with 2 accounts should have aggregate")
	}
	if len(r.Providers[0].Aggregate.Windows) == 0 {
		t.Fatal("aggregate should have windows")
	}
}

func TestAddProxyEligibilityKeepsDiscoveredAndEligibleAggregatesDistinct(t *testing.T) {
	now := time.Unix(1_000, 0)
	results := []quota.Result{
		{Status: quota.StatusOK, AccountID: "excluded", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{
			quota.Window7Day: {RemainingPct: 97, ResetAtUnix: now.Unix() + 302_400},
		}},
		{Status: quota.StatusOK, AccountID: "eligible-a", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{
			quota.Window7Day: {RemainingPct: 54, ResetAtUnix: now.Unix() + 302_400},
		}},
		{Status: quota.StatusOK, AccountID: "eligible-b", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{
			quota.Window7Day: {RemainingPct: 55, ResetAtUnix: now.Unix() + 302_400},
		}},
	}
	report := buildReport(now, []provider.ID{provider.Codex}, map[provider.ID][]quota.Result{provider.Codex: results}, nil)

	AddProxyEligibility(&report, provider.Codex, func(result quota.Result) bool {
		return result.AccountID != "excluded"
	})

	got := report.Providers[0]
	if got.Aggregate.Windows[quota.Window7Day].RemainingPct != 69 {
		t.Fatalf("discovered remaining = %d, want 69", got.Aggregate.Windows[quota.Window7Day].RemainingPct)
	}
	if got.ProxyEligibility == nil {
		t.Fatal("proxy eligibility missing")
	}
	if got.ProxyEligibility.DiscoveredCount != 3 || got.ProxyEligibility.EligibleCount != 2 || got.ProxyEligibility.ExcludedCount != 1 {
		t.Fatalf("proxy eligibility counts = %#v", got.ProxyEligibility)
	}
	if got.ProxyEligibility.Aggregate == nil {
		t.Fatal("proxy-eligible aggregate missing")
	}
	if remaining := got.ProxyEligibility.Aggregate.Windows[quota.Window7Day].RemainingPct; remaining != 55 {
		t.Fatalf("proxy-eligible remaining = %d, want 55", remaining)
	}
}

func TestBuildReportPreservesOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	order := []provider.ID{provider.Gemini, provider.Claude, provider.Codex}
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {{Status: quota.StatusOK}},
		provider.Codex:  {{Status: quota.StatusOK}},
		provider.Gemini: {{Status: quota.StatusOK}},
	}
	r := buildReport(now, order, fetched, nil)
	for i, id := range order {
		if r.Providers[i].ID != id {
			t.Errorf("provider[%d] = %q, want %q", i, r.Providers[i].ID, id)
		}
	}
}

func TestBuildReportNoAggregateForSingleClaude(t *testing.T) {
	now := time.Unix(1000, 0)
	fetched := map[provider.ID][]quota.Result{
		provider.Claude: {{Status: quota.StatusOK}},
	}
	r := buildReport(now, []provider.ID{provider.Claude}, fetched, nil)
	if r.Providers[0].Aggregate != nil {
		t.Fatal("single Claude account should not have aggregate")
	}
}

func TestBuildReportAddsProviderAvailability(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		name         string
		results      []quota.Result
		wantState    ProviderAvailabilityState
		wantReason   string
		wantMinPct   int
		wantResetIn  int64
		wantGuidance string
	}{
		{
			name:         "empty results",
			results:      nil,
			wantState:    ProviderAvailabilityExhausted,
			wantReason:   "unavailable",
			wantMinPct:   -1,
			wantGuidance: "cannot currently be assessed or used",
		},
		{
			name: "available",
			results: []quota.Result{{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 1000 + 9000},
			}}},
			wantState:    ProviderAvailabilityAvailable,
			wantReason:   "healthy_quota",
			wantMinPct:   80,
			wantResetIn:  9000,
			wantGuidance: "available for normal work",
		},
		{
			name: "limited",
			results: []quota.Result{{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
				quota.WindowName("5h:opus"): {RemainingPct: 5, ResetAtUnix: 1000 + 1800},
			}}},
			wantState:    ProviderAvailabilityLimited,
			wantReason:   "low_remaining_quota",
			wantMinPct:   5,
			wantResetIn:  1800,
			wantGuidance: "Use only for small, necessary, or user-approved work",
		},
		{
			name: "exhausted",
			results: []quota.Result{{Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: 1000 + 600},
			}}},
			wantState:    ProviderAvailabilityExhausted,
			wantReason:   "exhausted_quota",
			wantMinPct:   0,
			wantResetIn:  600,
			wantGuidance: "Do not select it",
		},
		{
			name: "exhausted status overrides non-zero windows",
			results: []quota.Result{{Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 3, ResetAtUnix: 1000 + 600},
			}}},
			wantState:    ProviderAvailabilityExhausted,
			wantReason:   "exhausted_quota",
			wantMinPct:   0,
			wantResetIn:  600,
			wantGuidance: "Do not select it",
		},
		{
			name: "error only",
			results: []quota.Result{
				quota.ErrorResult("fetch_error", "boom", 0),
			},
			wantState:    ProviderAvailabilityExhausted,
			wantReason:   "unavailable",
			wantMinPct:   -1,
			wantGuidance: "cannot currently be assessed or used",
		},
		{
			name:         "unknown quota remains available",
			results:      []quota.Result{{Status: quota.StatusOK}},
			wantState:    ProviderAvailabilityAvailable,
			wantReason:   "unknown_quota",
			wantMinPct:   -1,
			wantGuidance: "available for normal work",
		},
		{
			name: "best routeable account beats active exhausted account",
			results: []quota.Result{
				{Active: true, Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: 1000 + 600},
				}},
				{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 40, ResetAtUnix: 1000 + 3600},
				}},
			},
			wantState:    ProviderAvailabilityAvailable,
			wantReason:   "healthy_quota",
			wantMinPct:   40,
			wantResetIn:  3600,
			wantGuidance: "available for normal work",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetched := map[provider.ID][]quota.Result{provider.Codex: tt.results}

			r := buildReport(now, []provider.ID{provider.Codex}, fetched, nil)
			got := r.Providers[0].Availability

			if got.State != tt.wantState {
				t.Fatalf("availability state = %q, want %q", got.State, tt.wantState)
			}
			if got.Reason != tt.wantReason {
				t.Fatalf("availability reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if got.MinRemainingPct != tt.wantMinPct {
				t.Fatalf("min remaining pct = %v, want %v", got.MinRemainingPct, tt.wantMinPct)
			}
			if got.ResetsInSeconds != tt.wantResetIn {
				t.Fatalf("resets in seconds = %d, want %d", got.ResetsInSeconds, tt.wantResetIn)
			}
			if !strings.Contains(got.Guidance, tt.wantGuidance) {
				t.Fatalf("guidance = %q, want to contain %q", got.Guidance, tt.wantGuidance)
			}
		})
	}
}
