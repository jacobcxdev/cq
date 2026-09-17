package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/output"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	geminiprov "github.com/jacobcxdev/cq/internal/provider/gemini"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type v2CheckPrepared struct {
	Runner   *app.Runner
	Enrich   func(context.Context, *app.Report) error
	Warnings []cli.Diagnostic
}
type v2CheckDependencies struct {
	Prepare func(context.Context, []provider.ID) (v2CheckPrepared, error)
}
type v2CheckData struct {
	Report app.Report `json:"report"`
}

func handleV2Check(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2CheckWithDependencies(ctx, inv, session, v2CheckDependencies{Prepare: prepareV2Check})
}

// Only the selected integrations are constructed. Resolving cache storage does
// not resolve unrelated service roots or install/start a background component.
func prepareV2Check(ctx context.Context, ids []provider.ID) (v2CheckPrepared, error) {
	roots, err := userdirs.Default(userdirs.CacheRoot)
	if err != nil {
		return v2CheckPrepared{}, err
	}
	prepared := v2CheckPrepared{Runner: &app.Runner{Clock: systemClock{}, Services: map[provider.ID]provider.Services{}}}
	client := httputil.NewClient(10*time.Second, version)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return prepared, err
		}
		switch id {
		case provider.Claude:
			prepared.Runner.Services[id] = provider.Services{Usage: claudeprov.New(client)}
		case provider.Codex:
			p := codexprov.New(client)
			prepared.Runner.Services[id] = provider.Services{Usage: p}
			prepared.Enrich = func(ctx context.Context, report *app.Report) error {
				return enrichCodexProxyEligibility(ctx, report, p)
			}
		case provider.Gemini:
			prepared.Runner.Services[id] = provider.Services{Usage: geminiprov.New(client, geminiOAuthClientSecret)}
		}
	}
	if err := ctx.Err(); err != nil {
		return prepared, err
	}
	c, err := cache.New(cache.OSFileSystem{}, roots.Cache, cacheTTL())
	if err != nil {
		prepared.Warnings = append(prepared.Warnings, cli.Diagnostic{Code: "cache_open_failed", Message: "Quota cache could not be opened."})
	} else {
		prepared.Runner.Cache = c
	}
	if err := ctx.Err(); err != nil {
		return prepared, err
	}
	h, err := history.New(fsutil.OSFileSystem{}, roots.Cache)
	if err != nil {
		prepared.Warnings = append(prepared.Warnings, cli.Diagnostic{Code: "history_open_failed", Message: "Quota history could not be opened."})
	} else {
		prepared.Runner.History = h
	}
	return prepared, nil
}

func handleV2CheckWithDependencies(parent context.Context, inv cli.Invocation, _ *cli.Session, deps v2CheckDependencies) cli.Outcome {
	timeout, err := time.ParseDuration(inv.Options["timeout"][0])
	if err != nil {
		return v2CheckFailure(1, "check_failed", "Quota checks failed for the selected providers.")
	}
	budget := cli.BeginBudget(parent, timeout, 0)
	defer budget.Close()
	ctx := budget.Work()
	ids := []provider.ID{provider.Claude, provider.Codex, provider.Gemini}
	if len(inv.Arguments["providers"]) > 0 {
		ids = nil
		for _, id := range inv.Arguments["providers"] {
			ids = append(ids, provider.ID(id))
		}
	}
	report := app.Report{GeneratedAt: time.Now().UTC(), Providers: []app.ProviderReport{}}
	type preparation struct {
		value v2CheckPrepared
		err   error
	}
	ready := make(chan preparation, 1)
	if ctx.Err() == nil {
		go func() {
			result := preparation{}
			defer func() {
				if recover() != nil {
					result.err = errors.New("check preparation failed")
				}
				ready <- result
			}()
			if ctx.Err() != nil {
				result.err = ctx.Err()
				return
			}
			result.value, result.err = deps.Prepare(ctx, ids)
		}()
	}
	var prepared v2CheckPrepared
	select {
	case result := <-ready:
		if result.err != nil && ctx.Err() == nil {
			var environment *userdirs.EnvironmentError
			if errors.As(result.err, &environment) {
				return v2CheckFailure(environment.ExitCode, environment.Code, environment.Error())
			}
			return v2CheckFailure(1, "check_failed", "Quota checks failed for the selected providers.")
		}
		prepared = result.value
	case <-ctx.Done():
	}
	warnings := append([]cli.Diagnostic(nil), prepared.Warnings...)
	stale := false
	if ctx.Err() == nil {
		var observed []app.ReportWarning
		report, observed, err = prepared.Runner.BuildReportObserved(ctx, app.RunRequest{Providers: ids, Refresh: inv.Options["fresh"][0] == "true"})
		for _, warning := range observed {
			stale = stale || warning.Code == "quota_stale_fallback"
			warnings = append(warnings, cli.Diagnostic{Code: warning.Code, Message: warning.Message})
		}
		if err != nil && ctx.Err() == nil {
			return v2CheckFailure(1, "check_failed", "Quota checks failed for the selected providers.")
		}
	}
	if prepared.Enrich != nil && ctx.Err() == nil {
		// The enrichment worker owns this copy; cancellation cannot race rendering.
		enriched := report
		enriched.Providers = append([]app.ProviderReport(nil), report.Providers...)
		type enrichment struct {
			report app.Report
			err    error
		}
		done := make(chan enrichment, 1)
		go func() {
			result := enrichment{report: enriched}
			defer func() {
				if recover() != nil {
					result.err = errors.New("quota eligibility failed")
				}
				done <- result
			}()
			if ctx.Err() != nil {
				result.err = ctx.Err()
				return
			}
			result.err = prepared.Enrich(ctx, &result.report)
		}()
		select {
		case result := <-done:
			report = result.report
			if result.err != nil {
				warnings = append(warnings, cli.Diagnostic{Code: "proxy_eligibility_failed", Message: "Codex proxy eligibility could not be read."})
			}
		case <-ctx.Done():
		}
	}
	normaliseV2QuotaReport(&report)
	outcome := v2CheckStatus(parent, ctx, report, stale)
	outcome.Warnings = warnings
	outcome.Human = output.QuotaReportHumanV2(report)
	outcome.Data, err = cli.EncodeData(v2CheckData{Report: report})
	if err != nil {
		return v2CheckFailure(1, "check_failed", "Quota checks failed for the selected providers.")
	}
	return outcome
}

func v2CheckFailure(exit int, code, message string) cli.Outcome {
	return cli.Outcome{ExitCode: exit, Errors: []cli.Diagnostic{{Code: code, Message: message}}}
}
func v2CheckStatus(parent, ctx context.Context, report app.Report, stale bool) cli.Outcome {
	if parent.Err() == context.Canceled || ctx.Err() == context.Canceled {
		return v2CheckFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
	}
	if ctx.Err() == context.DeadlineExceeded {
		return v2CheckFailure(7, "check_timeout", "Quota checking timed out.")
	}
	usable, failed, authentication, operational := false, stale, false, false
	for _, p := range report.Providers {
		if len(p.Results) == 0 {
			failed = true
		}
		for _, row := range p.Results {
			if row.IsUsable() {
				usable = true
				continue
			}
			failed = true
			code := ""
			if row.Error != nil {
				code = row.Error.Code
			}
			switch code {
			case "no_token", "auth_expired":
				authentication = true
			case "not_configured", "empty_result", "free_plan", "usage_unverified", "unavailable":
			default:
				operational = true
			}
		}
	}
	switch {
	case usable && failed:
		return v2CheckFailure(8, "check_partial", "Some quota results could not be refreshed.")
	case usable:
		return cli.Outcome{}
	case authentication:
		return v2CheckFailure(5, "check_authentication", "Authentication is required for the selected providers.")
	case operational:
		return v2CheckFailure(1, "check_failed", "Quota checks failed for the selected providers.")
	default:
		return v2CheckFailure(4, "check_unavailable", "No quota results are available.")
	}
}

func normaliseV2QuotaReport(report *app.Report) {
	report.GeneratedAt = report.GeneratedAt.UTC()
	if report.Providers == nil {
		report.Providers = []app.ProviderReport{}
	}
	for i := range report.Providers {
		p := &report.Providers[i]
		p.Results = provider.CloneResults(p.Results)
		sort.SliceStable(p.Results, func(i, j int) bool {
			a, b := p.Results[i], p.Results[j]
			if a.AccountID != b.AccountID {
				return v2QuotaIdentifierLess(a.AccountID, b.AccountID)
			}
			return v2QuotaIdentifierLess(a.Email, b.Email)
		})
		sort.SliceStable(p.ProxyPools, func(i, j int) bool { return p.ProxyPools[i].Name < p.ProxyPools[j].Name })
		for j := range p.Results {
			if e := p.Results[j].Error; e != nil {
				// Domain codes/statuses remain intact; arbitrary upstream messages never
				// cross the CLI boundary. Static explanations retain useful diagnostics.
				if e.Message != "" {
					e.Message = v2QuotaErrorMessage(e.Code)
				}
			}
		}
	}
}
func v2QuotaIdentifierLess(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	return a < b
}
func v2QuotaErrorMessage(code string) string {
	switch code {
	case "not_configured":
		return "Provider is not configured."
	case "no_token", "auth_expired":
		return "Authentication is required."
	case "empty_result":
		return "Provider returned no quota results."
	case "usage_unverified":
		return "Quota usage is not available in cache."
	case "free_plan":
		return "Quota usage is unavailable on this plan."
	default:
		return "Quota observation failed."
	}
}
