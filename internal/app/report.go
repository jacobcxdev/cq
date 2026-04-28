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
	UpdateAndGetBurnRates(ctx context.Context, results []quota.Result, nowEpoch int64) (history.BurnRates, error)
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
	ID           provider.ID          `json:"id"`
	Name         string               `json:"name"`
	Availability ProviderAvailability `json:"availability"`
	Results      []quota.Result       `json:"results"`
	Aggregate    *AggregateReport     `json:"aggregate,omitempty"`
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

// flattenFetched returns all results from the fetched map in a single slice,
// for the history store to process in one pass.
func flattenFetched(fetched map[provider.ID][]quota.Result) []quota.Result {
	var out []quota.Result
	for _, results := range fetched {
		out = append(out, results...)
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
		if windows, summary := aggregate.Compute(results, now.Unix(), burnRates); len(windows) > 0 && summary != nil {
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
