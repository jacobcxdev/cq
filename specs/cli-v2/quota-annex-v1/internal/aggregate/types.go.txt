package aggregate

import "github.com/jacobcxdev/cq/internal/quota"

// AggregateResult is an alias for quota.AggregateResult retained for
// backward compatibility with existing callers.
type AggregateResult = quota.AggregateResult

// AccountSummary holds multiplier info for display.
type AccountSummary struct {
	Count      int    `json:"count"`
	TotalMulti int    `json:"total_multi"`
	Label      string `json:"label"`
}
