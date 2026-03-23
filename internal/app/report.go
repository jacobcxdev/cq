package app

import (
	"context"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/aggregate"
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
	Age(ctx context.Context, id string) (time.Duration, bool)
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

// buildReport is a pure function that assembles a Report from fetched results.
// Any provider with 2+ usable results gets aggregate computation.
func buildReport(now time.Time, ordered []provider.ID, fetched map[provider.ID][]quota.Result) Report {
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
		if windows, summary := aggregate.Compute(results, now.Unix()); len(windows) > 0 && summary != nil {
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
