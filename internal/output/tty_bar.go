package output

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const barWidth = 20

// renderBar returns a 20-character progress bar string with optional pace marker.
// pct is the filled percentage (0-100), forceStyle overrides the bar colour when
// non-zero, and expectedPct places a "|" pace marker (-1 to disable).
func renderBar(pct int, forceStyle lipgloss.Style, expectedPct int) string {
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	filled := pct * barWidth / 100
	if filled > barWidth {
		filled = barWidth
	}

	color := forceStyle
	if !hasStyle(forceStyle) {
		color = pctStyle(pct)
	}

	marker := -1
	var markerStyle lipgloss.Style
	if expectedPct >= 0 {
		marker = expectedPct * barWidth / 100
		if marker >= barWidth {
			marker = barWidth - 1
		}
		if pct >= expectedPct {
			markerStyle = greenStyle
		} else {
			markerStyle = redStyle
		}
		if hasStyle(forceStyle) {
			markerStyle = dimStyle
		}
	}

	var b strings.Builder
	b.Grow(barWidth * 12) // rough upper bound with ANSI codes
	for i := 0; i < barWidth; i++ {
		if marker >= 0 && i == marker {
			b.WriteString(markerStyle.Render("|"))
		} else if i < filled {
			b.WriteString(color.Render("\u2501"))
		} else {
			b.WriteString(dimStyle.Render("\u254C"))
		}
	}
	return b.String()
}

// hasStyle reports whether s has any non-default styling applied.
func hasStyle(s lipgloss.Style) bool {
	return s.GetForeground() != lipgloss.NoColor{} ||
		s.GetBold() ||
		s.GetFaint() ||
		s.GetItalic()
}
