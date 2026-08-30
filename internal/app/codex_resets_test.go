package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/history"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

type resetTestClock struct{ now time.Time }

func (c resetTestClock) Now() time.Time { return c.now }

type recordingResetBackend struct {
	snapshot     codexprov.ResetAccountSnapshot
	snapshotErr  error
	credits      map[codexprov.AccountKey]codexprov.ResetCreditInventory
	creditErrors map[codexprov.AccountKey]error
	creditPanics map[codexprov.AccountKey]bool
	consume      codexprov.ConsumeResetResult
	consumeErr   error
	consumePanic bool
	consumeCalls int
	consumeHook  func()
}

func (b *recordingResetBackend) Snapshot(context.Context) (codexprov.ResetAccountSnapshot, error) {
	return b.snapshot, b.snapshotErr
}

func (b *recordingResetBackend) ListCredits(_ context.Context, account codexprov.ResetAccount) (codexprov.ResetCreditInventory, error) {
	if b.creditPanics[account.AccountKey] {
		panic("credit panic")
	}
	return b.credits[account.AccountKey], b.creditErrors[account.AccountKey]
}

func (b *recordingResetBackend) Consume(context.Context, codexprov.ResetAccount, string, string) (codexprov.ConsumeResetResult, error) {
	b.consumeCalls++
	if b.consumePanic {
		panic("consume panic")
	}
	if b.consumeHook != nil {
		b.consumeHook()
	}
	return b.consume, b.consumeErr
}

type recordingResetUsage struct {
	results []quota.Result
	err     error
	panic   bool
	calls   int
}

func (u *recordingResetUsage) Fetch(context.Context, time.Time) ([]quota.Result, error) {
	u.calls++
	if u.panic {
		panic("usage panic")
	}
	return u.results, u.err
}

type recordingResetHistory struct {
	estimates history.RateEstimates
	err       error
	panic     bool
	calls     int
}

func (h *recordingResetHistory) UpdateAndGetEstimates(_ context.Context, _ map[string][]quota.Result, _ int64) (history.BurnRates, history.RateEstimates, error) {
	h.calls++
	if h.panic {
		panic("history panic")
	}
	return nil, h.estimates, h.err
}

type recordingResetAttempts struct {
	pending     map[codexprov.AccountKey][]codexprov.ResetAttempt
	ensureCalls int
	removeCalls int
	ensureHook  func()
	removeErr   error
}

func (a *recordingResetAttempts) Pending(account codexprov.AccountKey) ([]codexprov.ResetAttempt, error) {
	return append([]codexprov.ResetAttempt(nil), a.pending[account]...), nil
}

func (a *recordingResetAttempts) Ensure(account codexprov.AccountKey, creditID string, now time.Time) (codexprov.ResetAttempt, error) {
	a.ensureCalls++
	if a.ensureHook != nil {
		a.ensureHook()
	}
	attempt := codexprov.ResetAttempt{
		Version: 1, AccountKey: account, CreditID: creditID,
		IdempotencyKey: codexprov.ResetIdempotencyKey(account, creditID), StartedAt: now,
	}
	a.pending[account] = []codexprov.ResetAttempt{attempt}
	return attempt, nil
}

func (a *recordingResetAttempts) Remove(account codexprov.AccountKey, creditID string) error {
	a.removeCalls++
	if a.removeErr == nil {
		delete(a.pending, account)
	}
	return a.removeErr
}

type recordingResetCache struct{ deleteCalls int }

func (c *recordingResetCache) Get(context.Context, string) ([]quota.Result, bool, error) {
	return nil, false, nil
}
func (c *recordingResetCache) Put(context.Context, string, []quota.Result) error { return nil }
func (c *recordingResetCache) Delete(context.Context, string) error {
	c.deleteCalls++
	return nil
}
func (c *recordingResetCache) Age(context.Context, string) (time.Duration, bool) { return 0, false }

func resetAppSnapshot() codexprov.ResetAccountSnapshot {
	return codexprov.ResetAccountSnapshot{Accounts: []codexprov.ResetAccount{
		{AccountKey: "account-a", AccountID: "acct-a", Email: "a@example.com", PlanType: "pro", Active: true},
		{AccountKey: "account-b", AccountID: "acct-b", Email: "b@example.com", PlanType: "pro"},
		{AccountKey: "account-c", AccountID: "acct-c", Email: "c@example.com", PlanType: "pro"},
	}, Inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{
		{Key: "account-a", Identity: codexprov.AccountIdentity{AccountID: "acct-a", Email: "a@example.com"}},
		{Key: "account-b", Identity: codexprov.AccountIdentity{AccountID: "acct-b", Email: "b@example.com"}},
		{Key: "account-c", Identity: codexprov.AccountIdentity{AccountID: "acct-c", Email: "c@example.com"}},
	}}}
}

func resetAppCredit(id string, expiresAt time.Time) codexprov.ResetCredit {
	return codexprov.ResetCredit{
		ID: id, ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditAvailable,
		GrantedAt: expiresAt.Add(-24 * time.Hour), ExpiresAt: &expiresAt,
	}
}

func resetAppUsage(now time.Time, accounts ...string) []quota.Result {
	results := make([]quota.Result, 0, len(accounts))
	for _, suffix := range accounts {
		results = append(results, quota.Result{
			AccountID: "acct-" + suffix, Email: suffix + "@example.com", Status: quota.StatusOK, RateLimitTier: "pro",
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 20, ResetAtUnix: now.Add(5 * time.Hour).Unix()},
				quota.Window7Day:  {RemainingPct: 40, ResetAtUnix: now.Add(7 * 24 * time.Hour).Unix()},
			},
		})
	}
	return results
}

func completeResetApp(now time.Time) (*CodexResetApp, *recordingResetBackend, *recordingResetUsage, *recordingResetAttempts) {
	expiry := now.Add(24 * time.Hour)
	backend := &recordingResetBackend{
		snapshot: resetAppSnapshot(), credits: map[codexprov.AccountKey]codexprov.ResetCreditInventory{},
		creditErrors: map[codexprov.AccountKey]error{}, creditPanics: map[codexprov.AccountKey]bool{},
	}
	for _, account := range backend.snapshot.Accounts {
		backend.credits[account.AccountKey] = codexprov.ResetCreditInventory{Credits: []codexprov.ResetCredit{resetAppCredit("credit-"+string(account.AccountKey), expiry)}, AvailableCount: 1}
	}
	usage := &recordingResetUsage{results: resetAppUsage(now, "a", "b", "c")}
	attempts := &recordingResetAttempts{pending: map[codexprov.AccountKey][]codexprov.ResetAttempt{}}
	return &CodexResetApp{
		Backend: backend, Usage: usage, History: &recordingResetHistory{}, Attempts: attempts,
		Cache: &recordingResetCache{}, Clock: resetTestClock{now: now},
	}, backend, usage, attempts
}

func TestCodexResetListKeepsOtherAccountsAfterFailureAndPanic(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, _, _ := completeResetApp(now)
	backend.creditErrors["account-b"] = errors.New("offline")
	backend.creditPanics["account-c"] = true
	got, err := app.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 3 || got.Accounts[0].Email != "a@example.com" || got.Accounts[1].Error.Code != "credits_unavailable" || got.Accounts[2].Error.Code != "credits_unavailable" {
		t.Fatalf("list = %+v", got)
	}
	data, _ := json.Marshal(got)
	for _, secretField := range []string{"account_key", "candidate", "revision"} {
		if strings.Contains(string(data), secretField) {
			t.Fatalf("internal field %q leaked in %s", secretField, data)
		}
	}
}

func TestCodexResetListResolvesOneAccount(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, _, _, _ := completeResetApp(now)
	got, err := app.List(context.Background(), "B@example.com")
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].AccountID != "acct-b" {
		t.Fatalf("list = %+v, %v", got, err)
	}
}

func TestCodexResetListKeepsValidCreditsWithEntryError(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, _, _ := completeResetApp(now)
	backend.credits["account-a"] = codexprov.ResetCreditInventory{
		Credits:     []codexprov.ResetCredit{resetAppCredit("valid-credit", now.Add(time.Hour))},
		EntryErrors: []codexprov.ResetCreditEntryError{{Index: 1, Code: "invalid_granted_at"}},
	}
	backend.creditErrors["account-a"] = &codexprov.ResetCreditInventoryError{Code: "invalid_credit_entries"}

	got, err := app.List(context.Background(), "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || len(got.Accounts[0].Credits) != 1 || got.Accounts[0].Credits[0].ID != "valid-credit" ||
		got.Accounts[0].Error == nil || got.Accounts[0].Error.Code != "credits_unavailable" {
		t.Fatalf("list = %+v", got)
	}
}

func TestCodexResetRecommendNeverConsumesAndUsesFreshUsage(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, usage, _ := completeResetApp(now)
	got, err := app.Recommend(context.Background())
	if err != nil || !got.Complete {
		t.Fatalf("Recommend() = %+v, %v", got, err)
	}
	if backend.consumeCalls != 0 || usage.calls != 1 || len(got.Items) != 3 {
		t.Fatalf("consume=%d usage=%d items=%+v", backend.consumeCalls, usage.calls, got.Items)
	}
}

func TestCodexResetRecommendIncompleteClearsActionableTiming(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, _, usage, _ := completeResetApp(now)
	usage.results = resetAppUsage(now, "a", "b")
	got, err := app.Recommend(context.Background())
	var resetErr *CodexResetError
	if !errors.As(err, &resetErr) || resetErr.Code != "recommendation_incomplete" || got.Complete {
		t.Fatalf("Recommend() = %+v, %v", got, err)
	}
	for _, item := range got.Items {
		if !item.UseAt.IsZero() || !item.UseBy.IsZero() {
			t.Fatalf("incomplete item remained actionable: %+v", item)
		}
	}
}

func TestCodexResetRecommendFallsBackWithoutHistory(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, _, _, _ := completeResetApp(now)
	app.History = nil
	got, err := app.Recommend(context.Background())
	if err != nil || !got.Complete || got.Confidence != "low" {
		t.Fatalf("Recommend() = %+v, %v", got, err)
	}
}

func TestCodexResetRecommendFallsBackAfterHistoryPanic(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, _, _, _ := completeResetApp(now)
	app.History.(*recordingResetHistory).panic = true
	got, err := app.Recommend(context.Background())
	if err != nil || !got.Complete || got.Confidence != "low" {
		t.Fatalf("Recommend() = %+v, %v", got, err)
	}
}

func TestCodexResetRecommendMapsAggregateValidationToIncomplete(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, _, usage, _ := completeResetApp(now)
	nan := math.NaN()
	window := usage.results[0].Windows[quota.Window5Hour]
	window.RemainingPctExact = &nan
	usage.results[0].Windows[quota.Window5Hour] = window
	got, err := app.Recommend(context.Background())
	var resetErr *CodexResetError
	if !errors.As(err, &resetErr) || resetErr.Code != "recommendation_incomplete" || got.Complete {
		t.Fatalf("Recommend() = %+v, %v", got, err)
	}
}

func TestCodexResetPrepareIsReadOnlyAndDefaultsNextExpiry(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, _, attempts := completeResetApp(now)
	earlier := now.Add(time.Hour)
	later := now.Add(2 * time.Hour)
	backend.credits["account-a"] = codexprov.ResetCreditInventory{Credits: []codexprov.ResetCredit{
		resetAppCredit("later", later), resetAppCredit("earlier", earlier),
	}, AvailableCount: 2}
	historyCalls := app.History.(*recordingResetHistory).calls
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "")
	if err != nil || plan.Credit.ID != "earlier" || backend.consumeCalls != 0 || attempts.ensureCalls != 0 || app.History.(*recordingResetHistory).calls != historyCalls {
		t.Fatalf("plan = %+v err=%v consume=%d ensure=%d", plan, err, backend.consumeCalls, attempts.ensureCalls)
	}
	if len(plan.CurrentWindows) != 2 {
		t.Fatalf("windows = %+v", plan.CurrentWindows)
	}
}

func TestCodexResetExecutePersistsBeforeConsumeAndReportsChange(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, usage, attempts := completeResetApp(now)
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "credit-account-a")
	if err != nil {
		t.Fatal(err)
	}
	sequence := []string{}
	attempts.ensureHook = func() { sequence = append(sequence, "persist") }
	backend.consumeHook = func() { sequence = append(sequence, "post") }
	backend.consume = codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeReset, WindowsReset: 2}
	usage.results[0].Windows[quota.Window5Hour] = quota.Window{RemainingPct: 100, ResetAtUnix: plan.CurrentWindows[quota.Window5Hour].ResetAtUnix}
	usage.results[0].Windows[quota.Window7Day] = quota.Window{RemainingPct: 100, ResetAtUnix: plan.CurrentWindows[quota.Window7Day].ResetAtUnix}
	result, err := app.ExecuteUse(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sequence, []string{"persist", "post"}) || result.Outcome != codexprov.ConsumeReset || len(result.ChangedWindows) != 2 {
		t.Fatalf("sequence=%v result=%+v", sequence, result)
	}
	if attempts.removeCalls != 1 || app.Cache.(*recordingResetCache).deleteCalls != 1 || app.History.(*recordingResetHistory).calls == 0 {
		t.Fatalf("cleanup=%d cache=%d history=%d", attempts.removeCalls, app.Cache.(*recordingResetCache).deleteCalls, app.History.(*recordingResetHistory).calls)
	}
}

func TestCodexResetExecuteRetainsAttemptOnIndeterminateFailure(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, _, attempts := completeResetApp(now)
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "credit-account-a")
	if err != nil {
		t.Fatal(err)
	}
	backend.consumeErr = context.DeadlineExceeded
	_, err = app.ExecuteUse(context.Background(), plan)
	var resetErr *CodexResetError
	if !errors.As(err, &resetErr) || resetErr.Code != "consume_indeterminate" || attempts.removeCalls != 0 {
		t.Fatalf("error=%v removeCalls=%d", err, attempts.removeCalls)
	}
}

func TestCodexResetExecuteRecoversConsumePanicAsIndeterminate(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, _, attempts := completeResetApp(now)
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "credit-account-a")
	if err != nil {
		t.Fatal(err)
	}
	backend.consumePanic = true
	_, err = app.ExecuteUse(context.Background(), plan)
	var resetErr *CodexResetError
	if !errors.As(err, &resetErr) || resetErr.Code != "consume_indeterminate" || attempts.removeCalls != 0 {
		t.Fatalf("error=%v removeCalls=%d", err, attempts.removeCalls)
	}
}

func TestCodexResetExecuteRetainsAttemptOnDefinitiveHTTPFailure(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, _, attempts := completeResetApp(now)
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "credit-account-a")
	if err != nil {
		t.Fatal(err)
	}
	backend.consumeErr = &codexprov.ResetHTTPError{Status: 400}
	_, err = app.ExecuteUse(context.Background(), plan)
	var resetErr *CodexResetError
	if !errors.As(err, &resetErr) || resetErr.Code != "consume_failed" || attempts.removeCalls != 0 || !strings.Contains(err.Error(), "--credit credit-account-a") {
		t.Fatalf("error=%v removeCalls=%d", err, attempts.removeCalls)
	}
}

func TestCodexResetExecuteWarnsWhenTerminalCleanupFails(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, _, attempts := completeResetApp(now)
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "credit-account-a")
	if err != nil {
		t.Fatal(err)
	}
	attempts.removeErr = errors.New("disk offline")
	backend.consume = codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeNoCredit}
	result, err := app.ExecuteUse(context.Background(), plan)
	if err != nil || len(result.Warnings) != 1 || result.Warnings[0].Code != "attempt_cleanup_failed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCodexResetExecuteWarnsWhenSuccessRefetchPanics(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, usage, _ := completeResetApp(now)
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "credit-account-a")
	if err != nil {
		t.Fatal(err)
	}
	backend.consume = codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeReset, WindowsReset: 2}
	usage.panic = true
	result, err := app.ExecuteUse(context.Background(), plan)
	if err != nil || len(result.Warnings) == 0 || result.Warnings[0].Code != "usage_refetch_failed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCodexResetExecuteNoOpRemovesAttemptWithoutRefetch(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	app, backend, usage, attempts := completeResetApp(now)
	plan, err := app.PrepareUse(context.Background(), "a@example.com", "credit-account-a")
	if err != nil {
		t.Fatal(err)
	}
	beforeFetches := usage.calls
	backend.consume = codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeNothingToReset}
	result, err := app.ExecuteUse(context.Background(), plan)
	if err != nil || result.Outcome != codexprov.ConsumeNothingToReset || attempts.removeCalls != 1 || usage.calls != beforeFetches {
		t.Fatalf("result=%+v err=%v remove=%d fetches=%d/%d", result, err, attempts.removeCalls, usage.calls, beforeFetches)
	}
}
