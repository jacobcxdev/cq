package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

type codexResetCLIClock struct{ now time.Time }

func (c codexResetCLIClock) Now() time.Time { return c.now }

type codexResetCLIBackend struct {
	snapshot     codexprov.ResetAccountSnapshot
	credits      map[codexprov.AccountKey]codexprov.ResetCreditInventory
	creditErrors map[codexprov.AccountKey]error
	consume      codexprov.ConsumeResetResult
	consumeCalls int
	consumeIDs   []string
	consumeFunc  func(string) (codexprov.ConsumeResetResult, error)
	listFunc     func(codexprov.ResetAccount) (codexprov.ResetCreditInventory, error)
}

func (b *codexResetCLIBackend) Snapshot(context.Context) (codexprov.ResetAccountSnapshot, error) {
	return b.snapshot, nil
}

func (b *codexResetCLIBackend) ListCredits(_ context.Context, account codexprov.ResetAccount) (codexprov.ResetCreditInventory, error) {
	if b.listFunc != nil {
		return b.listFunc(account)
	}
	return b.credits[account.AccountKey], b.creditErrors[account.AccountKey]
}

func (b *codexResetCLIBackend) Consume(_ context.Context, _ codexprov.ResetAccount, _ string, requestID string) (codexprov.ConsumeResetResult, error) {
	b.consumeCalls++
	b.consumeIDs = append(b.consumeIDs, requestID)
	if b.consumeFunc != nil {
		return b.consumeFunc(requestID)
	}
	return b.consume, nil
}

type codexResetCLIUsage struct {
	results   []quota.Result
	err       error
	calls     int
	fetchFunc func() ([]quota.Result, error)
}

func (u *codexResetCLIUsage) Fetch(context.Context, time.Time) ([]quota.Result, error) {
	u.calls++
	if u.fetchFunc != nil {
		return u.fetchFunc()
	}
	return append([]quota.Result(nil), u.results...), u.err
}

type codexResetCLIAttempts struct {
	pending     map[codexprov.AccountKey][]codexprov.ResetAttempt
	ensureCalls int
	removeCalls int
}

func (a *codexResetCLIAttempts) Pending(account codexprov.AccountKey) ([]codexprov.ResetAttempt, error) {
	return append([]codexprov.ResetAttempt(nil), a.pending[account]...), nil
}

func (a *codexResetCLIAttempts) Ensure(account codexprov.AccountKey, creditID string, now time.Time) (codexprov.ResetAttempt, error) {
	a.ensureCalls++
	attempt := codexprov.ResetAttempt{
		Version: 1, AccountKey: account, CreditID: creditID,
		IdempotencyKey: codexprov.ResetIdempotencyKey(account, creditID), StartedAt: now,
	}
	a.pending[account] = []codexprov.ResetAttempt{attempt}
	return attempt, nil
}

func (a *codexResetCLIAttempts) Remove(account codexprov.AccountKey, _ string) error {
	a.removeCalls++
	delete(a.pending, account)
	return nil
}

type codexResetCLIHistory struct{}

func (codexResetCLIHistory) UpdateAndGetEstimates(context.Context, map[string][]quota.Result, int64) (history.BurnRates, history.RateEstimates, error) {
	return nil, nil, nil
}

type staticCodexResetCLIHistory struct{ estimates history.RateEstimates }

func (h staticCodexResetCLIHistory) UpdateAndGetEstimates(context.Context, map[string][]quota.Result, int64) (history.BurnRates, history.RateEstimates, error) {
	return nil, h.estimates, nil
}

type trackingCodexResetCLIHistory struct {
	store     *history.Store
	estimates history.RateEstimates
}

func (h *trackingCodexResetCLIHistory) UpdateAndGetEstimates(ctx context.Context, results map[string][]quota.Result, now int64) (history.BurnRates, history.RateEstimates, error) {
	rates, estimates, err := h.store.UpdateAndGetEstimates(ctx, results, now)
	h.estimates = estimates
	return rates, estimates, err
}

type codexResetCLICache struct{ deleteCalls int }

func (*codexResetCLICache) Get(context.Context, string) ([]quota.Result, bool, error) {
	return nil, false, nil
}
func (*codexResetCLICache) Put(context.Context, string, []quota.Result) error { return nil }
func (c *codexResetCLICache) Delete(context.Context, string) error {
	c.deleteCalls++
	return nil
}
func (*codexResetCLICache) Age(context.Context, string) (time.Duration, bool) { return 0, false }

type codexResetCLIFixture struct {
	deps     codexResetsDependencies
	backend  *codexResetCLIBackend
	attempts *codexResetCLIAttempts
	usage    *codexResetCLIUsage
	cache    *codexResetCLICache
	out      *bytes.Buffer
	errOut   *bytes.Buffer
}

func newCodexResetCLIFixture(input io.Reader) codexResetCLIFixture {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	expires := now.Add(12 * time.Hour)
	credit := codexprov.ResetCredit{
		ID: "credit-1", ResetType: codexprov.ResetTypeCodexRateLimits,
		Status: codexprov.ResetCreditAvailable, GrantedAt: now.Add(-time.Hour), ExpiresAt: &expires,
		Title: "Banked reset", Description: "Restores shared Codex windows",
	}
	account := codexprov.ResetAccount{
		AccountKey: "internal-account-key", AccountID: "acct-1", Email: "user@example.com", PlanType: "pro", Active: true,
	}
	backend := &codexResetCLIBackend{
		snapshot: codexprov.ResetAccountSnapshot{
			Accounts: []codexprov.ResetAccount{account},
			Inventory: codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{
				Key: account.AccountKey, Identity: codexprov.AccountIdentity{AccountID: account.AccountID, Email: account.Email},
			}}},
		},
		credits: map[codexprov.AccountKey]codexprov.ResetCreditInventory{
			account.AccountKey: {Credits: []codexprov.ResetCredit{credit}, AvailableCount: 1},
		},
		creditErrors: map[codexprov.AccountKey]error{},
		consume:      codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeReset, WindowsReset: 2},
	}
	usage := &codexResetCLIUsage{results: []quota.Result{{
		AccountID: account.AccountID, Email: account.Email, Status: quota.StatusOK, RateLimitTier: "pro",
		Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 20, ResetAtUnix: now.Add(5 * time.Hour).Unix()},
			quota.Window7Day:  {RemainingPct: 40, ResetAtUnix: now.Add(7 * 24 * time.Hour).Unix()},
		},
	}}}
	attempts := &codexResetCLIAttempts{pending: map[codexprov.AccountKey][]codexprov.ResetAttempt{}}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	resetCache := &codexResetCLICache{}
	service := &app.CodexResetApp{
		Backend: backend, Usage: usage, History: codexResetCLIHistory{}, Attempts: attempts,
		Cache: resetCache, Clock: codexResetCLIClock{now: now},
	}
	return codexResetCLIFixture{
		deps:    codexResetsDependencies{App: service, In: input, Out: out, ErrOut: errOut},
		backend: backend, attempts: attempts, usage: usage, cache: resetCache, out: out, errOut: errOut,
	}
}

func parseCodexResetCLI(t *testing.T, args ...string) (*CLI, *kong.Context, error) {
	t.Helper()
	cli := &CLI{}
	parser, err := kong.New(cli, kong.Writers(io.Discard, io.Discard), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	parsed, err := parser.Parse(args)
	return cli, parsed, err
}

func TestCLIParsesCodexResets(t *testing.T) {
	tests := []struct {
		args       []string
		command    string
		reference  string
		credit     *string
		yes        bool
		jsonOutput bool
	}{
		{args: []string{"codex", "resets", "list"}, command: "codex resets list"},
		{args: []string{"codex", "resets", "list", "user@example.com"}, command: "codex resets list <account-reference>", reference: "user@example.com"},
		{args: []string{"codex", "resets", "recommend"}, command: "codex resets recommend"},
		{args: []string{"codex", "resets", "use", "user@example.com"}, command: "codex resets use <account-reference>", reference: "user@example.com"},
		{args: []string{"codex", "resets", "use", "user@example.com", "--credit", "credit-1"}, command: "codex resets use <account-reference>", reference: "user@example.com", credit: stringPointer("credit-1")},
		{args: []string{"codex", "resets", "use", "user@example.com", "--credit", "credit-1", "--yes"}, command: "codex resets use <account-reference>", reference: "user@example.com", credit: stringPointer("credit-1"), yes: true},
		{args: []string{"--json", "codex", "resets", "recommend"}, command: "codex resets recommend", jsonOutput: true},
	}

	for _, test := range tests {
		cli, parsed, err := parseCodexResetCLI(t, test.args...)
		if err != nil {
			t.Fatalf("Parse(%v): %v", test.args, err)
		}
		if got := parsed.Command(); got != test.command {
			t.Fatalf("Parse(%v) command = %q, want %q", test.args, got, test.command)
		}
		if test.command == "codex resets list <account-reference>" && cli.Codex.Resets.List.Reference != test.reference {
			t.Fatalf("list reference = %q", cli.Codex.Resets.List.Reference)
		}
		if strings.HasPrefix(test.command, "codex resets use") {
			if cli.Codex.Resets.Use.Reference != test.reference || !equalStringPointers(cli.Codex.Resets.Use.Credit, test.credit) || cli.Codex.Resets.Use.Yes != test.yes {
				t.Fatalf("use options = %+v", cli.Codex.Resets.Use)
			}
		}
		if cli.JSON != test.jsonOutput {
			t.Fatalf("JSON = %t, want %t", cli.JSON, test.jsonOutput)
		}
	}
}

func TestCLIRejectsInvalidCodexResetsInvocations(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "resets", "recommend", "user@example.com"},
		{"codex", "resets", "use"},
		{"codex", "resets", "use", "user@example.com", "extra"},
		{"codex", "resets", "use", "user@example.com", "--unknown"},
	} {
		if _, _, err := parseCodexResetCLI(t, args...); err == nil {
			t.Fatalf("Parse(%v) error = nil", args)
		}
	}
}

func TestRunCodexResetsListRendersTTYAndJSON(t *testing.T) {
	fixture := newCodexResetCLIFixture(strings.NewReader(""))
	if err := runCodexResetsList(context.Background(), CodexResetsListCmd{}, false, fixture.deps); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"user@example.com", "credit-1", "codex_rate_limits", "available", "Banked reset", "Restores shared Codex windows"} {
		if !strings.Contains(fixture.out.String(), text) {
			t.Fatalf("TTY output missing %q:\n%s", text, fixture.out.String())
		}
	}

	fixture = newCodexResetCLIFixture(strings.NewReader(""))
	if err := runCodexResetsList(context.Background(), CodexResetsListCmd{}, true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	assertPublicCodexResetJSON(t, fixture.out.Bytes())
	var decoded struct {
		Accounts []app.CodexResetAccountCredits `json:"accounts"`
	}
	if err := json.Unmarshal(fixture.out.Bytes(), &decoded); err != nil || len(decoded.Accounts) != 1 {
		t.Fatalf("JSON = %s, %v", fixture.out.String(), err)
	}
}

func TestRunCodexResetsRecommendIsPortfolioAdvisory(t *testing.T) {
	fixture := newCodexResetCLIFixture(strings.NewReader("panic if read"))
	if err := runCodexResetsRecommend(context.Background(), true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	if fixture.backend.consumeCalls != 0 || fixture.attempts.ensureCalls != 0 {
		t.Fatalf("recommend mutated state: consume=%d ensure=%d", fixture.backend.consumeCalls, fixture.attempts.ensureCalls)
	}
	assertPublicCodexResetJSON(t, fixture.out.Bytes())
	if !bytes.Contains(fixture.out.Bytes(), []byte(`"complete":true`)) || !bytes.Contains(fixture.out.Bytes(), []byte(`"exact"`)) {
		t.Fatalf("recommend JSON = %s", fixture.out.String())
	}
}

func TestRunCodexResetsRecommendRendersIncompleteBodyThenErrors(t *testing.T) {
	fixture := newCodexResetCLIFixture(strings.NewReader(""))
	fixture.backend.creditErrors["internal-account-key"] = errors.New("offline")
	err := runCodexResetsRecommend(context.Background(), true, fixture.deps)
	if err == nil || !strings.Contains(err.Error(), "recommendation_incomplete") {
		t.Fatalf("error = %v", err)
	}
	if !bytes.Contains(fixture.out.Bytes(), []byte(`"complete":false`)) || !bytes.Contains(fixture.out.Bytes(), []byte(`"credits_unavailable"`)) {
		t.Fatalf("incomplete JSON = %s", fixture.out.String())
	}
	if fixture.backend.consumeCalls != 0 || fixture.attempts.ensureCalls != 0 {
		t.Fatal("incomplete recommendation mutated state")
	}
}

func TestRunCodexResetsUseDefaultsConfirmationToNo(t *testing.T) {
	fixture := newCodexResetCLIFixture(strings.NewReader("\n"))
	err := runCodexResetsUse(context.Background(), CodexResetsUseCmd{Reference: "user@example.com"}, false, fixture.deps)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.backend.consumeCalls != 0 || fixture.attempts.ensureCalls != 0 {
		t.Fatalf("decline mutated state: consume=%d ensure=%d", fixture.backend.consumeCalls, fixture.attempts.ensureCalls)
	}
	for _, text := range []string{"user@example.com", "credit-1", "20%", "40%", "Use this banked reset? [y/N]"} {
		combined := fixture.out.String() + fixture.errOut.String()
		if !strings.Contains(combined, text) {
			t.Fatalf("preflight missing %q:\n%s", text, combined)
		}
	}
	if !strings.Contains(fixture.out.String(), "cancelled") {
		t.Fatalf("output = %q", fixture.out.String())
	}
}

func TestRunCodexResetsUseAcceptsYesAndYesFlagSkipsInput(t *testing.T) {
	for _, test := range []struct {
		name string
		cmd  CodexResetsUseCmd
		in   io.Reader
	}{
		{name: "prompt", cmd: CodexResetsUseCmd{Reference: "user@example.com"}, in: strings.NewReader("YeS\n")},
		{name: "flag", cmd: CodexResetsUseCmd{Reference: "user@example.com", Yes: true}, in: panicReader{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCodexResetCLIFixture(test.in)
			if err := runCodexResetsUse(context.Background(), test.cmd, true, fixture.deps); err != nil {
				t.Fatal(err)
			}
			if fixture.backend.consumeCalls != 1 || fixture.attempts.ensureCalls != 1 {
				t.Fatalf("consume=%d ensure=%d", fixture.backend.consumeCalls, fixture.attempts.ensureCalls)
			}
			assertPublicCodexResetJSON(t, fixture.out.Bytes())
			if !bytes.Contains(fixture.out.Bytes(), []byte(`"outcome":"reset"`)) {
				t.Fatalf("result JSON = %s", fixture.out.String())
			}
		})
	}
}

func TestRunCodexResetsUseRejectsEmptyCredit(t *testing.T) {
	fixture := newCodexResetCLIFixture(strings.NewReader("yes\n"))
	empty := ""
	err := runCodexResetsUse(context.Background(), CodexResetsUseCmd{Reference: "user@example.com", Credit: &empty}, false, fixture.deps)
	if err == nil || !strings.Contains(err.Error(), "credit ID is empty") {
		t.Fatalf("error = %v", err)
	}
	if fixture.backend.consumeCalls != 0 {
		t.Fatal("empty credit consumed")
	}
}

func TestDispatchCodexResetsPreservesGlobalJSON(t *testing.T) {
	original := codexResetsDependenciesFactory
	t.Cleanup(func() { codexResetsDependenciesFactory = original })

	for _, args := range [][]string{
		{"--json", "codex", "resets", "list"},
		{"--json", "codex", "resets", "recommend"},
		{"--json", "codex", "resets", "use", "user@example.com", "--yes"},
	} {
		fixture := newCodexResetCLIFixture(panicReader{})
		codexResetsDependenciesFactory = func(context.Context) (codexResetsDependencies, func(), error) {
			return fixture.deps, func() {}, nil
		}
		cli, parsed, err := parseCodexResetCLI(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		if err := dispatch(parsed, cli); err != nil {
			t.Fatalf("dispatch(%v): %v", args, err)
		}
		assertPublicCodexResetJSON(t, fixture.out.Bytes())
	}
}

func TestCodexResetsCommandsSkipAutomaticAgent(t *testing.T) {
	for _, command := range []string{
		"codex resets list",
		"codex resets list <account-reference>",
		"codex resets recommend",
		"codex resets use <account-reference>",
	} {
		if shouldEnsureAgentAfter(command) {
			t.Fatalf("shouldEnsureAgentAfter(%q) = true", command)
		}
	}
	if !shouldEnsureAgentAfter("codex accounts") {
		t.Fatal("ordinary account command unexpectedly skipped agent")
	}
}

func TestCodexResetsRecommendIsAdvisoryAcrossLayers(t *testing.T) {
	fixture := newCodexResetCLIFixture(panicReader{})
	root := newCodexResetCLITempDir(t)
	attempts, err := codexprov.NewResetAttemptStore(fsutil.OSFileSystem{}, root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.deps.App.Attempts = attempts

	if err := runCodexResetsRecommend(context.Background(), true, fixture.deps); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(attempts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("recommendation created attempts: %v", entries)
	}
	if fixture.backend.consumeCalls != 0 || fixture.cache.deleteCalls != 0 || fixture.errOut.Len() != 0 {
		t.Fatalf("recommendation side effects: consume=%d cache-delete=%d prompt=%q",
			fixture.backend.consumeCalls, fixture.cache.deleteCalls, fixture.errOut.String())
	}
	assertPublicCodexResetJSON(t, fixture.out.Bytes())
}

func TestCodexResetsUseReplaysLostResponseWithSameRequestID(t *testing.T) {
	fixture := newCodexResetCLIFixture(panicReader{})
	root := newCodexResetCLITempDir(t)
	fs := fsutil.OSFileSystem{}
	attempts, err := codexprov.NewResetAttemptStore(fs, filepath.Join(root, "attempts"))
	if err != nil {
		t.Fatal(err)
	}
	historyStore, err := history.New(fs, filepath.Join(root, "history"))
	if err != nil {
		t.Fatal(err)
	}
	now := fixture.deps.App.Clock.Now().UTC()
	beforeFirst := codexResetCLIUsageResult("acct-1", "user@example.com", 70, 80, now)
	beforeSecond := codexResetCLIUsageResult("acct-1", "user@example.com", 60, 70, now)
	_, firstEstimates, err := historyStore.UpdateAndGetEstimates(context.Background(), map[string][]quota.Result{
		string(provider.Codex): {beforeFirst},
	}, now.Add(-2*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(firstEstimates) != 0 {
		t.Fatalf("first estimate = %+v", firstEstimates)
	}
	_, expectedEstimates, err := historyStore.UpdateAndGetEstimates(context.Background(), map[string][]quota.Result{
		string(provider.Codex): {beforeSecond},
	}, now.Add(-time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	trackingHistory := &trackingCodexResetCLIHistory{store: historyStore}
	fixture.deps.App.Attempts = attempts
	fixture.deps.App.History = trackingHistory

	redeemed := false
	originalInventory := fixture.backend.credits["internal-account-key"]
	fixture.backend.listFunc = func(codexprov.ResetAccount) (codexprov.ResetCreditInventory, error) {
		if redeemed {
			return codexprov.ResetCreditInventory{Credits: []codexprov.ResetCredit{}}, nil
		}
		return originalInventory, nil
	}
	fixture.backend.consumeFunc = func(string) (codexprov.ConsumeResetResult, error) {
		if !redeemed {
			redeemed = true
			return codexprov.ConsumeResetResult{}, errors.New("response lost after upstream redemption")
		}
		return codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeAlreadyRedeemed, WindowsReset: 2}, nil
	}
	fixture.usage.fetchFunc = func() ([]quota.Result, error) {
		if redeemed {
			return []quota.Result{codexResetCLIUsageResult("acct-1", "user@example.com", 100, 100, now)}, nil
		}
		return []quota.Result{codexResetCLIUsageResult("acct-1", "user@example.com", 60, 70, now)}, nil
	}

	firstErr := runCodexResetsUse(context.Background(), CodexResetsUseCmd{
		Reference: "user@example.com", Yes: true,
	}, true, fixture.deps)
	if firstErr == nil || !strings.Contains(firstErr.Error(), "consume_indeterminate") {
		t.Fatalf("first use error = %v", firstErr)
	}
	pending, err := attempts.Pending("internal-account-key")
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after lost response = %+v, %v", pending, err)
	}

	secondOut := &bytes.Buffer{}
	secondErrOut := &bytes.Buffer{}
	secondDeps := fixture.deps
	secondDeps.Out = secondOut
	secondDeps.ErrOut = secondErrOut
	if err := runCodexResetsUse(context.Background(), CodexResetsUseCmd{
		Reference: "user@example.com", Yes: true,
	}, true, secondDeps); err != nil {
		t.Fatal(err)
	}
	if len(fixture.backend.consumeIDs) != 2 || fixture.backend.consumeIDs[0] != fixture.backend.consumeIDs[1] {
		t.Fatalf("redeem request IDs = %v", fixture.backend.consumeIDs)
	}
	if fixture.backend.consumeIDs[0] != codexprov.ResetIdempotencyKey("internal-account-key", "credit-1") {
		t.Fatalf("redeem request ID = %q", fixture.backend.consumeIDs[0])
	}
	pending, err = attempts.Pending("internal-account-key")
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after replay = %+v, %v", pending, err)
	}
	if fixture.cache.deleteCalls != 1 {
		t.Fatalf("cache deletes = %d, want 1", fixture.cache.deleteCalls)
	}
	key := history.BurnRateKey{ProviderID: string(provider.Codex), AccountKey: "acct-1", Window: string(quota.Window5Hour)}
	wantEstimate, ok := expectedEstimates.Get(key)
	if !ok {
		t.Fatalf("missing seeded estimate: %+v", expectedEstimates)
	}
	gotEstimate, ok := trackingHistory.estimates.Get(key)
	if !ok || gotEstimate.Samples != wantEstimate.Samples || gotEstimate.RatePctPerS != wantEstimate.RatePctPerS {
		t.Fatalf("estimate across replay = %+v, want %+v", gotEstimate, wantEstimate)
	}
	assertPublicCodexResetJSON(t, secondOut.Bytes())
	if !bytes.Contains(secondOut.Bytes(), []byte(`"outcome":"already_redeemed"`)) {
		t.Fatalf("second output = %s", secondOut.String())
	}
}

func TestCodexResetsRecommendSeparatesThreeSameDayExpiries(t *testing.T) {
	first := newCodexResetCLIFixture(panicReader{})
	configureThreeAccountCodexResetFixture(&first)
	beforeEpochs := codexResetCLIUsageEpochs(first.usage.results)
	if err := runCodexResetsRecommend(context.Background(), true, first.deps); err != nil {
		t.Fatal(err)
	}

	second := newCodexResetCLIFixture(panicReader{})
	configureThreeAccountCodexResetFixture(&second)
	if err := runCodexResetsRecommend(context.Background(), true, second.deps); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.out.Bytes(), second.out.Bytes()) {
		t.Fatalf("recommendations differ:\n%s\n%s", first.out.String(), second.out.String())
	}
	var schedule codexResetScheduleJSON
	if err := json.Unmarshal(first.out.Bytes(), &schedule); err != nil {
		t.Fatal(err)
	}
	deadlines := map[time.Time]bool{}
	for _, item := range schedule.Items {
		if item.UseBy != nil {
			deadlines[*item.UseBy] = true
		}
	}
	if len(schedule.Items) != 3 || len(deadlines) != 3 {
		t.Fatalf("schedule items/deadlines = %+v / %+v", schedule.Items, deadlines)
	}
	if first.backend.consumeCalls != 0 || first.attempts.ensureCalls != 0 || first.cache.deleteCalls != 0 {
		t.Fatal("recommendation mutated state")
	}
	if got := codexResetCLIUsageEpochs(first.usage.results); !equalCodexResetEpochs(got, beforeEpochs) {
		t.Fatalf("natural reset epochs changed: got=%v want=%v", got, beforeEpochs)
	}
}

func TestCodexResetsRecommendIncompletePortfolioIsNonActionable(t *testing.T) {
	fixture := newCodexResetCLIFixture(panicReader{})
	configureTwoAccountCodexResetFixture(&fixture)
	fixture.backend.creditErrors["internal-key-b"] = errors.New("offline")

	err := runCodexResetsRecommend(context.Background(), true, fixture.deps)
	if err == nil || !strings.Contains(err.Error(), "recommendation_incomplete") {
		t.Fatalf("error = %v", err)
	}
	if !bytes.Contains(fixture.out.Bytes(), []byte(`"complete":false`)) ||
		!bytes.Contains(fixture.out.Bytes(), []byte(`"credits_unavailable"`)) ||
		!bytes.Contains(fixture.out.Bytes(), []byte("user@example.com")) ||
		!bytes.Contains(fixture.out.Bytes(), []byte("b@example.com")) {
		t.Fatalf("incomplete output = %s", fixture.out.String())
	}
	if bytes.Contains(fixture.out.Bytes(), []byte(`"use_at"`)) || bytes.Contains(fixture.out.Bytes(), []byte(`"use_by"`)) {
		t.Fatalf("incomplete schedule remained actionable: %s", fixture.out.String())
	}
	if fixture.backend.consumeCalls != 0 || fixture.attempts.ensureCalls != 0 || fixture.cache.deleteCalls != 0 {
		t.Fatal("incomplete recommendation mutated state")
	}
}

func configureTwoAccountCodexResetFixture(fixture *codexResetCLIFixture) {
	now := fixture.deps.App.Clock.Now().UTC()
	expires := now.Add(20 * time.Hour)
	account := codexprov.ResetAccount{AccountKey: "internal-key-b", AccountID: "acct-b", Email: "b@example.com", PlanType: "pro"}
	fixture.backend.snapshot.Accounts = append(fixture.backend.snapshot.Accounts, account)
	fixture.backend.snapshot.Inventory.Accounts = append(fixture.backend.snapshot.Inventory.Accounts, codexprov.LogicalAccount{
		Key: account.AccountKey, Identity: codexprov.AccountIdentity{AccountID: account.AccountID, Email: account.Email},
	})
	fixture.backend.credits[account.AccountKey] = codexprov.ResetCreditInventory{Credits: []codexprov.ResetCredit{{
		ID: "credit-b", ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditAvailable,
		GrantedAt: now.Add(-time.Hour), ExpiresAt: &expires,
	}}, AvailableCount: 1}
	fixture.usage.results = append(fixture.usage.results, codexResetCLIUsageResult(account.AccountID, account.Email, 10, 10, now))
}

func configureThreeAccountCodexResetFixture(fixture *codexResetCLIFixture) {
	now := fixture.deps.App.Clock.Now().UTC()
	expires := now.Add(20 * time.Hour)
	estimates := history.RateEstimates{}
	fixture.backend.snapshot.Accounts = nil
	fixture.backend.snapshot.Inventory.Accounts = nil
	fixture.backend.credits = map[codexprov.AccountKey]codexprov.ResetCreditInventory{}
	fixture.backend.creditErrors = map[codexprov.AccountKey]error{}
	fixture.usage.results = nil
	for index, remaining := range []int{5, 10, 15} {
		suffix := string(rune('a' + index))
		account := codexprov.ResetAccount{
			AccountKey: codexprov.AccountKey("internal-key-" + suffix), AccountID: "acct-" + suffix,
			Email: suffix + "@example.com", PlanType: "pro", Active: index == 0,
		}
		fixture.backend.snapshot.Accounts = append(fixture.backend.snapshot.Accounts, account)
		fixture.backend.snapshot.Inventory.Accounts = append(fixture.backend.snapshot.Inventory.Accounts, codexprov.LogicalAccount{
			Key: account.AccountKey, Identity: codexprov.AccountIdentity{AccountID: account.AccountID, Email: account.Email},
		})
		fixture.backend.credits[account.AccountKey] = codexprov.ResetCreditInventory{Credits: []codexprov.ResetCredit{{
			ID: "credit-" + suffix, ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditAvailable,
			GrantedAt: now.Add(-time.Hour), ExpiresAt: &expires,
		}}, AvailableCount: 1}
		usage := codexResetCLIUsageResult(account.AccountID, account.Email, remaining, remaining, now)
		usage.RateLimitTier = fmt.Sprintf("codex_pro_%dx", index+1)
		fiveHour := usage.Windows[quota.Window5Hour]
		fiveHour.ResetAtUnix = now.Add(5*time.Hour + time.Duration(index)*15*time.Minute).Unix()
		usage.Windows[quota.Window5Hour] = fiveHour
		sevenDay := usage.Windows[quota.Window7Day]
		sevenDay.ResetAtUnix = now.Add(7*24*time.Hour + time.Duration(index)*time.Hour).Unix()
		usage.Windows[quota.Window7Day] = sevenDay
		fixture.usage.results = append(fixture.usage.results, usage)
		for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
			estimates[history.BurnRateKey{
				ProviderID: string(provider.Codex), AccountKey: account.AccountID, Window: string(name),
			}] = history.RateEstimate{RatePctPerS: 0.01 - float64(index)*0.002, Samples: 3, LastSeenUnix: now.Unix()}
		}
	}
	fixture.deps.App.History = staticCodexResetCLIHistory{estimates: estimates}
}

func codexResetCLIUsageResult(accountID, email string, remaining5Hour, remaining7Day int, now time.Time) quota.Result {
	return quota.Result{
		AccountID: accountID, Email: email, Status: quota.StatusOK, RateLimitTier: "codex_pro_1x",
		Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: remaining5Hour, ResetAtUnix: now.Add(5 * time.Hour).Unix()},
			quota.Window7Day:  {RemainingPct: remaining7Day, ResetAtUnix: now.Add(7 * 24 * time.Hour).Unix()},
		},
	}
}

func codexResetCLIUsageEpochs(results []quota.Result) map[string]map[quota.WindowName]int64 {
	epochs := make(map[string]map[quota.WindowName]int64, len(results))
	for _, result := range results {
		epochs[result.AccountID] = map[quota.WindowName]int64{
			quota.Window5Hour: result.Windows[quota.Window5Hour].ResetAtUnix,
			quota.Window7Day:  result.Windows[quota.Window7Day].ResetAtUnix,
		}
	}
	return epochs
}

func equalCodexResetEpochs(left, right map[string]map[quota.WindowName]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for account, leftWindows := range left {
		rightWindows, ok := right[account]
		if !ok || leftWindows[quota.Window5Hour] != rightWindows[quota.Window5Hour] || leftWindows[quota.Window7Day] != rightWindows[quota.Window7Day] {
			return false
		}
	}
	return true
}

func newCodexResetCLITempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func assertPublicCodexResetJSON(t *testing.T, body []byte) {
	t.Helper()
	if !json.Valid(body) {
		t.Fatalf("invalid JSON: %s", body)
	}
	for _, forbidden := range []string{"internal-account-key", "account_key", "candidate_id", "revision", "access_token", "refresh_token", "secret-fixture"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("JSON exposed %q: %s", forbidden, body)
		}
	}
}

func stringPointer(value string) *string { return &value }

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("unexpected input read") }
