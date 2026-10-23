package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/history"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

type v2ResetBackend struct {
	f            *v2Fixture
	snapshot     codexprov.ResetAccountSnapshot
	inventories  map[codexprov.AccountKey]codexprov.ResetCreditInventory
	failures     map[codexprov.AccountKey]error
	snapshotHook func()
	creditHook   func(codexprov.ResetAccount)
}

func (b *v2ResetBackend) Snapshot(context.Context) (codexprov.ResetAccountSnapshot, error) {
	b.f.Call("inventory")
	if b.snapshotHook != nil {
		b.snapshotHook()
	}
	return b.snapshot, nil
}
func (b *v2ResetBackend) ListCredits(ctx context.Context, a codexprov.ResetAccount) (codexprov.ResetCreditInventory, error) {
	b.f.Call("credits:" + string(a.AccountKey))
	if b.creditHook != nil {
		b.creditHook(a)
	}
	return b.inventories[a.AccountKey], b.failures[a.AccountKey]
}
func (b *v2ResetBackend) Consume(context.Context, codexprov.ResetAccount, string, string) (codexprov.ConsumeResetResult, error) {
	b.f.Call("consume")
	panic("inspection consumed")
}
func init() {
	registerV2Fixture("reset-inventory-partial", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		b := &v2ResetBackend{f: f, snapshot: codexprov.ResetAccountSnapshot{Accounts: []codexprov.ResetAccount{{AccountKey: "key-a", AccountID: "id-a"}, {AccountKey: "key-b", AccountID: "id-b"}}}, inventories: map[codexprov.AccountKey]codexprov.ResetCreditInventory{"key-a": {Credits: []codexprov.ResetCredit{{ID: "credit", ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditAvailable, GrantedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}}, AvailableCount: 1}}, failures: map[codexprov.AccountKey]error{"key-b": errors.New("broker failure")}}
		a := &app.CodexResetApp{Backend: b}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2ResetInspection(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ResetInspectionWithApp(ctx, inv, s, a)
			}, ok
		}
		return f
	})
}
func TestCLIV2ResetInspectionContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "partial inventory exposes incompleteness", Scenario: "reset-inventory-partial", Args: []string{"codex", "reset", "list", "--json"}, Exit: 8, Command: "codex reset list", Code: "codex_reset_inventory_partial", WantJSON: `{"complete":false}`, Forbid: []string{"consume", "credential-activate", "service"}})
}

type v2ResetClock struct{ now time.Time }

func (c v2ResetClock) Now() time.Time { return c.now }

type v2ResetUsage struct {
	f       *v2Fixture
	results []quota.Result
	err     error
	hook    func()
}

func (u v2ResetUsage) Fetch(context.Context, time.Time) ([]quota.Result, error) {
	u.f.Call("usage")
	if u.hook != nil {
		u.hook()
	}
	return u.results, u.err
}

type v2ResetHistory struct {
	f    *v2Fixture
	hook func()
}

func (h v2ResetHistory) UpdateAndGetEstimates(context.Context, map[string][]quota.Result, int64) (history.BurnRates, history.RateEstimates, error) {
	h.f.Call("history")
	if h.hook != nil {
		h.hook()
	}
	return nil, nil, nil
}
func v2ResetTestApp() (*app.CodexResetApp, *v2ResetBackend) {
	f := &v2Fixture{}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(5 * time.Hour)
	b := &v2ResetBackend{f: f, inventories: map[codexprov.AccountKey]codexprov.ResetCreditInventory{}, failures: map[codexprov.AccountKey]error{}}
	results := []quota.Result{}
	for _, suffix := range []string{"a", "b"} {
		key := codexprov.AccountKey("key-" + suffix)
		id := "id-" + suffix
		b.snapshot.Accounts = append(b.snapshot.Accounts, codexprov.ResetAccount{AccountKey: key, AccountID: id, Email: "same@example.com"})
		b.snapshot.Inventory.Accounts = append(b.snapshot.Inventory.Accounts, codexprov.LogicalAccount{Key: key, Identity: codexprov.AccountIdentity{AccountID: id, Email: "same@example.com"}})
		b.inventories[key] = codexprov.ResetCreditInventory{Credits: []codexprov.ResetCredit{{ID: "credit-" + suffix, ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditAvailable, GrantedAt: now.Add(-time.Hour), ExpiresAt: &expiry}}, AvailableCount: 1}
		results = append(results, quota.Result{AccountID: id, Email: "same@example.com", Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 10, ResetAtUnix: expiry.Unix()}, quota.Window7Day: {RemainingPct: 10, ResetAtUnix: now.Add(24 * time.Hour).Unix()}}})
	}
	return &app.CodexResetApp{Backend: b, Usage: v2ResetUsage{f: f, results: results}, History: v2ResetHistory{f: f}, Clock: v2ResetClock{now}}, b
}
func v2ResetInvocation(path string) cli.Invocation {
	return cli.Invocation{Path: path, Options: map[string][]string{"timeout": {"1s"}}, Arguments: map[string][]string{}}
}
func init() {
	registerV2Fixture("reset-recommend-boundaries", func(t *testing.T) *v2Fixture {
		a, b := v2ResetTestApp()
		b.f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2ResetInspection(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ResetInspectionWithApp(ctx, inv, s, a)
			}, ok
		}
		return b.f
	})
}
func TestCLIV2ResetInspectionNormative(t *testing.T) {
	runV2Case(t, v2Case{Name: "portfolio fresh boundary schedule", Scenario: "reset-recommend-boundaries", Args: []string{"codex", "reset", "recommend", "--json"}, Command: "codex reset recommend", WantJSON: `{"schedule":{"complete":true,"confidence":"low"}}`, Calls: map[string]int{"inventory": 1, "usage": 1, "history": 1, "credits:key-a": 1, "credits:key-b": 1}, Forbid: []string{"consume", "credential-activate", "service", "cache"}})
	for _, tc := range []struct {
		name, ref, code string
		exit            int
	}{{"exact", "key-a", "", 0}, {"ambiguous", "same@example.com", "account_ambiguous", 6}, {"missing", "unknown", "account_not_found", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := v2ResetTestApp()
			inv := v2ResetInvocation("codex reset list")
			inv.Arguments["account"] = []string{tc.ref}
			out := handleV2ResetInspectionWithApp(context.Background(), inv, nil, a)
			if out.ExitCode != tc.exit {
				t.Fatalf("outcome=%+v", out)
			}
			if tc.code != "" {
				if out.Errors[0].Code != tc.code || out.Data != nil {
					t.Fatalf("outcome=%+v", out)
				}
				if b.f.calls["credits:key-a"]+b.f.calls["credits:key-b"] != 0 {
					t.Fatal("HTTP before selection")
				}
			} else {
				if b.f.calls["credits:key-a"] != 1 || b.f.calls["credits:key-b"] != 0 {
					t.Fatal("selected wrong account")
				}
			}
		})
	}
	t.Run("empty", func(t *testing.T) {
		a, b := v2ResetTestApp()
		b.snapshot = codexprov.ResetAccountSnapshot{}
		out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset list"), nil, a)
		if out.ExitCode != 0 || string(out.Data) != `{"accounts":[],"complete":true}` || out.Human != "Reset inventories: 0; complete=true.\n" {
			t.Fatalf("outcome=%+v", out)
		}
	})
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a, b := v2ResetTestApp()
			for key := range b.inventories {
				b.failures[key] = &codexprov.ResetHTTPError{Status: status}
			}
			out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset list"), nil, a)
			if out.ExitCode != 8 || out.Errors[0].Code != "codex_reset_inventory_partial" {
				t.Fatalf("partial precedence: %+v", out)
			}
			var data struct{ Accounts []ResetAccountInventory }
			if err := json.Unmarshal(out.Data, &data); err != nil {
				t.Fatal(err)
			}
			want := "credits_unavailable"
			if status == 401 || status == 403 {
				want = "auth_failed"
			}
			for _, r := range data.Accounts {
				if r.AvailableCount != nil || r.Errors[0].Code != want {
					t.Fatalf("row=%+v", r)
				}
			}
		})
	}
}
func TestCLIV2ResetInspectionDTOAndIncompleteSchedule(t *testing.T) {
	t.Setenv("CQ_TTL", "999h")
	a, b := v2ResetTestApp()
	b.failures["key-b"] = &codexprov.ResetCreditInventoryError{Code: "invalid_credit_entries"}
	b.inventories["key-b"] = codexprov.ResetCreditInventory{EntryErrors: []codexprov.ResetCreditEntryError{{Index: 1, Code: "missing_status"}}}
	inv := v2ResetInvocation("codex reset list")
	out := handleV2ResetInspectionWithApp(context.Background(), inv, nil, a)
	var data struct {
		Accounts []ResetAccountInventory
		Complete bool
	}
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Complete || data.Accounts[0].AccountReference == nil || *data.Accounts[0].AccountReference != "key-a" || len(data.Accounts[1].Errors) != 1 || *data.Accounts[1].Errors[0].EntryIndex != 1 {
		t.Fatalf("data=%s", out.Data)
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(out.Data, &raw)
	var rows []map[string]json.RawMessage
	json.Unmarshal(raw["accounts"], &rows)
	if len(rows[0]) != 6 {
		t.Fatalf("wrong inventory keys: %s", out.Data)
	}
	var credits []map[string]json.RawMessage
	json.Unmarshal(rows[0]["credits"], &credits)
	if len(credits[0]) != 8 || string(credits[0]["description"]) != "null" {
		t.Fatalf("wrong credit schema: %s", out.Data)
	}
	out = handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset recommend"), nil, a)
	var recommendation struct{ Schedule ResetSchedule }
	json.Unmarshal(out.Data, &recommendation)
	schedule := recommendation.Schedule
	if out.ExitCode != 8 || schedule.Complete || len(schedule.Blockers) != 1 || schedule.Blockers[0].Code != "inventory_invalid" || *schedule.Blockers[0].AccountReference != "key-b" {
		t.Fatalf("incomplete=%s errors=%+v", out.Data, out.Errors)
	}
	for _, item := range schedule.Items {
		if item.UseAt != nil || item.AccountReference != "key-a" || item.UseBy == nil {
			t.Fatalf("incomplete item=%+v", item)
		}
	}
}
func TestCLIV2ResetInspectionScheduleProjection(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	expiry := now.Add(time.Hour)
	source := aggregate.ResetSchedule{GeneratedAt: now, Complete: true, Exact: false, Confidence: aggregate.ResetConfidenceHigh, Items: []aggregate.ResetScheduleItem{{AccountReference: "b", CreditID: "b", Status: aggregate.ResetDeferred, Confidence: aggregate.ResetConfidenceHigh}, {AccountReference: "a", CreditID: "a", UseAt: now, CreditExpiresAt: &expiry, Status: aggregate.ResetScheduled, Confidence: aggregate.ResetConfidenceHigh, RestoredPct: map[quota.WindowName]float64{quota.Window7Day: 30, quota.Window5Hour: 50}, ReasonCodes: []aggregate.ResetScheduleReason{aggregate.ResetReasonRateFallback, aggregate.ResetReasonGapAvoidance, aggregate.ResetReasonGapAvoidance}}}}
	dto := v2ResetSchedule(source)
	if !dto.Complete || dto.Exact || dto.Items[0].AccountReference != "a" || len(dto.Items[0].ReasonCodes) != 2 || dto.Items[0].ReasonCodes[0] != aggregate.ResetReasonGapAvoidance || dto.Items[0].RestoredPct[0].Name != quota.Window5Hour || dto.GeneratedAt.Location() != time.UTC || dto.Items[1].UseBy != nil {
		t.Fatalf("dto=%+v", dto)
	}
}
func TestCLIV2ResetInspectionHumanAndParser(t *testing.T) {
	a, b := v2ResetTestApp()
	b.snapshot.Accounts = b.snapshot.Accounts[:1]
	credit := b.inventories["key-a"]
	credit.Credits[0].ID = "id\x1b\n"
	credit.Credits[0].ExpiresAt = nil
	b.inventories["key-a"] = credit
	out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset list"), nil, a)
	if out.Human != "Reset inventories: 1; complete=true.\nkey-a\tid\\u001b\\u000a\tavailable\tnever\n" {
		t.Fatalf("human=%q", out.Human)
	}
	for _, args := range [][]string{{"codex", "reset", "list", "--timeout", "1s", "--timeout", "2s"}, {"codex", "reset", "recommend", "key-a"}, {"codex", "reset", "list", "--credit", "c"}, {"codex", "reset", "list", "--timeout", "999ms"}, {"codex", "reset", "recommend", "--timeout", "11m"}, {"codex", "reset", "list", " "}} {
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) { t.Fatal("invalid syntax reached dependencies"); return nil, false })
		if exit != 2 {
			t.Fatalf("args=%v exit=%d stderr=%s", args, exit, stderr.String())
		}
	}
	for _, path := range []string{"list", "recommend"} {
		var stdout, stderr bytes.Buffer
		exit := cli.Run(context.Background(), []string{"codex", "reset", path, "--help"}, &cli.Session{Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) { t.Fatal("help reached dependencies"); return nil, false })
		want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "help", "codex-reset-"+path+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if exit != 0 || stdout.String() != string(want) {
			t.Fatal("exact help mismatch")
		}
	}
}

func TestCLIV2ResetInspectionDeadlines(t *testing.T) {
	for _, stage := range []string{"inventory", "credits", "usage", "history", "factory", "close"} {
		t.Run(stage, func(t *testing.T) {
			a, b := v2ResetTestApp()
			release := make(chan struct{})
			finished := make(chan struct{})
			started := make(chan struct{})
			block := func() { close(started); <-release; close(finished) }
			path := "codex reset list"
			factory := func(context.Context, bool) (v2ResetDependencies, error) { return v2ResetDependencies{app: a}, nil }
			switch stage {
			case "inventory":
				b.snapshotHook = block
			case "credits":
				b.creditHook = func(account codexprov.ResetAccount) {
					if account.AccountKey == "key-b" {
						block()
					}
				}
			case "usage":
				path = "codex reset recommend"
				u := a.Usage.(v2ResetUsage)
				u.hook = block
				a.Usage = u
			case "history":
				path = "codex reset recommend"
				a.History = v2ResetHistory{f: b.f, hook: block}
			case "factory":
				factory = func(context.Context, bool) (v2ResetDependencies, error) {
					block()
					return v2ResetDependencies{app: a}, nil
				}
			case "close":
				factory = func(context.Context, bool) (v2ResetDependencies, error) {
					return v2ResetDependencies{app: a, close: func() error { block(); return nil }}, nil
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			done := make(chan cli.Outcome, 1)
			go func() { done <- handleV2ResetInspectionWithFactory(ctx, v2ResetInvocation(path), nil, factory) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("blocked dependency not entered")
			}
			var out cli.Outcome
			select {
			case out = <-done:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("total deadline did not bound dependency")
			}
			close(release)
			<-finished
			if out.ExitCode != 7 || out.Errors[0].Code != "codex_reset_timeout" {
				t.Fatalf("outcome=%+v", out)
			}
			if stage == "credits" {
				var data struct{ Accounts []ResetAccountInventory }
				json.Unmarshal(out.Data, &data)
				if len(data.Accounts) != 2 || len(data.Accounts[0].Credits) != 1 || data.Accounts[1].AvailableCount != nil {
					t.Fatalf("deadline lost completed row: %s", out.Data)
				}
			}
		})
	}
	t.Run("interruption precedence", func(t *testing.T) {
		a, _ := v2ResetTestApp()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out := handleV2ResetInspectionWithApp(ctx, v2ResetInvocation("codex reset list"), nil, a)
		if out.ExitCode != 130 {
			t.Fatalf("outcome=%+v", out)
		}
	})
	t.Run("late dependencies close", func(t *testing.T) {
		release := make(chan struct{})
		closed := make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		factory := func(context.Context, bool) (v2ResetDependencies, error) {
			<-release
			return v2ResetDependencies{close: func() error { close(closed); return nil }}, nil
		}
		out := handleV2ResetInspectionWithFactory(ctx, v2ResetInvocation("codex reset list"), nil, factory)
		close(release)
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("late dependency leaked")
		}
		if out.ExitCode != 7 {
			t.Fatal(out)
		}
	})
}
func TestCLIV2ResetInspectionNeverBootstrapsAttempts(t *testing.T) {
	for _, recommend := range []bool{false, true} {
		t.Run(fmt.Sprint(recommend), func(t *testing.T) {
			tempRoot, err := filepath.EvalSymlinks("/tmp")
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.MkdirTemp(tempRoot, "cqri-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "state"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
			t.Setenv("CQ_TTL", "invalid-unused")
			deps, err := newV2ResetDependencies(context.Background(), recommend)
			if err != nil {
				t.Fatal(err)
			}
			defer deps.close()
			if deps.app.Attempts != nil || deps.app.Cache != nil {
				t.Fatal("inspection received consume/cache authority")
			}
			if (deps.app.History != nil) != recommend || (deps.app.Usage != nil) != recommend {
				t.Fatal("unneeded inspection dependencies")
			}
			err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if strings.Contains(path, "reset-attempts") {
					t.Error("inspection bootstrapped consumption attempts")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCLIV2ResetInspectionEligibleCreditsAndFreshReads(t *testing.T) {
	a, b := v2ResetTestApp()
	now := a.Clock.Now()
	past := now.Add(-time.Second)
	credits := []codexprov.ResetCredit{{ID: "expired", ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditAvailable, GrantedAt: past, ExpiresAt: &now}, {ID: "redeemed", ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditRedeemed, GrantedAt: past}, {ID: "future", ResetType: "future", Status: codexprov.ResetCreditAvailable, GrantedAt: past}, {ID: "no-expiry", ResetType: codexprov.ResetTypeCodexRateLimits, Status: codexprov.ResetCreditAvailable, GrantedAt: past}}
	b.inventories["key-a"] = codexprov.ResetCreditInventory{Credits: credits, AvailableCount: 3}
	inv := v2ResetInvocation("codex reset list")
	inv.Arguments["account"] = []string{"key-a"}
	for range 2 {
		out := handleV2ResetInspectionWithApp(context.Background(), inv, nil, a)
		if out.ExitCode != 0 || !bytes.Contains(out.Data, []byte(`"supported":false`)) || !bytes.Contains(out.Data, []byte(`"available_count":3`)) {
			t.Fatalf("outcome=%+v", out)
		}
	}
	if b.f.calls["credits:key-a"] != 2 {
		t.Fatal("reused reset inventory")
	}
	out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset recommend"), nil, a)
	var data struct{ Schedule ResetSchedule }
	json.Unmarshal(out.Data, &data)
	if out.ExitCode != 0 {
		t.Fatalf("outcome=%+v", out)
	}
	for _, item := range data.Schedule.Items {
		if (item.CreditID == "expired" || item.CreditID == "redeemed" || item.CreditID == "future") && item.UseAt != nil {
			t.Fatalf("ineligible credit scheduled: %+v", item)
		}
	}
}
func TestCLIV2ResetInspectionNoLateOutput(t *testing.T) {
	a, b := v2ResetTestApp()
	release := make(chan struct{})
	finished := make(chan struct{})
	b.creditHook = func(account codexprov.ResetAccount) {
		if account.AccountKey == "key-b" {
			<-release
			close(finished)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	exit := cli.Run(ctx, []string{"codex", "reset", "list", "--json"}, &cli.Session{Out: &stdout, Err: &stderr}, func(string) (cli.Handler, bool) {
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ResetInspectionWithApp(ctx, inv, s, a)
		}, true
	})
	before := stdout.String() + stderr.String()
	close(release)
	<-finished
	time.Sleep(10 * time.Millisecond)
	if exit != 7 || before != stdout.String()+stderr.String() {
		t.Fatal("late output or wrong timeout")
	}
	var envelope map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(stdout.String()))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if decoder.More() {
		t.Fatal("multiple envelopes")
	}
}
func TestCLIV2ResetInspectionDependencyErrorCleanup(t *testing.T) {
	var closed atomic.Int32
	factory := func(context.Context, bool) (v2ResetDependencies, error) {
		return v2ResetDependencies{close: func() error { closed.Add(1); return nil }}, errors.New("private-secret")
	}
	out := handleV2ResetInspectionWithFactory(context.Background(), v2ResetInvocation("codex reset list"), nil, factory)
	until := time.Now().Add(time.Second)
	for closed.Load() == 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if out.ExitCode != 4 || closed.Load() != 1 || strings.Contains(fmt.Sprint(out), "private-secret") {
		t.Fatalf("outcome=%+v closed=%d", out, closed.Load())
	}
}

func TestCLIV2ResetInspectionRejectsIncompleteInventory(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			f := &v2Fixture{}
			inventory := codexprov.Inventory{Accounts: []codexprov.LogicalAccount{{Key: "key", Identity: codexprov.AccountIdentity{AccountID: "id", Email: "email@test"}, Candidates: []codexprov.CredentialCandidate{{Ref: codexprov.CandidateRef{AccountKey: "key", CandidateID: "candidate"}, Revision: "revision", Source: codexprov.SourceSystem}}}}}
			if blocked {
				inventory.Accounts[0].Candidates[0].DispatchBlocked = true
			} else {
				inventory.ExternalSources = []codexprov.ExternalSourceStatus{{ErrorCode: "unavailable"}}
			}
			backend := &codexprov.ResetBackend{Inventory: v2ResetInventory{v2InspectionInventory{inventory: inventory, fixture: f}}}
			a := &app.CodexResetApp{Backend: backend}
			inv := v2ResetInvocation("codex reset list")
			inv.Arguments["account"] = []string{"key"}
			out := handleV2ResetInspectionWithApp(context.Background(), inv, nil, a)
			if out.ExitCode != 4 || out.Errors[0].Code != "codex_reset_credentials_unavailable" || out.Data != nil || f.calls["codex-inventory"] != 1 {
				t.Fatalf("outcome=%+v calls=%+v", out, f.calls)
			}
		})
	}
}
func TestCLIV2ResetInspectionMissingUsageBlocksWholePortfolio(t *testing.T) {
	for _, which := range []string{"missing", "ambiguous", "invalid-windows", "failure"} {
		t.Run(which, func(t *testing.T) {
			a, _ := v2ResetTestApp()
			u := a.Usage.(v2ResetUsage)
			want := "usage_account_missing"
			switch which {
			case "missing":
				u.results = u.results[:1]
			case "ambiguous":
				u.results = append(u.results, u.results[1])
				want = "usage_account_ambiguous"
			case "invalid-windows":
				delete(u.results[1].Windows, quota.Window7Day)
				want = "usage_windows_invalid"
			case "failure":
				u.err = errors.New("private-error")
				want = "usage_unavailable"
			}
			a.Usage = u
			out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset recommend"), nil, a)
			var data struct{ Schedule ResetSchedule }
			json.Unmarshal(out.Data, &data)
			if out.ExitCode != 8 || data.Schedule.Complete || len(data.Schedule.Blockers) != 1 || data.Schedule.Blockers[0].Code != want {
				t.Fatalf("outcome=%+v data=%s", out, out.Data)
			}
			for _, item := range data.Schedule.Items {
				if item.UseAt != nil {
					t.Fatal("incomplete schedule actionable")
				}
			}
		})
	}
}

func TestCLIV2ResetInspectionUnselectableReference(t *testing.T) {
	a, b := v2ResetTestApp()
	b.snapshot.Accounts[0].Unselectable = true
	out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset list"), nil, a)
	var data struct{ Accounts []ResetAccountInventory }
	json.Unmarshal(out.Data, &data)
	for _, row := range data.Accounts {
		if row.AccountID != nil && *row.AccountID == "id-a" && row.AccountReference != nil {
			t.Fatalf("unselectable reference exposed: %s", out.Data)
		}
	}
	if !strings.Contains(out.Human, "—\tcredit-a") {
		t.Fatalf("human=%s", out.Human)
	}
}

func TestCLIV2ResetInspectionAliasesAndDefaultBudgets(t *testing.T) {
	for _, leaf := range []string{"list", "recommend"} {
		runV2Case(t, v2Case{Name: "plural " + leaf, Scenario: "reset-recommend-boundaries", Args: []string{"codex", "resets", leaf, "--json"}, Command: "codex reset " + leaf, Forbid: []string{"consume", "service", "credential-activate"}})
		t.Run(leaf, func(t *testing.T) {
			a, _ := v2ResetTestApp()
			var stdout, stderr bytes.Buffer
			want := 60 * time.Second
			if leaf == "recommend" {
				want = 120 * time.Second
			}
			exit := cli.Run(context.Background(), []string{"codex", "reset", leaf, "--json"}, &cli.Session{Out: &stdout, Err: &stderr}, func(path string) (cli.Handler, bool) {
				return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
					return handleV2ResetInspectionWithFactory(ctx, inv, s, func(ctx context.Context, _ bool) (v2ResetDependencies, error) {
						deadline, ok := ctx.Deadline()
						remaining := time.Until(deadline)
						if !ok || remaining > want || remaining < want-time.Second {
							t.Errorf("budget=%s expected=%s", remaining, want)
						}
						return v2ResetDependencies{app: a}, nil
					})
				}, true
			})
			if exit != 0 {
				t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
			}
		})
	}
}
func TestCLIV2ResetInspectionUnstableSelectionBeforeHTTP(t *testing.T) {
	a, b := v2ResetTestApp()
	b.snapshot.Inventory.Accounts[0].Unstable = true
	inv := v2ResetInvocation("codex reset list")
	inv.Arguments["account"] = []string{"key-a"}
	out := handleV2ResetInspectionWithApp(context.Background(), inv, nil, a)
	if out.ExitCode != 6 || out.Errors[0].Code != "account_unstable" || out.Data != nil || b.f.calls["credits:key-a"] != 0 {
		t.Fatalf("outcome=%+v", out)
	}
}
func TestCLIV2ResetInspectionExpiryJustAfterSnapshot(t *testing.T) {
	a, b := v2ResetTestApp()
	expiry := a.Clock.Now().Add(time.Second)
	inventory := b.inventories["key-a"]
	inventory.Credits[0].ExpiresAt = &expiry
	b.inventories["key-a"] = inventory
	out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset recommend"), nil, a)
	var data struct{ Schedule ResetSchedule }
	json.Unmarshal(out.Data, &data)
	found := false
	for _, item := range data.Schedule.Items {
		if item.CreditID == "credit-a" {
			found = true
			if item.UseBy == nil || !item.UseBy.Equal(expiry) {
				t.Fatalf("lost eligible expiry: %+v", item)
			}
		}
	}
	if out.ExitCode != 0 || !found {
		t.Fatalf("eligible credit vanished: %s", out.Data)
	}
}

func TestCLIV2ResetInspectionCleanupPanicIsSafe(t *testing.T) {
	a, _ := v2ResetTestApp()
	out := handleV2ResetInspectionWithFactory(context.Background(), v2ResetInvocation("codex reset list"), nil, func(context.Context, bool) (v2ResetDependencies, error) {
		return v2ResetDependencies{app: a, close: func() error { panic("private-secret") }}, nil
	})
	if out.ExitCode != 4 || strings.Contains(fmt.Sprint(out), "private-secret") || out.Data == nil {
		t.Fatalf("cleanup discarded result or leaked details: %+v", out)
	}
}
