package output

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/quota"
)

// TTYRenderer renders a Report as styled terminal output.
type TTYRenderer struct {
	W   io.Writer
	Now time.Time
}

func (r *TTYRenderer) Render(_ context.Context, report app.Report) error {
	model := BuildTTYModel(report, r.Now)
	return writeTTY(r.W, model)
}

// errWriter wraps an io.Writer and captures the first error.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) write(s string) {
	if ew.err != nil {
		return
	}
	_, ew.err = io.WriteString(ew.w, s)
}

func writeTTY(w io.Writer, model TTYModel) error {
	ew := &errWriter{w: w}
	for i, section := range model.Sections {
		if section.Separator != "" {
			ew.write(section.Separator)
			ew.write("\n\n")
		} else {
			ew.write("\n")
		}
		ew.write(section.Header)
		ew.write("\n")

		for _, row := range section.WindowRows {
			writeWindowRow(ew, row)
		}

		if section.AggHeader != "" || section.ProxyHeader != "" || len(section.ProxyPools) > 0 {
			ew.write("\n")
			ew.write(section.ThinSep)
			ew.write("\n\n")
			if section.AggHeader != "" {
				ew.write(section.AggHeader)
				ew.write("\n")
				for _, row := range section.AggRows {
					writeWindowRow(ew, row)
				}
			}
			wroteBlock := section.AggHeader != ""
			if section.ProxyHeader != "" {
				if wroteBlock {
					ew.write("\n")
				}
				ew.write(section.ProxyHeader)
				ew.write("\n")
				for _, row := range section.ProxyRows {
					writeWindowRow(ew, row)
				}
				wroteBlock = true
			}
			for _, pool := range section.ProxyPools {
				if wroteBlock {
					ew.write("\n")
				}
				ew.write(pool.Header)
				ew.write("\n")
				for _, row := range pool.Rows {
					writeWindowRow(ew, row)
				}
				wroteBlock = true
			}
		}

		ew.write("\n")

		if i == len(model.Sections)-1 {
			ew.write(model.ClosingSeparator)
			ew.write("\n")
		}
	}
	return ew.err
}

func writeWindowRow(ew *errWriter, row TTYWindowRow) {
	if row.Bar == "" && row.Pct == "" && row.Reset == "" && row.PaceDiff == "" && row.Burndown == "" {
		ew.write("\n")
		ew.write(row.Label)
		ew.write("\n")
		return
	}

	ew.write(row.Label)
	ew.write(row.Bar)
	ew.write("  ")
	ew.write(row.Pct)
	ew.write("  ")
	ew.write(row.Reset)
	ew.write("  ")
	ew.write(row.PaceDiff)
	ew.write("  ")
	ew.write(row.Burndown)
	ew.write("\n")
}

// QuotaReportHumanV2 renders the plain, width-independent quota contract.
// The report already contains frozen arithmetic; this function only formats it.
func QuotaReportHumanV2(report app.Report) string {
	var out strings.Builder
	value := cli.HumanValue
	optional := func(s string) string {
		if s == "" {
			return "—"
		}
		return value(s)
	}
	for _, p := range report.Providers {
		fmt.Fprintf(&out, "%s\n  Availability: %s (%s)\n", value(p.Name), value(string(p.Availability.State)), value(p.Availability.Reason))
		for _, row := range p.Results {
			account := row.Email
			if account == "" {
				account = row.AccountID
			}
			if account == "" {
				account = "Unknown account"
			}
			active := ""
			if row.Active {
				active = " [active]"
			}
			fmt.Fprintf(&out, "  %s%s  %s\n", value(account), active, value(string(row.Status)))
			keys := make([]quota.WindowName, 0, len(row.Windows))
			for key := range row.Windows {
				keys = append(keys, key)
			}
			for _, key := range quota.OrderedWindowNames(keys) {
				window := row.Windows[key]
				reset := "—"
				if window.ResetAtUnix != 0 {
					reset = time.Unix(window.ResetAtUnix, 0).UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(&out, "    %s: %d%% remaining; reset %s\n", value(quota.DisplayWindowLabel(key)), window.RemainingPct, reset)
			}
			if row.Error != nil {
				fmt.Fprintf(&out, "    Error: %s", value(row.Error.Code))
				if row.Error.Message != "" {
					fmt.Fprintf(&out, " — %s", value(row.Error.Message))
				}
				out.WriteByte('\n')
			}
			if row.CacheAge > 0 {
				fmt.Fprintf(&out, "    Cache age: %ds\n", row.CacheAge)
			}
			if row.Plan != "" || row.Tier != "" || row.RateLimitTier != "" {
				fmt.Fprintf(&out, "    Plan: %s; tier: %s; rate-limit tier: %s\n", optional(row.Plan), optional(row.Tier), optional(row.RateLimitTier))
			}
		}
		writeV2QuotaAggregate(&out, p.Aggregate)
		if e := p.ProxyEligibility; e != nil {
			fmt.Fprintf(&out, "  Proxy eligibility: eligible=%d; excluded=%d; discovered=%d\n", e.EligibleCount, e.ExcludedCount, e.DiscoveredCount)
			writeV2QuotaAggregate(&out, e.Aggregate)
		}
		for _, pool := range p.ProxyPools {
			fmt.Fprintf(&out, "  Proxy pool %s: eligible=%d; excluded=%d; discovered=%d\n", value(pool.Name), pool.EligibleCount, pool.ExcludedCount, pool.DiscoveredCount)
			writeV2QuotaAggregate(&out, pool.Aggregate)
		}
	}
	return out.String()
}

func writeV2QuotaAggregate(out *strings.Builder, a *app.AggregateReport) {
	if a == nil {
		return
	}
	fmt.Fprintf(out, "  Aggregate (%s): %s; accounts=%d; capacity=%d\n", cli.HumanValue(a.Kind), cli.HumanValue(a.Summary.Label), a.Summary.Count, a.Summary.TotalMulti)
	seconds := func(n int64) string {
		if n <= 0 {
			return "—"
		}
		return strconv.FormatInt(n, 10) + "s"
	}
	keys := make([]quota.WindowName, 0, len(a.Windows))
	for key := range a.Windows {
		keys = append(keys, key)
	}
	for _, key := range quota.OrderedWindowNames(keys) {
		w := a.Windows[key]
		sustainability, gauge, waste, override := "—", "—", "—", "—"
		if w.Sustainability != 0 {
			sustainability = strconv.FormatFloat(w.Sustainability, 'f', -1, 64)
		}
		if w.GaugePos != -1 {
			gauge = strconv.Itoa(w.GaugePos)
		}
		if w.WastedPct > 0 {
			waste = strconv.Itoa(w.WastedPct) + "%"
		}
		if w.GaugeOverride != "" {
			override = cli.HumanValue(w.GaugeOverride)
		}
		fmt.Fprintf(out, "    %s: remaining=%d%%; expected=%d%%; pace=%+dpp; burndown=%s; sustainability=%s; gauge=%s; gap-start=%s; gap-duration=%s; waste=%s; waste-deadline=%s; override=%s\n", cli.HumanValue(string(key)), w.RemainingPct, w.ExpectedPct, w.PaceDiff, seconds(w.Burndown), sustainability, gauge, seconds(w.GapStartS), seconds(w.GapDurationS), waste, seconds(w.WasteDeadlineS), override)
	}
}
