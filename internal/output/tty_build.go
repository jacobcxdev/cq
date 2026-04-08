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
// Sections are built first, then separators are sized to the widest rendered line.
func BuildTTYModel(report app.Report, now time.Time) TTYModel {
	nowEpoch := now.Unix()

	model := TTYModel{
		Sections: make([]TTYSection, 0, len(report.Providers)),
	}

	// Track which sections are provider-first (get separator) vs continuation (no separator).
	var providerFirstIdx []int
	for _, pr := range report.Providers {
		section := TTYSection{}
		isFirst := true

		for _, result := range pr.Results {
			header, rows := buildResultBlock(result, pr.ID, nowEpoch)
			if section.Header == "" {
				section.Header = header
				section.WindowRows = rows
			} else {
				if isFirst {
					providerFirstIdx = append(providerFirstIdx, len(model.Sections))
					isFirst = false
				}
				model.Sections = append(model.Sections, section)
				section = TTYSection{
					Header:     header,
					WindowRows: rows,
				}
			}
		}

		if pr.Aggregate != nil {
			section.AggHeader = buildAggHeader(pr.Aggregate)
			section.AggRows = buildAggRows(pr.Aggregate.Windows)
		}

		if isFirst {
			providerFirstIdx = append(providerFirstIdx, len(model.Sections))
		}
		model.Sections = append(model.Sections, section)
	}

	// Measure actual max width across all rendered content.
	sepWidth := measuredSepWidth(model)

	// Insert separators only on provider-first sections.
	firstSet := make(map[int]bool, len(providerFirstIdx))
	for _, idx := range providerFirstIdx {
		firstSet[idx] = true
	}
	for i := range model.Sections {
		if firstSet[i] {
			model.Sections[i].Separator = buildSeparator(sepWidth)
		}
		if model.Sections[i].AggHeader != "" {
			model.Sections[i].ThinSep = buildThinSeparator(sepWidth)
		}
	}
	if len(model.Sections) > 0 {
		model.ClosingSeparator = buildSeparator(sepWidth)
	}

	return model
}

// buildResultBlock builds the header and window rows for a single quota.Result.
func buildResultBlock(r quota.Result, id provider.ID, nowEpoch int64) (string, []TTYWindowRow) {
	displayName := providerDisplayName(id)
	icon := providerIcon(id)

	if !r.IsUsable() {
		errMsg := "error"
		if r.Error != nil && r.Error.Message != "" {
			errMsg = r.Error.Message
			if r.Error.HTTPStatus > 0 {
				errMsg = fmt.Sprintf("%s %d", errMsg, r.Error.HTTPStatus)
			}
		} else if r.Error != nil && r.Error.Code != "" {
			errMsg = strings.ReplaceAll(r.Error.Code, "_", " ")
			if r.Error.HTTPStatus > 0 {
				errMsg = fmt.Sprintf("%s %d", errMsg, r.Error.HTTPStatus)
			}
		}

		header := fmt.Sprintf("  %s  %s",
			boldRedStyle.Render(icon),
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
		header += " " + brightBlackStyle.Render("\u00b7") + " " + boldRedStyle.Render(errMsg)
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

	if r.CacheAge > 0 && r.Error != nil {
		errMsg := "error"
		if r.Error.Message != "" {
			errMsg = r.Error.Message
			if r.Error.HTTPStatus > 0 {
				errMsg = fmt.Sprintf("%s %d", errMsg, r.Error.HTTPStatus)
			}
		} else if r.Error.Code != "" {
			errMsg = strings.ReplaceAll(r.Error.Code, "_", " ")
			if r.Error.HTTPStatus > 0 {
				errMsg = fmt.Sprintf("%s %d", errMsg, r.Error.HTTPStatus)
			}
		}
		header += " " + brightBlackStyle.Render("\u00b7") + " " +
			boldRedStyle.Render(errMsg) + " " +
			dimStyle.Render(fmt.Sprintf("\u2014 using cache from %s ago", fmtDuration(r.CacheAge)))
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
			row.Reset = rc.Render(fmt.Sprintf("\U000F0996 %7s", rel))
		} else {
			row.Reset = dimStyle.Render(fmt.Sprintf("\U000F0996 %7s", "\u2014"))
		}

		// Pace diff + burndown
		if hasPace {
			diff := w.RemainingPct - pace
			dc := diffStyle(diff)
			if isExhausted {
				dc = dimStyle
			}
			row.PaceDiff = dc.Render(fmt.Sprintf("\U000F04C5 %+7d", diff))

			burn := ""
			if periodS > 0 && w.ResetAtUnix > 0 {
				if b, ok := calcBurndown(periodS, w.ResetAtUnix, nowEpoch, w.RemainingPct); ok {
					burn = fmtDuration(b)
				}
			}
			if burn != "" {
				row.Burndown = dc.Render(fmt.Sprintf("\U000F0152 %7s", burn))
			} else {
				row.Burndown = dc.Render(fmt.Sprintf("\U000F0152 %7s", "\u2014"))
			}
		} else {
			row.PaceDiff = dimStyle.Render(fmt.Sprintf("\U000F04C5 %7s", "\u2014"))
			row.Burndown = dimStyle.Render(fmt.Sprintf("\U000F0152 %7s", "\u2014"))
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

		// Sustainability gauge (replaces reset time slot). When the gauge
		// was snapped to severe by the imminent-block override, swap the
		// reset glyph for an mdi-alert warning triangle so the user sees
		// the escalation even before reading the gauge.
		isDim := a.RemainingPct <= 0
		sc := gaugeStyle(a.GaugePos)
		if isDim {
			sc = dimStyle
		}
		gaugeIcon := "\U000F029A" // mdi-refresh
		if a.GaugeOverride == "imminent_block" && !isDim {
			gaugeIcon = "\U0000F071" // fa-warning
			sc = boldRedStyle
		}
		gauge := renderSustainGauge(a.GaugePos, isDim)
		if !isDim && a.PaceDiff <= -15 {
			gauge = renderSustainGaugeWithStyle(a.GaugePos, isDim, sc, true)
		}
		row.Reset = sc.Render(gaugeIcon) + " " + gauge

		// Gauge-contextual columns: impact + timing (icon + value format).
		gc := gaugeStyle(a.GaugePos)
		if a.RemainingPct <= 0 {
			gc = dimStyle
		}
		switch {
		case a.GaugePos >= 0 && a.GaugePos < 3: // overburn
			row.PaceDiff = gc.Render(fmt.Sprintf("\uEE8E %7s", fmtDuration(a.GapDurationS)))
			row.Burndown = gc.Render(fmt.Sprintf("\U000F0152 %7s", fmtDuration(a.GapStartS)))
		case a.GaugePos >= 4: // underburn
			row.PaceDiff = gc.Render(fmt.Sprintf("\uEFC7 %7s", fmt.Sprintf("%d%%", a.WastedPct)))
			if a.WasteDeadlineS > 0 {
				row.Burndown = gc.Render(fmt.Sprintf("\U000F1557 %7s", fmtDuration(a.WasteDeadlineS)))
			} else {
				row.Burndown = gc.Render(fmt.Sprintf("\U000F1557 %7s", "\u2014"))
			}
		default: // on pace or unknown
			row.PaceDiff = gc.Render(fmt.Sprintf("\U000F012C %7s", "\u2014"))
			row.Burndown = gc.Render(fmt.Sprintf("\U000F0152 %7s", "\u2014"))
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

// measuredSepWidth returns the widest visible line across all sections.
func measuredSepWidth(model TTYModel) int {
	maxW := 0
	for _, sec := range model.Sections {
		if w := visibleWidth(sec.Header); w > maxW {
			maxW = w
		}
		for _, row := range sec.WindowRows {
			if w := rowVisibleWidth(row); w > maxW {
				maxW = w
			}
		}
		if w := visibleWidth(sec.AggHeader); w > maxW {
			maxW = w
		}
		for _, row := range sec.AggRows {
			if w := rowVisibleWidth(row); w > maxW {
				maxW = w
			}
		}
	}
	return maxW
}

// rowVisibleWidth returns the visible character width of a rendered window row.
// Mirrors the concatenation in writeWindowRow.
func rowVisibleWidth(row TTYWindowRow) int {
	// row.Label + row.Bar + "  " + row.Pct + "  " + row.Reset + "  " + row.PaceDiff + "  " + row.Burndown
	return visibleWidth(row.Label) + visibleWidth(row.Bar) +
		2 + visibleWidth(row.Pct) +
		2 + visibleWidth(row.Reset) +
		2 + visibleWidth(row.PaceDiff) +
		2 + visibleWidth(row.Burndown)
}

// visibleWidth returns the number of visible characters in a styled string
// by stripping ANSI escape sequences.
func visibleWidth(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
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
