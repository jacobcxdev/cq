package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/output"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

var terminalCheckFixtures = []terminalFixtureRegistration{
	{"cq check", "Success", "stdout", "text/cq-check/02-success.txt"},
	{"cq check", "Partial with stale data", "stdout", "text/cq-check/03-partial-with-stale-data.txt"},
	{"cq check", "Partial with stale data", "stderr", "text/cq-check/04-partial-with-stale-data.txt"},
	{"cq check", "Failure", "stdout", "text/cq-check/05-failure.txt"},
	{"cq check", "Failure", "stderr", "text/cq-check/06-failure.txt"},
	{"cq check", "Timeout with no completed rows", "stderr", "text/cq-check/07-timeout-with-no-completed-rows.txt"},
}

func terminalCheckReport(stale bool) app.Report {
	now := time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC)
	recent := float64(89) / float64((33*24+22)*3600)
	row := quota.Result{
		AccountID: "account-alex", Email: "alex@example.test", Active: !stale,
		Status: quota.StatusOK, Plan: "pro", RateLimitTier: "codex_pro_20x",
		Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 89, ResetAtUnix: now.Add(67 * time.Hour).Unix(), RecentBurnRate: &recent}},
	}
	if stale {
		row.CacheAge = 240
		row.Error = &quota.ErrorInfo{Code: "fetch_error", Message: "Quota observation failed."}
	}
	return app.Report{GeneratedAt: now, Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{row}}}}
}

func TestTerminalCheckFixtures(t *testing.T) {
	o := terminalFixtureOptions(100)
	for _, f := range terminalCheckFixtures {
		t.Run(f.File, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", f.File))
			if err != nil {
				t.Fatal(err)
			}
			var got string
			switch f.File {
			case "text/cq-check/02-success.txt":
				got = output.QuotaReportTerminalV2(terminalCheckReport(false), o)
			case "text/cq-check/03-partial-with-stale-data.txt":
				got = output.QuotaReportTerminalV2(terminalCheckReport(true), o)
			case "text/cq-check/05-failure.txt":
				report := app.Report{GeneratedAt: o.Now, Providers: []app.ProviderReport{{ID: provider.Gemini, Results: []quota.Result{{AccountID: "antigravity-cli", Status: quota.StatusError, Error: &quota.ErrorInfo{Code: "no_token", Message: "Authentication is required."}}}}}}
				got = output.QuotaReportTerminalV2(report, o)
			case "text/cq-check/04-partial-with-stale-data.txt":
				got = terminalCheckDiagnostic(cli.Outcome{ExitCode: 8, Errors: []cli.Diagnostic{{Code: "check_partial", Message: "Some quota results could not be refreshed."}}, Warnings: []cli.Diagnostic{{Code: "quota_stale_fallback", Message: "Cached quota used after a failed fetch for codex."}}}, o)
			case "text/cq-check/06-failure.txt":
				got = terminalCheckDiagnostic(cli.Outcome{ExitCode: 5, Errors: []cli.Diagnostic{{Code: "check_authentication", Message: "Authentication is required for the selected providers."}}}, o)
			case "text/cq-check/07-timeout-with-no-completed-rows.txt":
				got = terminalCheckDiagnostic(cli.Outcome{ExitCode: 7, Errors: []cli.Diagnostic{{Code: "check_timeout", Message: "Quota checking timed out."}}}, o)
			}
			if ansi.Strip(got) != string(want) {
				t.Fatalf("got:\n%s\nwant:\n%s", ansi.Strip(got), want)
			}
		})
	}
}

func terminalCheckDiagnostic(outcome cli.Outcome, o cli.TerminalOptions) string {
	var stdout, stderr bytes.Buffer
	cli.Run(context.Background(), []string{"check"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, func(path string) (cli.Handler, bool) {
		return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome { return outcome }, path == "check"
	})
	return stderr.String()
}

func TestTerminalCheckAdapter(t *testing.T) {
	rows := []quota.Result{{AccountID: "usable", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 0}}}}
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: rows, provider.Claude: {quota.ErrorResult("not_configured", "not configured", 0)}, provider.Gemini: {quota.ErrorResult("not_configured", "not configured", 0)}})
	inv, err := cli.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := handleV2CheckWithDependencies(context.Background(), inv, nil, v2CheckDependencies{Prepare: func(context.Context, []provider.ID) (v2CheckPrepared, error) {
		return v2CheckPrepared{Runner: runner}, nil
	}})
	if got.ExitCode != 0 || got.Terminal == nil {
		t.Fatalf("exit=%d terminal=%v", got.ExitCode, got.Terminal != nil)
	}
	if s := ansi.Strip(got.Terminal(terminalFixtureOptions(20))); !strings.Contains(s, "0%") || !strings.Contains(s, "Claude · not\nconfigured") {
		t.Fatal(s)
	}
}

func TestTerminalCheckRegistration(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct{ Command, Scenario, Stream, File string }
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	registered := map[string]terminalFixtureRegistration{}
	for _, f := range terminalCheckFixtures {
		if _, exists := registered[f.File]; exists {
			t.Fatal("duplicate", f.File)
		}
		registered[f.File] = f
	}
	for _, f := range index.Fixtures {
		if f.Command != "cq check" || f.Scenario == "Help" {
			continue
		}
		r, ok := registered[f.File]
		if !ok || r.Command != f.Command || r.Scenario != f.Scenario || r.Stream != f.Stream {
			t.Fatal("missing or mismatched fixture", f.File)
		}
		delete(registered, f.File)
	}
	if len(registered) != 0 {
		t.Fatal("unrecognised fixtures", registered)
	}
}

func TestTerminalCheckRolesAndWidths(t *testing.T) {
	report := terminalCheckReport(true)
	report.Providers[0].Results[0].Email = "failed界@example.test"
	report.Providers[0].Results = append(report.Providers[0].Results, quota.Result{AccountID: "unknown-reset", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 0}}})
	for _, width := range []int{20, 40, 80, 120} {
		o := terminalFixtureOptions(width)
		got := output.QuotaReportTerminalV2(report, o)
		if !strings.Contains(got, "\x1b[36mfailed界") || !strings.Contains(got, "\x1b[1;34m89%") || !strings.Contains(got, "\x1b[33mStale") || !strings.Contains(got, "\x1b[35m4m") {
			t.Fatalf("missing semantic role at width %d: %q", width, got)
		}
		plain := ansi.Strip(got)
		if !strings.Contains(plain, "Reset unknown") || !strings.Contains(plain, "Empty unknown") || !strings.Contains(plain, "0%") {
			t.Fatalf("unknown or exhausted state lost at width %d: %q", width, plain)
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d exceeded by %q", width, line)
			}
		}
		o.Colour = false
		if colourless := output.QuotaReportTerminalV2(report, o); colourless != plain {
			t.Fatalf("NO_COLOR changed text at width %d", width)
		}
	}
}

func TestTerminalCheckAggregateGaugeAndReserveWindows(t *testing.T) {
	o := terminalFixtureOptions(100)
	agg := &app.AggregateReport{ProviderID: provider.Codex, Windows: map[quota.WindowName]quota.AggregateResult{
		"5h": {RemainingPct: 0, ExpectedPct: 20, GaugePos: 1, GapStartS: 3600, GapDurationS: 7200},
		"7d": {RemainingPct: 80, ExpectedPct: 40, GaugePos: 5, WastedPct: 20, WasteDeadlineS: 86400},
	}}
	agg.Summary.Label = "4 × pro 20x = 80x"
	report := app.Report{GeneratedAt: o.Now, Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{{
		AccountID: "reserve-account", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 68}, "7d:Reserve": {RemainingPct: 50}},
	}}, Aggregate: agg, ProxyEligibility: &app.ProxyEligibilityReport{ExcludedCount: 1, Aggregate: agg}}}}
	got := output.QuotaReportTerminalV2(report, o)
	plain := ansi.Strip(got)
	for _, want := range []string{"4 × pro 20x = 80x", "Exhausted", "Projected waste", "20% wasted in 1d", "Proxy", "Reserve", "unknown"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q: %s", want, plain)
		}
	}
	if strings.Contains(plain, "sustainability") || strings.Contains(got, "\x1b[31m0%") {
		t.Fatal("aggregate leaked raw arithmetic or coloured zero as a failure")
	}
}

func TestTerminalCheckProxyHeadersWithoutAggregates(t *testing.T) {
	for _, eligible := range []int{0, 1} {
		report := app.Report{Providers: []app.ProviderReport{{
			ID:               provider.Codex,
			Results:          []quota.Result{{AccountID: "one", Status: quota.StatusOK}},
			ProxyEligibility: &app.ProxyEligibilityReport{DiscoveredCount: 2, EligibleCount: eligible, ExcludedCount: 2 - eligible},
			ProxyPools:       []app.ProxyPoolReport{{Name: "团队", ProxyEligibilityReport: app.ProxyEligibilityReport{DiscoveredCount: 2, EligibleCount: eligible, ExcludedCount: 2 - eligible}}},
		}}}
		for _, width := range []int{20, 40, 80, 120} {
			got := ansi.Strip(output.QuotaReportTerminalV2(report, terminalFixtureOptions(width)))
			if !strings.Contains(got, "\nProxy\n") || !strings.Contains(got, "\n团队\n") {
				t.Fatalf("eligible=%d width=%d: missing proxy heading: %q", eligible, width, got)
			}
		}
	}
}

func TestTerminalCheckUnauthorizedOverridesNotConfigured(t *testing.T) {
	report := app.Report{Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{quota.ErrorResult("not_configured", "Provider is not configured.", http.StatusUnauthorized)}}}}
	outcome := v2CheckStatus(context.Background(), context.Background(), report, false)
	if outcome.ExitCode != 5 || outcome.Errors[0].Code != "check_authentication" {
		t.Fatalf("status = %+v", outcome)
	}
	got := ansi.Strip(output.QuotaReportTerminalV2(report, terminalFixtureOptions(100)))
	if !strings.Contains(got, "Codex · Authentication required") || !strings.Contains(got, "Authentication is required.") || strings.Contains(got, "not configured") {
		t.Fatal(got)
	}
}
