package app

import (
	"context"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func capitalise(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

type Clock interface {
	Now() time.Time
}

type Cache interface {
	Get(ctx context.Context, id string) ([]quota.Result, bool, error)
	Put(ctx context.Context, id string, results []quota.Result) error
	Delete(ctx context.Context, id string) error
	Age(ctx context.Context, id string) (time.Duration, bool)
}

// History abstracts the persistent burn-rate store so the Runner can be
// tested without touching the filesystem. Nil-safe: a nil History causes the
// runner to skip rate computation, and the gauge cold-starts (GaugePos = -1).
type History interface {
	UpdateAndGetBurnRates(ctx context.Context, results map[string][]quota.Result, nowEpoch int64) (history.BurnRates, error)
}

type Renderer interface {
	Render(ctx context.Context, report Report) error
}

type RunRequest struct {
	Providers []provider.ID
	Refresh   bool
}

type Report struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Providers   []ProviderReport `json:"providers"`
}

type ProviderReport struct {
	ID               provider.ID             `json:"id"`
	Name             string                  `json:"name"`
	Availability     ProviderAvailability    `json:"availability"`
	Results          []quota.Result          `json:"results"`
	Aggregate        *AggregateReport        `json:"aggregate,omitempty"`
	ProxyEligibility *ProxyEligibilityReport `json:"proxy_eligibility,omitempty"`
	ProxyPools       []ProxyPoolReport       `json:"proxy_pools,omitempty"`
}

type ProviderAvailabilityState string

const (
	ProviderAvailabilityAvailable ProviderAvailabilityState = "available"
	ProviderAvailabilityLimited   ProviderAvailabilityState = "limited"
	ProviderAvailabilityExhausted ProviderAvailabilityState = "exhausted"
)

type ProviderAvailability struct {
	State           ProviderAvailabilityState `json:"state"`
	Guidance        string                    `json:"guidance"`
	Reason          string                    `json:"reason"`
	MinRemainingPct int                       `json:"min_remaining_pct"`
	ResetsInSeconds int64                     `json:"resets_in_s,omitempty"`
}

type AggregateReport struct {
	ProviderID provider.ID                                `json:"provider_id"`
	Kind       string                                     `json:"kind"`
	Summary    aggregate.AccountSummary                   `json:"summary"`
	Windows    map[quota.WindowName]quota.AggregateResult `json:"windows"`
}

type ProxyEligibilityReport struct {
	DiscoveredCount int              `json:"discovered_count"`
	EligibleCount   int              `json:"eligible_count"`
	ExcludedCount   int              `json:"excluded_count"`
	Aggregate       *AggregateReport `json:"aggregate,omitempty"`
}

type ProxyPoolReport struct {
	Name string `json:"name"`
	ProxyEligibilityReport
}

const providerLimitedThresholdPct = 5

func providerAvailability(results []quota.Result, now time.Time) ProviderAvailability {
	best := ProviderAvailability{
		State:           ProviderAvailabilityExhausted,
		Guidance:        "Provider cannot currently be assessed or used because all results are errors.",
		Reason:          "unavailable",
		MinRemainingPct: -1,
	}
	foundUsable := false
	for _, result := range results {
		if !result.IsUsable() {
			continue
		}
		minPct := result.MinRemainingPct()
		if result.Status == quota.StatusExhausted {
			minPct = 0
		}
		resetIn := resetHorizonSeconds(result.Windows, minPct, now)
		if result.Status == quota.StatusExhausted && resetIn == 0 {
			resetIn = soonestResetHorizonSeconds(result.Windows, now)
		}
		availability := availabilityForMargin(minPct, resetIn)
		if !foundUsable || availabilityRank(availability.State) > availabilityRank(best.State) {
			best = availability
			foundUsable = true
		}
	}
	return best
}

func availabilityForMargin(minPct int, resetIn int64) ProviderAvailability {
	if minPct < 0 {
		return ProviderAvailability{
			State:           ProviderAvailabilityAvailable,
			Guidance:        "Provider is available for normal work. Quota margin is currently unknown.",
			Reason:          "unknown_quota",
			MinRemainingPct: -1,
			ResetsInSeconds: resetIn,
		}
	}
	if minPct == 0 {
		return ProviderAvailability{
			State:           ProviderAvailabilityExhausted,
			Guidance:        guidanceWithReset("Provider is exhausted or unavailable for new work. Do not select it unless the user explicitly overrides this decision.", resetIn),
			Reason:          "exhausted_quota",
			MinRemainingPct: 0,
			ResetsInSeconds: resetIn,
		}
	}
	if minPct <= providerLimitedThresholdPct {
		return ProviderAvailability{
			State:           ProviderAvailabilityLimited,
			Guidance:        guidanceWithReset("Provider is available but quota is low. Use only for small, necessary, or user-approved work; prefer another available provider for broad exploration or verification.", resetIn),
			Reason:          "low_remaining_quota",
			MinRemainingPct: minPct,
			ResetsInSeconds: resetIn,
		}
	}
	return ProviderAvailability{
		State:           ProviderAvailabilityAvailable,
		Guidance:        guidanceWithReset("Provider is available for normal work.", resetIn),
		Reason:          "healthy_quota",
		MinRemainingPct: minPct,
		ResetsInSeconds: resetIn,
	}
}

func availabilityRank(state ProviderAvailabilityState) int {
	switch state {
	case ProviderAvailabilityAvailable:
		return 3
	case ProviderAvailabilityLimited:
		return 2
	case ProviderAvailabilityExhausted:
		return 1
	default:
		return 0
	}
}

func resetHorizonSeconds(windows map[quota.WindowName]quota.Window, minPct int, now time.Time) int64 {
	if minPct < 0 {
		return 0
	}
	var soonest int64
	for _, window := range windows {
		if window.RemainingPct != minPct || window.ResetAtUnix <= 0 {
			continue
		}
		resetIn := max(window.ResetAtUnix-now.Unix(), 0)
		if soonest == 0 || resetIn < soonest {
			soonest = resetIn
		}
	}
	return soonest
}

func soonestResetHorizonSeconds(windows map[quota.WindowName]quota.Window, now time.Time) int64 {
	var soonest int64
	for _, window := range windows {
		if window.ResetAtUnix <= 0 {
			continue
		}
		resetIn := max(window.ResetAtUnix-now.Unix(), 0)
		if soonest == 0 || resetIn < soonest {
			soonest = resetIn
		}
	}
	return soonest
}

func guidanceWithReset(guidance string, resetIn int64) string {
	if resetIn <= 0 {
		return guidance
	}
	minutes := (resetIn + 59) / 60
	return guidance + " The limiting window resets in about " + strconv.FormatInt(minutes, 10) + " minutes."
}

func providerFetched(fetched map[provider.ID][]quota.Result) map[string][]quota.Result {
	out := make(map[string][]quota.Result, len(fetched))
	for id, results := range fetched {
		out[string(id)] = results
	}
	return out
}

// buildReport is a pure function that assembles a Report from fetched results.
// Any provider with 2+ usable results gets aggregate computation.
func buildReport(now time.Time, ordered []provider.ID, fetched map[provider.ID][]quota.Result, burnRates history.BurnRates) Report {
	report := Report{
		GeneratedAt: now,
		Providers:   make([]ProviderReport, 0, len(ordered)),
	}
	for _, id := range ordered {
		results := fetched[id]
		pr := ProviderReport{
			ID:           id,
			Name:         capitalise(string(id)),
			Availability: providerAvailability(results, now),
			Results:      results,
		}
		if windows, summary := aggregate.Compute(results, now.Unix(), string(id), burnRates); len(windows) > 0 && summary != nil {
			pr.Aggregate = &AggregateReport{
				ProviderID: id,
				Kind:       "weighted_pace",
				Summary:    *summary,
				Windows:    windows,
			}
		}
		report.Providers = append(report.Providers, pr)
	}
	return report
}

// AddProxyEligibility adds a proxy-scoped capacity view without changing the
// provider-wide aggregate or the configured routing allowlist.
func AddProxyEligibility(report *Report, id provider.ID, eligible func(quota.Result) bool) {
	if report == nil || eligible == nil {
		return
	}
	for i := range report.Providers {
		pr := &report.Providers[i]
		if pr.ID != id {
			continue
		}
		eligibility := buildProxyEligibilityReport(pr.Results, report.GeneratedAt, id, "proxy_eligible_weighted_pace", eligible)
		pr.ProxyEligibility = &eligibility
		return
	}
}

// AddProxyPool adds one named, bound routing-pool capacity view when it is a
// strict subset of the provider's discovered accounts.
func AddProxyPool(report *Report, id provider.ID, name string, eligible func(quota.Result) bool) {
	if report == nil || name == "" || eligible == nil {
		return
	}
	for i := range report.Providers {
		pr := &report.Providers[i]
		if pr.ID != id {
			continue
		}
		eligibility := buildProxyEligibilityReport(pr.Results, report.GeneratedAt, id, "proxy_pool_weighted_pace", eligible)
		if eligibility.ExcludedCount == 0 {
			return
		}
		pr.ProxyPools = append(pr.ProxyPools, ProxyPoolReport{
			Name:                   name,
			ProxyEligibilityReport: eligibility,
		})
		return
	}
}

func buildProxyEligibilityReport(results []quota.Result, generatedAt time.Time, id provider.ID, kind string, eligible func(quota.Result) bool) ProxyEligibilityReport {
	eligibleResults := make([]quota.Result, 0, len(results))
	for _, result := range results {
		if eligible(result) {
			eligibleResults = append(eligibleResults, result)
		}
	}
	report := ProxyEligibilityReport{
		DiscoveredCount: len(results),
		EligibleCount:   len(eligibleResults),
		ExcludedCount:   len(results) - len(eligibleResults),
	}
	if windows, summary := aggregate.Compute(eligibleResults, generatedAt.Unix(), string(id), nil); len(windows) > 0 && summary != nil {
		report.Aggregate = &AggregateReport{
			ProviderID: id,
			Kind:       kind,
			Summary:    *summary,
			Windows:    windows,
		}
	}
	return report
}
