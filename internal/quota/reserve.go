package quota

import (
	"math"
	"time"
)

// ReservePolicy identifies protected capacity without changing observed quota.
// An empty window means no active protection, including an explicit bypass.
type ReservePolicy struct {
	AccountKey string
	Window     WindowName
	Percent    float64
}

// ReserveReleaseAt leaves enough time to spend protected quota before its
// natural refill, plus the reset planner's five-minute execution margin.
// Zero demand only permits that margin; invalid evidence grants no early release.
func ReserveReleaseAt(now, resetAt time.Time, remaining, reserve, rate float64) time.Time {
	if resetAt.IsZero() || !resetAt.After(now) || math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 ||
		math.IsNaN(reserve) || math.IsInf(reserve, 0) || reserve <= 0 || reserve >= 100 ||
		math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return resetAt
	}
	lead := (5 * time.Minute).Seconds()
	if rate > 0 {
		lead += min(remaining, reserve) / rate
	}
	if lead >= resetAt.Sub(now).Seconds() {
		return now
	}
	return resetAt.Add(-time.Duration(lead * float64(time.Second)))
}
