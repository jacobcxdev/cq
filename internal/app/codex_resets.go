package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

type CodexResetBackend interface {
	Snapshot(context.Context) (codexprov.ResetAccountSnapshot, error)
	ListCredits(context.Context, codexprov.ResetAccount) (codexprov.ResetCreditInventory, error)
	Consume(context.Context, codexprov.ResetAccount, string, string) (codexprov.ConsumeResetResult, error)
}

type CodexResetUsage interface {
	Fetch(context.Context, time.Time) ([]quota.Result, error)
}

type CodexResetHistory interface {
	UpdateAndGetEstimates(context.Context, map[string][]quota.Result, int64) (history.BurnRates, history.RateEstimates, error)
}

type CodexResetAttempts interface {
	Pending(codexprov.AccountKey) ([]codexprov.ResetAttempt, error)
	Ensure(codexprov.AccountKey, string, time.Time) (codexprov.ResetAttempt, error)
	Remove(codexprov.AccountKey, string) error
}

type CodexResetApp struct {
	Backend  CodexResetBackend
	Usage    CodexResetUsage
	History  CodexResetHistory
	Attempts CodexResetAttempts
	Cache    Cache
	Clock    Clock
}

type CodexResetPublicError struct {
	Code string `json:"code"`
}

type CodexResetAccountCredits struct {
	AccountID string                  `json:"account_id,omitempty"`
	Email     string                  `json:"email,omitempty"`
	Credits   []codexprov.ResetCredit `json:"credits"`
	Error     *CodexResetPublicError  `json:"error,omitempty"`
}

type CodexResetListResult struct {
	Accounts []CodexResetAccountCredits `json:"accounts"`
}

type CodexResetUsePlan struct {
	AccountID      string                            `json:"account_id,omitempty"`
	Email          string                            `json:"email,omitempty"`
	Credit         codexprov.ResetCredit             `json:"credit"`
	CurrentWindows map[quota.WindowName]quota.Window `json:"current_windows"`
	Recommendation *aggregate.ResetSchedule          `json:"recommendation,omitempty"`
	account        codexprov.ResetAccount
	selection      codexprov.ResetCreditSelection
}

type CodexResetWindowChange struct {
	Before quota.Window `json:"before"`
	After  quota.Window `json:"after"`
}

type CodexResetWarning struct {
	Code string `json:"code"`
}

type CodexResetUseResult struct {
	AccountID      string                                      `json:"account_id,omitempty"`
	Email          string                                      `json:"email,omitempty"`
	CreditID       string                                      `json:"credit_id"`
	Outcome        codexprov.ConsumeResetOutcome               `json:"outcome"`
	WindowsReset   int64                                       `json:"windows_reset"`
	ChangedWindows map[quota.WindowName]CodexResetWindowChange `json:"changed_windows,omitempty"`
	Warnings       []CodexResetWarning                         `json:"warnings,omitempty"`
}

type CodexResetError struct {
	Code string
	Err  error
}

func (e *CodexResetError) Error() string {
	if e == nil {
		return "Codex reset failed"
	}
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *CodexResetError) Unwrap() error { return e.Err }

type codexResetCreditRead struct {
	inventory codexprov.ResetCreditInventory
	err       error
}

func (a *CodexResetApp) List(ctx context.Context, reference string) (CodexResetListResult, error) {
	snapshot, err := a.snapshot(ctx)
	if err != nil {
		return CodexResetListResult{}, resetAppError("credential_unavailable", err)
	}
	accounts := snapshot.Accounts
	if strings.TrimSpace(reference) != "" {
		account, err := snapshot.ResolveReference(reference)
		if err != nil {
			return CodexResetListResult{}, resetAppError("account_reference_invalid", err)
		}
		accounts = []codexprov.ResetAccount{account}
	}
	reads := a.readCredits(ctx, accounts)
	result := CodexResetListResult{Accounts: make([]CodexResetAccountCredits, len(accounts))}
	for index, account := range accounts {
		row := CodexResetAccountCredits{
			AccountID: account.AccountID, Email: account.Email,
			Credits: append([]codexprov.ResetCredit{}, reads[index].inventory.Credits...),
		}
		if reads[index].err != nil {
			row.Error = &CodexResetPublicError{Code: resetReadErrorCode(reads[index].err)}
		}
		result.Accounts[index] = row
	}
	return result, nil
}

func (a *CodexResetApp) Recommend(ctx context.Context) (aggregate.ResetSchedule, error) {
	return a.recommend(ctx, true)
}

func (a *CodexResetApp) recommend(ctx context.Context, updateHistory bool) (aggregate.ResetSchedule, error) {
	now := a.now()
	snapshot, err := a.snapshot(ctx)
	if err != nil {
		return aggregate.ResetSchedule{GeneratedAt: now, Complete: false, Exact: false}, resetAppError("credential_unavailable", err)
	}
	usage, usageErr, creditReads := a.readRecommendationInputs(ctx, snapshot.Accounts, now)

	var estimates history.RateEstimates
	if updateHistory && usageErr == nil && a.History != nil {
		_, estimates, _ = callResetHistory(ctx, a.History, usage, now)
	}

	input := aggregate.ResetScheduleInput{Now: now}
	blockers := make([]aggregate.ResetScheduleBlocker, 0)
	if usageErr != nil {
		blockers = append(blockers, aggregate.ResetScheduleBlocker{Code: "usage_unavailable"})
	}
	for index, account := range snapshot.Accounts {
		if creditReads[index].err != nil {
			blockers = append(blockers, resetScheduleBlocker(account, resetReadErrorCode(creditReads[index].err)))
			continue
		}
		usageResult, matchCode := matchResetUsage(account, usage)
		if usageErr != nil || matchCode != "" {
			if usageErr == nil {
				blockers = append(blockers, resetScheduleBlocker(account, matchCode))
			}
			continue
		}
		accountInput, ok := resetScheduleAccountInput(account, usageResult, creditReads[index].inventory.Credits, estimates)
		if !ok {
			blockers = append(blockers, resetScheduleBlocker(account, "usage_windows_invalid"))
			continue
		}
		input.Accounts = append(input.Accounts, accountInput)
	}

	schedule := aggregate.RecommendResetSchedule(input)
	if len(blockers) > 0 || !schedule.Complete {
		schedule.Blockers = append(schedule.Blockers, blockers...)
		sortResetScheduleBlockers(schedule.Blockers)
		schedule.Complete = false
		schedule.Exact = false
		for index := range schedule.Items {
			if schedule.Items[index].Status == aggregate.ResetScheduled || schedule.Items[index].Status == aggregate.ResetDueNow {
				schedule.Items[index].Status = aggregate.ResetDeferred
			}
			schedule.Items[index].UseAt = time.Time{}
			schedule.Items[index].UseBy = time.Time{}
		}
		return schedule, resetAppError("recommendation_incomplete", errors.New("fresh reset inputs are incomplete"))
	}
	return schedule, nil
}

func (a *CodexResetApp) PrepareUse(ctx context.Context, reference, explicitCreditID string) (CodexResetUsePlan, error) {
	if a.Attempts == nil {
		return CodexResetUsePlan{}, resetAppError("consume_failed", errors.New("reset attempt storage unavailable"))
	}
	snapshot, err := a.snapshot(ctx)
	if err != nil {
		return CodexResetUsePlan{}, resetAppError("credential_unavailable", err)
	}
	account, err := snapshot.ResolveReference(reference)
	if err != nil {
		return CodexResetUsePlan{}, resetAppError("account_reference_invalid", err)
	}
	inventory, err := a.Backend.ListCredits(ctx, account)
	if err != nil {
		return CodexResetUsePlan{}, resetAppError(resetReadErrorCode(err), err)
	}
	pending, err := a.Attempts.Pending(account.AccountKey)
	if err != nil {
		return CodexResetUsePlan{}, resetAppError("consume_indeterminate", err)
	}
	selection, err := codexprov.SelectResetCredit(a.now(), inventory.Credits, pending, explicitCreditID)
	if err != nil {
		return CodexResetUsePlan{}, resetAppError(resetSelectionErrorCode(a.now(), inventory.Credits, explicitCreditID, err), err)
	}
	usage, err := callResetUsage(ctx, a.Usage, a.now())
	if err != nil {
		return CodexResetUsePlan{}, resetAppError("credits_unavailable", err)
	}
	usageResult, matchCode := matchResetUsage(account, usage)
	if matchCode != "" {
		return CodexResetUsePlan{}, resetAppError("credits_unavailable", errors.New(matchCode))
	}
	recommendation, _ := a.recommend(ctx, false)
	return CodexResetUsePlan{
		AccountID: account.AccountID, Email: account.Email, Credit: selection.Credit,
		CurrentWindows: sharedResetQuotaWindows(usageResult.Windows), Recommendation: &recommendation,
		account: account, selection: selection,
	}, nil
}

func (a *CodexResetApp) ExecuteUse(ctx context.Context, plan CodexResetUsePlan) (CodexResetUseResult, error) {
	if a.Attempts == nil || plan.account.AccountKey == "" || plan.Credit.ID == "" {
		return CodexResetUseResult{}, resetAppError("consume_failed", errors.New("invalid reset use plan"))
	}
	snapshot, err := a.snapshot(ctx)
	if err != nil {
		return CodexResetUseResult{}, resetAppError("credential_unavailable", err)
	}
	account, err := snapshot.ResolveReference(string(plan.account.AccountKey))
	if err != nil || account.AccountKey != plan.account.AccountKey {
		return CodexResetUseResult{}, resetAppError("account_reference_invalid", err)
	}
	inventory, err := a.Backend.ListCredits(ctx, account)
	if err != nil {
		return CodexResetUseResult{}, resetAppError(resetReadErrorCode(err), err)
	}
	pending, err := a.Attempts.Pending(account.AccountKey)
	if err != nil {
		return CodexResetUseResult{}, resetAppError("consume_indeterminate", err)
	}
	selection, err := codexprov.SelectResetCredit(a.now(), inventory.Credits, pending, plan.Credit.ID)
	if err != nil {
		return CodexResetUseResult{}, resetAppError(resetSelectionErrorCode(a.now(), inventory.Credits, plan.Credit.ID, err), err)
	}
	if selection.Credit.ID != plan.Credit.ID {
		return CodexResetUseResult{}, resetAppError("credit_not_found", errors.New("reset credit selection changed"))
	}
	attempt, err := a.Attempts.Ensure(account.AccountKey, selection.Credit.ID, a.now())
	if err != nil {
		return CodexResetUseResult{}, resetAppError("consume_failed", err)
	}
	consumed, err := callResetConsume(ctx, a.Backend, account, selection.Credit.ID, attempt.IdempotencyKey)
	if err != nil {
		reference := account.Email
		if reference == "" {
			reference = account.AccountID
		}
		return CodexResetUseResult{}, resetAppError(classifyResetConsumeError(err), fmt.Errorf("retry with cq codex resets use %s --credit %s: %w", reference, selection.Credit.ID, err))
	}
	result := CodexResetUseResult{
		AccountID: account.AccountID, Email: account.Email, CreditID: selection.Credit.ID,
		Outcome: consumed.Outcome, WindowsReset: consumed.WindowsReset,
	}
	switch consumed.Outcome {
	case codexprov.ConsumeReset, codexprov.ConsumeAlreadyRedeemed:
		a.finishResetAttempt(&result, account, selection.Credit.ID)
		a.refreshAfterReset(ctx, &result, account, plan.CurrentWindows)
		return result, nil
	case codexprov.ConsumeNothingToReset, codexprov.ConsumeNoCredit:
		a.finishResetAttempt(&result, account, selection.Credit.ID)
		return result, nil
	default:
		return CodexResetUseResult{}, resetAppError("consume_indeterminate", errors.New("unknown reset consume outcome"))
	}
}

func callResetConsume(
	ctx context.Context,
	backend CodexResetBackend,
	account codexprov.ResetAccount,
	creditID string,
	idempotencyKey string,
) (result codexprov.ConsumeResetResult, err error) {
	defer func() {
		if recover() != nil {
			result = codexprov.ConsumeResetResult{}
			err = errors.New("reset consume panic")
		}
	}()
	return backend.Consume(ctx, account, creditID, idempotencyKey)
}

func callResetUsage(ctx context.Context, usage CodexResetUsage, now time.Time) (results []quota.Result, err error) {
	if usage == nil {
		return nil, errors.New("Codex usage unavailable")
	}
	defer func() {
		if recover() != nil {
			results = nil
			err = errors.New("usage fetch panic")
		}
	}()
	return usage.Fetch(ctx, now)
}

func callResetHistory(
	ctx context.Context,
	historyStore CodexResetHistory,
	usage []quota.Result,
	now time.Time,
) (rates history.BurnRates, estimates history.RateEstimates, err error) {
	defer func() {
		if recover() != nil {
			rates = nil
			estimates = nil
			err = errors.New("reset history panic")
		}
	}()
	return historyStore.UpdateAndGetEstimates(ctx, map[string][]quota.Result{string(provider.Codex): usage}, now.Unix())
}

func (a *CodexResetApp) snapshot(ctx context.Context) (codexprov.ResetAccountSnapshot, error) {
	if a == nil || a.Backend == nil {
		return codexprov.ResetAccountSnapshot{}, errors.New("Codex reset backend unavailable")
	}
	return a.Backend.Snapshot(ctx)
}

func (a *CodexResetApp) now() time.Time {
	if a != nil && a.Clock != nil {
		return a.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *CodexResetApp) readCredits(ctx context.Context, accounts []codexprov.ResetAccount) []codexResetCreditRead {
	reads := make([]codexResetCreditRead, len(accounts))
	var wait sync.WaitGroup
	for index := range accounts {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			defer func() {
				if recover() != nil {
					reads[index].err = errors.New("credit inventory panic")
				}
			}()
			reads[index].inventory, reads[index].err = a.Backend.ListCredits(ctx, accounts[index])
		}(index)
	}
	wait.Wait()
	return reads
}

func (a *CodexResetApp) readRecommendationInputs(ctx context.Context, accounts []codexprov.ResetAccount, now time.Time) ([]quota.Result, error, []codexResetCreditRead) {
	reads := make([]codexResetCreditRead, len(accounts))
	var usage []quota.Result
	var usageErr error
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer func() {
			if recover() != nil {
				usageErr = errors.New("usage fetch panic")
			}
		}()
		if a.Usage == nil {
			usageErr = errors.New("Codex usage unavailable")
			return
		}
		usage, usageErr = callResetUsage(ctx, a.Usage, now)
	}()
	for index := range accounts {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			defer func() {
				if recover() != nil {
					reads[index].err = errors.New("credit inventory panic")
				}
			}()
			reads[index].inventory, reads[index].err = a.Backend.ListCredits(ctx, accounts[index])
		}(index)
	}
	wait.Wait()
	return usage, usageErr, reads
}

func matchResetUsage(account codexprov.ResetAccount, results []quota.Result) (quota.Result, string) {
	matches := make([]quota.Result, 0, 1)
	for _, result := range results {
		if !result.IsUsable() || result.CacheAge != 0 {
			continue
		}
		if account.AccountID != "" {
			if result.AccountID == account.AccountID {
				matches = append(matches, result)
			}
			continue
		}
		if account.Email != "" && strings.EqualFold(strings.TrimSpace(result.Email), strings.TrimSpace(account.Email)) {
			matches = append(matches, result)
		}
	}
	if len(matches) == 0 {
		return quota.Result{}, "usage_missing"
	}
	if len(matches) != 1 {
		return quota.Result{}, "usage_duplicate"
	}
	return matches[0], ""
}

func resetScheduleAccountInput(account codexprov.ResetAccount, result quota.Result, credits []codexprov.ResetCredit, estimates history.RateEstimates) (aggregate.ResetScheduleAccountInput, bool) {
	input := aggregate.ResetScheduleAccountInput{
		Key: string(account.AccountKey), Email: account.Email, AccountID: account.AccountID,
		Multiplier: quota.ExtractMultiplier(result.RateLimitTier),
		Windows:    make(map[quota.WindowName]aggregate.ResetScheduleWindowInput),
	}
	accountKey := result.AccountID
	if accountKey == "" {
		accountKey = result.Email
	}
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		window, ok := result.Windows[name]
		if !ok || window.ResetAtUnix <= 0 {
			return aggregate.ResetScheduleAccountInput{}, false
		}
		remaining := float64(window.RemainingPct)
		if window.RemainingPctExact != nil {
			remaining = *window.RemainingPctExact
		}
		mapped := aggregate.ResetScheduleWindowInput{
			RemainingPct: remaining, ResetAt: time.Unix(window.ResetAtUnix, 0).UTC(), Period: quota.PeriodFor(name),
			RateSource: aggregate.ResetRateCycleAverage,
		}
		if estimate, ok := estimates.Get(history.BurnRateKey{ProviderID: string(provider.Codex), AccountKey: accountKey, Window: string(name)}); ok {
			mapped.BurnPctPerS = estimate.RatePctPerS
			mapped.RateSource = aggregate.ResetRateEWMA
			mapped.Samples = estimate.Samples
		}
		input.Windows[name] = mapped
	}
	for _, credit := range credits {
		if credit.Status != codexprov.ResetCreditAvailable {
			continue
		}
		input.Credits = append(input.Credits, aggregate.ResetScheduleCreditInput{
			ID: credit.ID, GrantedAt: credit.GrantedAt, ExpiresAt: credit.ExpiresAt,
			Supported: credit.ResetType == codexprov.ResetTypeCodexRateLimits,
		})
	}
	return input, true
}

func resetScheduleBlocker(account codexprov.ResetAccount, code string) aggregate.ResetScheduleBlocker {
	return aggregate.ResetScheduleBlocker{Code: code, AccountEmail: account.Email, AccountID: account.AccountID}
}

func sortResetScheduleBlockers(blockers []aggregate.ResetScheduleBlocker) {
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].AccountEmail != blockers[j].AccountEmail {
			return blockers[i].AccountEmail < blockers[j].AccountEmail
		}
		if blockers[i].AccountID != blockers[j].AccountID {
			return blockers[i].AccountID < blockers[j].AccountID
		}
		return blockers[i].Code < blockers[j].Code
	})
}

func sharedResetQuotaWindows(windows map[quota.WindowName]quota.Window) map[quota.WindowName]quota.Window {
	shared := make(map[quota.WindowName]quota.Window, 2)
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		if window, ok := windows[name]; ok {
			shared[name] = window
		}
	}
	return shared
}

func resetReadErrorCode(err error) string {
	var statusErr *codexprov.ResetHTTPError
	if errors.As(err, &statusErr) && (statusErr.Status == http.StatusUnauthorized || statusErr.Status == http.StatusForbidden) {
		return "auth_expired"
	}
	return "credits_unavailable"
}

func resetSelectionErrorCode(now time.Time, credits []codexprov.ResetCredit, explicitID string, err error) string {
	var selectionErr *codexprov.ResetCreditSelectionError
	if errors.As(err, &selectionErr) && selectionErr.Code == "pending_attempt_conflict" {
		return "consume_indeterminate"
	}
	if explicitID != "" {
		for _, credit := range credits {
			if credit.ID != explicitID {
				continue
			}
			if credit.ResetType != codexprov.ResetTypeCodexRateLimits {
				return "unsupported_reset_type"
			}
			if credit.ExpiresAt != nil && !credit.ExpiresAt.After(now) {
				return "credit_expired"
			}
		}
	}
	return "credit_not_found"
}

func classifyResetConsumeError(err error) string {
	var statusErr *codexprov.ResetHTTPError
	if errors.As(err, &statusErr) && statusErr.Status >= 400 && statusErr.Status < 500 {
		return "consume_failed"
	}
	return "consume_indeterminate"
}

func (a *CodexResetApp) finishResetAttempt(result *CodexResetUseResult, account codexprov.ResetAccount, creditID string) {
	if err := a.Attempts.Remove(account.AccountKey, creditID); err != nil {
		result.Warnings = append(result.Warnings, CodexResetWarning{Code: "attempt_cleanup_failed"})
	}
}

func (a *CodexResetApp) refreshAfterReset(ctx context.Context, result *CodexResetUseResult, account codexprov.ResetAccount, before map[quota.WindowName]quota.Window) {
	if a.Cache != nil {
		if err := a.Cache.Delete(ctx, string(provider.Codex)); err != nil {
			result.Warnings = append(result.Warnings, CodexResetWarning{Code: "cache_invalidate_failed"})
		}
	}
	if a.Usage == nil {
		result.Warnings = append(result.Warnings, CodexResetWarning{Code: "usage_refetch_failed"})
		return
	}
	now := a.now()
	usage, err := callResetUsage(ctx, a.Usage, now)
	if err != nil {
		result.Warnings = append(result.Warnings, CodexResetWarning{Code: "usage_refetch_failed"})
		return
	}
	afterResult, matchCode := matchResetUsage(account, usage)
	if matchCode != "" {
		result.Warnings = append(result.Warnings, CodexResetWarning{Code: "usage_match_failed"})
	} else {
		after := sharedResetQuotaWindows(afterResult.Windows)
		changes := make(map[quota.WindowName]CodexResetWindowChange)
		for name, beforeWindow := range before {
			afterWindow, ok := after[name]
			if ok && !resetQuotaWindowsEqual(beforeWindow, afterWindow) {
				changes[name] = CodexResetWindowChange{Before: beforeWindow, After: afterWindow}
			}
		}
		if len(changes) > 0 {
			result.ChangedWindows = changes
		}
	}
	if a.History != nil {
		if _, _, err := callResetHistory(ctx, a.History, usage, now); err != nil {
			result.Warnings = append(result.Warnings, CodexResetWarning{Code: "history_update_failed"})
		}
	}
}

func resetQuotaWindowsEqual(left, right quota.Window) bool {
	if left.RemainingPct != right.RemainingPct || left.ResetAtUnix != right.ResetAtUnix {
		return false
	}
	if left.RemainingPctExact == nil || right.RemainingPctExact == nil {
		return left.RemainingPctExact == nil && right.RemainingPctExact == nil
	}
	return *left.RemainingPctExact == *right.RemainingPctExact
}

func resetAppError(code string, err error) error {
	return &CodexResetError{Code: code, Err: err}
}
