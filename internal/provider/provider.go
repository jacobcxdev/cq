package provider

import (
	"context"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// ID identifies a provider.
type ID string

const (
	Claude ID = "claude"
	Codex  ID = "codex"
	Gemini ID = "gemini"
)

// Provider fetches quota information for an AI service.
type Provider interface {
	Fetch(ctx context.Context, now time.Time) ([]quota.Result, error)
}

// Discoverer can enumerate local accounts without making network calls.
// Providers that support it allow the runner to synthesise auth_expired rows
// for accounts that are locally known but not in the cache.
type Discoverer interface {
	DiscoverAccounts(ctx context.Context) ([]Account, error)
}

type Account struct {
	AccountID     string `json:"id"`
	Email         string `json:"email,omitempty"`
	Label         string `json:"label,omitempty"`
	RateLimitTier string `json:"rate_limit_tier,omitempty"`
	Active        bool   `json:"active"`
	SwitchID      string `json:"switch_id,omitempty"`
}

type AccountManager interface {
	ProviderID() ID
	Discover(ctx context.Context) ([]Account, error)
	Switch(ctx context.Context, identifier string) (Account, error)
	Remove(ctx context.Context, identifier string) error
}

// Services groups the service implementations for a provider.
type Services struct {
	Usage Provider
}

// Observation receives safe diagnostics and completed quota rows for a bounded
// interactive check. Callbacks must be concurrency-safe. Fetch retains ownership
// of its workers; cancelling the observer never closes resources under them.
type Observation struct {
	Results func([]quota.Result)
	Warning func(code, message string)
}
type observationKey struct{}

func WithObservation(ctx context.Context, observation Observation) context.Context {
	return context.WithValue(ctx, observationKey{}, observation)
}
func Observed(ctx context.Context) bool {
	_, ok := ctx.Value(observationKey{}).(Observation)
	return ok
}
func WithResultObserver(ctx context.Context, results func([]quota.Result)) context.Context {
	observation, _ := ctx.Value(observationKey{}).(Observation)
	observation.Results = results
	return WithObservation(ctx, observation)
}
func ObserveResults(ctx context.Context, results []quota.Result) {
	observation, _ := ctx.Value(observationKey{}).(Observation)
	if observation.Results != nil {
		observation.Results(CloneResults(results))
	}
}

// ObserveWarning returns true when a structured observer owns diagnostics.
// Callers must supply fixed safe text, never upstream errors or panic values.
func ObserveWarning(ctx context.Context, code, message string) bool {
	observation, ok := ctx.Value(observationKey{}).(Observation)
	if ok && observation.Warning != nil {
		observation.Warning(code, message)
	}
	return ok
}

// CloneResults freezes a completed snapshot, including mutable window maps and
// optional pointers, before handing it to another goroutine.
func CloneResults(results []quota.Result) []quota.Result {
	out := make([]quota.Result, len(results))
	for i, row := range results {
		out[i] = row
		if row.Error != nil {
			value := *row.Error
			out[i].Error = &value
		}
		if row.Windows != nil {
			out[i].Windows = make(map[quota.WindowName]quota.Window, len(row.Windows))
			for name, window := range row.Windows {
				if window.RemainingPctExact != nil {
					value := *window.RemainingPctExact
					window.RemainingPctExact = &value
				}
				out[i].Windows[name] = window
			}
		}
	}
	return out
}
