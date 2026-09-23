package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func TerminalCount(value uint64) Span {
	digits := strconv.FormatUint(value, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return Span{Text: digits, Role: Quantity}
}

func TerminalPercent(value float64, exactSetting bool) Span {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Span{Text: "—", Role: Secondary}
	}
	if exactSetting {
		return Span{Text: strconv.FormatFloat(value, 'f', -1, 64) + "%", Role: Quantity}
	}
	switch {
	case value > 0 && value < 0.1:
		return Span{Text: "<0.1%", Role: Quantity}
	case value > 99.9 && value < 100:
		return Span{Text: ">99.9%", Role: Quantity}
	default:
		return Span{Text: strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + "%", Role: Quantity}
	}
}

func TerminalTime(value time.Time, options TerminalOptions, deadline bool) Span {
	location := options.Location
	if location == nil {
		location = time.Local
	}
	local := value.In(location)
	stamp := terminalTimestampText(local, local.Second() != 0 || local.Nanosecond() != 0)
	if options.Now.IsZero() {
		return Span{Text: stamp, Role: TimeValue}
	}
	delta := value.Sub(options.Now)
	var relative string
	if deadline && delta == 0 {
		relative = "now"
	} else if deadline && delta < 0 {
		relative = "expired " + durationUnits(-delta, false) + " ago"
	} else if delta > 0 {
		relative = "in " + durationUnits(delta, true)
	} else {
		relative = durationUnits(-delta, false) + " ago"
	}
	return Span{Text: relative + " · " + stamp, Role: TimeValue}
}

func durationUnits(delta time.Duration, roundUp bool) string {
	if delta < time.Second {
		value := delta / time.Millisecond
		if roundUp && delta%time.Millisecond != 0 {
			value++
		}
		return fmt.Sprintf("%dms", value)
	}
	if delta < time.Minute {
		value := delta / time.Second
		if roundUp && delta%time.Second != 0 {
			value++
		}
		return fmt.Sprintf("%ds", value)
	}

	minutes := int64(delta / time.Minute)
	if roundUp && delta%time.Minute != 0 {
		minutes++
	}
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	if minutes < 24*60 {
		if minutes%60 == 0 {
			return fmt.Sprintf("%dh", minutes/60)
		}
		return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	}
	hours := int64(delta / time.Hour)
	if roundUp && delta%time.Hour != 0 {
		hours++
	}
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dd %dh", hours/24, hours%24)
}

// TerminalTimestamp retains observation and event ordering precision without a relative age.
func TerminalTimestamp(value time.Time, options TerminalOptions) Span {
	location := options.Location
	if location == nil {
		location = time.Local
	}
	return Span{Text: terminalTimestampText(value.In(location), true), Role: TimeValue}
}
func terminalTimestampText(local time.Time, seconds bool) string {
	layout := "2 Jan 2006, 15:04"
	if seconds {
		layout += ":05"
	}
	stamp := local.Format(layout)
	if local.Nanosecond() != 0 {
		stamp += strings.TrimRight(fmt.Sprintf(".%09d", local.Nanosecond()), "0")
	}
	return stamp + " " + local.Format("MST") + " (UTC" + local.Format("-07:00") + ")"
}
