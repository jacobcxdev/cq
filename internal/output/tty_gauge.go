package output

import (
	"math"
	"strings"
)

const gaugeWidth = 7

// renderSustainGauge returns a 7-character sustainability gauge string.
// The gauge shows a marker at the sustainability position; dim suppresses colour.
func renderSustainGauge(s float64, dim bool) string {
	var b strings.Builder
	b.Grow(gaugeWidth * 12)
	for i := 0; i < gaugeWidth; i++ {
		if s >= 0 && i == sustainGaugePos(s) {
			style := sustainStyle(s)
			if dim {
				style = dimStyle
			}
			b.WriteString(style.Render("\u2502"))
		} else {
			b.WriteString(dimStyle.Render("\u2500"))
		}
	}
	return b.String()
}

// sustainGaugePos maps a sustainability factor to a gauge position (0-6).
// Position 3 is "on pace" (s=1.0), lower positions indicate overconsumption,
// higher positions indicate underconsumption.
func sustainGaugePos(s float64) int {
	if s <= 0 {
		return 0
	}
	pos := 3 + int(math.Round(math.Log2(s)))
	if pos < 0 {
		return 0
	}
	if pos > 6 {
		return 6
	}
	return pos
}
