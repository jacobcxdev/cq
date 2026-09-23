package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

var terminalResetsFixtures = []terminalFixtureRegistration{
	{"cq codex reset", "Failure", "stderr", "text/cq-codex-reset/02-failure.txt"},
	{"cq codex reset list", "Success · all accounts", "stdout", "text/cq-codex-reset-list/02-success-all-accounts.txt"},
	{"cq codex reset list", "Success · one account", "stdout", "text/cq-codex-reset-list/03-success-one-account.txt"},
	{"cq codex reset list", "Empty", "stdout", "text/cq-codex-reset-list/04-empty.txt"},
	{"cq codex reset list", "Failure", "stderr", "text/cq-codex-reset-list/05-failure.txt"},
	{"cq codex reset list", "Partial inventory", "stdout", "text/cq-codex-reset-list/06-partial-inventory.txt"},
	{"cq codex reset list", "Partial inventory", "stderr", "text/cq-codex-reset-list/07-partial-inventory.txt"},
	{"cq codex reset recommend", "Success", "stdout", "text/cq-codex-reset-recommend/02-success.txt"},
	{"cq codex reset recommend", "No action recommended", "stdout", "text/cq-codex-reset-recommend/03-no-action-recommended.txt"},
	{"cq codex reset recommend", "Failure", "stderr", "text/cq-codex-reset-recommend/04-failure.txt"},
	{"cq codex reset recommend", "Incomplete recommendation", "stdout", "text/cq-codex-reset-recommend/05-incomplete-recommendation.txt"},
	{"cq codex reset recommend", "Incomplete recommendation", "stderr", "text/cq-codex-reset-recommend/06-incomplete-recommendation.txt"},
	{"cq codex reset use", "Success", "stdout", "text/cq-codex-reset-use/02-success.txt"},
	{"cq codex reset use", "Success", "stderr", "text/cq-codex-reset-use/03-success.txt"},
	{"cq codex reset use", "Confirmation declined", "stdout", "text/cq-codex-reset-use/04-confirmation-declined.txt"},
	{"cq codex reset use", "Confirmation declined", "stderr", "text/cq-codex-reset-use/05-confirmation-declined.txt"},
	{"cq codex reset use", "Failure", "stderr", "text/cq-codex-reset-use/06-failure.txt"},
	{"cq codex reset use", "Indeterminate consumption", "stdout", "text/cq-codex-reset-use/07-indeterminate-consumption.txt"},
	{"cq codex reset use", "Indeterminate consumption", "stderr", "text/cq-codex-reset-use/08-indeterminate-consumption.txt"},
	{"cq codex reset use", "Known reset, incomplete postcheck", "stdout", "text/cq-codex-reset-use/09-known-reset-incomplete-postcheck.txt"},
	{"cq codex reset use", "Known reset, incomplete postcheck", "stderr", "text/cq-codex-reset-use/10-known-reset-incomplete-postcheck.txt"},
	{"cq codex reset use", "Already redeemed replay", "stdout", "text/cq-codex-reset-use/11-already-redeemed-replay.txt"},
	{"cq codex reset use", "Already redeemed replay", "stderr", "text/cq-codex-reset-use/12-already-redeemed-replay.txt"},
	{"cq codex reset use", "Nothing to reset", "stdout", "text/cq-codex-reset-use/13-nothing-to-reset.txt"},
	{"cq codex reset use", "Nothing to reset", "stderr", "text/cq-codex-reset-use/14-nothing-to-reset.txt"},
	{"cq codex reset use", "No credit returned by consumption", "stdout", "text/cq-codex-reset-use/15-no-credit-returned-by-consumption.txt"},
	{"cq codex reset use", "No credit returned by consumption", "stderr", "text/cq-codex-reset-use/16-no-credit-returned-by-consumption.txt"},
}

func resetFixtureTime(days int, hours int, minutes int) time.Time {
	return terminalFixtureOptions(100).Now.Add(time.Duration(days)*24*time.Hour + time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute)
}
func resetFixtureInventory() []ResetAccountInventory {
	one, zero := 1, 0
	expiry := resetFixtureTime(2, 0, 0)
	return []ResetAccountInventory{
		{AccountReference: v2AccountString("account-alex"), AccountID: v2AccountString("acct_alex_01"), Email: v2AccountString("alex@example.test"), AvailableCount: &one, Credits: []ResetCredit{{ID: "credit-september-01", ResetType: codexprov.ResetTypeCodexRateLimits, Status: "available", Supported: true, GrantedAt: resetFixtureTime(-2, 0, 0), ExpiresAt: &expiry}}},
		{AccountReference: v2AccountString("account-sam"), AccountID: v2AccountString("acct_sam_01"), Email: v2AccountString("sam@example.test"), AvailableCount: &zero},
	}
}
func resetFixtureSchedule(scenario string) ResetSchedule {
	s := ResetSchedule{Complete: true, Exact: true, Confidence: aggregate.ResetConfidenceHigh, GeneratedAt: resetFixtureTime(0, 0, 0), Horizon: resetFixtureTime(7, 0, 0)}
	if scenario == "Success" {
		at, by := resetFixtureTime(0, 2, 15), resetFixtureTime(2, 0, 0)
		s.Horizon = resetFixtureTime(9, 0, 0)
		s.Items = []ResetScheduleItem{{AccountReference: "account-alex", AccountEmail: v2AccountString("alex@example.test"), AccountID: v2AccountString("acct_alex_01"), CreditID: "credit-september-01", UseAt: &at, UseBy: &by, Status: "scheduled", Confidence: aggregate.ResetConfidenceHigh, AvoidedGapSeconds: 2700, RestoredPct: []ResetRestoredWindow{{Name: quota.Window5Hour, Percentage: 100}, {Name: quota.Window7Day, Percentage: 80}}, ReasonCodes: []aggregate.ResetScheduleReason{aggregate.ResetReasonGapAvoidance, aggregate.ResetReasonExpiryPressure}}}
		s.Objective.WeightedDiscardedPct = 4
		s.Objective.RestoredPct = 180
	}
	if scenario == "Incomplete recommendation" {
		s.Complete = false
		s.Exact = false
		s.Confidence = aggregate.ResetConfidenceLow
		s.Blockers = []ResetScheduleBlocker{{Code: "usage_unavailable", AccountReference: v2AccountString("account-alex"), AccountEmail: v2AccountString("alex@example.test"), AccountID: v2AccountString("acct_alex_01")}}
	}
	return s
}
func resetFixturePreview(scenario string) app.CodexResetUsePlan {
	short, long := 75, 5
	if scenario == "Already redeemed replay" {
		long = 100
	}
	if scenario == "Nothing to reset" {
		short, long = 100, 100
	}
	return app.CodexResetUsePlan{AccountKey: "account-alex", Email: "alex@example.test", Credit: codexprov.ResetCredit{ID: "credit-september-01"}, CurrentWindows: map[quota.WindowName]quota.Window{
		quota.Window5Hour: {RemainingPct: short, ResetAtUnix: resetFixtureTime(0, 2, 0).Unix()}, quota.Window7Day: {RemainingPct: long, ResetAtUnix: resetFixtureTime(2, 18, 0).Unix()},
	}}
}
func resetFixtureUse(scenario string) (ResetUseResult, cli.Outcome) {
	d := ResetUseResult{AccountReference: "account-alex", CreditID: v2AccountString("credit-september-01"), Outcome: "reset", WindowsReset: 1}
	out := cli.Outcome{}
	switch scenario {
	case "Confirmation declined":
		d.Outcome = "cancelled"
		d.WindowsReset = 0
	case "Indeterminate consumption":
		d.Outcome = "indeterminate"
		d.WindowsReset = 0
		d.Retry = &ResetRetry{AccountReference: d.AccountReference, CreditID: *d.CreditID}
		out = v2ResetUseFailure("consume_indeterminate")
	case "Known reset, incomplete postcheck":
		out = v2ResetUseFailure("postcheck_partial")
		out.Warnings = []cli.Diagnostic{{Code: "codex_reset_usage_refetch_failed", Message: "Fresh quota could not be read."}}
	case "Already redeemed replay":
		d.Outcome = "already_redeemed"
		d.WindowsReset = 0
	case "Nothing to reset":
		d.Outcome = "nothing_to_reset"
		d.WindowsReset = 0
	case "No credit returned by consumption":
		d.Outcome = "no_credit"
		d.WindowsReset = 0
	case "Success":
		d.ChangedWindows = []ResetWindowChange{{Name: quota.Window7Day, Before: ResetWindow{RemainingPct: 5, ResetAt: resetFixtureTime(2, 18, 0), PeriodSeconds: 604800}, After: ResetWindow{RemainingPct: 100, ResetAt: resetFixtureTime(2, 18, 0), PeriodSeconds: 604800}}}
	}
	return d, out
}
func TestTerminalResetFixtures(t *testing.T) {
	for _, f := range terminalResetsFixtures {
		t.Run(f.File, func(t *testing.T) {
			for _, width := range []int{20, 40, 80, 100, 120} {
				var stdout, stderr bytes.Buffer
				s := &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: terminalFixtureOptions(width), ErrTerminal: terminalFixtureOptions(width)}
				args := strings.Fields(strings.TrimPrefix(f.Command, "cq "))
				if f.Command == "cq codex reset" {
					args = append(args, "inspect")
				}
				if f.Command == "cq codex reset use" {
					args = append(args, "account-alex", "--yes")
				}
				cli.Run(context.Background(), args, s, func(string) (cli.Handler, bool) {
					return func(_ context.Context, _ cli.Invocation, s *cli.Session) cli.Outcome {
						switch f.Command {
						case "cq codex reset list":
							if f.Scenario == "Failure" {
								return v2ResetDiagnostic(5, "codex_reset_auth_failed", "Codex reset authentication failed.")
							}
							rows := resetFixtureInventory()
							complete := true
							selected := false
							out := cli.Outcome{}
							switch f.Scenario {
							case "Success · one account":
								rows = rows[:1]
								selected = true
							case "Empty":
								rows = rows[:1]
								selected = true
								zero := 0
								rows[0].AvailableCount = &zero
								rows[0].Credits = nil
							case "Partial inventory":
								complete = false
								zero := 0
								rows[0].AvailableCount = &zero
								rows[0].Credits = nil
								rows[1].AvailableCount = nil
								rows[1].Errors = []ResetInventoryError{v2ResetInventoryError("credits_unavailable", nil)}
								out = v2ResetDiagnostic(8, "codex_reset_inventory_partial", "Reset-credit inventory is incomplete.")
							}
							out.Terminal = func(o cli.TerminalOptions) string { return terminalResetInventory(rows, complete, selected, o) }
							return out
						case "cq codex reset recommend":
							if f.Scenario == "Failure" {
								return v2ResetFailure(fmt.Errorf("unavailable"))
							}
							schedule := resetFixtureSchedule(f.Scenario)
							out := cli.Outcome{}
							if !schedule.Complete {
								out = v2ResetDiagnostic(8, "codex_reset_recommendation_incomplete", "Reset recommendation is incomplete; resolve the reported blockers.")
							}
							out.Terminal = func(o cli.TerminalOptions) string { return terminalResetSchedule(schedule, o) }
							return out
						case "cq codex reset use":
							if f.Scenario == "Failure" {
								return v2ResetUseFailure("credit_not_found")
							}
							if err := writeV2ResetPreview(s, resetFixturePreview(f.Scenario), f.Scenario == "Confirmation declined"); err != nil {
								t.Fatal(err)
							}
							d, out := resetFixtureUse(f.Scenario)
							return v2ResetUseData(out, d)
						}
						return cli.Outcome{}
					}, true
				})
				got := stdout.String()
				if f.Stream == "stderr" {
					got = stderr.String()
				}
				if width >= 100 {
					assertTerminalFixture(t, f.File, got)
					if f.Stream == "stderr" {
						switch f.Scenario {
						case "Partial inventory", "Incomplete recommendation", "Known reset, incomplete postcheck":
							if !strings.Contains(got, "\x1b[33mWarning\x1b[0m") {
								t.Fatal("warning role", got)
							}
						case "Failure", "Indeterminate consumption":
							if !strings.Contains(got, "\x1b[31mError\x1b[0m") {
								t.Fatal("failure role", got)
							}
						}
					}
				}
				for _, line := range strings.Split(got, "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Fatalf("width %d overflow %q", width, line)
					}
				}
			}
		})
	}
}
func TestTerminalResetRegistration(t *testing.T) {
	data, err := os.ReadFile("../../specs/cli-v2/presentation/examples/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct{ Fixtures []terminalFixtureRegistration }
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	registered := map[string]terminalFixtureRegistration{}
	for _, f := range terminalResetsFixtures {
		if _, ok := registered[f.File]; ok {
			t.Fatal("duplicate", f.File)
		}
		registered[f.File] = f
	}
	for _, f := range index.Fixtures {
		if !strings.HasPrefix(f.Command, "cq codex reset") || f.Scenario == "Help" {
			continue
		}
		if registered[f.File] != f {
			t.Fatal("missing fixture", f)
		}
		delete(registered, f.File)
	}
	if len(registered) > 0 {
		t.Fatal("extra fixtures", registered)
	}
}

func TestTerminalResetRolesAndCompleteFields(t *testing.T) {
	rows := resetFixtureInventory()
	rows[0].Email = v2AccountString("failed-1000@example.test")
	zero := 0
	rows[0].AvailableCount = &zero
	expired := resetFixtureTime(-1, 0, 0)
	rows[0].Credits[0].ExpiresAt = &expired
	rows[0].Credits[0].Title = v2AccountString("Custom title")
	rows[0].Credits[0].Description = v2AccountString("Complete description")
	index := 3
	rows[0].Errors = []ResetInventoryError{{Code: "invalid_entry", Message: "Reset-credit entry is invalid.", EntryIndex: &index}}
	rows[1].AvailableCount = nil
	colour := terminalResetInventory(rows, false, false, terminalFixtureOptions(100))
	for _, want := range []string{"\x1b[36mfailed-1000@example.test\x1b[0m", "\x1b[1;34m0 credits\x1b[0m", "\x1b[33munknown\x1b[0m", "\x1b[32mavailable\x1b[0m", "\x1b[33mExpired\x1b[0m", "\x1b[35mexpired 1d ago", "\x1b[36mYes\x1b[0m"} {
		if !strings.Contains(colour, want) {
			t.Fatalf("missing typed role %q in %q", want, colour)
		}
	}
	for _, width := range []int{20, 40, 80, 120} {
		o := terminalFixtureOptions(width)
		got := terminalResetInventory(rows, false, false, o)
		if width == 40 || width == 120 {
			t.Logf("%d-column inventory:\n%s", width, ansi.Strip(got))
		}
		compact := strings.Join(strings.Fields(ansi.Strip(got)), "")
		for _, want := range []string{"failed-1000@example.test", "credit-september-01", "codex_rate_limits", "Customtitle", "Completedescription", "invalid_entry", "entry3", "expired1dago"} {
			if !strings.Contains(compact, want) {
				t.Fatalf("width %d dropped %s: %s", width, want, got)
			}
		}
		o.Colour = false
		if plain := terminalResetInventory(rows, false, false, o); plain != ansi.Strip(got) {
			t.Fatal("colour changed visible output")
		}
	}
	d, out := resetFixtureUse("Known reset, incomplete postcheck")
	got := v2ResetUseData(out, d).Terminal(terminalFixtureOptions(100))
	for _, want := range []string{"\x1b[32mCompleted\x1b[0m", "\x1b[33mVerification incomplete\x1b[0m", "\x1b[1;34m1\x1b[0m"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %q", want, got)
		}
	}
}

func TestTerminalResetRetryQuotingAndPurity(t *testing.T) {
	d, out := resetFixtureUse("Indeterminate consumption")
	d.Retry.AccountReference = "account with ' quote"
	d.Retry.CreditID = "credit-$(touch hostile)"
	result := v2ResetUseData(out, d)
	before := string(result.Data)
	for range 2 {
		got := ansi.Strip(result.Terminal(terminalFixtureOptions(100)))
		if !strings.Contains(got, "'account with '\\'' quote' --credit 'credit-$(touch hostile)'") || strings.Contains(got, "--yes") {
			t.Fatal(got)
		}
	}
	if string(result.Data) != before {
		t.Fatal("renderer mutated receipt")
	}
}

func TestTerminalResetAdaptersAndCleanup(t *testing.T) {
	for _, path := range []string{"codex reset list", "codex reset recommend"} {
		t.Run(path, func(t *testing.T) {
			a, b := v2ResetTestApp()
			var closes atomic.Int32
			out := handleV2ResetInspectionWithFactory(context.Background(), v2ResetInvocation(path), nil, func(context.Context, bool) (v2ResetDependencies, error) {
				return v2ResetDependencies{app: a, close: func() error { closes.Add(1); return fmt.Errorf("cleanup failed") }}, nil
			})
			if out.ExitCode == 0 || out.Terminal == nil {
				t.Fatalf("missing failure/renderer: %+v", out)
			}
			for range 2 {
				got := ansi.Strip(out.Terminal(terminalFixtureOptions(100)))
				if !strings.Contains(got, "Command verification: incomplete") {
					t.Fatal(got)
				}
			}
			if closes.Load() != 1 || useCalls(b.f, "consume") != 0 || useCalls(b.f, "credits:key-a") != 1 || useCalls(b.f, "credits:key-b") != 1 {
				t.Fatal("renderer performed operations")
			}
		})
	}
	for _, mode := range []string{"success", "cleanup", "refetch", "indeterminate", "declined", "pre-dispatch-failure", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			a, b, s, inv, store := newUseTest(t)
			var closes atomic.Int32
			if mode == "pre-dispatch-failure" {
				a.Attempts = useTestAttempts{CodexResetAttempts: store, ensureErr: fmt.Errorf("write unavailable")}
			}
			inv.JSON = false
			s.OutTerminal = terminalFixtureOptions(100)
			s.ErrTerminal = terminalFixtureOptions(100)
			s.In = strings.NewReader("n\n")
			s.Interactive = true
			if mode == "declined" {
				inv.Options["yes"] = []string{"false"}
			}
			original := b.consume
			b.consume = func(ctx context.Context, account codexprov.ResetAccount, credit, key string) (codexprov.ConsumeResetResult, error) {
				if mode == "rejected" {
					return codexprov.ConsumeResetResult{}, &codexprov.ResetHTTPError{Status: 400}
				}
				if mode == "indeterminate" {
					return codexprov.ConsumeResetResult{}, fmt.Errorf("transport failed")
				}
				result, err := original(ctx, account, credit, key)
				if mode == "refetch" {
					a.Usage = v2ResetUsage{f: b.f, err: fmt.Errorf("usage unavailable")}
				}
				return result, err
			}
			out := handleV2ResetUseWithFactory(context.Background(), inv, s, func(context.Context, bool) (v2ResetDependencies, error) {
				return v2ResetDependencies{app: a, close: func() error {
					closes.Add(1)
					if mode == "cleanup" {
						return fmt.Errorf("cleanup failed")
					}
					return nil
				}}, nil
			})
			calls := 1
			if mode == "declined" || mode == "pre-dispatch-failure" {
				calls = 0
			}
			data := decodeUse(t, out)
			saved := string(out.Data)
			for range 2 {
				got := ansi.Strip(out.Terminal(terminalFixtureOptions(100)))
				if mode == "cleanup" || mode == "refetch" {
					if out.ExitCode != 8 || !strings.Contains(got, "Completed · Verification incomplete") {
						t.Fatalf("%+v %s", out, got)
					}
				}
				if mode == "indeterminate" {
					if !strings.Contains(got, "Completion unknown") || !strings.Contains(got, "cq codex reset use key-a --credit credit-a") || strings.Contains(got, "--yes") {
						t.Fatal(got)
					}
				}
				if mode == "rejected" || mode == "pre-dispatch-failure" {
					if out.ExitCode == 0 || !strings.Contains(got, "Not completed") || !strings.Contains(got, "Consumption: not confirmed") || strings.Contains(got, "No credit consumed") || strings.Contains(got, "no change requested") {
						t.Fatalf("failed attempt overstated: %s", got)
					}
				}
				if mode == "declined" && !strings.Contains(got, "No credit consumed.") {
					t.Fatal(got)
				}
			}
			if closes.Load() != 1 || useCalls(b.f, "consume") != calls || string(out.Data) != saved {
				t.Fatalf("render changed receipt or send count: %+v", data)
			}
		})
	}
}

func TestTerminalResetDestinationStreamsAndJSON(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		for _, stdoutTTY := range []bool{false, true} {
			for _, stderrTTY := range []bool{false, true} {
				for _, colour := range []bool{false, true} {
					t.Run(fmt.Sprintf("json=%t/out=%t/err=%t/colour=%t", jsonMode, stdoutTTY, stderrTTY, colour), func(t *testing.T) {
						a, b, _, _, _ := newUseTest(t)
						var stdout, stderr bytes.Buffer
						session := &cli.Session{Out: &stdout, Err: &stderr}
						if stdoutTTY {
							session.OutTerminal = terminalFixtureOptions(100)
							session.OutTerminal.Colour = colour
						}
						if stderrTTY {
							session.ErrTerminal = terminalFixtureOptions(100)
							session.ErrTerminal.Colour = colour
						}
						args := []string{"codex", "reset", "use", "alice@example.com", "--yes"}
						if jsonMode {
							args = append(args, "--json")
						}
						var result cli.Outcome
						exit := cli.Run(context.Background(), args, session, func(string) (cli.Handler, bool) {
							return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
								result = handleV2ResetUseWithApp(ctx, inv, s, a)
								return result
							}, true
						})
						if exit != 0 || useCalls(b.f, "consume") != 1 {
							t.Fatalf("exit %d", exit)
						}
						if strings.Contains(stdout.String(), "\x1b[") != (stdoutTTY && colour && !jsonMode) {
							t.Fatal("stdout colour gate")
						}
						if strings.Contains(stderr.String(), "\x1b[") != (stderrTTY && colour && !jsonMode) {
							t.Fatal("stderr colour gate")
						}
						if !stdoutTTY && !jsonMode && stdout.String() != result.Human {
							t.Fatal("redirected Human bytes changed")
						}
						if jsonMode {
							if !json.Valid(stdout.Bytes()) || !strings.HasPrefix(stderr.String(), "Account: alice@example.com.\nAccount reference: key-a.\nCredit: credit-a.\n") {
								t.Fatalf("JSON/plain preview changed: %s %s", stdout.String(), stderr.String())
							}
						} else if stderrTTY {
							if !strings.HasPrefix(ansi.Strip(stderr.String()), "Codex reset preview\n") || strings.HasSuffix(stderr.String(), "\n\n") {
								t.Fatal("preview separator")
							}
						}
					})
				}
			}
		}
	}
}

func TestTerminalResetEnvironmentColourGates(t *testing.T) {
	for _, tc := range []struct {
		name, noColour, term string
		want                 bool
	}{{"colour", "", "xterm-256color", true}, {"no-colour", "1", "xterm-256color", false}, {"empty-no-colour", "", "xterm", true}, {"dumb", "", "dumb", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColour)
			t.Setenv("TERM", tc.term)
			fixed := terminalFixtureOptions(100)
			o := terminalPresentation(100, fixed.Now, fixed.Location)
			for _, got := range []string{terminalResetInventory(resetFixtureInventory(), true, false, o), terminalResetPreview(resetFixturePreview("Success"), false, o)} {
				if strings.Contains(got, "\x1b[") != tc.want {
					t.Fatal("environment colour policy", got)
				}
			}
		})
	}
}

func TestTerminalResetUnobservedInventory(t *testing.T) {
	row := ansi.Strip(terminalResetInventory([]ResetAccountInventory{{AccountReference: v2AccountString("account-a")}}, false, false, terminalFixtureOptions(100)))
	if strings.Contains(row, "No reset credits.") {
		t.Fatal(row)
	}
	got := ansi.Strip(terminalResetInventory(nil, false, false, terminalFixtureOptions(100)))
	if strings.Contains(got, "No Codex accounts") || !strings.Contains(got, "Account inventory unavailable.") {
		t.Fatal(got)
	}
	known := ansi.Strip(terminalResetInventory(nil, true, false, terminalFixtureOptions(100)))
	if !strings.Contains(known, "No Codex accounts found.") {
		t.Fatal(known)
	}
}

func TestTerminalResetUnobservedRecommendation(t *testing.T) {
	a, b := v2ResetTestApp()
	b.snapshotHook = func() { panic("snapshot unavailable") }
	out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset recommend"), nil, a)
	got := ansi.Strip(out.Terminal(terminalFixtureOptions(100)))
	if out.ExitCode == 0 || strings.Contains(got, "0001") || strings.Contains(got, "0 %-seconds") || !strings.Contains(got, "Objective unavailable.") {
		t.Fatalf("exit %d: %s", out.ExitCode, got)
	}
	if !strings.Contains(got, "Horizon     unknown") || !strings.Contains(got, "Confidence  unknown") {
		t.Fatal(got)
	}
}
