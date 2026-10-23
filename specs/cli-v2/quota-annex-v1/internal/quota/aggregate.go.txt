package quota

// AggregateResult holds cross-account weighted pace data for a single window.
type AggregateResult struct {
	RemainingPct   int     `json:"remaining_pct"`
	ExpectedPct    int     `json:"expected_pct"`
	PaceDiff       int     `json:"pace_diff"`
	Burndown       int64   `json:"burndown_s,omitempty"`
	Sustainability float64 `json:"sustainability,omitempty"`
	GaugePos       int     `json:"gauge_pos"`
	GapStartS      int64   `json:"gap_start_s,omitempty"`
	GapDurationS   int64   `json:"gap_duration_s,omitempty"`
	WastedPct      int     `json:"wasted_pct,omitempty"`
	WasteDeadlineS int64   `json:"waste_deadline_s,omitempty"`
	// GaugeOverride is set when an imminent-block warning should be surfaced
	// in addition to the natural rho-derived GaugePos. Empty string means no
	// override warning was applied. Current values: "", "imminent_block".
	GaugeOverride string `json:"gauge_override,omitempty"`
}
