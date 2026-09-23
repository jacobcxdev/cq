package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/output"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	"github.com/jacobcxdev/cq/internal/quota"
)

type v2QuotaClock struct{}

func (v2QuotaClock) Now() time.Time { return time.Unix(1700000000, 0).UTC() }

type v2QuotaProvider func(context.Context, time.Time) ([]quota.Result, error)

func (f v2QuotaProvider) Fetch(ctx context.Context, now time.Time) ([]quota.Result, error) {
	return f(ctx, now)
}
func init() {
	registerV2Fixture("quota-partial", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		deps := v2CheckDependencies{Prepare: func(ctx context.Context, ids []provider.ID) (v2CheckPrepared, error) {
			services := map[provider.ID]provider.Services{}
			for _, id := range ids {
				services[id] = provider.Services{Usage: v2QuotaProvider(func(context.Context, time.Time) ([]quota.Result, error) {
					f.Call("fetch:" + string(id))
					return []quota.Result{{AccountID: "a", Status: quota.StatusOK}, quota.ErrorResult("fetch_error", "upstream body", 500)}, nil
				})}
			}
			return v2CheckPrepared{Runner: &app.Runner{Clock: v2QuotaClock{}, Services: services}}, nil
		}}
		f.Lookup = func(path string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2CheckWithDependencies(ctx, inv, s, deps)
			}, path == "check"
		}
		return f
	})
}
func TestCLIV2CheckContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "partial report is not success", Scenario: "quota-partial", Args: []string{"check", "codex", "--json"}, Exit: 8, Command: "check", Code: "check_partial", Forbid: []string{"service", "consume", "credential-activate"}, Calls: map[string]int{"fetch:codex": 1}})
}

func quotaTestRunner(rows map[provider.ID][]quota.Result) *app.Runner {
	services := map[provider.ID]provider.Services{}
	for id, results := range rows {
		services[id] = provider.Services{Usage: v2QuotaProvider(func(context.Context, time.Time) ([]quota.Result, error) { return results, nil })}
	}
	return &app.Runner{Clock: v2QuotaClock{}, Services: services}
}
func quotaTestRun(t *testing.T, ctx context.Context, args []string, prepared v2CheckPrepared) (int, string, string) {
	t.Helper()
	var out, diagnostics bytes.Buffer
	deps := v2CheckDependencies{Prepare: func(context.Context, []provider.ID) (v2CheckPrepared, error) { return prepared, nil }}
	exit := cli.Run(ctx, args, &cli.Session{In: strings.NewReader(""), Out: &out, Err: &diagnostics, Interactive: true}, func(path string) (cli.Handler, bool) {
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2CheckWithDependencies(ctx, inv, s, deps)
		}, path == "check"
	})
	return exit, out.String(), diagnostics.String()
}
func quotaTestReport(t *testing.T, text string) app.Report {
	t.Helper()
	var envelope struct{ Data v2CheckData }
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.Report
}
func init() {
	registerV2Fixture("quota-exhausted", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		f.Lookup = func(path string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2CheckWithDependencies(ctx, inv, s, v2CheckDependencies{Prepare: func(context.Context, []provider.ID) (v2CheckPrepared, error) {
					f.Call("prepare")
					return v2CheckPrepared{Runner: quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {{Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{"5h": {RemainingPct: 0}}}}})}, nil
				}})
			}, path == "check"
		}
		return f
	})
	registerV2Fixture("quota-timeout", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Claude: {{Status: quota.StatusOK, AccountID: "completed"}}})
		runner.Services[provider.Codex] = provider.Services{Usage: v2QuotaProvider(func(ctx context.Context, _ time.Time) ([]quota.Result, error) {
			f.Call("fetch:codex")
			<-ctx.Done()
			return nil, ctx.Err()
		})}
		f.Lookup = func(path string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2CheckWithDependencies(ctx, inv, s, v2CheckDependencies{Prepare: func(context.Context, []provider.ID) (v2CheckPrepared, error) {
					return v2CheckPrepared{Runner: runner}, nil
				}})
			}, path == "check"
		}
		return f
	})
}
func TestCLIV2CheckExhaustedAndTimeoutFixtures(t *testing.T) {
	runV2Case(t, v2Case{Name: "exhausted is observed", Scenario: "quota-exhausted", Args: []string{"check", "codex", "--json"}, Exit: 0, Command: "check", WantJSON: `{"report":{"providers":[{"id":"codex","results":[{"status":"exhausted","windows":{"5h":{"remaining_pct":0}}}]}]}}`, Forbid: []string{"service", "consume", "credential-activate"}})
	runV2Case(t, v2Case{Name: "timeout keeps completed provider", Scenario: "quota-timeout", Args: []string{"check", "claude", "codex", "--timeout", "100ms", "--json"}, Exit: 7, Code: "check_timeout", Command: "check", WantJSON: `{"report":{"providers":[{"id":"claude","results":[{"account_id":"completed","status":"ok"}]},{"id":"codex","results":[]}]}}`, Forbid: []string{"service", "consume", "credential-activate"}})
}
func TestCLIV2CheckPrecedence(t *testing.T) {
	usable := quota.Result{Status: quota.StatusOK}
	fail := func(code string) quota.Result { return quota.ErrorResult(code, "sentinel-upstream-body", 500) }
	for _, tc := range []struct {
		name string
		rows []quota.Result
		exit int
		code string
	}{
		{"success", []quota.Result{usable}, 0, ""},
		{"no rows", nil, 4, "check_unavailable"},
		{"unconfigured", []quota.Result{fail("not_configured")}, 4, "check_unavailable"},
		{"missing token", []quota.Result{fail("no_token")}, 5, "check_authentication"},
		{"expired", []quota.Result{fail("auth_expired")}, 5, "check_authentication"},
		{"authentication over operation", []quota.Result{fail("fetch_error"), fail("auth_expired")}, 5, "check_authentication"},
		{"operation over unavailable", []quota.Result{fail("not_configured"), fail("parse_error")}, 1, "check_failed"},
		{"partial over authentication", []quota.Result{usable, fail("auth_expired")}, 8, "check_partial"},
		{"partial over unavailable", []quota.Result{usable, fail("not_configured")}, 8, "check_partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit, text, diagnostics := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: tc.rows})})
			if exit != tc.exit {
				t.Fatalf("exit=%d want=%d", exit, tc.exit)
			}
			if err := validateV2Envelope([]byte(text), v2Case{Command: "check", Exit: tc.exit, Code: tc.code}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(text+diagnostics, "sentinel-upstream-body") {
				t.Fatal("raw error leaked")
			}
		})
	}
}
func TestCLIV2CheckProviderOrderAndDefaults(t *testing.T) {
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Claude: {{Status: quota.StatusOK}}, provider.Codex: {{Status: quota.StatusOK}}, provider.Gemini: {{Status: quota.StatusOK}}})
	var first string
	for _, args := range [][]string{{"--json"}, {"check", "--json"}, {"check", "claude", "codex", "gemini", "--json"}} {
		exit, text, _ := quotaTestRun(t, context.Background(), args, v2CheckPrepared{Runner: runner})
		if exit != 0 {
			t.Fatal(exit)
		}
		if first == "" {
			first = text
		} else if text != first {
			t.Fatal("default report differs")
		}
	}
	runner.Services[provider.Claude] = provider.Services{Usage: v2QuotaProvider(func(context.Context, time.Time) ([]quota.Result, error) { panic("unselected provider accessed") })}
	exit, text, _ := quotaTestRun(t, context.Background(), []string{"check", "gemini", "codex", "--json"}, v2CheckPrepared{Runner: runner})
	report := quotaTestReport(t, text)
	if exit != 0 || len(report.Providers) != 2 || report.Providers[0].ID != provider.Gemini || report.Providers[1].ID != provider.Codex {
		t.Fatal("selected provider scope/order lost")
	}
}
func TestCLIV2CheckSyntaxBeforeAccess(t *testing.T) {
	for _, args := range [][]string{{"check", "codex", "codex", "--json"}, {"check", "codex", "--dry-run", "--json"}, {"check", "Codex", "--json"}, {"check", "--timeout", "0s", "--json"}, {"check", "--timeout", "601s", "--json"}, {"check", "--timeout", "30", "--json"}} {
		f := noAccessV2Fixture(t)
		var out, err bytes.Buffer
		if exit := cli.Run(context.Background(), args, &cli.Session{Out: &out, Err: &err}, f.Lookup); exit != 2 {
			t.Fatalf("%v exit=%d", args, exit)
		}
	}
	for _, args := range [][]string{{"check", "--help"}, {"check", "--help", "--json"}} {
		f := noAccessV2Fixture(t)
		var out, err bytes.Buffer
		if exit := cli.Run(context.Background(), args, &cli.Session{Out: &out, Err: &err}, f.Lookup); exit != 0 {
			t.Fatal(exit)
		}
		want, _ := cli.Help("check")
		if out.String() != want {
			t.Fatal("help differs")
		}
	}
}

type v2QuotaCache struct {
	rows []quota.Result
	gets atomic.Int32
	puts atomic.Int32
	err  error
}

func (c *v2QuotaCache) Get(ctx context.Context, _ string) ([]quota.Result, bool, error) {
	c.gets.Add(1)
	return provider.CloneResults(c.rows), len(c.rows) > 0, nil
}
func (c *v2QuotaCache) Put(ctx context.Context, _ string, _ []quota.Result) error {
	c.puts.Add(1)
	return c.err
}
func (c *v2QuotaCache) Delete(context.Context, string) error { return nil }
func (c *v2QuotaCache) Age(context.Context, string) (time.Duration, bool) {
	return 9 * time.Second, true
}

type v2QuotaHistory func(context.Context, map[string][]quota.Result, int64) (history.BurnRates, error)

func (f v2QuotaHistory) UpdateAndGetEstimates(ctx context.Context, rows map[string][]quota.Result, now int64) (history.BurnRates, history.RateEstimates, error) {
	rates, err := f(ctx, rows, now)
	return rates, nil, err
}
func TestCLIV2CheckFreshAndStaleEvidence(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "cache hit", true: "fresh fallback"}[fresh], func(t *testing.T) {
			cache := &v2QuotaCache{rows: []quota.Result{{AccountID: "a", Status: quota.StatusOK}}}
			runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {quota.ErrorResult("fetch_error", "secret-body", 500)}})
			var fetched atomic.Int32
			runner.Services[provider.Codex] = provider.Services{Usage: v2QuotaProvider(func(context.Context, time.Time) ([]quota.Result, error) {
				fetched.Add(1)
				row := quota.ErrorResult("fetch_error", "secret-body", 500)
				row.AccountID = "a"
				return []quota.Result{row}, nil
			})}
			runner.Cache = cache
			args := []string{"check", "codex", "--json"}
			if fresh {
				args = append(args, "--fresh")
			}
			exit, text, diagnostics := quotaTestRun(t, context.Background(), args, v2CheckPrepared{Runner: runner})
			want := 0
			if fresh {
				want = 8
			}
			if exit != want {
				t.Fatalf("exit=%d want=%d", exit, want)
			}
			row := quotaTestReport(t, text).Providers[0].Results[0]
			if row.CacheAge != 9 || !row.IsUsable() {
				t.Fatal("cached row missing")
			}
			if fresh && (!strings.Contains(text, "quota_stale_fallback") || fetched.Load() != 1 || cache.gets.Load() != 1) {
				t.Fatal("fresh must skip only initial lookup")
			}
			if !fresh && (fetched.Load() != 0 || strings.Contains(text, "quota_stale_fallback")) {
				t.Fatal("cache age confused with fallback")
			}
			if strings.Contains(text+diagnostics, "secret-body") {
				t.Fatal("secret leaked")
			}
		})
	}
}
func TestCLIV2CheckOptionalWarningsDoNotFailRows(t *testing.T) {
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {{Status: quota.StatusOK}}})
	runner.Cache = &v2QuotaCache{err: errors.New("secret-cache")}
	runner.History = v2QuotaHistory(func(context.Context, map[string][]quota.Result, int64) (history.BurnRates, error) {
		return nil, errors.New("secret-history")
	})
	exit, text, diagnostics := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner, Enrich: func(context.Context, *app.Report) error { return errors.New("secret-eligibility") }})
	if exit != 0 {
		t.Fatal(exit)
	}
	for _, code := range []string{"cache_put_failed", "history_update_failed", "proxy_eligibility_failed"} {
		if !strings.Contains(text, code) {
			t.Errorf("missing %s", code)
		}
	}
	if strings.Contains(text+diagnostics, "secret-") {
		t.Fatal("optional failure leaked")
	}
}
func TestCLIV2CheckCompletedAccountSurvivesCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupt", true: "deadline"}[deadline], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			published, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			runner := quotaTestRunner(nil)
			runner.Services[provider.Codex] = provider.Services{Usage: v2QuotaProvider(func(ctx context.Context, _ time.Time) ([]quota.Result, error) {
				defer close(finished)
				exact := 12.25
				rows := []quota.Result{{AccountID: "done", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{"5h": {RemainingPct: 12, RemainingPctExact: &exact}}}}
				provider.ObserveResults(ctx, rows)
				close(published)
				<-release
				rows[0].AccountID = "late"
				exact = 99
				rows[0].Windows["5h"] = quota.Window{RemainingPct: 99}
				provider.ObserveWarning(ctx, "late_warning", "late output forbidden")
				provider.ObserveResults(ctx, rows)
				return rows, nil
			})}
			cache := &v2QuotaCache{}
			runner.Cache = cache
			runner.History = v2QuotaHistory(func(context.Context, map[string][]quota.Result, int64) (history.BurnRates, error) {
				t.Error("history after cancellation")
				return nil, nil
			})
			type result struct {
				exit int
				text string
			}
			done := make(chan result, 1)
			args := []string{"check", "codex", "--fresh", "--json"}
			if deadline {
				args = append(args, "--timeout", "100ms")
			}
			go func() {
				exit, text, _ := quotaTestRun(t, ctx, args, v2CheckPrepared{Runner: runner})
				done <- result{exit, text}
			}()
			<-published
			if !deadline {
				cancel()
			}
			got := <-done
			want := 130
			if deadline {
				want = 7
			}
			if got.exit != want {
				t.Fatalf("exit=%d want=%d", got.exit, want)
			}
			row := quotaTestReport(t, got.text).Providers[0].Results[0]
			if row.AccountID != "done" || *row.Windows["5h"].RemainingPctExact != 12.25 {
				t.Fatal("completed snapshot lost")
			}
			close(release)
			<-finished
			if cache.puts.Load() != 0 {
				t.Fatal("cache persisted after cancellation")
			}
			if strings.Contains(got.text, "late") {
				t.Fatal("late output")
			}
		})
	}
}
func TestCLIV2CheckBudgetIncludesPreparationAndHistory(t *testing.T) {
	for _, phase := range []string{"preparation", "history", "cache", "eligibility"} {
		t.Run(phase, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {{Status: quota.StatusOK}}})
			block := func(ctx context.Context) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("missing budget")
				}
				cancel()
				<-ctx.Done()
			}
			prepared := v2CheckPrepared{Runner: runner}
			if phase == "history" {
				runner.History = v2QuotaHistory(func(ctx context.Context, _ map[string][]quota.Result, _ int64) (history.BurnRates, error) {
					block(ctx)
					return nil, ctx.Err()
				})
			}
			if phase == "eligibility" {
				prepared.Enrich = func(ctx context.Context, _ *app.Report) error { block(ctx); return ctx.Err() }
			}
			if phase == "cache" {
				runner.Cache = &v2BlockingQuotaCache{block: block}
			}
			deps := v2CheckDependencies{Prepare: func(ctx context.Context, _ []provider.ID) (v2CheckPrepared, error) {
				if phase == "preparation" {
					block(ctx)
				}
				return prepared, nil
			}}
			inv, err := cli.Parse([]string{"check", "codex", "--json"})
			if err != nil {
				t.Fatal(err)
			}
			got := handleV2CheckWithDependencies(parent, inv, nil, deps)
			if got.ExitCode != 130 {
				t.Fatalf("exit=%d", got.ExitCode)
			}
			if phase == "history" || phase == "eligibility" {
				var data v2CheckData
				if json.Unmarshal(got.Data, &data) != nil || len(data.Report.Providers[0].Results) != 1 {
					t.Fatal("completed rows lost during optional work")
				}
			}
		})
	}
}

type v2BlockingQuotaCache struct {
	v2QuotaCache
	block func(context.Context)
}

func (c *v2BlockingQuotaCache) Get(ctx context.Context, _ string) ([]quota.Result, bool, error) {
	c.block(ctx)
	return nil, false, ctx.Err()
}
func TestCLIV2CheckPanicIsSafe(t *testing.T) {
	runner := quotaTestRunner(nil)
	runner.Services[provider.Codex] = provider.Services{Usage: v2QuotaProvider(func(context.Context, time.Time) ([]quota.Result, error) { panic("credential-secret") })}
	exit, text, diagnostics := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
	if exit != 1 || strings.Contains(text+diagnostics, "credential-secret") || !strings.Contains(text, "fetch_panic") {
		t.Fatal("unsafe panic outcome")
	}
}
func TestCLIV2CheckSortingAndFrozenProjection(t *testing.T) {
	zero := 0.0
	rows := []quota.Result{{Status: quota.StatusOK}, {AccountID: "z", Status: quota.StatusOK}, {AccountID: "a", Email: "z", Status: quota.StatusOK}, {AccountID: "a", Email: "a", Status: quota.StatusExhausted, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 0, RemainingPctExact: &zero}}}}
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: rows})
	baseline, err := runner.BuildReport(context.Background(), app.RunRequest{Providers: []provider.ID{provider.Codex}})
	if err != nil {
		t.Fatal(err)
	}
	exit, text, _ := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
	got := quotaTestReport(t, text)
	if exit != 0 || got.Providers[0].Results[0].Email != "a" || got.Providers[0].Results[1].Email != "z" || got.Providers[0].Results[2].AccountID != "z" || got.Providers[0].Results[3].AccountID != "" {
		t.Fatal("row order")
	}
	if !reflect.DeepEqual(got.Providers[0].Aggregate, baseline.Providers[0].Aggregate) || !reflect.DeepEqual(got.Providers[0].Availability, baseline.Providers[0].Availability) {
		t.Fatal("frozen metrics changed")
	}
	if !strings.Contains(text, `"remaining_pct_exact":0`) || strings.Contains(text, `"reset_at_unix"`) || strings.Contains(text, `"cache_age_s"`) || strings.Contains(text, `"proxy_pools"`) || strings.Contains(text, `"error"`) {
		t.Fatal("v1 omission rules changed")
	}
}
func TestCLIV2CheckHumanReport(t *testing.T) {
	now := v2QuotaClock{}.Now()
	rows := []quota.Result{
		{AccountID: "a", Email: "other@example.com", Status: quota.StatusOK, Plan: "pro", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 87, ResetAtUnix: now.Unix() + 86400}}},
		{AccountID: "z", Email: "active@example.com", Active: true, Status: quota.StatusOK, Plan: "pro", RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 89, ResetAtUnix: now.Unix() + 86400}}},
	}
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: rows, provider.Claude: {quota.ErrorResult("not_configured", "not configured", 0)}, provider.Gemini: {quota.ErrorResult("not_configured", "not configured", 0)}})
	for _, args := range [][]string{nil, {"check", "codex"}} {
		exit, text, _ := quotaTestRun(t, context.Background(), args, v2CheckPrepared{Runner: runner})
		// Bare cq additionally reports unconfigured providers, hence partial status.
		if exit != 0 && exit != 8 {
			t.Fatalf("exit=%d", exit)
		}
		for _, want := range []string{"Codex pro 20x", "━", "╌", "89%", "1d", "40x", "─", "active@example.com"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		if strings.Index(text, "active@example.com") > strings.Index(text, "other@example.com") {
			t.Fatal("active account not first")
		}
		if len(args) == 0 && (!strings.Contains(text, "Claude · not configured") || !strings.Contains(text, "Gemini · not configured")) {
			t.Fatalf("unconfigured providers lost compact headers: %s", text)
		}

		for _, absent := range []string{"Availability:", "Aggregate (", "sustainability=", "Proxy eligibility:"} {
			if strings.Contains(text, absent) {
				t.Fatalf("raw projection remains: %q", absent)
			}
		}
	}
	_, text, _ := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
	report := quotaTestReport(t, text)
	if report.Providers[0].Results[0].AccountID != "a" || !report.Providers[0].Results[1].Active || report.Providers[0].Aggregate.Windows["7d"].RemainingPct != 88 {
		t.Fatalf("human rendering changed JSON report: %+v", report)
	}
}

func TestCLIV2CheckHumanEscapesLabels(t *testing.T) {
	report := app.Report{GeneratedAt: v2QuotaClock{}.Now(), Providers: []app.ProviderReport{{ID: provider.Codex, Results: []quota.Result{{Email: "é\x1b\n\u0085", Status: quota.StatusOK, Plan: "pro\x7f", Windows: map[quota.WindowName]quota.Window{"7d:bucket\x1b": {RemainingPct: 50}}}}}}}
	before, _ := json.Marshal(report)
	text := output.QuotaReportHumanV2(report)
	for _, want := range []string{`é\u001b\u000a\u0085`, `pro\u007f`, `bucket\u001b`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing escaped label %q: %q", want, text)
		}
	}
	after, _ := json.Marshal(report)
	if !bytes.Equal(before, after) {
		t.Fatal("human renderer mutated JSON data")
	}
}

func TestCLIV2CheckFrozenMixedTierMetrics(t *testing.T) {
	// Frozen annex TestComputeWeightsMixedCodexTiers: 80%@20x + 20%@5x,
	// both halfway through 5h, gives 68% and 19125 seconds of burndown.
	now := v2QuotaClock{}.Now().Unix()
	rows := []quota.Result{{AccountID: "z", Status: quota.StatusOK, RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"5h": {RemainingPct: 80, ResetAtUnix: now + 9000}}}, {AccountID: "a", Status: quota.StatusOK, RateLimitTier: "codex_prolite_5x", Windows: map[quota.WindowName]quota.Window{"5h": {RemainingPct: 20, ResetAtUnix: now + 9000}}}}
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: rows})
	baseline, err := runner.BuildReport(context.Background(), app.RunRequest{Providers: []provider.ID{provider.Codex}})
	if err != nil {
		t.Fatal(err)
	}
	exit, text, _ := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
	got := quotaTestReport(t, text).Providers[0].Aggregate
	if exit != 0 || got == nil || got.Summary.Count != 2 || got.Summary.TotalMulti != 25 || got.Windows["5h"].RemainingPct != 68 || got.Windows["5h"].ExpectedPct != 50 || got.Windows["5h"].PaceDiff != 18 || got.Windows["5h"].Burndown != 19125 {
		t.Fatalf("frozen fixture changed: %+v", got)
	}
	if !reflect.DeepEqual(got, baseline.Providers[0].Aggregate) {
		t.Fatal("adapter changed frozen aggregate fields")
	}
}
func TestCLIV2CheckAggregateHumanAndOmissions(t *testing.T) {
	a := &app.AggregateReport{ProviderID: provider.Codex, Kind: "weighted_pace", Summary: aggregate.AccountSummary{Count: 2, TotalMulti: 25, Label: "25×"}, Windows: map[quota.WindowName]quota.AggregateResult{"5h": {RemainingPct: 68, ExpectedPct: 50, PaceDiff: 18, Burndown: 19125, Sustainability: 1.25, GaugePos: 4, GapStartS: 10, GapDurationS: 20, WastedPct: 30, WasteDeadlineS: 40, GaugeOverride: "imminent_block"}, "7d": {GaugePos: -1, Sustainability: -1}}}
	report := app.Report{Providers: []app.ProviderReport{{Name: "Codex", Availability: app.ProviderAvailability{State: app.ProviderAvailabilityAvailable, Reason: "healthy_quota"}, Results: []quota.Result{}, Aggregate: a, ProxyEligibility: &app.ProxyEligibilityReport{DiscoveredCount: 3, EligibleCount: 2, ExcludedCount: 1}, ProxyPools: []app.ProxyPoolReport{{Name: "pool\x7f", ProxyEligibilityReport: app.ProxyEligibilityReport{DiscoveredCount: 3, EligibleCount: 1, ExcludedCount: 2}}}}}}
	text := output.QuotaReportHumanV2(report)
	for _, want := range []string{"25×", "68%", "━", "─", `pool\u007f`, "Proxy"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing dashboard element %q: %q", want, text)
		}
	}
	if strings.Contains(text, "remaining=") {
		t.Fatal("raw aggregate projection remains")
	}

	encoded, err := json.Marshal(a.Windows["7d"])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"remaining_pct":0,"expected_pct":0,"pace_diff":0,"sustainability":-1,"gauge_pos":-1}` {
		t.Fatalf("omissions=%s", encoded)
	}
	// Required zero values stay present, optional zero sustainability disappears.
	encoded, err = json.Marshal(quota.AggregateResult{})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"remaining_pct":0,"expected_pct":0,"pace_diff":0,"gauge_pos":0}` {
		t.Fatalf("zero omissions=%s", encoded)
	}
}
func TestCLIV2CheckHTTPForbiddenIsNotAutomaticallyAuthentication(t *testing.T) {
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {quota.ErrorResult("api_error", "entitlement denied", 403)}})
	exit, _, _ := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
	if exit != 1 {
		t.Fatalf("explicit nonauthentication provider failure exit=%d", exit)
	}
}

func TestCLIV2CheckEmptyErrorMessageRemainsOmitted(t *testing.T) {
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {quota.ErrorResult("fetch_error", "", 0)}})
	exit, text, _ := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
	if exit != 1 || quotaTestReport(t, text).Providers[0].Results[0].Error.Message != "" || !strings.Contains(text, `"error":{"code":"fetch_error"}`) {
		t.Fatal("optional empty error fields changed")
	}
}

type v2PanickingQuotaCache struct {
	v2QuotaCache
	stage string
}

func (c *v2PanickingQuotaCache) Get(ctx context.Context, id string) ([]quota.Result, bool, error) {
	if c.stage == "get" {
		panic("cache-secret")
	}
	return c.v2QuotaCache.Get(ctx, id)
}
func (c *v2PanickingQuotaCache) Put(ctx context.Context, id string, rows []quota.Result) error {
	if c.stage == "put" {
		panic("cache-secret")
	}
	return c.v2QuotaCache.Put(ctx, id, rows)
}
func (c *v2PanickingQuotaCache) Age(ctx context.Context, id string) (time.Duration, bool) {
	if c.stage == "age" {
		panic("cache-secret")
	}
	return c.v2QuotaCache.Age(ctx, id)
}
func TestCLIV2CheckOptionalCachePanicDoesNotFailRows(t *testing.T) {
	for _, stage := range []string{"get", "put", "age"} {
		t.Run(stage, func(t *testing.T) {
			runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {{Status: quota.StatusOK}}})
			cache := &v2PanickingQuotaCache{stage: stage}
			if stage == "age" {
				cache.rows = []quota.Result{{Status: quota.StatusOK}}
			}
			runner.Cache = cache
			exit, text, diagnostics := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
			if exit != 0 {
				t.Fatalf("cache %s panic fabricated failed upstream row: exit=%d", stage, exit)
			}
			if strings.Contains(text+diagnostics, "cache-secret") || !strings.Contains(text, "cache_"+stage+"_failed") {
				t.Fatal("cache panic diagnostic unsafe or absent")
			}
		})
	}
}

func TestCLIV2CheckFrozenDepletedPoolCapacity(t *testing.T) {
	now := v2QuotaClock{}.Now().Unix()
	for _, tier := range []string{"codex_pro_20x", "codex_plus"} {
		t.Run(tier, func(t *testing.T) {
			rows := []quota.Result{
				{AccountID: "a", Status: quota.StatusOK, RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 86, ResetAtUnix: now + 504000}}},
				{AccountID: "b", Status: quota.StatusOK, RateLimitTier: "codex_pro_20x", Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 93, ResetAtUnix: now + 504000}}},
				{AccountID: "c", Status: quota.StatusOK, RateLimitTier: tier, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 0, ResetAtUnix: now + 504000}}},
			}
			runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: rows})
			exit, text, _ := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
			got := quotaTestReport(t, text).Providers[0].Aggregate
			if exit != 0 || got == nil {
				t.Fatalf("exit=%d aggregate=%v", exit, got)
			}
			wantCapacity := 41
			want := quota.AggregateResult{RemainingPct: 87, ExpectedPct: 83, PaceDiff: 4, Burndown: 859200, Sustainability: 2.65625, GaugePos: 4, WastedPct: 6, WasteDeadlineS: 504000}
			if tier == "codex_pro_20x" {
				wantCapacity = 60
				want.RemainingPct, want.PaceDiff, want.GaugePos = 60, -23, 0
				want.GapStartS, want.GapDurationS = 154949, 349051
			}
			if got.Summary.TotalMulti != wantCapacity || got.Windows["7d"] != want {
				t.Fatalf("capacity=%d weekly=%+v; want capacity=%d weekly=%+v", got.Summary.TotalMulti, got.Windows["7d"], wantCapacity, want)
			}
			t.Logf("capacity=%d weekly=%+v", got.Summary.TotalMulti, got.Windows["7d"])
		})
	}
}

type v2QuotaHTTP func(*http.Request) (*http.Response, error)

func (f v2QuotaHTTP) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestCLIV2CheckRealClaudeUnauthorized(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "authentication", true: "authentication before operation"}[mixed], func(t *testing.T) {
			claude := claudeprov.New(v2QuotaHTTP(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/oauth/usage" {
					t.Errorf("unexpected endpoint %s", req.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"private":"upstream-secret"}`))}, nil
			}))
			runner := quotaTestRunner(nil)
			runner.Services[provider.Claude] = provider.Services{Usage: v2QuotaProvider(func(ctx context.Context, now time.Time) ([]quota.Result, error) {
				row, _, err := claude.FetchAccountUsage(ctx, keyring.ClaudeOAuth{AccessToken: "fake", SubscriptionType: "max"}, now)
				return []quota.Result{row}, err
			})}
			args := []string{"check", "claude", "--json"}
			if mixed {
				runner.Services[provider.Codex] = provider.Services{Usage: v2QuotaProvider(func(context.Context, time.Time) ([]quota.Result, error) {
					return []quota.Result{quota.ErrorResult("fetch_error", "private-operation", 0)}, nil
				})}
				args = []string{"check", "codex", "claude", "--json"}
			}
			exit, text, diagnostics := quotaTestRun(t, context.Background(), args, v2CheckPrepared{Runner: runner})
			if exit != 5 || !strings.Contains(text, `"check_authentication"`) {
				t.Fatalf("actual Claude401: exit=%d want5: %s", exit, text)
			}
			report := quotaTestReport(t, text)
			row := report.Providers[len(report.Providers)-1].Results[0]
			if row.Error.Code != "api_error" || row.Error.HTTPStatus != 401 {
				t.Fatalf("domain evidence changed: %+v", row.Error)
			}
			if strings.Contains(text+diagnostics, "upstream-secret") || strings.Contains(text+diagnostics, "private-operation") {
				t.Fatal("raw failure leaked")
			}
		})
	}
}

type v2QuotaCacheReadFS struct {
	*fsutil.MemFS
	stage string
}

func (f *v2QuotaCacheReadFS) Stat(path string) (os.FileInfo, error) {
	if f.stage == "stat" {
		return nil, &os.PathError{Op: "stat", Path: "private-cache-path", Err: os.ErrPermission}
	}
	return f.MemFS.Stat(path)
}
func (f *v2QuotaCacheReadFS) ReadFile(path string) ([]byte, error) {
	if f.stage == "read" {
		return nil, &os.PathError{Op: "read", Path: "private-cache-path", Err: os.ErrPermission}
	}
	if f.stage == "read-io" {
		return nil, errors.New("private-IO-failure")
	}
	if f.stage == "disappeared" {
		return nil, os.ErrNotExist
	}
	return f.MemFS.ReadFile(path)
}
func TestCLIV2CheckRealCacheReadWarnings(t *testing.T) {
	for _, stage := range []string{"stat", "read", "read-io", "decode", "missing", "disappeared", "expired"} {
		t.Run(stage, func(t *testing.T) {
			fs := &v2QuotaCacheReadFS{MemFS: fsutil.NewMemFS(), stage: stage}
			ttl := time.Hour
			if stage == "expired" {
				ttl = -time.Second
			}
			c, err := cache.New(fs, "/cache", ttl)
			if err != nil {
				t.Fatal(err)
			}
			if stage != "missing" {
				if err := fs.MemFS.WriteFile("/cache/codex.json", []byte("private-malformed-cache-body"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok, err := c.Get(context.Background(), "codex"); ok || err != nil {
				t.Fatalf("legacy cache miss changed: ok=%v err=%v", ok, err)
			}
			runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: {{Status: quota.StatusOK, AccountID: "fresh"}}})
			runner.Cache = c
			exit, text, diagnostics := quotaTestRun(t, context.Background(), []string{"check", "codex", "--json"}, v2CheckPrepared{Runner: runner})
			if exit != 0 || quotaTestReport(t, text).Providers[0].Results[0].AccountID != "fresh" {
				t.Fatalf("optional cache failure lost successful fetch: exit=%d", exit)
			}
			wantWarning := stage == "stat" || stage == "read" || stage == "read-io" || stage == "decode"
			if strings.Contains(text, `"cache_get_failed"`) != wantWarning {
				t.Fatalf("stage=%s wantWarning=%v output=%s", stage, wantWarning, text)
			}
			if strings.Contains(text+diagnostics, "private-") {
				t.Fatal("raw cache path/body leaked")
			}
		})
	}
}

func TestCLIV2CheckRecentForecastSurvivesObservedHistoryAndSubsets(t *testing.T) {
	now := v2QuotaClock{}.Now().Unix()
	store, err := history.New(fsutil.NewMemFS(), "/history")
	if err != nil {
		t.Fatal(err)
	}
	rows := func(pct int) []quota.Result {
		var out []quota.Result
		for _, account := range []string{"a", "b", "c"} {
			out = append(out, quota.Result{AccountID: account, Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: pct, ResetAtUnix: now + 44*3600}}})
		}
		return out
	}
	for i, pct := range []int{24, 21} {
		if _, _, err := store.UpdateAndGetEstimates(context.Background(), map[string][]quota.Result{"codex": rows(pct)}, now-int64(2-i)*1800); err != nil {
			t.Fatal(err)
		}
	}
	input := rows(18)
	runner := quotaTestRunner(map[provider.ID][]quota.Result{provider.Codex: input})
	runner.History = store
	exit, encoded, diagnostics := quotaTestRun(t, context.Background(), []string{"check", "codex", "--fresh", "--json"}, v2CheckPrepared{Runner: runner, Enrich: func(_ context.Context, report *app.Report) error {
		eligible := func(r quota.Result) bool { return r.AccountID != "c" }
		app.AddProxyEligibility(report, provider.Codex, eligible)
		app.AddProxyPool(report, provider.Codex, "work", eligible)
		return nil
	}})
	if exit != 0 || diagnostics != "" || !strings.Contains(encoded, `"schema_version":2`) || !strings.Contains(encoded, `"command":"check"`) {
		t.Fatalf("exit=%d out=%s err=%s", exit, encoded, diagnostics)
	}
	pr := quotaTestReport(t, encoded).Providers[0]
	for _, aggregate := range []*app.AggregateReport{pr.Aggregate, pr.ProxyEligibility.Aggregate, pr.ProxyPools[0].Aggregate} {
		if aggregate == nil || aggregate.Windows[quota.Window7Day].Burndown != 10800 {
			t.Fatalf("recent forecast lost through v2: %+v", aggregate)
		}
	}
	for _, row := range input {
		if row.Windows[quota.Window7Day].RecentBurnRate != nil {
			t.Fatal("provider input mutated")
		}
	}
	if strings.Contains(encoded, "recent_burn") || strings.Contains(encoded, "RecentBurnRate") {
		t.Fatal("internal forecast annotation leaked into schema")
	}
}
