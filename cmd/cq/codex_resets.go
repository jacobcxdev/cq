package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	"github.com/jacobcxdev/cq/internal/quota"
)

type codexResetsDependencies struct {
	App    *app.CodexResetApp
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
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
	store, err := codexprov.NewManagedStore(fs)
	if err != nil {
		return codexResetsDependencies{}, func() {}, err
	}
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

	cacheRoot, err := cache.DefaultDir()
	if err != nil {
		return fail(err)
	}
	attempts, err := codexprov.NewResetAttemptStore(fs, cacheRoot)
	if err != nil {
		return fail(err)
	}
	historyStore, err := history.New(fs, cacheRoot)
	if err != nil {
		return fail(err)
	}
	quotaCache, err := cache.New(cache.OSFileSystem{}, cacheRoot, cacheTTL())
	if err != nil {
		return fail(err)
	}

	backend := &codexprov.ResetBackend{
		Inventory: control,
		Resolver:  control,
		Refresh:   control,
		Aliases: func() (codexprov.AccountAliasIndex, error) {
			return (codexprov.Registry{FS: fs, Home: store.Home}).AccountAliasIndex()
		},
		Credits: codexprov.ResetCreditClient{HTTP: httpClient},
		Now:     time.Now,
	}
	service := &app.CodexResetApp{
		Backend:  backend,
		Usage:    codexprov.New(httpClient),
		History:  historyStore,
		Attempts: attempts,
		Cache:    quotaCache,
		Clock:    systemClock{},
	}
	return codexResetsDependencies{
		App: service, In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr,
	}, closeDependencies, nil
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
	for accountIndex, account := range result.Accounts {
		if accountIndex > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, codexResetAccountLabel(account.Email, account.AccountID)); err != nil {
			return err
		}
		if account.Error != nil {
			if _, err := fmt.Fprintf(w, "  error: %s\n", account.Error.Code); err != nil {
				return err
			}
			continue
		}
		if len(account.Credits) == 0 {
			if _, err := fmt.Fprintln(w, "  no reset credits"); err != nil {
				return err
			}
			continue
		}
		for _, credit := range account.Credits {
			if _, err := fmt.Fprintf(w, "  %s\n    type: %s\n    status: %s\n    granted: %s\n    expires: %s\n",
				credit.ID, credit.ResetType, credit.Status, formatCodexResetTime(credit.GrantedAt), formatCodexResetExpiry(credit.ExpiresAt)); err != nil {
				return err
			}
			if credit.Title != "" {
				if _, err := fmt.Fprintf(w, "    title: %s\n", credit.Title); err != nil {
					return err
				}
			}
			if credit.Description != "" {
				if _, err := fmt.Fprintf(w, "    description: %s\n", credit.Description); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func renderCodexResetSchedule(w io.Writer, schedule aggregate.ResetSchedule) error {
	if _, err := fmt.Fprintf(w, "Codex banked reset schedule\n  exact: %t\n  complete: %t\n  confidence: %s\n",
		schedule.Exact, schedule.Complete, schedule.Confidence); err != nil {
		return err
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
	for _, item := range items {
		if _, err := fmt.Fprintf(w, "\n%s — %s\n  status: %s\n  use at: %s\n  use by: %s\n  restored: %s\n  reason: %s\n  confidence: %s\n",
			codexResetAccountLabel(item.AccountEmail, item.AccountID), item.CreditID, item.Status,
			formatCodexResetTime(item.UseAt), formatCodexResetTime(item.UseBy), formatCodexRestoredPct(item.RestoredPct),
			primaryCodexResetReason(item.ReasonCodes), item.Confidence); err != nil {
			return err
		}
	}
	for _, blocker := range schedule.Blockers {
		if _, err := fmt.Fprintf(w, "\nblocker: %s (%s)\n", blocker.Code, codexResetAccountLabel(blocker.AccountEmail, blocker.AccountID)); err != nil {
			return err
		}
	}
	return nil
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
		_, err := fmt.Fprintf(w, "Recommendation: %s; use at %s; use by %s; reason %s\n",
			item.Status, formatCodexResetTime(item.UseAt), formatCodexResetTime(item.UseBy), primaryCodexResetReason(item.ReasonCodes))
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
		if _, err := fmt.Fprintf(w, "  warning: %s\n", warning.Code); err != nil {
			return err
		}
	}
	return nil
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
	return string(reasons[0])
}
