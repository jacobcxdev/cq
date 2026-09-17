package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	registerV2Fixture("reset-no-consent", func(t *testing.T) *v2Fixture {
		a, b := v2ResetTestApp()
		b.snapshot.Accounts = b.snapshot.Accounts[:1]
		b.snapshot.Inventory.Accounts = b.snapshot.Inventory.Accounts[:1]
		b.snapshot.Accounts[0].Email = "alice@example.com"
		b.snapshot.Inventory.Accounts[0].Identity.Email = "alice@example.com"
		var err error
		a.Attempts, err = codexprov.NewResetAttemptStore(fsutil.OSFileSystem{}, useTestRoot(t))
		if err != nil {
			t.Fatal(err)
		}
		b.f.Lookup = func(path string) (cli.Handler, bool) {
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ResetUseWithApp(ctx, inv, s, a)
			}, path == "codex reset use"
		}
		return b.f
	})
}
func TestCLIV2ResetUseContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "JSON does not authorise consumption", Scenario: "reset-no-consent", Args: []string{"codex", "reset", "use", "alice@example.com", "--json"}, Exit: 6, Command: "codex reset use", Code: "codex_reset_confirmation_required", Forbid: []string{"consume", "credential-activate", "service", "inventory"}})
}

type useTestBackend struct {
	*v2ResetBackend
	consume func(context.Context, codexprov.ResetAccount, string, string) (codexprov.ConsumeResetResult, error)
}

func (b *useTestBackend) Consume(ctx context.Context, a codexprov.ResetAccount, credit, key string) (codexprov.ConsumeResetResult, error) {
	b.f.Call("consume")
	return b.consume(ctx, a, credit, key)
}

type useTestAttempts struct {
	app.CodexResetAttempts
	pendingErr, ensureErr, removeErr error
	beforeEnsure                     func()
}

func (a useTestAttempts) Pending(key codexprov.AccountKey) ([]codexprov.ResetAttempt, error) {
	if a.pendingErr != nil {
		return nil, a.pendingErr
	}
	return a.CodexResetAttempts.Pending(key)
}
func (a useTestAttempts) Ensure(key codexprov.AccountKey, credit string, now time.Time) (codexprov.ResetAttempt, error) {
	if a.beforeEnsure != nil {
		a.beforeEnsure()
	}
	if a.ensureErr != nil {
		return codexprov.ResetAttempt{}, a.ensureErr
	}
	return a.CodexResetAttempts.Ensure(key, credit, now)
}
func (a useTestAttempts) Remove(key codexprov.AccountKey, credit string) error {
	if a.removeErr != nil {
		return a.removeErr
	}
	return a.CodexResetAttempts.Remove(key, credit)
}
func newUseTest(t *testing.T) (*app.CodexResetApp, *useTestBackend, *cli.Session, cli.Invocation, *codexprov.ResetAttemptStore) {
	t.Helper()
	a, b := v2ResetTestApp()
	b.snapshot.Accounts = b.snapshot.Accounts[:1]
	b.snapshot.Inventory.Accounts = b.snapshot.Inventory.Accounts[:1]
	b.snapshot.Accounts[0].Email = "alice@example.com"
	b.snapshot.Inventory.Accounts[0].Identity.Email = "alice@example.com"
	attempts, err := codexprov.NewResetAttemptStore(fsutil.OSFileSystem{}, useTestRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	a.Attempts = attempts
	backend := &useTestBackend{v2ResetBackend: b}
	backend.consume = func(_ context.Context, account codexprov.ResetAccount, credit, key string) (codexprov.ConsumeResetResult, error) {
		pending, err := attempts.Pending(account.AccountKey)
		if err != nil || len(pending) != 1 || pending[0].CreditID != credit || pending[0].IdempotencyKey != key {
			t.Errorf("Consume without exact durable attempt: %+v %v", pending, err)
		}
		return codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeReset, WindowsReset: 2}, nil
	}
	a.Backend = backend
	inv, _ := cli.Parse([]string{"codex", "reset", "use", "alice@example.com", "--yes", "--json"})
	return a, backend, &cli.Session{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}, inv, attempts
}
func decodeUse(t *testing.T, out cli.Outcome) ResetUseResult {
	t.Helper()
	var data ResetUseResult
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatalf("invalid data %q: %v", out.Data, err)
	}
	return data
}
func useCalls(f *v2Fixture, name string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[name] }

func TestCLIV2ResetUseTerminalOutcomes(t *testing.T) {
	for _, outcome := range []codexprov.ConsumeResetOutcome{codexprov.ConsumeReset, codexprov.ConsumeAlreadyRedeemed, codexprov.ConsumeNothingToReset, codexprov.ConsumeNoCredit} {
		t.Run(string(outcome), func(t *testing.T) {
			a, b, s, inv, store := newUseTest(t)
			original := b.consume
			b.consume = func(ctx context.Context, account codexprov.ResetAccount, credit, key string) (codexprov.ConsumeResetResult, error) {
				_, err := original(ctx, account, credit, key)
				return codexprov.ConsumeResetResult{Outcome: outcome, WindowsReset: 2}, err
			}
			out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
			data := decodeUse(t, out)
			if out.ExitCode != 0 || data.Outcome != string(outcome) || data.AccountReference != "key-a" || data.CreditID == nil || *data.CreditID != "credit-a" || data.Retry != nil || data.ChangedWindows == nil || useCalls(b.f, "consume") != 1 {
				t.Fatalf("out=%+v data=%+v", out, data)
			}
			pending, err := store.Pending("key-a")
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending=%+v err=%v", pending, err)
			}
			var raw map[string]json.RawMessage
			json.Unmarshal(out.Data, &raw)
			if len(raw) != 6 || string(raw["retry"]) != "null" || string(raw["changed_windows"]) != "[]" {
				t.Fatalf("schema=%s", out.Data)
			}
		})
	}
}
func TestCLIV2ResetUseConsent(t *testing.T) {
	for _, answer := range []string{"", "yes", "\n", "n\n", "no\n", "YES\n", "y\n", "true\n"} {
		t.Run(fmt.Sprintf("%q", answer), func(t *testing.T) {
			a, b, s, inv, _ := newUseTest(t)
			inv.JSON = false
			inv.Options["yes"] = []string{"false"}
			s.Interactive = true
			s.In = strings.NewReader(answer)
			var preview bytes.Buffer
			s.Err = &preview
			out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
			data := decodeUse(t, out)
			yes := answer == "YES\n" || answer == "y\n"
			want := "cancelled"
			if yes {
				want = "reset"
			}
			if out.ExitCode != 0 || data.Outcome != want || data.Retry != nil || useCalls(b.f, "consume") != map[bool]int{false: 0, true: 1}[yes] {
				t.Fatalf("out=%+v", out)
			}
			if !strings.HasSuffix(preview.String(), "Use reset credit credit-a for alice@example.com? [y/N]") || !strings.Contains(preview.String(), "5h: 10% remaining") || !strings.Contains(preview.String(), "Recommendation:") {
				t.Fatalf("preview=%q", preview.String())
			}
		})
	}
	t.Run("nonterminal prevents construction", func(t *testing.T) {
		_, _, s, inv, _ := newUseTest(t)
		inv.JSON = false
		inv.Options["yes"] = []string{"false"}
		out := handleV2ResetUseWithFactory(context.Background(), inv, s, func(context.Context, bool) (v2ResetDependencies, error) {
			t.Fatal("constructed without consent")
			return v2ResetDependencies{}, nil
		})
		if out.ExitCode != 6 || out.Data != nil {
			t.Fatalf("out=%+v", out)
		}
	})
	t.Run("failed confirmation", func(t *testing.T) {
		a, b, s, inv, _ := newUseTest(t)
		inv.JSON = false
		inv.Options["yes"] = []string{"false"}
		s.Interactive = true
		s.In = useFailedInput{}
		out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
		if out.ExitCode != 1 || useCalls(b.f, "consume") != 0 || decodeUse(t, out).Outcome != "cancelled" {
			t.Fatalf("out=%+v", out)
		}
	})
}

type useFailedInput struct{}

func (useFailedInput) Read([]byte) (int, error) {
	return 0, errors.New("synthetic-secret confirmation failed")
}

type useConsentInput struct {
	before func()
	input  io.Reader
}

func (r *useConsentInput) Read(p []byte) (int, error) {
	if r.before != nil {
		f := r.before
		r.before = nil
		f()
	}
	return r.input.Read(p)
}
func TestCLIV2ResetUseRevalidation(t *testing.T) {
	for _, mode := range []string{"missing", "expired", "redeemed", "unsupported", "identity", "ensure", "pending"} {
		t.Run(mode, func(t *testing.T) {
			a, b, s, inv, store := newUseTest(t)
			inv.JSON = false
			inv.Options["yes"] = []string{"false"}
			s.Interactive = true
			s.In = &useConsentInput{input: strings.NewReader("yes\n"), before: func() {
				inventory := b.inventories["key-a"]
				switch mode {
				case "missing":
					inventory.Credits[0].ID = "replacement"
				case "expired":
					now := a.Clock.Now()
					inventory.Credits[0].ExpiresAt = &now
				case "redeemed":
					inventory.Credits[0].Status = codexprov.ResetCreditRedeemed
				case "unsupported":
					inventory.Credits[0].ResetType = "other"
				case "identity":
					b.snapshot.Accounts[0].AccountID = "different"
				case "ensure":
					a.Attempts = useTestAttempts{CodexResetAttempts: store, ensureErr: errors.New("secret disk error")}
				case "pending":
					a.Attempts = useTestAttempts{CodexResetAttempts: store, pendingErr: errors.New("secret disk error")}
				}
				b.inventories["key-a"] = inventory
			}}
			out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
			want := "codex_reset_credit_ineligible"
			if mode == "missing" {
				want = "codex_reset_credit_not_found"
			}
			if mode == "identity" {
				want = "account_unstable"
			}
			if mode == "ensure" || mode == "pending" {
				want = "codex_reset_attempt_unavailable"
			}
			if len(out.Errors) != 1 || out.Errors[0].Code != want || useCalls(b.f, "consume") != 0 || strings.Contains(string(out.Data), "replacement") {
				t.Fatalf("out=%+v", out)
			}
		})
	}
}
func TestCLIV2ResetUseIndeterminateRestartRetry(t *testing.T) {
	for _, cause := range []error{errors.New("synthetic-secret transport"), &codexprov.ResetHTTPError{Status: 503}, context.DeadlineExceeded} {
		t.Run(fmt.Sprintf("%T-%v", cause, cause), func(t *testing.T) {
			a, b, s, inv, store := newUseTest(t)
			var firstKey string
			b.consume = func(_ context.Context, account codexprov.ResetAccount, credit, key string) (codexprov.ConsumeResetResult, error) {
				firstKey = key
				return codexprov.ConsumeResetResult{}, cause
			}
			out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
			data := decodeUse(t, out)
			wantExit := 1
			if errors.Is(cause, context.DeadlineExceeded) {
				wantExit = 7
			}
			if out.ExitCode != wantExit || data.Outcome != "indeterminate" || data.Retry == nil || data.Retry.AccountReference != "key-a" || data.Retry.CreditID != "credit-a" || data.WindowsReset != 0 {
				t.Fatalf("out=%+v", out)
			}
			if strings.Contains(out.Human, firstKey) || strings.Contains(fmt.Sprint(out.Errors), "synthetic-secret") || !strings.Contains(out.Human, "cq codex reset use 'key-a' --credit 'credit-a' --yes") {
				t.Fatalf("unsafe result=%+v", out)
			}
			persisted, err := store.Pending("key-a")
			if err != nil || len(persisted) != 1 || persisted[0].IdempotencyKey != firstKey {
				t.Fatalf("pending=%+v err=%v", persisted, err)
			}
			// A separate store instance models process restart, with the credit now absent.
			restarted := &codexprov.ResetAttemptStore{FS: store.FS, Dir: store.Dir}
			a.Attempts = restarted
			b.inventories["key-a"] = codexprov.ResetCreditInventory{}
			b.consume = func(_ context.Context, account codexprov.ResetAccount, credit, key string) (codexprov.ConsumeResetResult, error) {
				if key != firstKey || account.AccountKey != "key-a" || credit != "credit-a" {
					t.Errorf("retry changed coordinates")
				}
				return codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeAlreadyRedeemed}, nil
			}
			out = handleV2ResetUseWithApp(context.Background(), inv, s, a)
			if out.ExitCode != 0 || decodeUse(t, out).Outcome != "already_redeemed" || useCalls(b.f, "consume") != 2 {
				t.Fatalf("replay=%+v", out)
			}
		})
	}
}
func TestCLIV2ResetUsePostcheckWarnings(t *testing.T) {
	for _, mode := range []string{"cleanup-reset", "cleanup-no_credit", "cleanup-nothing_to_reset", "usage", "match", "history", "cache"} {
		t.Run(mode, func(t *testing.T) {
			a, b, s, inv, store := newUseTest(t)
			b.consume = func(context.Context, codexprov.ResetAccount, string, string) (codexprov.ConsumeResetResult, error) {
				outcome := codexprov.ConsumeReset
				switch mode {
				case "cleanup-reset", "cleanup-no_credit", "cleanup-nothing_to_reset":
					outcome = codexprov.ConsumeResetOutcome(strings.TrimPrefix(mode, "cleanup-"))
					a.Attempts = useTestAttempts{CodexResetAttempts: store, removeErr: errors.New("secret remove")}
				case "usage":
					a.Usage = v2ResetUsage{f: b.f, err: errors.New("secret usage")}
				case "match":
					a.Usage = v2ResetUsage{f: b.f}
				case "history":
					a.History = useFailHistory{}
				case "cache":
					a.Cache = useFailCache{}
				}
				return codexprov.ConsumeResetResult{Outcome: outcome, WindowsReset: 2}, nil
			}
			out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
			data := decodeUse(t, out)
			if out.ExitCode != 8 || out.Errors[0].Code != "codex_reset_postcheck_partial" || data.Outcome == "indeterminate" || data.Retry != nil || len(out.Warnings) != 1 || strings.Contains(fmt.Sprint(out), "secret") {
				t.Fatalf("out=%+v", out)
			}
		})
	}
}

type useFailHistory struct{}

func (useFailHistory) UpdateAndGetEstimates(context.Context, map[string][]quota.Result, int64) (history.BurnRates, history.RateEstimates, error) {
	return nil, nil, errors.New("secret history")
}

type useFailCache struct{ app.Cache }

func (useFailCache) Delete(context.Context, string) error { return errors.New("secret cache") }

func useTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "cache")
}

type useUsageFunc func(context.Context, time.Time) ([]quota.Result, error)

func (f useUsageFunc) Fetch(ctx context.Context, now time.Time) ([]quota.Result, error) {
	return f(ctx, now)
}
func TestCLIV2ResetUseDeadlines(t *testing.T) {
	t.Run("before send", func(t *testing.T) {
		a, b, s, inv, store := newUseTest(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a.Attempts = useTestAttempts{CodexResetAttempts: store, beforeEnsure: cancel}
		out := handleV2ResetUseWithApp(ctx, inv, s, a)
		data := decodeUse(t, out)
		if out.ExitCode != 130 || data.Outcome != "cancelled" || data.Retry != nil || useCalls(b.f, "consume") != 0 {
			t.Fatalf("out=%+v", out)
		}
	})
	t.Run("uncooperative selected usage", func(t *testing.T) {
		a, b, s, inv, _ := newUseTest(t)
		entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		defer close(release)
		a.Usage = useUsageFunc(func(ctx context.Context, _ time.Time) ([]quota.Result, error) {
			close(entered)
			<-release
			defer close(finished)
			provider.ObserveWarning(ctx, "late", "must not escape")
			return nil, errors.New("late")
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan cli.Outcome, 1)
		go func() { done <- handleV2ResetUseWithApp(ctx, inv, s, a) }()
		<-entered
		cancel()
		select {
		case out := <-done:
			if out.ExitCode != 130 || len(out.Warnings) != 0 || useCalls(b.f, "consume") != 0 {
				t.Fatalf("out=%+v", out)
			}
		case <-time.After(time.Second):
			t.Fatal("usage exceeded deadline")
		}
	})
	for _, phase := range []string{"consume", "postcheck"} {
		t.Run(phase, func(t *testing.T) {
			a, b, s, inv, store := newUseTest(t)
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			b.consume = func(ctx context.Context, _ codexprov.ResetAccount, _, _ string) (codexprov.ConsumeResetResult, error) {
				if phase == "consume" {
					close(entered)
					<-release
					close(finished)
				} else {
					a.Usage = useUsageFunc(func(ctx context.Context, _ time.Time) ([]quota.Result, error) {
						close(entered)
						<-release
						defer close(finished)
						provider.ObserveWarning(ctx, "late", "must not escape")
						return nil, errors.New("late")
					})
				}
				return codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeReset, WindowsReset: 2}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan cli.Outcome, 1)
			go func() { done <- handleV2ResetUseWithApp(ctx, inv, s, a) }()
			<-entered
			var out cli.Outcome
			select {
			case out = <-done:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("work exceeded deadline")
			}
			data := decodeUse(t, out)
			if phase == "consume" {
				if out.ExitCode != 7 || data.Outcome != "indeterminate" || data.Retry == nil {
					t.Fatalf("out=%+v", out)
				}
			} else if out.ExitCode != 8 || data.Outcome != "reset" || data.Retry != nil || len(data.ChangedWindows) != 0 {
				t.Fatalf("out=%+v", out)
			}
			before := string(out.Data)
			close(release)
			<-finished
			if string(out.Data) != before || len(out.Warnings) > 1 || useCalls(b.f, "consume") != 1 {
				t.Fatal("late work altered outcome")
			}
			if phase == "consume" {
				pending, err := store.Pending("key-a")
				if err != nil || len(pending) != 1 {
					t.Fatalf("late success removed unresolved attempt: %+v %v", pending, err)
				}
			}
		})
	}
}
func TestCLIV2ResetUseConsentBudget(t *testing.T) {
	t.Run("quiescent wait excluded", func(t *testing.T) {
		a, b, s, inv, _ := newUseTest(t)
		inv.JSON = false
		inv.Options["yes"] = []string{"false"}
		inv.Options["timeout"] = []string{"100ms"}
		s.Interactive = true
		s.In = &useConsentInput{input: strings.NewReader("yes\n"), before: func() { time.Sleep(150 * time.Millisecond) }}
		out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
		if out.ExitCode != 0 || useCalls(b.f, "consume") != 1 {
			t.Fatalf("consent spent budget: %+v", out)
		}
	})
	t.Run("parent still expires", func(t *testing.T) {
		a, b, s, inv, _ := newUseTest(t)
		inv.JSON = false
		inv.Options["yes"] = []string{"false"}
		s.Interactive = true
		release := make(chan struct{})
		defer close(release)
		s.In = &useConsentInput{input: strings.NewReader("yes\n"), before: func() { <-release }}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		out := handleV2ResetUseWithApp(ctx, inv, s, a)
		if out.ExitCode != 7 || useCalls(b.f, "consume") != 0 {
			t.Fatalf("parent extended: %+v", out)
		}
	})
	t.Run("factory and work share allowance", func(t *testing.T) {
		a, b, s, inv, _ := newUseTest(t)
		inv.Options["timeout"] = []string{"100ms"}
		a.Usage = useUsageFunc(func(ctx context.Context, _ time.Time) ([]quota.Result, error) { <-ctx.Done(); return nil, ctx.Err() })
		start := time.Now()
		out := handleV2ResetUseWithFactory(context.Background(), inv, s, func(context.Context, bool) (v2ResetDependencies, error) {
			time.Sleep(60 * time.Millisecond)
			return v2ResetDependencies{app: a}, nil
		})
		if out.ExitCode != 7 || time.Since(start) > 145*time.Millisecond || useCalls(b.f, "consume") != 0 {
			t.Fatalf("budget restarted: %+v elapsed=%s", out, time.Since(start))
		}
	})
}
func TestCLIV2ResetUsePendingAndCancellation(t *testing.T) {
	a, b, s, inv, store := newUseTest(t)
	for _, credit := range []string{"credit-a", "second"} {
		if _, err := store.Ensure("key-a", credit, a.Clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
	if out.ExitCode != 6 || out.Errors[0].Code != "codex_reset_pending_ambiguous" || useCalls(b.f, "consume") != 0 {
		t.Fatalf("out=%+v", out)
	}
	inv.Options["credit"] = []string{"credit-a"}
	inv.Options["yes"] = []string{"false"}
	inv.JSON = false
	s.Interactive = true
	s.In = strings.NewReader("no\n")
	out = handleV2ResetUseWithApp(context.Background(), inv, s, a)
	pending, err := store.Pending("key-a")
	if out.ExitCode != 0 || decodeUse(t, out).Retry != nil || len(pending) != 2 || err != nil {
		t.Fatalf("cancelled replay=%+v pending=%+v err=%v", out, pending, err)
	}
}
func TestCLIV2ResetUseChangedWindowsAndOutputFailure(t *testing.T) {
	a, b, s, inv, _ := newUseTest(t)
	before := a.Usage.(v2ResetUsage).results
	b.consume = func(context.Context, codexprov.ResetAccount, string, string) (codexprov.ConsumeResetResult, error) {
		windows := map[quota.WindowName]quota.Window{}
		for name, w := range before[0].Windows {
			w.RemainingPct = 100
			windows[name] = w
		}
		a.Usage = v2ResetUsage{f: b.f, results: []quota.Result{{AccountID: "id-a", Status: quota.StatusOK, Windows: windows}}}
		return codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeReset, WindowsReset: 2}, nil
	}
	out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
	data := decodeUse(t, out)
	if out.ExitCode != 0 || len(data.ChangedWindows) != 2 || data.ChangedWindows[0].Name != quota.Window5Hour || data.ChangedWindows[1].Name != quota.Window7Day {
		t.Fatalf("out=%+v", out)
	}
	for _, change := range data.ChangedWindows {
		if change.Before.RemainingPct != 10 || change.After.RemainingPct != 100 || !change.Before.ResetAt.Equal(change.After.ResetAt) || change.After.PeriodSeconds <= 0 {
			t.Fatalf("change=%+v", change)
		}
	}
	s.Out = useFailedOutput{}
	exit := cli.Run(context.Background(), []string{"codex", "reset", "use", "key-a", "--yes", "--json"}, s, func(string) (cli.Handler, bool) {
		return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome { return out }, true
	})
	if exit != 1 || useCalls(b.f, "consume") != 1 {
		t.Fatalf("output failure replayed: exit=%d", exit)
	}
}

type useFailedOutput struct{}

func (useFailedOutput) Write([]byte) (int, error) { return 0, errors.New("synthetic output error") }
func TestCLIV2ResetUseParserAndHelp(t *testing.T) {
	for _, args := range [][]string{{"--credit", "a", "--credit", "b"}, {"--yes", "--yes"}, {"--unknown"}, {"--credit", " spaced "}} {
		code := "duplicate_option"
		if args[0] == "--unknown" {
			code = "unknown_option"
		}
		if len(args) == 2 && args[0] == "--credit" {
			code = "invalid_argument"
		}
		runV2Case(t, v2Case{Code: code, Name: strings.Join(args, " "), Scenario: "no-access", Args: append([]string{"codex", "reset", "use", "alice@example.com", "--json"}, args...), Exit: 2, Command: "codex reset use", Forbid: []string{"lookup", "consume"}})
	}
	for _, group := range []string{"reset", "resets"} {

		var stdout, stderr bytes.Buffer
		s := &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}
		exit := cli.Run(context.Background(), []string{"codex", group, "use", "--help"}, s, func(string) (cli.Handler, bool) { t.Fatal("help reached lookup"); return nil, false })
		want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "help", "codex-reset-use.txt"))
		if err != nil || exit != 0 || stdout.String() != string(want) {
			t.Fatalf("help exit=%d err=%v", exit, err)
		}

	}
}

func useProductionFixture(t *testing.T) (*codexprov.ManagedStore, codexprov.ManagedRecord, codexprov.RemovalJournal) {
	t.Helper()
	temp := os.TempDir()
	// Darwin's default temporary directory exceeds the Unix socket path limit.
	if runtime.GOOS == "darwin" {
		temp = "/tmp"
	}
	root, err := os.MkdirTemp(temp, "cqu-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "c"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "k"))
	store, err := codexprov.NewManagedStore(fsutil.OSFileSystem{})
	if err != nil {
		t.Fatal(err)
	}
	token := fakeRefreshCodexJWT("synthetic@example.test", "account", "user", time.Now().Add(time.Hour))
	record, err := store.SaveNew(codexprov.LoginCredential{Tokens: auth.CodexTokenResponse{AccessToken: token, IDToken: token, RefreshToken: "synthetic-refresh"}, Claims: auth.CodexClaims{AccountID: "account", UserID: "user", Email: "synthetic@example.test"}, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	roots, err := userdirs.Default(userdirs.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	return store, record, codexprov.RemovalJournal{FS: store.FS, Store: store, StateDir: roots.State}
}
func TestCLIV2ResetUseProductionAuthority(t *testing.T) {
	for _, authorised := range []bool{false, true} {
		for _, phase := range []int32{1, 2, 3} {
			t.Run(fmt.Sprintf("authority=%t/usage=%d", authorised, phase), func(t *testing.T) {
				store, record, journal := useProductionFixture(t)
				var usage, exchanges, consumes atomic.Int32
				client := testDoer(func(req *http.Request) (*http.Response, error) {
					switch req.URL.Path {
					case "/backend-api/wham/rate-limit-reset-credits":
						return resetProductionResponse(200, `{"credits":[{"id":"credit","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-01T00:00:00Z"}],"available_count":1}`), nil
					case "/backend-api/wham/usage":
						call := usage.Add(1)
						if call == phase && req.Header.Get("Authorization") != "Bearer refreshed-synthetic" {
							return resetProductionResponse(401, `{}`), nil
						}
						return resetProductionUsage(), nil
					case "/oauth/token":
						exchanges.Add(1)
						body, _ := json.Marshal(auth.CodexTokenResponse{AccessToken: "refreshed-synthetic", RefreshToken: "next-synthetic", IDToken: record.Credential.IDToken, ExpiresIn: 3600})
						return resetProductionResponse(200, string(body)), nil
					case "/backend-api/wham/rate-limit-reset-credits/consume":
						consumes.Add(1)
						return resetProductionResponse(200, `{"code":"reset","windows_reset":2}`), nil
					default:
						panic("unexpected fake HTTP")
					}
				})
				if authorised {
					coordinator, err := codexprov.NewCredentialCoordinator(store, journal.StateDir)
					if err != nil {
						t.Fatal(err)
					}
					coordinator.RefreshMutations = resetProductionRecorder{}
					coordinator.CredentialOwner = resetProductionRecorder{}
					coordinator.RefreshExchange = func(ctx context.Context, token string) (*auth.CodexTokenResponse, error) {
						return auth.RefreshCodexToken(ctx, client, token)
					}
					owner, err := codexprov.OpenCredentialControl(codexprov.DefaultCredentialControlPath(journal.StateDir), coordinator)
					if err != nil {
						t.Fatal(err)
					}
					defer owner.Close()
				}
				inv, _ := cli.Parse([]string{"codex", "reset", "use", string(record.Metadata.AccountKey), "--yes", "--json"})
				var stdout, stderr bytes.Buffer
				s := &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}
				out := handleV2ResetUseWithFactory(context.Background(), inv, s, func(ctx context.Context, _ bool) (v2ResetDependencies, error) {
					return newV2ResetUseDependenciesWithClient(ctx, store.FS, client)
				})
				if authorised {
					if exchanges.Load() != 1 || consumes.Load() != 1 || out.ExitCode != 0 || decodeUse(t, out).Outcome != "reset" {
						t.Fatalf("authority lost at usage %d: exchanges=%d consumes=%d out=%+v", phase, exchanges.Load(), consumes.Load(), out)
					}
				} else {
					if exchanges.Load() != 0 {
						t.Fatal("ephemeral owner granted refresh authority")
					}
					want := int32(1)
					if phase == 1 {
						want = 0
					}
					if consumes.Load() != want {
						t.Fatalf("consumes=%d want=%d out=%+v", consumes.Load(), want, out)
					}
					if phase == 3 && (out.ExitCode != 8 || decodeUse(t, out).Outcome != "reset") {
						t.Fatalf("lost postcheck terminal result: %+v", out)
					}
				}
				if strings.Contains(stderr.String(), record.Credential.AccessToken) || strings.Contains(fmt.Sprint(out.Errors), "synthetic-refresh") {
					t.Fatal("credential leaked")
				}
			})
		}
	}
}

type useResolverFunc func(context.Context, codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error)

func (f useResolverFunc) ResolveExact(ctx context.Context, p codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
	return f(ctx, p)
}
func TestCLIV2ResetUseProductionTimeoutBeforeDispatch(t *testing.T) {
	store, record, _ := useProductionFixture(t)
	var requests atomic.Int32
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	client := testDoer(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/wham/rate-limit-reset-credits":
			return resetProductionResponse(200, `{"credits":[{"id":"credit","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-01T00:00:00Z"}],"available_count":1}`), nil
		case "/backend-api/wham/usage":
			return resetProductionUsage(), nil
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			requests.Add(1)
			return resetProductionResponse(200, `{"code":"reset","windows_reset":2}`), nil
		default:
			panic("unexpected fake HTTP")
		}
	})
	inv, _ := cli.Parse([]string{"codex", "reset", "use", string(record.Metadata.AccountKey), "--yes", "--json"})
	s := &cli.Session{In: noAccessV2Input{}, Out: io.Discard, Err: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	factory := func(ctx context.Context, _ bool) (v2ResetDependencies, error) {
		deps, err := newV2ResetUseDependenciesWithClient(ctx, store.FS, client)
		if err != nil {
			return deps, err
		}
		backend := deps.app.Backend.(*codexprov.ResetBackend)
		resolver := backend.Resolver
		backend.Resolver = useResolverFunc(func(ctx context.Context, p codexprov.PlannedCandidate) (codexprov.CredentialMaterial, error) {
			material, err := resolver.ResolveExact(ctx, p)
			if calls.Add(1) == 4 {
				close(entered)
				<-release
				close(finished)
			}
			return material, err
		})
		return deps, nil
	}
	done := make(chan cli.Outcome, 1)
	go func() { done <- handleV2ResetUseWithFactory(ctx, inv, s, factory) }()
	select {
	case <-entered:
		cancel()
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("consume resolver boundary not reached")
	}
	var out cli.Outcome
	select {
	case out = <-done:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("resolver exceeded cancellation")
	}
	data := decodeUse(t, out)
	if out.ExitCode != 130 || data.Outcome != "cancelled" || data.Retry != nil || requests.Load() != 0 {
		t.Fatalf("out=%+v requests=%d", out, requests.Load())
	}
	close(release)
	<-finished
	// The dispatch gate remains closed even when the context-ignoring resolver
	// returns valid material after the command has already produced its outcome.
	time.Sleep(10 * time.Millisecond)
	if requests.Load() != 0 {
		t.Fatal("late resolver dispatched after return")
	}
}

type usePostcheckUsageWait struct {
	app.CodexResetUsage
	calls atomic.Int32
	done  chan struct{}
}

func (u *usePostcheckUsageWait) Fetch(ctx context.Context, now time.Time) ([]quota.Result, error) {
	if u.calls.Add(1) == 3 {
		defer close(u.done)
	}
	return u.CodexResetUsage.Fetch(ctx, now)
}
func TestCLIV2ResetUseProductionPostcheckDiagnostics(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			store, record, _ := useProductionFixture(t)
			process, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer process.Close()
			previous := os.Stderr
			os.Stderr = process
			defer func() { os.Stderr = previous }()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var usage atomic.Int32
			client := testDoer(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/backend-api/wham/rate-limit-reset-credits":
					return resetProductionResponse(200, `{"credits":[{"id":"credit","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-01T00:00:00Z"}],"available_count":1}`), nil
				case "/backend-api/wham/usage":
					if usage.Add(1) == 3 {
						close(entered)
						if late {
							<-release
						}
						panic("synthetic-sensitive-panic")
					}
					return resetProductionUsage(), nil
				case "/backend-api/wham/rate-limit-reset-credits/consume":
					return resetProductionResponse(200, `{"code":"reset","windows_reset":2}`), nil
				default:
					panic("unexpected fake HTTP")
				}
			})
			factory := func(ctx context.Context, _ bool) (v2ResetDependencies, error) {
				deps, err := newV2ResetUseDependenciesWithClient(ctx, store.FS, client)
				if err == nil {
					deps.app.Usage = &usePostcheckUsageWait{CodexResetUsage: deps.app.Usage, done: done}
				}
				return deps, err
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stdout, stderr bytes.Buffer
			complete := make(chan int, 1)
			go func() {
				complete <- cli.Run(ctx, []string{"codex", "reset", "use", string(record.Metadata.AccountKey), "--yes", "--json"}, &cli.Session{In: noAccessV2Input{}, Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) {
					return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
						return handleV2ResetUseWithFactory(ctx, inv, s, factory)
					}, true
				})
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("postcheck not reached")
			}
			if late {
				cancel()
			}
			var exit int
			select {
			case exit = <-complete:
			case <-time.After(time.Second):
				if late {
					close(release)
				}
				t.Fatal("postcheck did not finish")
			}
			before := stdout.String() + stderr.String()
			if late {
				close(release)
			}
			<-done
			raw, err := os.ReadFile(process.Name())
			if err != nil {
				t.Fatal(err)
			}
			wantExit := 8
			if late {
				wantExit = 130
			}
			if exit != wantExit || len(raw) != 0 || before != stdout.String()+stderr.String() || strings.Contains(before, "synthetic-sensitive") || strings.Contains(before, "goroutine") {
				t.Fatalf("diagnostics escaped or incorrect exit %d raw=%d", exit, len(raw))
			}
			var envelope struct{ Data ResetUseResult }
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Data.Outcome != "reset" || envelope.Data.Retry != nil {
				t.Fatalf("terminal result lost: %s", stdout.String())
			}
		})
	}
}

func TestCLIV2ResetUseProductionNoStateBeforeConsent(t *testing.T) {
	for _, mode := range []string{"missing-reference", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			store, record, _ := useProductionFixture(t)
			roots, err := userdirs.Default(userdirs.CacheRoot)
			if err != nil {
				t.Fatal(err)
			}
			client := testDoer(func(req *http.Request) (*http.Response, error) {
				if mode == "missing-reference" {
					t.Error("HTTP before reference validation")
				}
				switch req.URL.Path {
				case "/backend-api/wham/rate-limit-reset-credits":
					return resetProductionResponse(200, `{"credits":[{"id":"credit","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-01T00:00:00Z"}],"available_count":1}`), nil
				case "/backend-api/wham/usage":
					return resetProductionUsage(), nil
				default:
					panic("unexpected fake HTTP")
				}
			})
			reference := string(record.Metadata.AccountKey)
			if mode == "missing-reference" {
				reference = "missing-reference"
			}
			inv, _ := cli.Parse([]string{"codex", "reset", "use", reference})
			s := &cli.Session{In: strings.NewReader("no\n"), Out: io.Discard, Err: io.Discard, Interactive: true}
			out := handleV2ResetUseWithFactory(context.Background(), inv, s, func(ctx context.Context, _ bool) (v2ResetDependencies, error) {
				return newV2ResetUseDependenciesWithClient(ctx, store.FS, client)
			})
			if mode == "missing-reference" {
				if out.ExitCode != 3 || out.Errors[0].Code != "account_not_found" {
					t.Fatalf("out=%+v", out)
				}
			} else if out.ExitCode != 0 || decodeUse(t, out).Outcome != "cancelled" {
				t.Fatalf("out=%+v", out)
			}
			if _, err := os.Stat(roots.Cache); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reference failure or cancellation wrote cache/history/attempt state: %v", err)
			}
		})
	}
}

type useShortOutput struct{}

func (useShortOutput) Write([]byte) (int, error) { return 0, nil }
func TestCLIV2ResetUseFailedPreviewNeverConsumes(t *testing.T) {
	a, b, s, inv, _ := newUseTest(t)
	s.Err = useShortOutput{}
	out := handleV2ResetUseWithApp(context.Background(), inv, s, a)
	if out.ExitCode != 1 || useCalls(b.f, "consume") != 0 || decodeUse(t, out).Outcome != "cancelled" {
		t.Fatalf("out=%+v", out)
	}
}
func TestCLIV2ResetUseDuplicateInvocationRetainsRequestIdentity(t *testing.T) {
	a, b, s, inv, _ := newUseTest(t)
	inv.Options["credit"] = []string{"credit-a"}
	keys := []string{}
	b.consume = func(_ context.Context, account codexprov.ResetAccount, credit, key string) (codexprov.ConsumeResetResult, error) {
		keys = append(keys, key)
		outcome := codexprov.ConsumeReset
		if len(keys) > 1 {
			outcome = codexprov.ConsumeAlreadyRedeemed
		}
		return codexprov.ConsumeResetResult{Outcome: outcome}, nil
	}
	for range 2 {
		if out := handleV2ResetUseWithApp(context.Background(), inv, s, a); out.ExitCode != 0 {
			t.Fatalf("out=%+v", out)
		}
	}
	if len(keys) != 2 || keys[0] != keys[1] || useCalls(b.f, "consume") != 2 {
		t.Fatalf("duplicate changed key or send count")
	}
}
func init() {
	for _, scenario := range []string{"reset-indeterminate", "reset-postcheck-fails"} {
		registerV2Fixture(scenario, func(t *testing.T) *v2Fixture {
			a, b, _, _, _ := newUseTest(t)
			b.consume = func(context.Context, codexprov.ResetAccount, string, string) (codexprov.ConsumeResetResult, error) {
				if scenario == "reset-indeterminate" {
					return codexprov.ConsumeResetResult{}, context.DeadlineExceeded
				}
				a.Usage = v2ResetUsage{f: b.f, err: errors.New("synthetic usage failed")}
				return codexprov.ConsumeResetResult{Outcome: codexprov.ConsumeReset, WindowsReset: 2}, nil
			}
			b.f.Lookup = func(path string) (cli.Handler, bool) {
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					return handleV2ResetUseWithApp(ctx, inv, s, a)
				}, path == "codex reset use"
			}
			return b.f
		})
	}
}
func TestCLIV2ResetUseContractOutcomes(t *testing.T) {
	runV2Case(t, v2Case{Name: "indeterminate keeps retry", Scenario: "reset-indeterminate", Args: []string{"codex", "reset", "use", "alice@example.com", "--yes", "--json"}, Exit: 7, Command: "codex reset use", Code: "codex_reset_timeout", WantJSON: `{"outcome":"indeterminate","retry":{"account_reference":"key-a","credit_id":"credit-a"}}`, Calls: map[string]int{"consume": 1}, Forbid: []string{"credential-activate", "service"}})
	runV2Case(t, v2Case{Name: "known success survives postcheck", Scenario: "reset-postcheck-fails", Args: []string{"codex", "resets", "use", "alice@example.com", "--yes", "--json"}, Exit: 8, Command: "codex reset use", Code: "codex_reset_postcheck_partial", WantJSON: `{"outcome":"reset","windows_reset":2,"changed_windows":[],"retry":null}`, Calls: map[string]int{"consume": 1}, Forbid: []string{"credential-activate", "service"}})
}
