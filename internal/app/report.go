package app

import (
	"context"
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
	ID        provider.ID      `json:"id"`
	Name      string           `json:"name"`
	Results   []quota.Result   `json:"results"`
	Aggregate *AggregateReport `json:"aggregate,omitempty"`
}

type AggregateReport struct {
	ProviderID provider.ID                                    `json:"provider_id"`
	Kind       string                                         `json:"kind"`
	Summary    aggregate.AccountSummary                       `json:"summary"`
	Windows    map[quota.WindowName]quota.AggregateResult `json:"windows"`
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
			ID:      id,
			Name:    capitalise(string(id)),
			Results: results,
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
