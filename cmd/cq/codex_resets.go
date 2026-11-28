package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/httputil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type codexResetsDependencies struct {
	ExecutorStatus func(context.Context) proxy.CodexResetSchedulerStatus
	App            *app.CodexResetApp
	In             io.Reader
	Out            io.Writer
	ErrOut         io.Writer
}

type codexResetScheduleJSON struct {
	GeneratedAt time.Time                         `json:"generated_at"`
	Horizon     time.Time                         `json:"horizon"`
	Exact       bool                              `json:"exact"`
	Complete    bool                              `json:"complete"`
	Confidence  aggregate.ResetScheduleConfidence `json:"confidence"`
	Items       []codexResetScheduleItemJSON      `json:"items"`
	Objective   codexResetScheduleObjectiveJSON   `json:"objective"`
	Blockers    []codexResetScheduleBlockerJSON   `json:"blockers,omitempty"`
}

type codexResetScheduleItemJSON struct {
	AccountEmail  string                            `json:"account_email,omitempty"`
	AccountID     string                            `json:"account_id,omitempty"`
	CreditID      string                            `json:"credit_id"`
	UseAt         *time.Time                        `json:"use_at,omitempty"`
	UseBy         *time.Time                        `json:"use_by,omitempty"`
	Status        aggregate.ResetScheduleStatus     `json:"status"`
	Confidence    aggregate.ResetScheduleConfidence `json:"confidence"`
	RestoredPct   map[quota.WindowName]float64      `json:"restored_pct,omitempty"`
	AvoidedGapSec int64                             `json:"avoided_gap_seconds"`
	ReasonCodes   []aggregate.ResetScheduleReason   `json:"reason_codes"`
}

type codexResetScheduleObjectiveJSON struct {
	UnmetDemandPctSeconds float64 `json:"unmet_demand_pct_seconds"`
	GapDurationSeconds    int64   `json:"gap_duration_seconds"`
	UsefulExpiredUnused   int     `json:"useful_expired_unused"`
	WeightedDiscardedPct  float64 `json:"weighted_discarded_pct"`
	RestoredPct           float64 `json:"restored_pct"`
}

type codexResetScheduleBlockerJSON struct {
	Code         string `json:"code"`
	AccountEmail string `json:"account_email,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

var codexResetsDependenciesFactory = newCodexResetsDependencies

func withCodexResetsDependencies(ctx context.Context, run func(codexResetsDependencies) error) error {
	deps, closeDependencies, err := codexResetsDependenciesFactory(ctx)
	if err != nil {
		return err
	}
	defer closeDependencies()
	return run(deps)
}

func newCodexResetsDependencies(ctx context.Context) (codexResetsDependencies, func(), error) {
	fs := fsutil.OSFileSystem{}
	httpClient := httputil.NewClient(10*time.Second, version)
	control, err := codexprov.OpenDefaultCredentialRefreshControl(ctx, fs, httpClient)
	if err != nil {
		return codexResetsDependencies{}, func() {}, err
	}
	closeDependencies := func() {
		if err := control.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "cq: close Codex credential control: %v\n", err)
		}
	}
	fail := func(err error) (codexResetsDependencies, func(), error) {
		closeDependencies()
		return codexResetsDependencies{}, func() {}, err
	}

	roots, err := userdirs.Default()
	if err != nil {
		return fail(err)
	}
	service, err := newCodexResetService(fs, control, httpClient, roots)
	if err != nil {
		return fail(err)
	}

	return codexResetsDependencies{
		App: service, In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr, ExecutorStatus: codexResetExecutorStatus,
	}, closeDependencies, nil
}

// Reuse the runtime's credential authority; constructing a reset service never
// opens a second coordinator owner or starts background work.
func newCodexResetService(fs fsutil.DurableFileSystem, control *codexprov.CredentialControl, httpClient httputil.Doer, roots userdirs.Roots) (*app.CodexResetApp, error) {
	store, err := codexprov.NewManagedStore(fs)
	if err != nil {
		return nil, err
	}
	attempts, err := codexprov.NewResetAttemptStore(fs, roots.Cache)
	if err != nil {
		return nil, err
	}
	historyStore, err := history.New(fs, roots.Cache)
	if err != nil {
		return nil, err
	}
	quotaCache, err := cache.New(cache.OSFileSystem{}, roots.Cache, cacheTTL())
	if err != nil {
		return nil, err
	}
	backend := &codexprov.ResetBackend{
		Inventory: control, Resolver: control, Refresh: control,
		Aliases: func() (codexprov.AccountAliasIndex, error) {
			return (codexprov.Registry{FS: fs, Home: store.Home}).AccountAliasIndex()
		},
		Credits: codexprov.ResetCreditClient{HTTP: httpClient}, Now: time.Now,
	}
	return &app.CodexResetApp{
		Reserve: func(_ context.Context, snapshot codexprov.ResetAccountSnapshot, usage []quota.Result, now time.Time) (quota.ReservePolicy, error) {
			return readCodexResetReserve(fs, filepath.Join(roots.State, "codex-reserve.json"), snapshot, usage, now)
		},
		Schedule:   codexprov.NewResetAutomationStore(fs, filepath.Join(roots.State, "codex-reset-schedule.json")),
		AfterReset: notifyProxyCodexReset, Backend: backend, Usage: codexprov.New(httpClient), History: historyStore, Attempts: attempts, Cache: quotaCache, Clock: systemClock{},
	}, nil
}

func runCodexResetsList(ctx context.Context, cmd CodexResetsListCmd, jsonOutput bool, deps codexResetsDependencies) error {
	if err := validateCodexResetsDependencies(deps); err != nil {
		return err
	}
	result, err := deps.App.List(ctx, cmd.Reference)
	if err != nil {
		return err
	}
	if jsonOutput {
		return encodeCodexResetJSON(deps.Out, result)
	}
	return renderCodexResetList(deps.Out, result)
}

func runCodexResetsRecommend(ctx context.Context, jsonOutput bool, deps codexResetsDependencies) error {
	if err := validateCodexResetsDependencies(deps); err != nil {
		return err
	}
	schedule, recommendationErr := deps.App.Recommend(ctx)
	var renderErr error
	if jsonOutput {
		renderErr = encodeCodexResetJSON(deps.Out, publicCodexResetSchedule(schedule))
	} else {
		renderErr = renderCodexResetSchedule(deps.Out, schedule)
	}
	if renderErr != nil {
		return renderErr
	}
	return recommendationErr
}

func runCodexResetsUse(ctx context.Context, cmd CodexResetsUseCmd, jsonOutput bool, deps codexResetsDependencies) error {
	if err := validateCodexResetsDependencies(deps); err != nil {
		return err
	}
	if strings.TrimSpace(cmd.Reference) == "" {
		return errors.New("account reference is empty")
	}
	creditID := ""
	if cmd.Credit != nil {
		creditID = strings.TrimSpace(*cmd.Credit)
		if creditID == "" {
			return errors.New("credit ID is empty")
		}
	}

	plan, err := deps.App.PrepareUse(ctx, cmd.Reference, creditID)
	if err != nil {
		return err
	}
	previewWriter := deps.Out
	if jsonOutput {
		previewWriter = deps.ErrOut
	}
	if err := renderCodexResetUsePreview(previewWriter, plan); err != nil {
		return err
	}
	if !cmd.Yes && !confirmCodexResetUse(deps.In, deps.ErrOut) {
		if jsonOutput {
			return encodeCodexResetJSON(deps.Out, struct {
				Status string `json:"status"`
			}{Status: "cancelled"})
		}
		_, err := fmt.Fprintln(deps.Out, "cancelled")
		return err
	}

	result, err := deps.App.ExecuteUse(ctx, plan)
	if err != nil {
		return err
	}
	if jsonOutput {
		return encodeCodexResetJSON(deps.Out, result)
	}
	return renderCodexResetUseResult(deps.Out, result)
}

func validateCodexResetsDependencies(deps codexResetsDependencies) error {
	if deps.App == nil {
		return errors.New("Codex reset service unavailable")
	}
	if deps.Out == nil || deps.ErrOut == nil {
		return errors.New("Codex reset output unavailable")
	}
	return nil
}

func encodeCodexResetJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func publicCodexResetSchedule(schedule aggregate.ResetSchedule) codexResetScheduleJSON {
	result := codexResetScheduleJSON{
		GeneratedAt: schedule.GeneratedAt,
		Horizon:     schedule.Horizon,
		Exact:       schedule.Exact,
		Complete:    schedule.Complete,
		Confidence:  schedule.Confidence,
		Items:       make([]codexResetScheduleItemJSON, len(schedule.Items)),
		Objective: codexResetScheduleObjectiveJSON{
			UnmetDemandPctSeconds: schedule.Objective.UnmetDemandPctSeconds,
			GapDurationSeconds:    schedule.Objective.GapDurationSeconds,
			UsefulExpiredUnused:   schedule.Objective.UsefulExpiredUnused,
			WeightedDiscardedPct:  schedule.Objective.WeightedDiscardedPct,
			RestoredPct:           schedule.Objective.RestoredPct,
		},
		Blockers: make([]codexResetScheduleBlockerJSON, len(schedule.Blockers)),
	}
	for index, item := range schedule.Items {
		mapped := codexResetScheduleItemJSON{
			AccountEmail:  item.AccountEmail,
			AccountID:     item.AccountID,
			CreditID:      item.CreditID,
			Status:        item.Status,
			Confidence:    item.Confidence,
			RestoredPct:   item.RestoredPct,
			AvoidedGapSec: item.AvoidedGapSec,
			ReasonCodes:   append([]aggregate.ResetScheduleReason(nil), item.ReasonCodes...),
		}
		if !item.UseAt.IsZero() {
			useAt := item.UseAt
			mapped.UseAt = &useAt
		}
		if !item.UseBy.IsZero() {
			useBy := item.UseBy
			mapped.UseBy = &useBy
		}
		result.Items[index] = mapped
	}
	for index, blocker := range schedule.Blockers {
		result.Blockers[index] = codexResetScheduleBlockerJSON{
			Code: blocker.Code, AccountEmail: blocker.AccountEmail, AccountID: blocker.AccountID,
		}
	}
	return result
}

func renderCodexResetList(w io.Writer, result app.CodexResetListResult) error {
	var out strings.Builder
	fmt.Fprintln(&out, "Codex banked reset credits")
	for _, account := range result.Accounts {
		fmt.Fprintf(&out, "\n%s\n", codexResetAccountLabel(account.Email, account.AccountID))
		if account.Error != nil {
			fmt.Fprintf(&out, "  Credit inventory unavailable (%s).\n", account.Error.Code)
		}
		if len(account.Credits) == 0 {
			if account.Error == nil {
				fmt.Fprintln(&out, "  No banked credits.")
			}
			continue
		}
		credits := append([]codexprov.ResetCredit(nil), account.Credits...)
		sort.SliceStable(credits, func(i, j int) bool {
			left, right := credits[i].ExpiresAt, credits[j].ExpiresAt
			return left != nil && (right == nil || left.Before(*right))
		})
		for _, credit := range credits {
			expiry := "No expiry"
			if credit.ExpiresAt != nil {
				expiry = "Expires " + formatCodexResetDisplayTime(*credit.ExpiresAt)
			}
			kind := credit.Title
			if kind == "" {
				kind = string(credit.ResetType)
			}
			fmt.Fprintf(&out, "  %s · %s · %s\n    %s\n", kind, credit.Status, expiry, credit.ID)
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func renderCodexResetSchedule(w io.Writer, schedule aggregate.ResetSchedule) error {
	var out strings.Builder
	fmt.Fprintln(&out, "Codex reset recommendation · Advisory")
	if !schedule.Complete {
		fmt.Fprintln(&out, "  Timing unavailable. CQ needs fresh quota and credit inventory for every account.")
	} else {
		if schedule.Confidence != "" {
			fmt.Fprintf(&out, "  Forecast confidence %s\n", schedule.Confidence)
		}
		if !schedule.Exact {
			fmt.Fprintln(&out, "  Approximate plan. Timing can change as usage changes.")
		}
	}
	items := append([]aggregate.ResetScheduleItem(nil), schedule.Items...)
	sort.SliceStable(items, func(left, right int) bool {
		leftTime, rightTime := items[left].UseAt, items[right].UseAt
		if leftTime.IsZero() != rightTime.IsZero() {
			return !leftTime.IsZero()
		}
		if !leftTime.Equal(rightTime) {
			return leftTime.Before(rightTime)
		}
		if items[left].AccountEmail != items[right].AccountEmail {
			return items[left].AccountEmail < items[right].AccountEmail
		}
		return items[left].CreditID < items[right].CreditID
	})
	if len(items) == 0 && schedule.Complete {
		fmt.Fprintln(&out, "  No banked credits available.")
	}
	for _, item := range items {
		fmt.Fprintf(&out, "\n%s · %s\n", codexResetAccountLabel(item.AccountEmail, item.AccountID), codexResetRecommendationStatus(item.Status))
		if !item.UseAt.IsZero() {
			fmt.Fprintf(&out, "  Use %s · Forecast deadline %s\n", formatCodexResetDisplayTime(item.UseAt), formatCodexResetDisplayTime(item.UseBy))
			fmt.Fprintf(&out, "  Estimated recovery %s · %s\n", formatCodexRestoredPct(item.RestoredPct), primaryCodexResetReason(item.ReasonCodes))
		}
		fmt.Fprintf(&out, "  Credit %s\n", item.CreditID)
	}
	for _, blocker := range schedule.Blockers {
		fmt.Fprintf(&out, "\n  %s · %s\n", codexResetAccountLabel(blocker.AccountEmail, blocker.AccountID), strings.ReplaceAll(blocker.Code, "_", " "))
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func codexResetRecommendationStatus(status aggregate.ResetScheduleStatus) string {
	switch status {
	case aggregate.ResetDueNow:
		return "Due now"
	case aggregate.ResetScheduled:
		return "Planned"
	case aggregate.ResetDeferred:
		return "Deferred"
	case aggregate.ResetNotUseful:
		return "Waiting for useful recovery"
	case aggregate.ResetUnsupported:
		return "Unsupported credit"
	default:
		return string(status)
	}
}

func renderCodexResetUsePreview(w io.Writer, plan app.CodexResetUsePlan) error {
	if _, err := fmt.Fprintf(w, "Account: %s\nCredit: %s\nExpires: %s\nCurrent shared windows:\n",
		codexResetAccountLabel(plan.Email, plan.AccountID), codexResetCreditLabel(plan.Credit), formatCodexResetExpiry(plan.Credit.ExpiresAt)); err != nil {
		return err
	}
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		window, ok := plan.CurrentWindows[name]
		if !ok {
			continue
		}
		if _, err := fmt.Fprintf(w, "  %s: %s%% (natural reset %s)\n", name, formatCodexRemainingPct(window), formatCodexResetTime(time.Unix(window.ResetAtUnix, 0))); err != nil {
			return err
		}
	}
	if plan.Recommendation == nil {
		_, err := fmt.Fprintln(w, "Recommendation: unavailable")
		return err
	}
	for _, item := range plan.Recommendation.Items {
		if item.CreditID != plan.Credit.ID || (plan.AccountID != "" && item.AccountID != plan.AccountID) {
			continue
		}
		_, err := fmt.Fprintf(w, "Recommendation: %s; use at %s; forecast deadline %s; %s\n",
			codexResetRecommendationStatus(item.Status), formatCodexResetTime(item.UseAt), formatCodexResetTime(item.UseBy), primaryCodexResetReason(item.ReasonCodes))
		return err
	}
	_, err := fmt.Fprintf(w, "Recommendation: complete=%t, no actionable time for selected credit\n", plan.Recommendation.Complete)
	return err
}

func renderCodexResetUseResult(w io.Writer, result app.CodexResetUseResult) error {
	if _, err := fmt.Fprintf(w, "Codex banked reset %s\n  account: %s\n  credit: %s\n  windows reset: %d\n",
		result.Outcome, codexResetAccountLabel(result.Email, result.AccountID), result.CreditID, result.WindowsReset); err != nil {
		return err
	}
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		change, ok := result.ChangedWindows[name]
		if !ok {
			continue
		}
		if _, err := fmt.Fprintf(w, "  %s: %s%% -> %s%%\n", name, formatCodexRemainingPct(change.Before), formatCodexRemainingPct(change.After)); err != nil {
			return err
		}
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(w, "  %s\n", codexResetWarningMessage(warning.Code)); err != nil {
			return err
		}
	}
	return nil
}

func codexResetWarningMessage(code string) string {
	switch code {
	case "redistribution_failed":
		return "Quota reset recorded. CQ could not complete the routing refresh."
	case "usage_refetch_failed", "usage_match_failed":
		return "CQ could not verify quota after the reset."
	case "cache_invalidate_failed":
		return "CQ could not clear cached quota."
	case "history_update_failed":
		return "CQ could not update quota history."
	case "attempt_cleanup_failed":
		return "CQ could not clear the saved attempt."
	case "schedule_update_failed":
		return "CQ could not record the result in the schedule."
	default:
		return "Warning: " + code
	}
}

func confirmCodexResetUse(input io.Reader, output io.Writer) bool {
	if _, err := fmt.Fprint(output, "Use this banked reset? [y/N] "); err != nil {
		return false
	}
	if input == nil || !codexResetInputIsInteractive(input) {
		return false
	}
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func codexResetInputIsInteractive(input io.Reader) bool {
	file, ok := input.(*os.File)
	if !ok {
		return true
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func codexResetAccountLabel(email, accountID string) string {
	if email != "" {
		return email
	}
	if accountID != "" {
		return accountID
	}
	return "unknown account"
}

func codexResetCreditLabel(credit codexprov.ResetCredit) string {
	if credit.Title == "" {
		return credit.ID
	}
	return credit.Title + " (" + credit.ID + ")"
}

func formatCodexResetExpiry(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "never"
	}
	return formatCodexResetTime(*expiresAt)
}

func formatCodexResetTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.UTC().Format(time.RFC3339)
}

func formatCodexResetDisplayTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.UTC().Format("2 Jan 2006 15:04 UTC")
}

func formatCodexRemainingPct(window quota.Window) string {
	if window.RemainingPctExact != nil {
		return fmt.Sprintf("%.2f", *window.RemainingPctExact)
	}
	return fmt.Sprintf("%d", window.RemainingPct)
}

func formatCodexRestoredPct(restored map[quota.WindowName]float64) string {
	parts := make([]string, 0, 2)
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		if value, ok := restored[name]; ok {
			parts = append(parts, fmt.Sprintf("%s %.2fpp", name, value))
		}
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}

func primaryCodexResetReason(reasons []aggregate.ResetScheduleReason) string {
	if len(reasons) == 0 {
		return "—"
	}
	switch reasons[0] {
	case aggregate.ResetReasonGapAvoidance:
		return "Avoids running out of quota"
	case aggregate.ResetReasonExpiryPressure:
		return "Uses credit before expiry"
	case aggregate.ResetReasonNaturalReset:
		return "Accounts for a natural reset"
	case aggregate.ResetReasonWasteReduction:
		return "Preserves existing quota"
	case aggregate.ResetReasonRateFallback:
		return "Uses estimated usage rate"
	default:
		return string(reasons[0])
	}
}
