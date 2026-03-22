package aggregate

import (
	"math"
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

// --- firstGapSpan ---

func TestFirstGapSpan(t *testing.T) {
	tests := []struct {
		name      string
		intervals []interval
		period    float64
		wantStart float64
		wantEnd   float64
		wantFound bool
	}{
		{"empty intervals", nil, 100, 0, 100, true},
		{"zero period", []interval{{0, 50}}, 0, 0, 0, false},
		{"full coverage", []interval{{0, 100}}, 100, 0, 0, false},
		{"overlapping full coverage", []interval{{0, 60}, {50, 100}}, 100, 0, 0, false},
		{"gap at start", []interval{{20, 100}}, 100, 0, 20, true},
		{"gap in middle", []interval{{0, 40}, {60, 100}}, 100, 40, 60, true},
		{"gap at end", []interval{{0, 80}}, 100, 80, 100, true},
		{"multiple gaps returns first", []interval{{0, 30}, {50, 70}}, 100, 30, 50, true},
		{"single point gap", []interval{{0, 50}, {50.001, 100}}, 100, 50, 50.001, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, found := firstGapSpan(tt.intervals, tt.period)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if !found {
				return
			}
			if math.Abs(start-tt.wantStart) > 0.01 {
				t.Errorf("gapStart = %.2f, want %.2f", start, tt.wantStart)
			}
			if math.Abs(end-tt.wantEnd) > 0.01 {
				t.Errorf("gapEnd = %.2f, want %.2f", end, tt.wantEnd)
			}
		})
	}
}

// --- buildIntervals ---

func TestBuildIntervals(t *testing.T) {
	period := 18000.0

	t.Run("near-zero rate covers gate to period", func(t *testing.T) {
		sustainers := []sustainAccount{
			{remaining: 100, rate: math.SmallestNonzeroFloat64, reset: period, gateOffset: 5000},
		}
		ivs := buildIntervals(sustainers, 1.0, period)
		if len(ivs) != 1 {
			t.Fatalf("got %d intervals, want 1", len(ivs))
		}
		if ivs[0].start != 5000 || ivs[0].end != period {
			t.Errorf("interval = [%.0f, %.0f], want [5000, %.0f]", ivs[0].start, ivs[0].end, period)
		}
	})

	t.Run("gate offset clips current remaining", func(t *testing.T) {
		sustainers := []sustainAccount{
			{remaining: 50, rate: 0.01, reset: period, elapsed: 9000, gateOffset: 3000},
		}
		ivs := buildIntervals(sustainers, 1.0, period)
		if len(ivs) == 0 {
			t.Fatal("expected at least 1 interval")
		}
		if ivs[0].start != 3000 {
			t.Errorf("interval start = %.0f, want 3000 (gate offset)", ivs[0].start)
		}
	})

	t.Run("post-reset interval respects gate", func(t *testing.T) {
		sustainers := []sustainAccount{
			{remaining: 0, rate: 0.01, reset: 5000, elapsed: 5000, gateOffset: 7000},
		}
		ivs := buildIntervals(sustainers, 1.0, period)
		// No current remaining interval (remaining=0).
		// Post-reset starts at max(reset=5000, gate=7000) = 7000.
		for _, iv := range ivs {
			if iv.start < 7000 {
				t.Errorf("interval starts at %.0f, should be >= 7000 (gate)", iv.start)
			}
		}
	})

	t.Run("two accounts tile the period", func(t *testing.T) {
		sustainers := []sustainAccount{
			{remaining: 50, rate: 50.0 / 9000, reset: 9000, elapsed: 9000},
			{remaining: 50, rate: 50.0 / 9000, reset: 18000, elapsed: 9000},
		}
		ivs := buildIntervals(sustainers, 1.0, period)
		if !coversPeriod(ivs, period) {
			t.Error("expected two accounts to cover the full period")
		}
	})
}

// --- projectedWasteInfo ---

func TestProjectedWasteInfo(t *testing.T) {
	period := 18000.0

	t.Run("no waste when rate consumes all remaining", func(t *testing.T) {
		sustainers := []sustainAccount{
			{remaining: 50, rate: 50.0 / 9000, reset: 9000, elapsed: 9000},
		}
		waste, deadline := projectedWasteInfo(sustainers, period)
		if waste != 0 {
			t.Errorf("waste = %f, want 0", waste)
		}
		if deadline != -1 {
			t.Errorf("deadline = %f, want -1", deadline)
		}
	})

	t.Run("waste when rate is too low", func(t *testing.T) {
		sustainers := []sustainAccount{
			// remaining=80, rate=0.001 (very slow), reset in 5000s, elapsed=13000
			{remaining: 80, rate: 0.001, reset: 5000, elapsed: 13000},
		}
		waste, deadline := projectedWasteInfo(sustainers, period)
		if waste <= 0 {
			t.Error("expected positive waste for slow-burning account")
		}
		if deadline != 5000 {
			t.Errorf("deadline = %f, want 5000 (reset time)", deadline)
		}
	})

	t.Run("near-zero rate accounts skipped", func(t *testing.T) {
		sustainers := []sustainAccount{
			{remaining: 100, rate: math.SmallestNonzeroFloat64, reset: period, elapsed: 0},
		}
		waste, _ := projectedWasteInfo(sustainers, period)
		if waste != 0 {
			t.Errorf("waste = %f, want 0 (near-zero rate should be skipped)", waste)
		}
	})

	t.Run("timing weight scales with elapsed", func(t *testing.T) {
		// Same waste fraction but different elapsed → different urgency.
		earlyAccount := []sustainAccount{
			{remaining: 90, rate: 0.001, reset: 9000, elapsed: 1800}, // 10% into window
		}
		lateAccount := []sustainAccount{
			{remaining: 90, rate: 0.001, reset: 9000, elapsed: 16200}, // 90% into window
		}
		wasteEarly, _ := projectedWasteInfo(earlyAccount, period)
		wasteLate, _ := projectedWasteInfo(lateAccount, period)
		if wasteLate <= wasteEarly {
			t.Errorf("late urgency (%f) should exceed early urgency (%f)", wasteLate, wasteEarly)
		}
	})
}

// --- computeGaugeInfo ---

func TestComputeGaugeInfoUnknown(t *testing.T) {
	now := int64(1_000)
	// No window data at all → unknown.
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, 18_000, now)
	if gi.Pos != -1 {
		t.Errorf("Pos = %d, want -1 (unknown)", gi.Pos)
	}
}

func TestComputeGaugeInfoAllDry(t *testing.T) {
	now := int64(1_000)
	// Both accounts depleted, non-zero elapsed → all dry (position 0).
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: now + 1_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: now + 2_000},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, 18_000, now)
	if gi.Pos != 0 {
		t.Errorf("Pos = %d, want 0 (all dry)", gi.Pos)
	}
	if gi.GapStart < 0 {
		t.Error("expected GapStart >= 0 for all-dry scenario")
	}
}

func TestComputeGaugeInfoSevereOverburn(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Account A nearly depleted, account B far from reset.
	// Coverage gap should appear within 10% of period → position 0.
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 2, ResetAtUnix: now + 16_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: now + 10_000},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	if gi.Pos != 0 {
		t.Errorf("Pos = %d, want 0 (severe overburn)", gi.Pos)
	}
	if gi.GapDuration <= 0 {
		t.Error("expected positive gap duration")
	}
}

func TestComputeGaugeInfoModerateOverburn(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Set up so gap appears at ~20% of period (between 10-25%) → position 1.
	elapsed := float64(period) - 12600 // 12600s remaining, 5400s elapsed
	used := 100.0 - 30.0              // 70% used
	rate := used / elapsed             // ~0.013%/s
	// remaining=30 at rate 0.013 → lasts 30/0.013 ≈ 2307s from now
	// That's 2307/18000 ≈ 12.8% of period → moderate (position 1)
	_ = rate
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 30, ResetAtUnix: now + 12_600},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	if gi.Pos != 1 {
		t.Errorf("Pos = %d, want 1 (moderate overburn)", gi.Pos)
	}
}

func TestComputeGaugeInfoMildOverburn(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Two accounts: A covers [0, 5000] only, B covers [0, 500] + [15000, 18000].
	// Gap starts at 5000 (27.8% of period) → mild overburn (position 2).
	//
	// A: remaining=25, used=75, reset=16000 (elapsed=2000). Rate=75/2000=0.0375.
	//    Current: [0, min(16000, 25/0.0375)] = [0, 666]. After: [16000, 18000].
	// B: remaining=25, used=75, reset=5500 (elapsed=12500). Rate=75/12500=0.006.
	//    Current: [0, min(5500, 25/0.006)] = [0, 4166]. After: [5500, min(18000, 5500+16666)] = [5500, 18000].
	// Union: [0, 4166] + [5500, 18000] + [16000, 18000]. Gap: [4166, 5500].
	// 4166/18000 = 23% → moderate (position 1).
	//
	// Adjust to push gap start past 25%: increase B's coverage.
	// B: remaining=30, used=70, reset=5500 (elapsed=12500). Rate=70/12500=0.0056.
	//    Current: [0, min(5500, 30/0.0056)] = [0, 5357]. After: [5500, 18000].
	// Union: [0, 5357] + [5500, 18000]. Gap: [5357, 5500].
	// 5357/18000 = 29.8% → mild (position 2).
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 25, ResetAtUnix: now + 16_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 30, ResetAtUnix: now + 5_500},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	if gi.Pos != 2 {
		t.Errorf("Pos = %d, want 2 (mild overburn)", gi.Pos)
	}
}

func TestComputeGaugeInfoOnPace(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Two accounts with moderate usage, no gap, low waste → position 3.
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	if gi.Pos != 3 {
		t.Errorf("Pos = %d, want 3 (on pace)", gi.Pos)
	}
}

func TestComputeGaugeInfoUnderburn(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Account with very low usage rate, high remaining, well into window → waste.
	// 95% remaining with only 5% used and 90% of window elapsed.
	// Rate = 5/16200 = 0.000309. Projected remaining at reset = 95 - 0.000309*1800 = 94.4.
	// waste = 94.4/100 * (16200/18000) = 0.85. That's > 25% → severe (position 6).
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 95, ResetAtUnix: now + 1_800},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	if gi.Pos < 4 {
		t.Errorf("Pos = %d, want >= 4 (underburn)", gi.Pos)
	}
	if gi.WastedPct <= 0 {
		t.Error("expected positive WastedPct for underburn")
	}
}

func TestComputeGaugeInfoWeeklyGateWithinHorizon(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Account A has 5h quota, account B is 7d-exhausted but 7d resets within 5h.
	// B should contribute coverage after its gate lifts, not be excluded.
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
			quota.Window7Day:  {RemainingPct: 50, ResetAtUnix: now + 100_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 9_000},
			quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 3_600}, // 7d resets in 1h
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	// Account B provides coverage after 3600s. Should not be position 0 (all dry).
	if gi.Pos == 0 {
		t.Error("weekly-gated account with reset within horizon should contribute coverage")
	}
}

func TestComputeGaugeInfoWeeklyGateBeyondHorizon(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Account B's 7d reset is beyond the 5h horizon → excluded.
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
			quota.Window7Day:  {RemainingPct: 50, ResetAtUnix: now + 100_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 9_000},
			quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 100_000}, // 7d resets far away
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	// Only account A contributes. Should still produce a valid position.
	if gi.Pos < -1 || gi.Pos > 6 {
		t.Errorf("Pos = %d, want valid position", gi.Pos)
	}
}

func TestComputeGaugeInfoElapsedZeroFallback(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Account with elapsed=0 (reset is full period away) and 0% remaining.
	// Should not be dropped — uses fallback rate.
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 9_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			// elapsed = period - (reset - now) = 18000 - 18000 = 0
			quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: now + period},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	// Should produce a valid position, not unknown (-1).
	if gi.Pos == -1 {
		t.Error("elapsed=0 account should not cause unknown result")
	}
}

func TestComputeGaugeInfoNoSustainersAllGated(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// All accounts are weekly-gated with 7d resets beyond 5h horizon → no sustainers.
	accounts := []acctInfo{
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 9_000},
			quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 100_000},
		}}},
		{result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 9_000},
			quota.Window7Day:  {RemainingPct: 0, ResetAtUnix: now + 200_000},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now)
	if gi.Pos != 0 {
		t.Errorf("Pos = %d, want 0 (all gated beyond horizon = all dry)", gi.Pos)
	}
}
