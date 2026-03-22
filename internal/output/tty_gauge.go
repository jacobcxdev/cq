package output

import (
	"strings"
)

const gaugeWidth = 7

// renderSustainGauge returns a 7-character sustainability gauge string.
// The gauge shows a marker at the given position; dim suppresses colour.
// pos -1 means unknown (no marker placed).
func renderSustainGauge(pos int, dim bool) string {
	var b strings.Builder
	b.Grow(gaugeWidth * 12)
	for i := 0; i < gaugeWidth; i++ {
		if pos >= 0 && pos < gaugeWidth && i == pos {
			style := gaugeStyle(pos)
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
