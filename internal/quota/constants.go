package quota

import "time"

type Status string

const (
	StatusOK        Status = "ok"
	StatusExhausted Status = "exhausted"
	StatusError     Status = "error"
)

type WindowName string

const (
	Window5Hour WindowName = "5h"
	Window7Day  WindowName = "7d"
	WindowQuota WindowName = "quota"
)

func PeriodFor(name WindowName) time.Duration {
	switch name {
	case Window5Hour:
		return 5 * time.Hour
	case Window7Day:
		return 7 * 24 * time.Hour
	case WindowQuota:
		return 24 * time.Hour
	default:
		return 0
	}
}

// OrderedWindows returns window names in canonical display order.
func OrderedWindows() []WindowName {
	return []WindowName{Window5Hour, Window7Day, WindowQuota}
}

// DefaultResetEpoch returns a fallback reset epoch when the API doesn't
// provide one: nowEpoch + periodS (i.e. one full period from now).
func DefaultResetEpoch(periodS, nowEpoch int64) int64 {
	return nowEpoch + periodS
}
