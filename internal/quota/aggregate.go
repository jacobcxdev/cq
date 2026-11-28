package quota

import "time"

// ResetPacePhase is a temporary target relative to the observed portfolio burn.
type ResetPacePhase struct {
	From    time.Time `json:"from"`
	Until   time.Time `json:"until"`
	Percent int       `json:"percent"`
}

type PaceAdvice string

const (
	PaceBurnHarder  PaceAdvice = "burn_harder"
	PaceKeep        PaceAdvice = "keep_pace"
	PaceSlowDown    PaceAdvice = "slow_down"
	PaceNeedsRefill PaceAdvice = "refill_needed"
)

// AggregateResult holds cross-account weighted pace data for a single window.
type AggregateResult struct {
	RemainingPct             int        `json:"remaining_pct"`
	ExpectedPct              int        `json:"expected_pct"`
	PaceDiff                 int        `json:"pace_diff"`
	Burndown                 int64      `json:"burndown_s,omitempty"`
	Sustainability           float64    `json:"sustainability,omitempty"`
	GaugePos                 int        `json:"gauge_pos"`
	GapStartS                int64      `json:"gap_start_s,omitempty"`
	GapDurationS             int64      `json:"gap_duration_s,omitempty"`
	WastedPct                int        `json:"wasted_pct,omitempty"`
	WasteDeadlineS           int64      `json:"waste_deadline_s,omitempty"`
	ScheduledResetCount      int        `json:"scheduled_reset_count,omitempty"`
	NextScheduledResetAtUnix int64      `json:"next_scheduled_reset_at_unix,omitempty"`
	PaceAdvice               PaceAdvice `json:"pace_advice,omitempty"`
	PaceTargetPct            *int       `json:"pace_target_pct,omitempty"` // percentage of estimated current burn until the next reset
	RemainingAtNextResetPct  *int       `json:"remaining_at_next_reset_pct,omitempty"`
	// GaugeOverride is set when an imminent-block warning should be surfaced
	// in addition to the natural rho-derived GaugePos. Empty string means no
	// override warning was applied. Current values: "", "imminent_block".
	GaugeOverride string `json:"gauge_override,omitempty"`
}
