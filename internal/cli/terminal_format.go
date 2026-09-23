package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func TerminalCount(value uint64) Span {
	return Span{Text: strconv.FormatUint(value, 10), Role: Quantity}
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
	stamp := local.Format("2006-01-02 15:04:05")
	if local.Nanosecond() != 0 {
		stamp += strings.TrimRight(fmt.Sprintf(".%09d", local.Nanosecond()), "0")
	}
	stamp += " " + local.Format("-07:00")
	if options.Now.IsZero() {
		return Span{Text: stamp, Role: TimeValue}
	}
	delta := value.Sub(options.Now)
	var relative string
	if deadline && delta < 0 {
		relative = "past " + durationUnits(-delta, false)
	} else if delta > 0 {
		relative = "in " + durationUnits(delta, true)
	} else {
		relative = durationUnits(-delta, false) + " ago"
	}
	return Span{Text: stamp + " (" + relative + ")", Role: TimeValue}
}

func durationUnits(delta time.Duration, roundUp bool) string {
	minutes := int64(delta / time.Minute)
	if roundUp && delta%time.Minute != 0 {
		minutes++
	}
	if minutes < 1 {
		return "0m"
	}
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	if minutes < 24*60 {
		return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	}
	hours := int64(delta / time.Hour)
	if roundUp && delta%time.Hour != 0 {
		hours++
	}
	return fmt.Sprintf("%dd %dh", hours/24, hours%24)
}
