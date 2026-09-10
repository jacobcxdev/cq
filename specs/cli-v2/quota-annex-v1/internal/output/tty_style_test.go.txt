package output

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// styleID returns a comparable identity for a lipgloss.Style by rendering a
// fixed string through it. Two styles with the same foreground/bold/faint
// settings will produce identical output, which is sufficient to distinguish
// the four style variants used in this package.
func styleID(s lipgloss.Style) string {
	return s.Render("X")
}

func TestPctStyle(t *testing.T) {
	tests := []struct {
		pct  int
		want lipgloss.Style
	}{
		{0, redStyle},
		{19, redStyle},
		{20, redStyle},  // boundary: pct > 20 is yellow, so 20 is red
		{21, yellowStyle},
		{49, yellowStyle},
		{50, yellowStyle}, // boundary: pct > 50 is green, so 50 is yellow
		{51, greenStyle},
		{100, greenStyle},
	}
	for _, tt := range tests {
		got := pctStyle(tt.pct)
		if styleID(got) != styleID(tt.want) {
			t.Errorf("pctStyle(%d): got style rendering %q, want %q",
				tt.pct, styleID(got), styleID(tt.want))
		}
	}
}

func TestDiffStyle(t *testing.T) {
	tests := []struct {
		diff int
		want lipgloss.Style
	}{
		{-10, redStyle},
		{-6, redStyle},
		{-5, yellowStyle}, // boundary: diff >= -5 is yellow
		{-4, yellowStyle},
		{-1, yellowStyle},
		{0, greenStyle}, // boundary: diff >= 0 is green
		{1, greenStyle},
		{5, greenStyle},
	}
	for _, tt := range tests {
		got := diffStyle(tt.diff)
		if styleID(got) != styleID(tt.want) {
			t.Errorf("diffStyle(%d): got style rendering %q, want %q",
				tt.diff, styleID(got), styleID(tt.want))
		}
	}
}

func TestGaugeStyle(t *testing.T) {
	tests := []struct {
		pos  int
		want lipgloss.Style
	}{
		{-1, dimStyle},    // unknown → dim
		{0, redStyle},     // severe overburn → red
		{1, redStyle},     // moderate overburn → red
		{2, redStyle},     // mild overburn → red
		{3, greenStyle},   // on pace → green
		{4, yellowStyle},  // mild underburn → yellow
		{5, yellowStyle},  // moderate underburn → yellow
		{6, yellowStyle},  // severe underburn → yellow
	}
	for _, tt := range tests {
		got := gaugeStyle(tt.pos)
		if styleID(got) != styleID(tt.want) {
			t.Errorf("gaugeStyle(%d): got style rendering %q, want %q",
				tt.pos, styleID(got), styleID(tt.want))
		}
	}
}

func TestResetStyle(t *testing.T) {
	// Use a fixed period of 1000s and a fixed nowEpoch of 0.
	const periodS = int64(1000)
	const nowEpoch = int64(0)

	tests := []struct {
		name       string
		pct        int
		resetEpoch int64
		want       lipgloss.Style
	}{
		// pct <= 0 always → boldRed regardless of timer
		{"pct zero", 0, 800, boldRedStyle},
		{"pct negative", -1, 800, boldRedStyle},

		// periodS > 0: style determined by remaining time percentage
		// remainingPct = (resetEpoch - nowEpoch) * 100 / periodS
		// resetEpoch=510 → remainingPct=51 > 50 → green
		{"51pct remaining time → green", 50, 510, greenStyle},
		// resetEpoch=500 → remainingPct=50, not > 50 → check > 20: yes → yellow
		{"50pct remaining time → yellow", 50, 500, yellowStyle},
		// resetEpoch=210 → remainingPct=21 > 20 → yellow
		{"21pct remaining time → yellow", 50, 210, yellowStyle},
		// resetEpoch=200 → remainingPct=20, not > 20 → red
		{"20pct remaining time → red", 50, 200, redStyle},
		// resetEpoch=0 → remainingPct=0 → red
		{"0pct remaining time → red", 50, 0, redStyle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resetStyle(tt.pct, tt.resetEpoch, nowEpoch, periodS)
			if styleID(got) != styleID(tt.want) {
				t.Errorf("resetStyle(pct=%d, reset=%d, now=%d, period=%d): got %q, want %q",
					tt.pct, tt.resetEpoch, nowEpoch, periodS,
					styleID(got), styleID(tt.want))
			}
		})
	}

	t.Run("periodS zero returns dim", func(t *testing.T) {
		got := resetStyle(50, 500, 0, 0)
		if styleID(got) != styleID(dimStyle) {
			t.Errorf("resetStyle with periodS=0: got %q, want dim style %q",
				styleID(got), styleID(dimStyle))
		}
	})

	t.Run("periodS negative returns dim", func(t *testing.T) {
		got := resetStyle(50, 500, 0, -1)
		if styleID(got) != styleID(dimStyle) {
			t.Errorf("resetStyle with periodS<0: got %q, want dim style %q",
				styleID(got), styleID(dimStyle))
		}
	})
}
