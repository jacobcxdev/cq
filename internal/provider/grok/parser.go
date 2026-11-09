package grok

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// parseQuota reads only the included subscription allowance. Purchased balances
// and on-demand billing fields are deliberately outside CQ's quota model.
func parseQuota(body []byte, now time.Time) (quota.Result, error) {
	var response struct {
		Config *struct {
			Used   json.RawMessage `json:"creditUsagePercent"`
			Period *struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	invalid := errors.New("Grok included subscription quota unavailable")
	if json.Unmarshal(body, &response) != nil || response.Config == nil || response.Config.Period == nil {
		return quota.Result{}, invalid
	}
	// The credits service omits zero-valued scalars. Match Grok Build's
	// credit_balance_from_config while still requiring a valid current period.
	// Explicit null is not an omitted proto3 zero.
	used := 0.0
	if len(response.Config.Used) != 0 {
		var value *float64
		if json.Unmarshal(response.Config.Used, &value) != nil || value == nil {
			return quota.Result{}, invalid
		}
		used = *value
	}
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return quota.Result{}, invalid
	}
	start, err := time.Parse(time.RFC3339Nano, response.Config.Period.Start)
	if err != nil {
		return quota.Result{}, invalid
	}
	end, err := time.Parse(time.RFC3339Nano, response.Config.Period.End)
	if err != nil || !end.After(start) || start.After(now) || !end.After(now) {
		return quota.Result{}, invalid
	}
	period := end.Sub(start)
	if period%time.Second != 0 {
		return quota.Result{}, invalid
	}
	name, ok := quota.WindowNameForPeriod(int64(period/time.Second), "")
	if !ok {
		return quota.Result{}, invalid
	}
	remaining := max(0, 100-used)
	windows := map[quota.WindowName]quota.Window{name: {
		// A display-rounded zero must not turn a positive allowance into an
		// exhausted account. Exact percentage remains authoritative.
		RemainingPct: int(math.Ceil(remaining)), RemainingPctExact: &remaining,
		ResetAtUnix: end.Unix(),
	}}
	return quota.Result{Status: quota.StatusFromWindows(windows), Windows: windows}, nil
}
