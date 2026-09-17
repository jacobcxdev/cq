package history

import (
	"context"
	"math"
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

// Samples here go through persistence and the public report snapshot, so a
// broken anchor, confidence gate or restored history changes the forecast.
func TestRecentBurnHistory(t *testing.T) {
	const start = int64(1_000_000)
	const reset = start + 100_000
	type sample struct {
		after    int64
		pct      int
		exact    *float64
		reset    int64
		cacheAge int64
		apiError bool
		want     *float64
	}
	for _, tt := range []struct {
		name    string
		samples []sample
	}{
		{"warmup and no zero seed", []sample{
			{pct: 24},
			{after: 300, pct: 23},
			{after: 600, pct: 22},
			{after: 900, pct: 21, want: recentPointer(1.0 / 300)},
		}},
		{"idle observations remain a known zero rate", []sample{
			{pct: 30},
			{after: 900, pct: 30, want: recentPointer(0)},
		}},
		{"rapid polls retain consumption", []sample{
			{pct: 24},
			{after: 60, pct: 23},
			{after: 120, pct: 23},
			{after: 300, pct: 23},
			{after: 360, pct: 22},
			{after: 600, pct: 22},
			{after: 900, pct: 21, want: recentPointer(1.0 / 300)},
		}},
		{"fractional decline in same displayed percent", []sample{
			{pct: 24, exact: recentPointer(24.5)},
			{after: 900, pct: 24, exact: recentPointer(24.25), want: recentPointer(1.0 / 3600)},
		}},
		{"recent acceleration and idle decay", []sample{
			{pct: 30},
			{after: 1800, pct: 29, want: recentPointer(1.0 / 1800)},
			{after: 3600, pct: 24, want: recentPointer(3.0 / 1800)},
			{after: 5400, pct: 24, want: recentPointer(1.5 / 1800)},
		}},
		{"cached observations cannot create warmup", []sample{
			{pct: 30},
			{after: 900, pct: 27, cacheAge: 1},
		}},
		{"fresh matching cache reuses rate without advancing history", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 1000, pct: 27, cacheAge: 100, want: recentPointer(1.0 / 300)},
			{after: 1801, pct: 27, cacheAge: 901},
		}},
		{"cached snapshot mismatch falls back", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 1000, pct: 28, cacheAge: 100},
		}},
		{"failed cached read cannot use recent rate", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 1000, pct: 27, cacheAge: 100, apiError: true},
		}},
		{"long observation gap starts fresh", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 8101, pct: 20},
			{after: 9001, pct: 19, want: recentPointer(1.0 / 900)},
		}},
		{"reset starts fresh without unwrapping unused capacity", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 1800, pct: 100, reset: reset + 604800},
			{after: 2700, pct: 99, reset: reset + 604800, want: recentPointer(1.0 / 900)},
		}},
		{"same epoch increase starts fresh", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 1800, pct: 100},
			{after: 2700, pct: 99, want: recentPointer(1.0 / 900)},
		}},
		{"precision change starts fresh", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 1800, pct: 27, exact: recentPointer(27.5)},
			{after: 2700, pct: 27, exact: recentPointer(27.25), want: recentPointer(1.0 / 3600)},
		}},
		{"clock reversal cannot alter anchor", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 800, pct: 10},
			{after: 900, pct: 10},
			{after: 1800, pct: 24, want: recentPointer(1.0 / 300)},
		}},
		{"exhaustion clears recent signal", []sample{
			{pct: 30},
			{after: 900, pct: 27, want: recentPointer(1.0 / 300)},
			{after: 1800, pct: 0},
			{after: 2700, pct: 0},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, fs := newTestStore(t)
			for i, observation := range tt.samples {
				epoch := observation.reset
				if epoch == 0 {
					epoch = reset
				}
				result := makeResult("account", map[quota.WindowName]quota.Window{quota.Window7Day: {
					RemainingPct: observation.pct, RemainingPctExact: observation.exact, ResetAtUnix: epoch,
				}})
				result.CacheAge = observation.cacheAge
				if observation.apiError {
					result.Error = &quota.ErrorInfo{Code: "fetch_error"}
				}
				_, estimates, err := store.UpdateAndGetEstimates(context.Background(), map[string][]quota.Result{"codex": {result}}, start+observation.after)
				if err != nil {
					t.Fatal(err)
				}
				estimate, _ := estimates.Get(BurnRateKey{ProviderID: "codex", AccountKey: "account", Window: "7d"})
				got := estimate.RecentRatePctPerS
				if observation.want == nil {
					if got != nil {
						t.Fatalf("sample %d: rate = %g, want unavailable", i, *got)
					}
				} else if got == nil || math.Abs(*got-*observation.want) > 1e-12 {
					t.Fatalf("sample %d: rate = %v, want %g", i, got, *observation.want)
				}
				// Each observation may come from a new CLI process.
				store, err = New(fs, testDir)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func recentPointer(value float64) *float64 { return &value }
