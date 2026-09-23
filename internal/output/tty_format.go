package output

import (
	"fmt"
	"math"

	"github.com/jacobcxdev/cq/internal/quota"
)

// fmtDuration formats seconds into a human-readable duration string.
func fmtDuration(s int64) string {
	if s <= 0 {
		return "now"
	}
	d := s / 86400
	h := (s % 86400) / 3600
	m := (s % 3600) / 60
	if d > 0 {
		if h > 0 {
			return fmt.Sprintf("%dd %dh", d, h)
		}
		return fmt.Sprintf("%dd", d)
	}
	if h > 0 {
		if m > 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	if m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	return "<1m"
}

// calcPace returns the expected remaining percentage given a window period,
// reset epoch, and current epoch.
func calcPace(periodS, resetEpoch, nowEpoch int64) int {
	if periodS <= 0 {
		return 100
	}
	elapsed := periodS - (resetEpoch - nowEpoch)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > periodS {
		elapsed = periodS
	}
	return int(math.Round(float64(100) - float64(elapsed)*100.0/float64(periodS)))
}

// periodSeconds returns the period length in seconds for a window name.
func periodSeconds(name quota.WindowName) int64 {
	d := quota.PeriodFor(name)
	if d <= 0 {
		return 0
	}
	return int64(d.Seconds())
}
