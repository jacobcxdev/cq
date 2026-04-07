package aggregate

import (
	"fmt"
	"sort"
	"testing"

	"github.com/jacobcxdev/cq/internal/quota"
)

// TestGaugePhaseInvariantAcrossOffsets is the headline regression test for
// the phase-stability fix. It sweeps (burnFactor × phaseOffset × multiplier
// pairs × window) scenarios and asserts that the rate-ratio gauge produces
// a phase-invariant GaugePos: the set of positions observed as time moves
// through a full second period must be either a singleton equal to the bucket
// posFromRho(burnFactor) maps to, or a mix of that bucket with position 0
// tagged as an imminent_block override (a legitimate severity escalation).
//
// This test MUST fail on main prior to Steps 5-8. The main failure mode is
// that sustain.go's legacy gap-fraction severity bucketing produces wildly
// different positions at different phase offsets for identical underlying
// burn conditions. After the fix, the rho-driven bucketing produces the same
// position regardless of where in the renewal cycle the user happens to look.
func TestGaugePhaseInvariantAcrossOffsets(t *testing.T) {
	// Step sizes and offsets are chosen so that burnFactor × 100 ×
	// elapsed/period is a whole integer at every sampled point across
	// every burn factor in this sweep. This eliminates int-rounding
	// jitter in RemainingPct that would otherwise push rho across bucket
	// boundaries for a fundamentally steady burn.
	windows := []struct {
		name     quota.WindowName
		periodS  int64
		stepS    int64
		offsetsS []int64
	}{
		{
			name:    quota.Window5Hour,
			periodS: int64(quota.PeriodFor(quota.Window5Hour).Seconds()),
			stepS:   3600, // 1h — LCM of burn-to-integer steps for 5h
			offsetsS: []int64{
				0,
				3600, // 1h
				7200, // 2h
			},
		},
		{
			name:    quota.Window7Day,
			periodS: int64(quota.PeriodFor(quota.Window7Day).Seconds()),
			// 120960s (33.6h) = 1/5 of the 7d period. At this step
			// burn × elapsed/period is integer for every burn factor.
			stepS: 120960,
			offsetsS: []int64{
				0,
				120960,
				241920,
			},
		},
	}

	multiplierPairs := [][]int{
		{1, 1},
		{1, 20},
	}

	burnFactors := []float64{0.5, 0.75, 0.9, 1.2, 1.5, 2.0}

	for _, win := range windows {
		for _, mults := range multiplierPairs {
			for _, burn := range burnFactors {
				expected := posFromRho(burn)
				for _, offset := range win.offsetsS {
					name := fmt.Sprintf("%s/m=%v/burn=%.2f/offset=%ds",
						win.name, mults, burn, offset)
					t.Run(name, func(t *testing.T) {
						observed := sweepPositions(
							t, win.name, win.periodS, win.stepS,
							mults, burn, offset,
						)
						assertPhaseInvariant(t, observed, expected, burn)
					})
				}
			}
		}
	}
}

// sweepPositions walks a synthetic timeline through the second period,
// computing the gauge position at each step and returning the observed
// positions (skipping transient reset-boundary moments where the cumulative
// rate on a fresh account would be zero and therefore uninformative).
//
// The burnRates argument is nil: the cumulative-rate-source fix derives rho
// from used/elapsed directly, so no EWMA preseeding is needed — the sweep
// now exercises the real production path end-to-end.
func sweepPositions(
	t *testing.T,
	winName quota.WindowName,
	periodS int64,
	stepS int64,
	mults []int,
	burnFactor float64,
	offsetS int64,
) []int {
	t.Helper()
	period := float64(periodS)

	// Constant nowEpoch base. Accounts reset at `nowBase` and
	// `nowBase - offsetS` respectively. As nowEpoch advances, each account
	// cycles through its window at a different phase.
	const nowBase = int64(10_000_000)
	resetBases := []int64{nowBase, nowBase - offsetS}

	positions := make([]int, 0, periodS/stepS+1)
	for elapsed := stepS; elapsed < periodS; elapsed += stepS {
		now := nowBase + periodS + elapsed // second period

		// Skip samples where any account has elapsedInWindow==0 (a fresh
		// reset): cumulative rate for that account is 0 and rho drops
		// transiently. This is a true limitation of cumulative-only rho —
		// a fresh account has no in-window demand signal yet — not a
		// phase-stability bug in the gauge.
		skip := false
		for i := range mults {
			if (now-resetBases[i])%periodS == 0 {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		accounts := make([]acctInfo, 0, len(mults))
		for i, m := range mults {
			rb := resetBases[i]
			elapsedInWindow := (now - rb) % periodS
			resetAt := now + (periodS - elapsedInWindow)
			used := burnFactor * 100.0 * float64(elapsedInWindow) / period
			if used > 100 {
				used = 100
			}
			remaining := int(100.0 - used)

			accounts = append(accounts, acctInfo{
				multiplier: m,
				result: quota.Result{
					AccountID: fmt.Sprintf("acct%d", i),
					Status:    quota.StatusOK,
					Windows: map[quota.WindowName]quota.Window{
						winName: {
							RemainingPct: remaining,
							ResetAtUnix:  resetAt,
						},
					},
				},
			})
		}

		gi := computeGaugeInfo(accounts, winName, periodS, now, nil)
		positions = append(positions, gi.Pos)
	}
	return positions
}

// assertPhaseInvariant checks that the set of observed gauge positions across
// a phase sweep is phase-stable: either a singleton matching the expected
// bucket, or (for overburn scenarios where one account reaches near-zero at
// some phase) a mix of the expected bucket and position 0 — the imminent-block
// override escalation. Any other pattern — different non-zero non-expected
// positions, flipping between overburn and underburn, etc. — indicates
// phase dependence in the rate-ratio severity and fails the test.
func assertPhaseInvariant(t *testing.T, observed []int, expected int, burnFactor float64) {
	t.Helper()

	set := map[int]int{}
	for _, p := range observed {
		set[p]++
	}
	uniq := make([]int, 0, len(set))
	for p := range set {
		uniq = append(uniq, p)
	}
	sort.Ints(uniq)

	// Singleton: perfect phase invariance.
	if len(uniq) == 1 && uniq[0] == expected {
		return
	}

	// Overburn scenarios may additionally hit position 0 via the
	// imminent-block override at phases where one account's remaining
	// becomes near-zero. This is correct behaviour: when a coverage gap is
	// imminent in absolute clock time, the gauge MUST escalate regardless
	// of the long-term rate ratio. We accept {expected} or {expected, 0}
	// as long as expected is an overburn bucket.
	if burnFactor > 1.05 && len(uniq) == 2 && uniq[0] == 0 && uniq[1] == expected {
		return
	}
	// The reverse ordering (expected < 0 is impossible, but the sort puts
	// 0 first only if present).

	// Anything else is a phase-stability failure.
	t.Errorf(
		"phase-stability broken: burnFactor=%.3f expected=%d observed=%v",
		burnFactor, expected, uniq,
	)
}

// TestGaugeImminentOverrideForShiftedTraffic verifies the imminent-block
// override fires via the max-rate pass when EWMA signals a recent ramp-up
// that cumulative rho hasn't caught yet. The shifted-traffic flaw would
// otherwise mask this by averaging over accounts.
//
// Cumulative pass alone sees no gap here: heavy's cumulative rate is only
// 98/302400 ≈ 3.24e-4 pct/s (based on its 50%-elapsed state), which under
// the uniform drain model gives light enough room to carry the period. The
// override fires because heavy's EWMA (0.005 pct/s, ~15× cumulative) pushes
// the max-rate demand high enough that light's apparent coverage runs out
// inside the 21600s 7d imminent threshold.
//
// Displayed GapStart comes from the cumulative pass (no gap → -1), while
// Override and Pos come from the max-rate escalation.
func TestGaugeImminentOverrideForShiftedTraffic(t *testing.T) {
	now := int64(10_000_000)
	periodS := int64(7 * 24 * 3600)
	accounts := []acctInfo{
		{multiplier: 20, result: quota.Result{AccountID: "heavy", Status: quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 2, ResetAtUnix: now + periodS/2},
			}}},
		{multiplier: 1, result: quota.Result{AccountID: "light", Status: quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 100, ResetAtUnix: now + periodS},
			}}},
	}

	// Heavy's EWMA is 0.005 pct/s, ~15× its cumulative rate. The max-rate
	// pass uses the larger value and drives the imminent-block decision.
	rates := burnRateFor("heavy", quota.Window7Day, 0.005)

	gi := computeGaugeInfo(accounts, quota.Window7Day, periodS, now, rates)
	if gi.Pos != 0 {
		t.Errorf("Pos = %d, want 0 (imminent block)", gi.Pos)
	}
	if gi.Override != "imminent_block" {
		t.Errorf("Override = %q, want %q", gi.Override, "imminent_block")
	}
}

// TestGaugeNoImminentOverrideWhenSustainable verifies the negative case:
// when cumulative burn lands exactly on the sustainable rate, the override
// stays silent and Pos lands inside the ±5% deadband at 3.
func TestGaugeNoImminentOverrideWhenSustainable(t *testing.T) {
	now := int64(10_000_000)
	periodS := int64(7 * 24 * 3600)
	// Both accounts at 60480s elapsed (10% used). Rate = 10/60480 pct/s
	// exactly equals the sustainable rate 100/604800 pct/s, so rho = 1.0
	// (inside the deadband) → Pos 3 and combined intervals cover the full
	// period → override does not fire.
	accounts := []acctInfo{
		{multiplier: 20, result: quota.Result{AccountID: "heavy", Status: quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 90, ResetAtUnix: now + periodS - 60480},
			}}},
		{multiplier: 1, result: quota.Result{AccountID: "light", Status: quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window7Day: {RemainingPct: 90, ResetAtUnix: now + periodS - 60480},
			}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window7Day, periodS, now, nil)
	if gi.Override != "" {
		t.Errorf("Override = %q, want empty (sustainable)", gi.Override)
	}
	if gi.Pos != 3 {
		t.Errorf("Pos = %d, want 3 (on pace)", gi.Pos)
	}
}

// TestGaugeColdStartReturnsUnknown verifies the new cold-start semantics:
// when every active account is still at the start of its window (elapsed=0,
// no cumulative burn signal yet), the gauge falls back to Pos=-1 rather
// than inventing a direction from zero demand data.
func TestGaugeColdStartReturnsUnknown(t *testing.T) {
	now := int64(10_000_000)
	accounts := []acctInfo{
		{multiplier: 1, result: quota.Result{AccountID: "a", Status: quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				// reset one full period away → elapsed = 0
				quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 18_000},
			}}},
		{multiplier: 1, result: quota.Result{AccountID: "b", Status: quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: now + 18_000},
			}}},
	}
	gi := computeGaugeInfo(accounts, quota.Window5Hour, 18_000, now, nil)
	if gi.Pos != -1 {
		t.Errorf("Pos = %d, want -1 (cold start)", gi.Pos)
	}
	if gi.Override != "" {
		t.Errorf("Override = %q, want empty", gi.Override)
	}
}

// TestGaugeDeadbandAtOnPaceBoundary walks posFromRho across the ±5% deadband
// at rho=1.0 and verifies the symmetric bucketing.
func TestGaugeDeadbandAtOnPaceBoundary(t *testing.T) {
	cases := []struct {
		rho  float64
		want int
	}{
		{1.03, 3},
		{1.06, 2},
		{0.97, 3},
		{0.94, 4},
		{1.0, 3},
		{0.95, 3},
		{1.05, 3},
	}
	for _, c := range cases {
		got := posFromRho(c.rho)
		if got != c.want {
			t.Errorf("posFromRho(%.3f) = %d, want %d", c.rho, got, c.want)
		}
	}
}

// TestBuildIntervalsAllocatedRatePreservesSustainabilityPath guards the
// computeSustainability path: buildIntervals with allocatedRate=0 must
// produce exactly the same intervals as the pre-fix behaviour.
func TestBuildIntervalsAllocatedRatePreservesSustainabilityPath(t *testing.T) {
	// Two accounts with distinct per-account rates.
	sustainers := []sustainAccount{
		{remaining: 80, rate: 0.002, reset: 9000, multiplier: 1},
		{remaining: 40, rate: 0.005, reset: 3000, multiplier: 1},
	}
	period := 18000.0
	ivs := buildIntervals(sustainers, 1.0, period, 0)

	// Expected: A current [0, min(9000, 80/0.002=40000)] = [0, 9000]; after [9000, min(18000, 9000+50000)] = [9000, 18000].
	// B current [0, min(3000, 40/0.005=8000)] = [0, 3000]; after [3000, min(18000, 3000+20000)] = [3000, 18000].
	if len(ivs) != 4 {
		t.Fatalf("expected 4 intervals, got %d: %v", len(ivs), ivs)
	}
	// Check each interval matches the per-account rate calculation.
	wantA1 := interval{start: 0, end: 9000}
	wantA2 := interval{start: 9000, end: 18000}
	wantB1 := interval{start: 0, end: 3000}
	wantB2 := interval{start: 3000, end: 18000}

	// Order is A1, A2, B1, B2 — buildIntervals processes accounts in order.
	if ivs[0] != wantA1 || ivs[1] != wantA2 || ivs[2] != wantB1 || ivs[3] != wantB2 {
		t.Errorf("intervals mismatch: got %v, want %v", ivs,
			[]interval{wantA1, wantA2, wantB1, wantB2})
	}
}
