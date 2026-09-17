package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ResetWindow struct {
	RemainingPct  float64   `json:"remaining_pct"`
	ResetAt       time.Time `json:"reset_at"`
	PeriodSeconds int64     `json:"period_seconds"`
}
type ResetWindowChange struct {
	Name   quota.WindowName `json:"name"`
	Before ResetWindow      `json:"before"`
	After  ResetWindow      `json:"after"`
}
type ResetRetry struct {
	AccountReference string `json:"account_reference"`
	CreditID         string `json:"credit_id"`
}
type ResetUseResult struct {
	AccountReference string              `json:"account_reference"`
	CreditID         *string             `json:"credit_id"`
	Outcome          string              `json:"outcome"`
	WindowsReset     int64               `json:"windows_reset"`
	ChangedWindows   []ResetWindowChange `json:"changed_windows"`
	Retry            *ResetRetry         `json:"retry"`
}

// T27 installs this leaf into the canonical executable dispatcher.
func lookupV2ResetUse(path string) (cli.Handler, bool) {
	return handleV2ResetUse, path == "codex reset use"
}
func handleV2ResetUse(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2ResetUseWithFactory(ctx, inv, session, newV2ResetUseDependencies)
}
func newV2ResetUseDependencies(ctx context.Context, _ bool) (v2ResetDependencies, error) {
	return newV2ResetUseDependenciesWithClient(ctx, fsutil.OSFileSystem{}, httputil.NewClient(10*time.Second, version))
}
func newV2ResetUseDependenciesWithClient(ctx context.Context, fs fsutil.DurableFileSystem, client httputil.Doer) (v2ResetDependencies, error) {
	deps, err := newV2ResetDependenciesWithClient(ctx, false, fs, client)
	if err != nil {
		return deps, err
	}
	roots, err := userdirs.Default(userdirs.CacheRoot)
	if err != nil {
		return deps, err
	}
	deps.app.Attempts, err = codexprov.OpenResetAttemptStore(fs, roots.Cache)
	if err != nil {
		return deps, &app.CodexResetError{Code: "attempt_unavailable", Err: err}
	}
	backend := deps.app.Backend.(*codexprov.ResetBackend)
	deps.app.Usage, err = codexprov.NewWithCredentialAuthority(client, backend.Inventory, backend.Resolver, backend.Refresh)
	deps.app.History = v2ResetUseHistory{fs: fs, dir: roots.Cache}
	deps.app.Cache = v2ResetUseCache{fs: fs, dir: roots.Cache}
	return deps, err
}
func handleV2ResetUseWithApp(ctx context.Context, inv cli.Invocation, session *cli.Session, a *app.CodexResetApp) cli.Outcome {
	return handleV2ResetUseWithFactory(ctx, inv, session, func(context.Context, bool) (v2ResetDependencies, error) { return v2ResetDependencies{app: a}, nil })
}
func handleV2ResetUseWithFactory(parent context.Context, inv cli.Invocation, s *cli.Session, factory v2ResetFactory) (out cli.Outcome) {
	confirmed := len(inv.Options["yes"]) > 0 && inv.Options["yes"][0] == "true"
	if !confirmed && (inv.JSON || !s.Interactive) {
		return v2ResetUseFailure("confirmation_required")
	}
	timeout, _ := time.ParseDuration(inv.Options["timeout"][0])
	// Observation belongs to the whole invocation, including factory and every
	// postcheck. Late workers keep legacy stderr disabled after detachment.
	var mu sync.Mutex
	warnings := []cli.Diagnostic{}
	detached := false
	observedParent := provider.WithObservation(parent, provider.Observation{Warning: func(code, message string) {
		mu.Lock()
		defer mu.Unlock()
		if !detached {
			warnings = append(warnings, cli.Diagnostic{Code: code, Message: message})
		}
	}})
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		detached = true
		for _, warning := range warnings {
			found := false
			for _, existing := range out.Warnings {
				if existing == warning {
					found = true
					break
				}
			}
			if !found {
				out.Warnings = append(out.Warnings, warning)
			}
		}
	}()
	budget := cli.BeginBudget(observedParent, timeout, 0)
	defer budget.Close()
	ctx := budget.Work()
	deps, err := openV2ResetUseDependencies(ctx, factory)
	if err != nil {
		return v2ResetUseError(err)
	}
	defer func() {
		if deps.close == nil {
			return
		}
		done := make(chan error, 1)
		go func() { done <- closeV2ResetDependencies(deps.close) }()
		var closeErr error
		select {
		case closeErr = <-done:
		case <-ctx.Done():
			closeErr = ctx.Err()
		}
		if closeErr != nil && out.ExitCode == 0 {
			failure := v2ResetUseError(closeErr)
			if len(out.Data) > 0 {
				failure = v2ResetUseFailure("postcheck_partial")
			}
			out.ExitCode, out.Errors = failure.ExitCode, failure.Errors
		}
	}()
	credit := ""
	if values := inv.Options["credit"]; len(values) > 0 {
		credit = values[0]
	}
	plan, err := deps.app.PrepareUse(ctx, inv.Arguments["account"][0], credit)
	if err != nil {
		return v2ResetUseError(err)
	}
	data := ResetUseResult{AccountReference: string(plan.AccountKey), CreditID: &plan.Credit.ID, Outcome: "cancelled", ChangedWindows: []ResetWindowChange{}}
	if err := writeV2ResetPreview(s, plan, !confirmed); err != nil {
		return v2ResetUseData(v2ResetUseIOError(err), data)
	}
	if err := ctx.Err(); err != nil {
		return v2ResetUseData(v2ResetUseError(err), data)
	}
	if !confirmed {
		// PrepareUse returns successfully only after its workers have quiesced.
		budget.Pause()
		type consent struct {
			yes bool
			err error
		}
		done := make(chan consent, 1)
		go func() {
			result := consent{}
			defer func() {
				if recover() != nil {
					result.err = errors.New("confirmation failed")
				}
				done <- result
			}()
			result.yes, result.err = cli.Confirm(s, "")
		}()
		select {
		case answer := <-done:
			confirmed, err = answer.yes, answer.err
		case <-parent.Done():
			err = parent.Err()
		}
		budget.Resume()
		ctx = budget.Work()
		if err != nil {
			return v2ResetUseData(v2ResetUseIOError(err), data)
		}
		if err := ctx.Err(); err != nil {
			return v2ResetUseData(v2ResetUseError(err), data)
		}
	}
	if !confirmed {
		return v2ResetUseData(cli.Outcome{}, data)
	}
	result, err := deps.app.ExecuteUse(ctx, plan)
	data.Outcome = string(result.Outcome)
	data.WindowsReset = result.WindowsReset
	if result.Unresolved {
		data.Retry = &ResetRetry{AccountReference: string(result.AccountKey), CreditID: result.CreditID}
	}
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		if change, ok := result.ChangedWindows[name]; ok {
			data.ChangedWindows = append(data.ChangedWindows, ResetWindowChange{Name: name, Before: v2ResetWindow(name, change.Before), After: v2ResetWindow(name, change.After)})
		}
	}
	if err != nil {
		out = v2ResetUseError(err)
	}
	for _, warning := range result.Warnings {
		message := ""
		switch warning.Code {
		case "attempt_cleanup_failed":
			message = "Terminal reset attempt cleanup failed."
		case "cache_invalidate_failed":
			message = "Quota cache invalidation failed."
		case "usage_refetch_failed":
			message = "Fresh quota could not be read."
		case "usage_match_failed":
			message = "Fresh quota could not be matched to the selected account."
		case "history_update_failed":
			message = "Quota history update failed."
		}
		if message != "" {
			out.Warnings = append(out.Warnings, cli.Diagnostic{Code: "codex_reset_" + warning.Code, Message: message})
		}
	}
	if err == nil && len(result.Warnings) > 0 {
		failure := v2ResetUseFailure("postcheck_partial")
		out.ExitCode, out.Errors = failure.ExitCode, failure.Errors
	}
	return v2ResetUseData(out, data)
}

func openV2ResetUseDependencies(ctx context.Context, factory v2ResetFactory) (v2ResetDependencies, error) {
	if err := ctx.Err(); err != nil {
		return v2ResetDependencies{}, err
	}
	type result struct {
		deps v2ResetDependencies
		err  error
	}
	done := make(chan result)
	go func() {
		r := result{}
		defer func() {
			if recover() != nil {
				r.err = errors.New("reset dependencies unavailable")
			}
			select {
			case done <- r:
			case <-ctx.Done():
				if r.deps.close != nil {
					_ = closeV2ResetDependencies(r.deps.close)
				}
			}
		}()
		r.deps, r.err = factory(ctx, true)
	}()
	select {
	case r := <-done:
		if r.err != nil && r.deps.close != nil {
			go func() { _ = closeV2ResetDependencies(r.deps.close) }()
		}
		return r.deps, r.err
	case <-ctx.Done():
		return v2ResetDependencies{}, ctx.Err()
	}
}
func writeV2ResetPreview(s *cli.Session, plan app.CodexResetUsePlan, prompt bool) error {
	name := plan.Email
	if name == "" {
		name = plan.AccountID
	}
	if name == "" {
		name = "Unknown account"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Account: %s.\nAccount reference: %s.\nCredit: %s.\n", cli.HumanValue(name), cli.HumanValue(string(plan.AccountKey)), cli.HumanValue(plan.Credit.ID))
	for _, window := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		current := v2ResetWindow(window, plan.CurrentWindows[window])
		fmt.Fprintf(&text, "%s: %g%% remaining; natural reset %s.\n", window, current.RemainingPct, current.ResetAt.Format(time.RFC3339))
	}
	if plan.Recommendation != nil {
		found := false
		for _, item := range plan.Recommendation.Items {
			if item.AccountReference == string(plan.AccountKey) && item.CreditID == plan.Credit.ID {
				fmt.Fprintf(&text, "Recommendation: %s.\n", cli.HumanValue(string(item.Status)))
				found = true
				break
			}
		}
		if !found {
			text.WriteString("Recommendation: unavailable.\n")
		}
	}
	if prompt {
		fmt.Fprintf(&text, "Use reset credit %s for %s? [y/N]", cli.HumanValue(plan.Credit.ID), cli.HumanValue(name))
	}
	output := text.String()
	written, err := fmt.Fprint(s.Err, output)
	if err == nil && written != len(output) {
		err = io.ErrShortWrite
	}
	return err
}
func v2ResetWindow(name quota.WindowName, window quota.Window) ResetWindow {
	remaining := float64(window.RemainingPct)
	if window.RemainingPctExact != nil {
		remaining = *window.RemainingPctExact
	}
	return ResetWindow{RemainingPct: remaining, ResetAt: time.Unix(window.ResetAtUnix, 0).UTC(), PeriodSeconds: int64(quota.PeriodFor(name) / time.Second)}
}
func v2ResetUseData(out cli.Outcome, data ResetUseResult) cli.Outcome {
	var err error
	out.Data, err = cli.EncodeData(data)
	if err != nil {
		failure := v2ResetUseIOError(err)
		out.ExitCode, out.Errors = failure.ExitCode, failure.Errors
	}
	credit := "none"
	if data.CreditID != nil {
		credit = cli.HumanValue(*data.CreditID)
	}
	out.Human = fmt.Sprintf("Reset outcome: %s.\nAccount: %s.\nCredit: %s.\nWindows reset: %d.\n", data.Outcome, cli.HumanValue(data.AccountReference), credit, data.WindowsReset)
	if data.Retry != nil {
		quote := func(value string) string { return "'" + strings.ReplaceAll(cli.HumanValue(value), "'", "'\\''") + "'" }
		out.Human += fmt.Sprintf("Retry the same credit: cq codex reset use %s --credit %s --yes\n", quote(data.Retry.AccountReference), quote(data.Retry.CreditID))
	}
	return out
}
func v2ResetUseError(err error) cli.Outcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return v2ResetFailure(err)
	}
	var appErr *app.CodexResetError
	if errors.As(err, &appErr) {
		switch appErr.Code {
		case "credit_expired", "unsupported_reset_type", "credit_ineligible":
			return v2ResetUseFailure("credit_ineligible")
		case "credit_not_found", "pending_ambiguous", "attempt_unavailable", "consume_indeterminate":
			return v2ResetUseFailure(appErr.Code)
		case "consume_failed", "credits_unavailable":
			var httpErr *codexprov.ResetHTTPError
			if errors.As(err, &httpErr) {
				return v2ResetFailure(err)
			}
			return v2ResetUseFailure("upstream_failed")
		}
	}
	return v2ResetFailure(err)
}
func v2ResetUseFailure(code string) cli.Outcome {
	exit, message := 1, "Codex reset request failed."
	switch code {
	case "confirmation_required":
		exit, message = 6, "Reset consumption requires --yes in non-interactive mode."
	case "credit_not_found":
		exit, message = 3, "Selected reset credit is unavailable; no replacement credit was consumed."
	case "credit_ineligible":
		exit, message = 6, "Selected reset credit is not eligible."
	case "pending_ambiguous":
		exit, message = 6, "Several reset attempts are unresolved; specify --credit."
	case "attempt_unavailable":
		message = "Reset attempt state is unavailable; no consumption was started."
	case "consume_indeterminate":
		message = "Reset outcome is unknown; retry only the same account and credit."
	case "postcheck_partial":
		exit, message = 8, "Reset outcome is known, but local verification or cleanup is incomplete."
	}
	return v2ResetDiagnostic(exit, "codex_reset_"+code, message)
}

func v2ResetUseIOError(err error) cli.Outcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return v2ResetFailure(err)
	}
	return v2ResetDiagnostic(1, "internal_error", "Operation failed because of an internal error.")
}

// These stores are needed only after a known reset. Construction then avoids
// state writes during reference validation, preview and cancelled confirmation.
type v2ResetUseHistory struct {
	fs  fsutil.FileSystem
	dir string
}

func (h v2ResetUseHistory) UpdateAndGetEstimates(ctx context.Context, rows map[string][]quota.Result, now int64) (history.BurnRates, history.RateEstimates, error) {
	return h.UpdateAndGetEstimatesObserved(ctx, rows, now, nil)
}
func (h v2ResetUseHistory) UpdateAndGetEstimatesObserved(ctx context.Context, rows map[string][]quota.Result, now int64, warning func(string, string)) (history.BurnRates, history.RateEstimates, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	store, err := history.New(h.fs, h.dir)
	if err != nil {
		return nil, nil, err
	}
	return store.UpdateAndGetEstimatesObserved(ctx, rows, now, warning)
}

type v2ResetUseCache struct {
	fs  fsutil.FileSystem
	dir string
}

func (c v2ResetUseCache) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := cache.New(c.fs, c.dir, time.Minute)
	if err != nil {
		return err
	}
	return store.Delete(ctx, id)
}
