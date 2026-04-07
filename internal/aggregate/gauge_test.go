package aggregate

import (
	"math"
	"testing"

	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/quota"
)

// burnRateFor is a test helper that builds a history.BurnRates map from a
// flat list of (accountKey, window, rate) arguments. The provider field is
// always empty, matching what internal/history.Store publishes.
//
// Call as burnRateFor("acctA", quota.Window5Hour, 0.04, "acctB", quota.Window5Hour, 0.01).
// Arguments must come in triples; odd trailing arguments are ignored.
func burnRateFor(args ...any) history.BurnRates {
	rates := make(history.BurnRates, len(args)/3)
	for i := 0; i+2 < len(args); i += 3 {
		account, _ := args[i].(string)
		window, _ := args[i+1].(quota.WindowName)
		rate, _ := args[i+2].(float64)
		rates[history.BurnRateKey{AccountKey: account, Window: string(window)}] = rate
	}
	return rates
}

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
		ivs := buildIntervals(sustainers, 1.0, period, 0)
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
		ivs := buildIntervals(sustainers, 1.0, period, 0)
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
		ivs := buildIntervals(sustainers, 1.0, period, 0)
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
		ivs := buildIntervals(sustainers, 1.0, period, 0)
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
	gi := computeGaugeInfo(accounts, quota.Window5Hour, 18_000, now, nil)
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
	gi := computeGaugeInfo(accounts, quota.Window5Hour, 18_000, now, nil)
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
	// Account A nearly depleted after only 2000s elapsed → cumulative
	// rate = 98/2000 = 0.049 pct/s. Account B is fresh (elapsed=0,
	// contributes 0 demand but full supply).
	// totalSupply = 2 × 100/18000 ≈ 0.01111; totalDemand = 0.049.
	// rho ≈ 4.41 → deficit = 0.773 > 0.40 → Pos 0.
	accounts := []acctInfo{
		{multiplier: 1, result: quota.Result{AccountID: "acctA", Windows: map[quota.WindowName]quota.Window{
			// elapsed = 18000 - 16000 = 2000
			quota.Window5Hour: {RemainingPct: 2, ResetAtUnix: now + 16_000},
		}}},
		{multiplier: 1, result: quota.Result{AccountID: "acctB", Windows: map[quota.WindowName]quota.Window{
			// elapsed = 0 (fresh, reset one full period out)
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + period},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
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
	// Two 1x accounts, both at elapsed=3600s.
	//   A: remaining=80, used=20, rate = 20/3600 = 0.00556 pct/s.
	//   B: remaining=60, used=40, rate = 40/3600 = 0.01111 pct/s.
	// totalSupply = 2 × 100/18000 = 0.01111 pct/s.
	// totalDemand = 0.00556 + 0.01111 = 0.01667 pct/s.
	// rho = 1.5 → deficit = 0.333 > 0.20 → Pos 1 (moderate overburn).
	// allocatedRate = 0.01667/2 = 0.00833 pct/s. Combined intervals leave
	// a gap at 9600s which is well outside the 3600s imminent threshold.
	accounts := []acctInfo{
		{multiplier: 1, result: quota.Result{AccountID: "acctA", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 14_400},
		}}},
		{multiplier: 1, result: quota.Result{AccountID: "acctB", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 60, ResetAtUnix: now + 14_400},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
	if gi.Pos != 1 {
		t.Errorf("Pos = %d, want 1 (moderate overburn)", gi.Pos)
	}
	if gi.Override != "" {
		t.Errorf("Override = %q, want empty (gap beyond imminent threshold)", gi.Override)
	}
}

func TestComputeGaugeInfoMildOverburn(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Two 1x accounts, both at elapsed=3600s.
	//   A: remaining=80, used=20, rate = 20/3600 = 0.00556 pct/s.
	//   B: remaining=72, used=28, rate = 28/3600 = 0.00778 pct/s.
	// totalSupply = 2 × 100/18000 = 0.01111 pct/s.
	// totalDemand = 0.00556 + 0.00778 = 0.01333 pct/s.
	// rho = 1.2 → deficit = 0.167 ≤ 0.20 → Pos 2 (mild overburn).
	// Gap at 12000s sits well outside the 3600s imminent threshold.
	accounts := []acctInfo{
		{multiplier: 1, result: quota.Result{AccountID: "acctA", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 14_400},
		}}},
		{multiplier: 1, result: quota.Result{AccountID: "acctB", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 72, ResetAtUnix: now + 14_400},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
	if gi.Pos != 2 {
		t.Errorf("Pos = %d, want 2 (mild overburn)", gi.Pos)
	}
	if gi.Override != "" {
		t.Errorf("Override = %q, want empty (gap beyond imminent threshold)", gi.Override)
	}
}

func TestComputeGaugeInfoOnPace(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Two 1x accounts at elapsed=9000s (halfway through the window), both
	// with remaining=50 → used=50 → rate = 50/9000 = 0.00556 pct/s each.
	// totalDemand = 0.01111 = totalSupply, rho = 1.0 → inside the ±5%
	// deadband → Pos 3 (on pace).
	accounts := []acctInfo{
		{multiplier: 1, result: quota.Result{AccountID: "acctA", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
		}}},
		{multiplier: 1, result: quota.Result{AccountID: "acctB", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
	if gi.Pos != 3 {
		t.Errorf("Pos = %d, want 3 (on pace)", gi.Pos)
	}
}

func TestComputeGaugeInfoUnderburn(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Single 1x account, high remaining after most of the window has
	// elapsed. elapsed = 16200, used = 5 → rate = 5/16200 ≈ 0.000309 pct/s.
	// totalSupply = 100/18000 ≈ 0.00556 pct/s.
	// rho ≈ 0.056 → surplus > 0.60 → Pos 6 (severe underburn).
	// WastedPct > 0 because projectedWasteInfo sees most of the window
	// gone with barely any burn, producing a large timing-weighted waste.
	accounts := []acctInfo{
		{multiplier: 1, result: quota.Result{AccountID: "acctA", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 95, ResetAtUnix: now + 1_800},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
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
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
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
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
	// Only account A contributes. Should still produce a valid position.
	if gi.Pos < -1 || gi.Pos > 6 {
		t.Errorf("Pos = %d, want valid position", gi.Pos)
	}
}

// TestComputeGaugeInfoAllElapsedZeroReturnsUnknown verifies the new
// cold-start semantics: when every active account is still fresh (elapsed=0
// for all), there is no cumulative burn signal yet and the gauge falls back
// to Pos=-1 (unknown). The old behaviour was "no EWMA data → unknown"; the
// new behaviour is "no cumulative activity → unknown".
func TestComputeGaugeInfoAllElapsedZeroReturnsUnknown(t *testing.T) {
	now := int64(1_000)
	period := int64(18_000)
	// Both accounts reset a full period in the future → elapsed = 0 for
	// both → rate = 0 for both → haveData = false → Pos = -1.
	accounts := []acctInfo{
		{multiplier: 1, result: quota.Result{AccountID: "acctA", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + period},
		}}},
		{multiplier: 1, result: quota.Result{AccountID: "acctB", Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + period},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
	if gi.Pos != -1 {
		t.Errorf("Pos = %d, want -1 (all elapsed=0 → no cumulative signal)", gi.Pos)
	}
}

// TestComputeGaugeInfoCumulativeOverburnDirection is the headline regression
// contract for the cumulative-rate-source fix. It mirrors the observed
// Claude 5h real-world failure: with nil burnRates (no persistent EWMA data),
// the gauge must still produce a directionally-correct overburn position
// derived from each account's within-window used/elapsed ratio.
//
// Scenario:
//   - Account A: multiplier=20, remaining=28%, elapsed=5400s (used=72,
//     rate=72/5400=0.01333 pct/s per own-unit).
//   - Account B: multiplier=20, remaining=100%, elapsed=0 (fresh, reset one
//     full period in the future).
//
// Computation:
//   - totalSupply = 2 × 20 × 100/18000 = 0.22222 pct/s.
//   - totalDemand = 20 × 0.01333 + 20 × 0 = 0.26667 pct/s.
//   - rho = 0.26667 / 0.22222 ≈ 1.2.
//   - deficit = 1 - 1/1.2 ≈ 0.167 ≤ 0.20 → Pos = 2 (mild overburn).
//
// This test MUST fail on the pre-fix tip: the current code keys demand off
// burnRates.Get which returns (0, false) for nil burnRates, so haveData=false
// and the gauge returns Pos=-1 (unknown). After the fix, rate is sourced from
// used/elapsed unconditionally and the test passes with Pos=2.
func TestComputeGaugeInfoCumulativeOverburnDirection(t *testing.T) {
	now := int64(1_000_000)
	period := int64(18_000)
	accounts := []acctInfo{
		{multiplier: 20, result: quota.Result{AccountID: "acctA", Windows: map[quota.WindowName]quota.Window{
			// elapsed = 18000 - (ResetAtUnix - now) = 18000 - 12600 = 5400
			quota.Window5Hour: {RemainingPct: 28, ResetAtUnix: now + 12_600},
		}}},
		{multiplier: 20, result: quota.Result{AccountID: "acctB", Windows: map[quota.WindowName]quota.Window{
			// elapsed = 0 (fresh, reset one full period out)
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + period},
		}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
	if gi.Pos != 2 {
		t.Errorf("Pos = %d, want 2 (mild overburn from cumulative used/elapsed)", gi.Pos)
	}
	if gi.Override != "" {
		t.Errorf("Override = %q, want empty (gap beyond imminent threshold)", gi.Override)
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
	gi := computeGaugeInfo(accounts, quota.Window5Hour, period, now, nil)
	if gi.Pos != 0 {
		t.Errorf("Pos = %d, want 0 (all gated beyond horizon = all dry)", gi.Pos)
	}
}
