package history

import (
	"github.com/jacobcxdev/cq/internal/quota"
	"math"
)

const (
	recentHalfLifeSeconds = 30 * 60
	recentBatchSeconds    = 5 * 60
	recentWarmupSeconds   = 15 * 60
	recentFreshSeconds    = 15 * 60
	recentGapSeconds      = 2 * 60 * 60
)

// Recent forecasting has a shorter horizon than weekly gauge smoothing.
// Retaining the batch anchor avoids losing deltas between frequent polls.
// This optional state cold-starts naturally when reading older history files.
type recentBurnState struct {
	RatePctPerS   float64 `json:"rate_pct_per_s"`
	StartedAtUnix int64   `json:"started_at_unix"`
	SampleAtUnix  int64   `json:"sample_at_unix"`
	RemainingPct  float64 `json:"remaining_pct"`
}

func newRecentBurnState(w quota.Window, now int64) *recentBurnState {
	if w.ResetAtUnix <= now || w.RemainingPercent() <= 0 {
		return nil
	}
	return &recentBurnState{StartedAtUnix: now, SampleAtUnix: now, RemainingPct: w.RemainingPercent()}
}

func updateRecentBurn(previous *WindowState, w quota.Window, now int64) {
	if w.RemainingPercent() <= 0 || w.ResetAtUnix <= now {
		previous.Recent = nil
		return
	}
	if previous.Recent == nil || now-previous.LastSeenUnix > recentGapSeconds ||
		w.ResetAtUnix != previous.LastResetAtUnix || remainingIncreased(previous, w) ||
		(w.RemainingPctExact == nil) != (previous.LastRemainingPctExact == nil) {
		previous.Recent = newRecentBurnState(w, now)
		return
	}
	recent := previous.Recent
	dt := now - recent.SampleAtUnix
	if dt < recentBatchSeconds {
		return
	}
	rate := (recent.RemainingPct - w.RemainingPercent()) / float64(dt)
	if rate < 0 {
		previous.Recent = newRecentBurnState(w, now)
		return
	}
	if recent.SampleAtUnix == recent.StartedAtUnix {
		recent.RatePctPerS = rate // Seed from evidence, not an artificial zero.
	} else {
		alpha := 1 - math.Exp2(-float64(dt)/recentHalfLifeSeconds)
		recent.RatePctPerS += alpha * (rate - recent.RatePctPerS)
	}
	recent.SampleAtUnix = now
	recent.RemainingPct = w.RemainingPercent()
}

func recentBurnRate(previous *WindowState, w quota.Window, now int64) *float64 {
	if previous == nil || previous.Recent == nil {
		return nil
	}
	recent := previous.Recent
	age := now - previous.LastSeenUnix
	if age < 0 || age > recentFreshSeconds || w.ResetAtUnix <= now ||
		w.ResetAtUnix != previous.LastResetAtUnix || w.RemainingPct != previous.LastRemainingPct ||
		(w.RemainingPctExact == nil) != (previous.LastRemainingPctExact == nil) ||
		recent.SampleAtUnix-recent.StartedAtUnix < recentWarmupSeconds {
		return nil
	}
	if w.RemainingPctExact != nil && *w.RemainingPctExact != *previous.LastRemainingPctExact {
		return nil
	}
	rate := recent.RatePctPerS
	return &rate
}
