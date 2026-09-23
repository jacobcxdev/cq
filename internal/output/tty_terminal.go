package output

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

// QuotaReportTerminalV2 presents a completed report without querying providers
// or changing the arithmetic and status already established by the runner.
func QuotaReportTerminalV2(report app.Report, options cli.TerminalOptions) string {
	if options.Now.IsZero() {
		options.Now = report.GeneratedAt
	}
	var blocks []string
	for _, source := range report.Providers {
		pr := source
		pr.Results = append([]quota.Result(nil), source.Results...)
		sort.SliceStable(pr.Results, func(i, j int) bool { return pr.Results[i].Active && !pr.Results[j].Active })
		for _, row := range pr.Results {
			blocks = append(blocks, terminalQuotaResult(pr.ID, row, report.GeneratedAt, options))
		}
		if len(pr.Results) == 0 {
			blocks = append(blocks, cli.TerminalLine(options, 0, cli.Span{Text: providerDisplayName(pr.ID), Role: cli.Heading}, cli.Span{Text: " · unavailable", Role: cli.Attention}))
		}
		if pr.Aggregate != nil {
			blocks = append(blocks, terminalQuotaAggregate(pr.Aggregate, providerDisplayName(pr.ID), options))
		}
		if pr.ProxyEligibility != nil && pr.ProxyEligibility.ExcludedCount > 0 && pr.ProxyEligibility.Aggregate != nil {
			blocks = append(blocks, terminalQuotaAggregate(pr.ProxyEligibility.Aggregate, "Proxy", options))
		}
		for _, pool := range pr.ProxyPools {
			if pool.Aggregate != nil {
				blocks = append(blocks, terminalQuotaAggregate(pool.Aggregate, pool.Name, options))
			}
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	out := strings.Join(blocks, "\n")
	if !report.GeneratedAt.IsZero() && terminalQuotaFresh(report) {
		out += "\n" + cli.TerminalLine(options, 0, cli.Span{Text: "Observed "}, cli.TerminalTimestamp(report.GeneratedAt, options))
	}
	return out
}

func terminalQuotaFresh(report app.Report) bool {
	for _, pr := range report.Providers {
		for _, r := range pr.Results {
			if r.IsUsable() && r.CacheAge == 0 {
				return true
			}
		}
	}
	return false
}

func terminalQuotaResult(id provider.ID, r quota.Result, observedAt time.Time, options cli.TerminalOptions) string {
	name := providerDisplayName(id)
	var title []cli.Span
	title = append(title, cli.Span{Text: name, Role: cli.Heading})
	if !r.IsUsable() {
		state, role := "Unavailable", cli.Attention
		if r.Error != nil {
			switch r.Error.Code {
			case "not_configured":
				state, role = "not configured", cli.Secondary
			case "no_token", "auth_expired":
				state, role = "Authentication required", cli.Failed
			default:
				state, role = "Failed", cli.Failed
			}
		}
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: state, Role: role})
		out := cli.TerminalLine(options, 0, title...)
		if state == "not configured" {
			return out
		}
		if r.AccountID != "" {
			out += cli.TerminalLine(options, 2, cli.Span{Text: r.AccountID, Role: cli.Identity})
		} else if r.Email != "" {
			out += cli.TerminalLine(options, 2, cli.Span{Text: r.Email, Role: cli.Identity})
		}
		if r.Error != nil && r.Error.Message != "" {
			out += cli.TerminalLine(options, 2, cli.Span{Text: r.Error.Message})
		}
		return out
	}
	label := r.Plan
	if label == "" {
		label = r.Tier
	}
	if label != "" && label != "null" {
		title = append(title, cli.Span{Text: " "}, cli.Span{Text: label, Role: cli.Identity})
		if multiplier := quota.ExtractMultiplier(r.RateLimitTier); multiplier > 1 {
			title = append(title, cli.Span{Text: fmt.Sprintf(" %dx", multiplier), Role: cli.Quantity})
		}
	}
	if r.Email != "" {
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: r.Email, Role: cli.Identity})
	} else if r.AccountID != "" {
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: r.AccountID, Role: cli.Identity})
	}
	if r.CacheAge > 0 && r.Error != nil {
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: "Stale", Role: cli.Attention}, cli.Span{Text: " · "}, cli.Span{Text: "refresh failed", Role: cli.Attention})
	} else if r.CacheAge > 0 {
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: "Cached", Role: cli.Attention})
	} else if r.Active {
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: "Native default", Role: cli.Identity})
	}
	if r.Status == quota.StatusExhausted {
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: "Exhausted", Role: cli.Attention})
	}
	out := cli.TerminalLine(options, 0, title...)
	currentBucket := ""
	for _, name := range orderedWindowKeys(r.Windows) {
		bucket := quota.WindowBucket(name)
		if bucket != "" && bucket != currentBucket {
			out += cli.TerminalLine(options, 2, cli.Span{Text: bucket, Role: cli.Heading})
		}
		currentBucket = bucket
		out += terminalQuotaWindow(name, r.Windows[name], options, r.CacheAge == 0)
	}
	if r.CacheAge > 0 {
		if observedAt.IsZero() {
			observedAt = options.Now
		}
		observed := observedAt.Add(-time.Duration(r.CacheAge) * time.Second)
		out += cli.TerminalLine(options, 2, cli.Span{Text: "Cached observation: "}, terminalQuotaDetailedTime(observed, options, false))
	}
	return out
}

func terminalQuotaWindow(name quota.WindowName, w quota.Window, options cli.TerminalOptions, detailGap bool) string {
	label := string(quota.BaseWindow(name))
	if label == "" {
		label = string(name)
	}
	reset := "unknown"
	resetRole := cli.Attention
	pace, hasPace := 0, false
	if w.ResetAtUnix > 0 {
		when := time.Unix(w.ResetAtUnix, 0)
		reset = terminalQuotaRelative(when, options)
		resetRole = cli.TimeValue
		if period := periodSeconds(name); period > 0 {
			pace = calcPace(period, w.ResetAtUnix, options.Now.Unix())
			hasPace = true
		}
	}
	paceMarker := -1
	if hasPace {
		paceMarker = pace
	}
	bar := terminalQuotaBar(w.RemainingPct, paceMarker)
	var out string
	if options.Width >= 100 {
		spans := []cli.Span{{Text: fmt.Sprintf("  %-2s  ", label), Role: cli.Identity}, {Text: bar + "  "}, {Text: "Remaining "}, cli.TerminalPercent(w.RemainingPercent(), false), {Text: "  Reset "}, {Text: reset, Role: resetRole}}
		if hasPace {
			spans = append(spans, cli.Span{Text: "  Pace "}, cli.Span{Text: fmt.Sprintf("%+dpp", w.RemainingPct-pace), Role: cli.Quantity})
			if burn, ok := w.Burndown(periodSeconds(name), options.Now.Unix()); ok {
				spans = append(spans, cli.Span{Text: "  Empty "}, cli.Span{Text: fmtDuration(burn), Role: cli.TimeValue})
			} else {
				spans = append(spans, cli.Span{Text: "  Empty "}, cli.Span{Text: "unknown", Role: cli.Attention})
			}
		} else {
			spans = append(spans, cli.Span{Text: "  Pace "}, cli.Span{Text: "unknown", Role: cli.Attention}, cli.Span{Text: "  Empty "}, cli.Span{Text: "unknown", Role: cli.Attention})
		}
		out = cli.TerminalLine(options, 0, spans...)
	} else {
		out = cli.TerminalLine(options, 2, cli.Span{Text: label, Role: cli.Heading})
		out += cli.TerminalLine(options, 4, cli.Span{Text: "Remaining "}, cli.TerminalPercent(w.RemainingPercent(), false))
		out += cli.TerminalLine(options, 4, cli.Span{Text: "Reset "}, cli.Span{Text: reset, Role: resetRole})
		if hasPace {
			out += cli.TerminalLine(options, 4, cli.Span{Text: "Pace "}, cli.Span{Text: fmt.Sprintf("%+dpp", w.RemainingPct-pace), Role: cli.Quantity})
			if burn, ok := w.Burndown(periodSeconds(name), options.Now.Unix()); ok {
				out += cli.TerminalLine(options, 4, cli.Span{Text: "Empty "}, cli.Span{Text: fmtDuration(burn), Role: cli.TimeValue})
			} else {
				out += cli.TerminalLine(options, 4, cli.Span{Text: "Empty "}, cli.Span{Text: "unknown", Role: cli.Attention})
			}
		} else {
			out += cli.TerminalLine(options, 4, cli.Span{Text: "Pace "}, cli.Span{Text: "unknown", Role: cli.Attention})
			out += cli.TerminalLine(options, 4, cli.Span{Text: "Empty "}, cli.Span{Text: "unknown", Role: cli.Attention})
		}
	}
	if w.ResetAtUnix > 0 {
		// The full timestamp supplements, rather than replaces, the countdown.
		if detailGap {
			out += "\n"
		}
		out += cli.TerminalLine(options, 2, cli.Span{Text: "Reset: "}, cli.TerminalTime(time.Unix(w.ResetAtUnix, 0), options, true))
	}
	return out
}

func terminalQuotaDetailedTime(when time.Time, options cli.TerminalOptions, deadline bool) cli.Span {
	relative, _, _ := strings.Cut(cli.TerminalTime(when, options, deadline).Text, " · ")
	return cli.Span{Text: relative + " · " + cli.TerminalTimestamp(when, options).Text, Role: cli.TimeValue}
}

func terminalQuotaBar(pct, expected int) string {
	filled := int(math.Round(float64(max(0, min(pct, 100))) * barWidth / 100))
	bar := strings.Repeat("━", filled) + strings.Repeat("╌", barWidth-filled)
	if expected < 0 {
		return bar
	}
	marker := max(0, min(expected*barWidth/100, barWidth))
	runes := []rune(bar)
	return string(runes[:marker]) + "|" + string(runes[marker:])
}

func terminalQuotaRelative(when time.Time, options cli.TerminalOptions) string {
	value := cli.TerminalTime(when, options, true).Text
	value, _, _ = strings.Cut(value, " · ")
	return strings.TrimPrefix(value, "in ")
}

func terminalQuotaAggregate(agg *app.AggregateReport, name string, options cli.TerminalOptions) string {
	title := []cli.Span{{Text: name, Role: cli.Heading}}
	if agg.Summary.Label != "" {
		title = append(title, cli.Span{Text: " "}, cli.Span{Text: agg.Summary.Label, Role: cli.Quantity})
	}
	out := cli.TerminalLine(options, 0, title...)
	currentBucket := ""
	for _, key := range orderedAggWindowKeys(agg.Windows) {
		bucket := quota.WindowBucket(key)
		if bucket != "" && bucket != currentBucket {
			out += cli.TerminalLine(options, 2, cli.Span{Text: bucket, Role: cli.Heading})
		}
		currentBucket = bucket
		a := agg.Windows[key]
		risk, role := "On pace", cli.Confirmed
		switch {
		case a.GaugePos < 0:
			risk, role = "unknown", cli.Attention
		case a.GaugePos < 3:
			risk, role = "Overburn", cli.Failed
		case a.GaugePos > 3:
			risk, role = "Projected waste", cli.Attention
		}
		if a.GaugeOverride == "imminent_block" {
			risk, role = "Imminent block", cli.Failed
		}
		if a.RemainingPct <= 0 {
			risk, role = "Exhausted", cli.Attention
		}
		bar := terminalQuotaBar(a.RemainingPct, a.ExpectedPct)
		gauge := ansi.Strip(renderSustainGauge(a.GaugePos, a.RemainingPct <= 0))
		if options.Width >= 100 {
			out += cli.TerminalLine(options, 0, cli.Span{Text: fmt.Sprintf("  %-2s  ", quota.BaseWindow(key)), Role: cli.Identity}, cli.Span{Text: bar + "  "}, cli.Span{Text: "Remaining "}, cli.TerminalPercent(float64(a.RemainingPct), false), cli.Span{Text: "  Gauge "}, cli.Span{Text: gauge, Role: role})
		} else {
			out += cli.TerminalLine(options, 2, cli.Span{Text: string(quota.BaseWindow(key)), Role: cli.Heading})
			out += cli.TerminalLine(options, 4, cli.Span{Text: "Remaining "}, cli.TerminalPercent(float64(a.RemainingPct), false))
			out += cli.TerminalLine(options, 4, cli.Span{Text: "Gauge "}, cli.Span{Text: gauge, Role: role})
		}
		spans := append([]cli.Span{{Text: risk, Role: role}}, terminalQuotaAggregateDetail(a)...)
		out += cli.TerminalLine(options, 2, spans...)
	}
	return out
}

func terminalQuotaAggregateDetail(a quota.AggregateResult) []cli.Span {
	switch {
	case a.GaugePos >= 0 && a.GaugePos < 3:
		if a.GapStartS > 0 || a.GapDurationS > 0 {
			return []cli.Span{{Text: " · dry gap "}, {Text: fmtDuration(a.GapDurationS), Role: cli.TimeValue}, {Text: ", starting in "}, {Text: fmtDuration(a.GapStartS), Role: cli.TimeValue}}
		}
	case a.GaugePos >= 4:
		if a.WasteDeadlineS > 0 {
			return []cli.Span{{Text: " · "}, cli.TerminalPercent(float64(a.WastedPct), false), {Text: " wasted in "}, {Text: fmtDuration(a.WasteDeadlineS), Role: cli.TimeValue}}
		}
	}
	return nil
}
