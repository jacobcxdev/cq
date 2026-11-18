package quota

// SharedBudgetReport describes a subscription allowance shared across models.
// It is separate from the request quota used for planning and routing.
type SharedBudgetReport struct {
	ProviderID   string        `json:"provider_id"`
	ProviderName string        `json:"provider_name"`
	Plan         string        `json:"plan,omitempty"`
	Email        string        `json:"email,omitempty"`
	Icon         string        `json:"-"`
	ExtraUsage   *ExtraCredits `json:"extra_usage,omitempty"`
	// An empty list means that no allowance windows were available.
	Windows []SharedBudgetWindow `json:"windows,omitempty"`
}

// SharedBudgetWindow records the provider's remaining allowance, in percent.
// ResetAtUnix is zero when the provider reports no reset time.
type SharedBudgetWindow struct {
	Name         string  `json:"name"`
	RemainingPct float64 `json:"remaining_pct"`
	ResetAtUnix  int64   `json:"reset_at_unix,omitempty"`
}
