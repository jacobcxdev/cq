package output

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

var icons = map[provider.ID]string{
	provider.Claude: "\u273B", // ✻
	provider.Codex:  "\uF120", //
	provider.Gemini: "\uF51B", //
}

// BuildTTYModel converts a Report into a structured TTYModel ready for rendering.
// It is a pure function — no I/O. The returned model contains pre-styled strings.
func BuildTTYModel(report app.Report, now time.Time) TTYModel {
	nowEpoch := now.Unix()
	sepWidth := calcSepWidth(report)

	model := TTYModel{
		Sections: make([]TTYSection, 0, len(report.Providers)),
	}

	closingSep := buildSeparator(sepWidth)

	for _, pr := range report.Providers {
		section := TTYSection{}

		// Separator before every provider (including first)
		section.Separator = buildSeparator(sepWidth)

		// Build per-result rows
		for _, result := range pr.Results {
			header, rows := buildResultBlock(result, pr.ID, nowEpoch)
			if section.Header == "" {
				// First result provides the section header
				section.Header = header
				section.WindowRows = rows
			} else {
				// Additional results within the same provider get no separator
				model.Sections = append(model.Sections, section)
				section = TTYSection{
					Header:     header,
					WindowRows: rows,
				}
			}
		}

		// Aggregate rows
		if pr.Aggregate != nil {
			section.ThinSep = buildThinSeparator(sepWidth)
			section.AggHeader = buildAggHeader(pr.Aggregate)
			section.AggRows = buildAggRows(pr.Aggregate.Windows)
		}

		model.Sections = append(model.Sections, section)
	}

	if len(model.Sections) > 0 {
		model.ClosingSeparator = closingSep
	}

	return model
}

// buildResultBlock builds the header and window rows for a single quota.Result.
func buildResultBlock(r quota.Result, id provider.ID, nowEpoch int64) (string, []TTYWindowRow) {
	displayName := providerDisplayName(id)
	icon := providerIcon(id)

	if !r.IsUsable() {
		errMsg := "unknown"
		if r.Error != nil && r.Error.Message != "" {
			errMsg = r.Error.Message
		} else if r.Error != nil && r.Error.Code != "" {
			errMsg = r.Error.Code
		}
		header := fmt.Sprintf("  %s  %7s %s", icon, displayName, dimStyle.Render(errMsg))
		return header, nil
	}

	// Header
	minPct := r.MinRemainingPct()
	iconStyle := pctStyle(minPct)
	if r.Status == quota.StatusExhausted {
		iconStyle = redStyle
	}

	header := fmt.Sprintf("  %s  %s",
		iconStyle.Render(icon),
		boldStyle.Render(fmt.Sprintf("%7s", displayName)),
	)

	label := r.Plan
	if label == "" {
		label = r.Tier
	}
	if label != "" && label != "null" {
		if r.RateLimitTier != "" {
			if m := quota.ExtractMultiplier(r.RateLimitTier); m > 1 {
				label += fmt.Sprintf(" %dx", m)
			}
		}
		header += " " + boldDimItalicStyle.Render(label)
	}

	if r.Email != "" {
		header += " " + brightBlackStyle.Render(fmt.Sprintf("\u00b7 %s", r.Email))
	}

	// Window rows
	isExhausted := r.Status == quota.StatusExhausted
	var barForce lipgloss.Style
	if isExhausted {
		barForce = redStyle
	}

	keys := orderedWindowKeys(r.Windows)
	rows := make([]TTYWindowRow, 0, len(keys))
	for _, winName := range keys {
		w := r.Windows[winName]
		periodS := periodSeconds(winName)
		if w.ResetAtUnix <= 0 && periodS > 0 && !isExhausted {
			w.ResetAtUnix = quota.DefaultResetEpoch(periodS, nowEpoch)
		}

		row := TTYWindowRow{
			Label: fmt.Sprintf("       %5s  ", winName),
		}

		// Bar with pace marker
		var pace int
		hasPace := false
		if periodS > 0 && w.ResetAtUnix > 0 {
			pace = calcPace(periodS, w.ResetAtUnix, nowEpoch)
			hasPace = true
		}
		pacePtr := -1
		if hasPace {
			pacePtr = pace
		}
		row.Bar = renderBar(w.RemainingPct, barForce, pacePtr)

		// Percentage
		pc := pctStyle(w.RemainingPct)
		if isExhausted {
			pc = dimStyle
		}
		row.Pct = pc.Render(fmt.Sprintf("\U000F0A9F %3d%%", w.RemainingPct))

		// Reset time
		if w.ResetAtUnix > 0 {
			rel := fmtDuration(w.ResetAtUnix - nowEpoch)
			rc := resetStyle(w.RemainingPct, w.ResetAtUnix, nowEpoch, periodS)
			if isExhausted && w.RemainingPct > 0 {
				rc = dimStyle
			}
			row.Reset = rc.Render(fmt.Sprintf("\U000F0996 %-7s", rel))
		} else {
			row.Reset = dimStyle.Render(fmt.Sprintf("\U000F0996 %-7s", "\u2014"))
		}

		// Pace diff + burndown
		if hasPace {
			diff := w.RemainingPct - pace
			dc := diffStyle(diff)
			if isExhausted {
				dc = dimStyle
			}
			row.PaceDiff = dc.Render(fmt.Sprintf("\U000F04C5 %+4d", diff))

			burn := ""
			if periodS > 0 && w.ResetAtUnix > 0 {
				if b, ok := calcBurndown(periodS, w.ResetAtUnix, nowEpoch, w.RemainingPct); ok {
					burn = fmtDuration(b)
				}
			}
			if burn != "" {
				row.Burndown = dc.Render(fmt.Sprintf("\U000F0152 %-7s", burn))
			} else {
				row.Burndown = dc.Render(fmt.Sprintf("\U000F0152 %-7s", "\u2014"))
			}
		} else {
			row.PaceDiff = dimStyle.Render(fmt.Sprintf("\U000F04C5 %4s", "\u2014"))
			row.Burndown = dimStyle.Render(fmt.Sprintf("\U000F0152 %-7s", "\u2014"))
		}

		rows = append(rows, row)
	}

	return header, rows
}

// buildAggHeader builds the aggregate header line.
func buildAggHeader(agg *app.AggregateReport) string {
	minPct := 100
	for _, a := range agg.Windows {
		if a.RemainingPct < minPct {
			minPct = a.RemainingPct
		}
	}
	iconStyle := pctStyle(minPct)
	label := "\u0192(n)"
	if agg.Summary.Label != "" {
		label = agg.Summary.Label
	}
	header := fmt.Sprintf("  %s  %s %s",
		iconStyle.Render(providerIcon(agg.ProviderID)),
		boldStyle.Render(fmt.Sprintf("%7s", providerDisplayName(agg.ProviderID))),
		boldDimItalicStyle.Render(label),
	)
	return header
}

// buildAggRows builds window rows for aggregate data.
func buildAggRows(windows map[quota.WindowName]quota.AggregateResult) []TTYWindowRow {
	orderedKeys := quota.OrderedWindows()
	rows := make([]TTYWindowRow, 0, len(orderedKeys))
	for _, winName := range orderedKeys {
		a, ok := windows[winName]
		if !ok {
			continue
		}

		row := TTYWindowRow{
			Label: fmt.Sprintf("       %5s  ", winName),
		}

		var barForce lipgloss.Style
		if a.RemainingPct <= 0 {
			barForce = redStyle
		}
		row.Bar = renderBar(a.RemainingPct, barForce, a.ExpectedPct)

		// Percentage
		pc := pctStyle(a.RemainingPct)
		if a.RemainingPct <= 0 {
			pc = dimStyle
		}
		row.Pct = pc.Render(fmt.Sprintf("\U000F0A9F %3d%%", a.RemainingPct))

		// Sustainability gauge (replaces reset time slot)
		isDim := a.RemainingPct <= 0
		sc := gaugeStyle(a.GaugePos)
		if isDim {
			sc = dimStyle
		}
		row.Reset = sc.Render("\U000F029A") + " " + renderSustainGauge(a.GaugePos, isDim)

		// Pace diff
		dc := diffStyle(a.PaceDiff)
		if a.RemainingPct <= 0 {
			dc = dimStyle
		}
		row.PaceDiff = dc.Render(fmt.Sprintf("\U000F04C5 %+4d", a.PaceDiff))

		// Burndown: positive means time until exhaustion; 0 is ambiguous (could be
		// "already exhausted" or "no data"), so both render as em-dash. Per-result
		// burndown uses calcBurndown's separate bool to distinguish these cases.
		if a.Burndown > 0 {
			row.Burndown = dc.Render(fmt.Sprintf("\U000F0152 %-7s", fmtDuration(a.Burndown)))
		} else {
			row.Burndown = dc.Render(fmt.Sprintf("\U000F0152 %-7s", "\u2014"))
		}

		rows = append(rows, row)
	}
	return rows
}

// buildSeparator returns a thick em-dash separator line.
func buildSeparator(width int) string {
	return dimStyle.Render(strings.Repeat("\u2014", width))
}

// buildThinSeparator returns a thin separator line indented by 2 spaces.
func buildThinSeparator(width int) string {
	return "  " + dimStyle.Render(strings.Repeat("-", max(0, width-2)))
}

// calcSepWidth calculates the separator width from the widest line in the report.
func calcSepWidth(report app.Report) int {
	maxW := windowLineWidth()
	for _, pr := range report.Providers {
		for _, r := range pr.Results {
			if w := headerVisibleWidth(r, pr.ID); w > maxW {
				maxW = w
			}
		}
	}
	return maxW
}

// windowLineWidth returns the character count of a fully populated window line.
func windowLineWidth() int {
	// indent(7) + winName(5) + gap(2) + bar(20) +
	// 4x metric: gap(2) + icon(1) + space(1) + field
	// fields: pct(4) + duration(7) + diff(4) + duration(7)
	return 7 + 5 + 2 + barWidth + 4*(2+1+1) + 4 + 7 + 4 + 7
}

// headerVisibleWidth calculates the visible character width of a header line.
func headerVisibleWidth(r quota.Result, id provider.ID) int {
	// "  <icon>  <7-char name>" = 2 + icon(1) + 2 + len(displayName padded to 7) = 12 base
	w := 12
	label := r.Plan
	if label == "" {
		label = r.Tier
	}
	if label != "" && label != "null" {
		w += 1 + utf8.RuneCountInString(label)
		if r.RateLimitTier != "" {
			if m := quota.ExtractMultiplier(r.RateLimitTier); m > 1 {
				w += utf8.RuneCountInString(fmt.Sprintf(" %dx", m))
			}
		}
	}
	if r.Email != "" {
		w += 3 + utf8.RuneCountInString(r.Email) // " · <email>"
	}
	return w
}

// providerDisplayName returns the capitalised display name for a provider ID.
func providerDisplayName(id provider.ID) string {
	name := string(id)
	if len(name) == 0 {
		return ""
	}
	r, size := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(r)) + name[size:]
}

// providerIcon returns the icon for a provider ID, defaulting to a bullet.
func providerIcon(id provider.ID) string {
	if icon, ok := icons[id]; ok {
		return icon
	}
	return "\u25CF" // ●
}

// orderedWindowKeys returns window names in canonical display order, filtered
// to only those present in the given map.
func orderedWindowKeys(windows map[quota.WindowName]quota.Window) []quota.WindowName {
	all := quota.OrderedWindows()
	keys := make([]quota.WindowName, 0, len(windows))
	for _, k := range all {
		if _, ok := windows[k]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}
