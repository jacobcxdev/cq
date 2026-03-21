package output

import "github.com/charmbracelet/lipgloss"

// Style definitions for TTY output. These are package-level mutable vars
// because lipgloss.Style is a value type that lipgloss.NewStyle() returns.
var (
	greenStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellowStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	dimStyle           = lipgloss.NewStyle().Faint(true)
	boldStyle          = lipgloss.NewStyle().Bold(true)
	boldDimItalicStyle = lipgloss.NewStyle().Bold(true).Faint(true).Italic(true)
	boldRedStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	brightBlackStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// pctStyle returns the style for a remaining-percentage value.
func pctStyle(pct int) lipgloss.Style {
	if pct > 50 {
		return greenStyle
	}
	if pct > 20 {
		return yellowStyle
	}
	return redStyle
}

// diffStyle returns the style for a pace-diff value.
func diffStyle(diff int) lipgloss.Style {
	if diff >= 0 {
		return greenStyle
	}
	if diff >= -5 {
		return yellowStyle
	}
	return redStyle
}

// sustainStyle returns the style for a sustainability value.
func sustainStyle(s float64) lipgloss.Style {
	if s < 0 {
		return dimStyle
	}
	pos := sustainGaugePos(s)
	if pos < 3 {
		return redStyle // overconsumption
	}
	if pos == 3 {
		return greenStyle // on pace
	}
	return yellowStyle // underconsumption
}

// resetStyle returns the style for the reset timer.
func resetStyle(pct int, resetEpoch, nowEpoch, periodS int64) lipgloss.Style {
	if pct <= 0 {
		return boldRedStyle
	}
	if periodS > 0 {
		remainingPct := int((resetEpoch - nowEpoch) * 100 / periodS)
		if remainingPct > 100 {
			remainingPct = 100
		}
		if remainingPct < 0 {
			remainingPct = 0
		}
		if remainingPct > 50 {
			return greenStyle
		}
		if remainingPct > 20 {
			return yellowStyle
		}
		return redStyle
	}
	return dimStyle
}
