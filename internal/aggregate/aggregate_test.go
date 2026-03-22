package aggregate

import (
	"math"
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestComputeFiltersWeeklyExhaustedFrom5h(t *testing.T) {
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_20x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 2, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 71, ResetAtUnix: now + 302_400},
			},
		},
		{
			Status:        quota.StatusExhausted,
			RateLimitTier: "default_claude_max_20x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 302_400},
			},
		},
	}

	agg, summary := Compute(results, now)
	if agg == nil || summary == nil {
		t.Fatal("expected aggregate and summary")
	}

	if got := agg[quota.Window5Hour].RemainingPct; got != 2 {
		t.Fatalf("5h remaining = %d, want 2", got)
	}
	if got := agg[quota.Window7Day].RemainingPct; got != 36 {
		t.Fatalf("7d remaining = %d, want 36", got)
	}
}

func TestComputeIncludesAccountsMissing7dIn5h(t *testing.T) {
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_20x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 40, ResetAtUnix: now + 9_000},
			},
		},
		{
			Status:        quota.StatusExhausted,
			RateLimitTier: "default_claude_max_20x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 302_400},
			},
		},
	}

	agg, _ := Compute(results, now)
	if got := agg[quota.Window5Hour].RemainingPct; got != 40 {
		t.Fatalf("5h remaining = %d, want 40", got)
	}
}

func TestComputeZeroes5hWhenAllWeeklyExhausted(t *testing.T) {
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusExhausted,
			RateLimitTier: "default_claude_max_20x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 30, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 302_400},
			},
		},
		{
			Status:        quota.StatusExhausted,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 302_400},
			},
		},
	}

	agg, _ := Compute(results, now)
	if got := agg[quota.Window5Hour].RemainingPct; got != 0 {
		t.Fatalf("5h remaining = %d, want 0", got)
	}
	if got := agg[quota.Window5Hour].ExpectedPct; got != 0 {
		t.Fatalf("5h expected = %d, want 0", got)
	}
	if got := agg[quota.Window5Hour].Burndown; got != 0 {
		t.Fatalf("5h burndown = %d, want 0", got)
	}
}

func TestComputeFiltersWeeklyExhaustedFrom5hBurndown(t *testing.T) {
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_20x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 10, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 50, ResetAtUnix: now + 302_400},
			},
		},
		{
			Status:        quota.StatusExhausted,
			RateLimitTier: "default_claude_max_20x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 302_400},
			},
		},
	}

	agg, _ := Compute(results, now)
	if got := agg[quota.Window5Hour].Burndown; got != 1000 {
		t.Fatalf("5h burndown = %d, want 1000", got)
	}
}

func TestComputeSustainabilityReturnsZeroWhenAllDepleted(t *testing.T) {
	now := int64(1_000)
	accounts := []acctInfo{
		{
			result: quota.Result{
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: now + 1_000},
				},
			},
		},
		{
			result: quota.Result{
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: now + 2_000},
				},
			},
		},
	}

	if got := computeSustainability(accounts, quota.Window5Hour, 18_000, now); got != 0 {
		t.Fatalf("sustainability = %v, want 0", got)
	}
}

func TestComputeSustainabilityFallbackRateWhenAllElapsedZero(t *testing.T) {
	now := int64(1_000)
	accounts := []acctInfo{
		{
			result: quota.Result{
				Windows: map[quota.WindowName]quota.Window{
					// elapsed=0 (reset is full period away), 20% used
					quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 18_000},
				},
			},
		},
		{
			result: quota.Result{
				Windows: map[quota.WindowName]quota.Window{
					// elapsed=0, 60% used
					quota.Window5Hour: {RemainingPct: 40, ResetAtUnix: now + 18_000},
				},
			},
		},
	}

	// With the fallback rate (used/period), these accounts have finite rates
	// and should produce a positive sustainability, not -1 (unknown).
	got := computeSustainability(accounts, quota.Window5Hour, 18_000, now)
	if got <= 0 {
		t.Fatalf("sustainability = %v, want > 0 (fallback rate should produce finite result)", got)
	}
}

func TestComputeSustainabilityCapsInfiniteWhenRateZero(t *testing.T) {
	now := int64(1_000)
	accounts := []acctInfo{
		{
			result: quota.Result{
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 9_000},
				},
			},
		},
	}

	if got := computeSustainability(accounts, quota.Window5Hour, 18_000, now); got != 100 {
		t.Fatalf("sustainability = %v, want 100", got)
	}
}

func TestWindowElapsed(t *testing.T) {
	period := int64(18_000)
	now := int64(1_000)

	tests := []struct {
		name      string
		w         quota.Window
		want      float64
	}{
		{"zero ResetAtUnix", quota.Window{ResetAtUnix: 0, RemainingPct: 50}, 0},
		{"future reset, elapsed negative clamped to 0", quota.Window{ResetAtUnix: now + period + 500, RemainingPct: 50}, 0},
		{"elapsed exceeds period clamped to period", quota.Window{ResetAtUnix: now - 1, RemainingPct: 50}, float64(period)},
		{"normal elapsed", quota.Window{ResetAtUnix: now + 9_000, RemainingPct: 50}, float64(period - 9_000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := windowElapsed(tt.w, period, now)
			if got != tt.want {
				t.Errorf("windowElapsed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCoversPeriod(t *testing.T) {
	tests := []struct {
		name      string
		intervals []interval
		period    float64
		want      bool
	}{
		{"empty intervals", nil, 100, false},
		{"zero period", []interval{{0, 50}}, 0, false},
		{"gap in middle", []interval{{0, 40}, {60, 100}}, 100, false},
		{"exact coverage", []interval{{0, 100}}, 100, true},
		{"overlapping covers", []interval{{0, 60}, {50, 100}}, 100, true},
		{"single short", []interval{{0, 50}}, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coversPeriod(tt.intervals, tt.period)
			if got != tt.want {
				t.Errorf("coversPeriod() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractMultiplier(t *testing.T) {
	tests := []struct {
		tier string
		want int
	}{
		{"default_claude_max_20x", 20},
		{"default_claude_max_1x", 1},
		{"default_claude_max_5x", 5},
		{"no_suffix_here", 1},
		{"", 1},
		{"invalid_0x", 1}, // n <= 0 → fallback to 1
	}
	for _, tt := range tests {
		t.Run(tt.tier, func(t *testing.T) {
			got := quota.ExtractMultiplier(tt.tier)
			if got != tt.want {
				t.Errorf("ExtractMultiplier(%q) = %d, want %d", tt.tier, got, tt.want)
			}
		})
	}
}

func TestComputeSustainabilityAccountsForPostResetDryGap(t *testing.T) {
	now := int64(1_000)
	accounts := []acctInfo{
		{
			result: quota.Result{
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 20, ResetAtUnix: now + 3_600},
				},
			},
		},
		{
			result: quota.Result{
				Windows: map[quota.WindowName]quota.Window{
					quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 16_200},
				},
			},
		},
	}

	got := computeSustainability(accounts, quota.Window5Hour, 18_000, now)
	want := 10.0 / 7.0
	if math.Abs(got-want) > 0.02 {
		t.Fatalf("sustainability = %.2f, want %.2f", got, want)
	}
}

func TestComputeDisjointWindows(t *testing.T) {
	// Account A has only Window5Hour; Account B has only Window7Day.
	// Both windows have exactly one contributing account each, so agg is non-nil.
	// Compute only returns nil when < 2 usable results.
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
			},
		},
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 50, ResetAtUnix: now + 302_400},
			},
		},
	}

	agg, summary := Compute(results, now)
	if agg == nil {
		t.Fatal("expected non-nil aggregate for disjoint windows with 2 usable results")
	}
	if len(agg) != 2 {
		t.Errorf("expected 2 aggregate windows, got %d", len(agg))
	}
	if summary == nil {
		t.Fatal("expected non-nil summary")
	}
}

func TestComputeDisjointWindowsBothAccountsSameWindow(t *testing.T) {
	// When all valid accounts share NO common window name, each window only has
	// one account contributing but that's still >= 1 weight. Verify Compute
	// doesn't return nil in this scenario (it should return results for each
	// window that has at least one contributing account).
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 40, ResetAtUnix: now + 9_000},
			},
		},
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 60, ResetAtUnix: now + 9_000},
			},
		},
		// Third account has ONLY Window7Day — disjoint from the 5h pair but
		// the 5h pair itself should still aggregate fine.
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 80, ResetAtUnix: now + 302_400},
			},
		},
	}

	agg, summary := Compute(results, now)
	if agg == nil || summary == nil {
		t.Fatal("expected non-nil aggregate when at least one window has >= 1 contributing account")
	}
	if _, ok := agg[quota.Window5Hour]; !ok {
		t.Error("expected Window5Hour in aggregate")
	}
}

func TestComputeSustainabilityInAggregateResult(t *testing.T) {
	// Two accounts both at 100% with future resets → sustainability should be
	// positive (rate of consumption is zero, so sustainability is capped high).
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 100, ResetAtUnix: now + 302_400},
			},
		},
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 100, ResetAtUnix: now + 302_400},
			},
		},
	}

	agg, summary := Compute(results, now)
	if agg == nil || summary == nil {
		t.Fatal("expected non-nil aggregate")
	}
	w5h, ok := agg[quota.Window5Hour]
	if !ok {
		t.Fatal("expected Window5Hour in aggregate")
	}
	// Both accounts at 100% with zero consumption → sustainability is capped at 100.
	if w5h.Sustainability != 100 {
		t.Errorf("Sustainability = %v, want 100 (zero-rate capped)", w5h.Sustainability)
	}
	// Zero-rate accounts with full remaining → severe underburn (position 6)
	// since all quota will be wasted at current (zero) rate. However, near-zero
	// rate accounts are skipped in projectedWaste, so waste=0 → on pace (3).
	if w5h.GaugePos != 3 {
		t.Errorf("GaugePos = %d, want 3 (unused accounts → on pace)", w5h.GaugePos)
	}
}

func TestComputeRequiresTwoUsableResults(t *testing.T) {
	// 3 results, only 1 is usable (status "ok"). Compute requires >= 2 usable.
	now := int64(1_000)
	results := []quota.Result{
		{
			Status:        quota.StatusOK,
			RateLimitTier: "default_claude_max_1x",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
				quota.Window7Day:  {RemainingPct: 50, ResetAtUnix: now + 302_400},
			},
		},
		{
			Status: quota.StatusError,
		},
		{
			Status: quota.StatusError,
		},
	}

	agg, summary := Compute(results, now)
	if agg != nil || summary != nil {
		t.Error("expected nil aggregate when fewer than 2 usable results")
	}
}

func TestBuildLabel(t *testing.T) {
	tests := []struct {
		name    string
		results []quota.Result
		want    string
	}{
		{
			name: "single account no multiplier",
			results: []quota.Result{
				{Status: quota.StatusOK, Plan: "pro", RateLimitTier: "default_claude_max_1x"},
			},
			want: "1 × pro",
		},
		{
			name: "multiple accounts same plan",
			results: []quota.Result{
				{Status: quota.StatusOK, Plan: "max", RateLimitTier: "default_claude_max_1x"},
				{Status: quota.StatusOK, Plan: "max", RateLimitTier: "default_claude_max_1x"},
			},
			want: "2 × max = 2x",
		},
		{
			name: "mixed plans",
			results: []quota.Result{
				{Status: quota.StatusOK, Plan: "max", RateLimitTier: "default_claude_max_20x"},
				{Status: quota.StatusOK, Plan: "pro", RateLimitTier: "default_claude_max_1x"},
			},
			want: "1 × max 20x + 1 × pro = 21x",
		},
		{
			name:    "empty group no usable results",
			results: []quota.Result{},
			want:    "",
		},
		{
			name: "single account with multiplier",
			results: []quota.Result{
				{Status: quota.StatusOK, Plan: "max", RateLimitTier: "default_claude_max_20x"},
			},
			want: "1 × max 20x = 20x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildLabel(tt.results)
			if got != tt.want {
				t.Errorf("BuildLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
