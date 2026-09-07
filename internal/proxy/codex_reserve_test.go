package proxy

import (
	"context"
	"errors"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
	"testing"
	"time"
)

type reserveInventory struct {
	active codex.AccountKey
	err    error
	calls  int
}

func (i *reserveInventory) List(context.Context) (codex.Inventory, error) {
	i.calls++
	if i.err != nil {
		return codex.Inventory{}, i.err
	}
	return codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "system", Active: i.active == "system"}, {Key: "other", Active: i.active == "other"}}}, nil
}
func TestCodexReserveAccountThresholdAndReset(t *testing.T) {
	now := time.Unix(1800000000, 0)
	fs := fsutil.NewMemFS()
	inventory := &reserveInventory{active: "system"}
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Minute)
	reserve, err := OpenCodexReserve(fs, "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	observe := func(account codex.AccountKey, remaining float64, reset int64) {
		ledger.ObserveQuotaSnapshot(account, QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: int(remaining), RemainingPctExact: &remaining, ResetAtUnix: reset}}}})
	}
	reset := now.Add(7 * 24 * time.Hour).Unix()
	observe("system", 2.1, reset)
	observe("other", 1, reset)
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	if ledger.Capacity("system", CapacityBucketBase).State != CapacityPositive {
		t.Fatal("2.1% must remain available")
	}
	now = now.Add(time.Second)
	observe("system", 2, reset)
	if ledger.Capacity("system", CapacityBucketBase).State != CapacityZero {
		t.Fatal("system reserve not enforced")
	}
	if ledger.Capacity("other", CapacityBucketBase).State != CapacityPositive {
		t.Fatal("other account affected")
	}
	if _, err = reserve.Control("disable", "", 0); err != nil {
		t.Fatal(err)
	}
	if ledger.Capacity("system", CapacityBucketBase).State != CapacityPositive {
		t.Fatal("disabled reserve still blocked")
	}
	restarted, err := OpenCodexReserve(fs, "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = restarted
	if restarted.Status().Enabled {
		t.Fatal("bypass lost on restart")
	}
	now = now.Add(time.Second)
	observe("system", 100, now.Add(7*24*time.Hour).Unix())
	if !restarted.Status().Enabled {
		t.Fatal("forced reset did not rearm")
	}
	now = now.Add(time.Second)
	observe("system", 1, reset)
	inventory.active = "other"
	if ledger.Capacity("system", CapacityBucketBase).State != CapacityPositive {
		t.Fatal("old system account still reserved")
	}
	if ledger.Capacity("other", CapacityBucketBase).State != CapacityZero {
		t.Fatal("new system account not reserved")
	}
}

func TestCodexReserveStaleWindowAndOrdering(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 50, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(71 * time.Second)
	if ledger.Capacity("system", CapacityBucketBase).State != CapacityZero {
		t.Fatal("stale reserve admitted system account")
	}
	if _, err = reserve.Control("disable", "", 0); err == nil {
		t.Fatal("disabled without fresh reset evidence")
	}
}

func TestCodexReserveNormalisesWindowsAndKeepsIdentityOnError(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	inventory := &reserveInventory{active: "system"}
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d:GPT-5.3-Codex-Spark": {RemainingPct: 2, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	if _, err = reserve.Control("set", "7d:gpt-5.3-codex-spark", 2); err != nil {
		t.Fatal(err)
	}
	inventory.err = errors.New("inventory offline")
	if blocked, _ := reserve.Blocked("system"); !blocked {
		t.Fatal("identity read failure bypassed reserve")
	}
	if blocked, _ := reserve.Blocked("other"); blocked {
		t.Fatal("identity failure blocked unrelated account")
	}
}
func TestCodexReserveRejectsOlderConnectionWindow(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	fact := func(source CapacitySource, generation uint64, remaining int, at time.Time) CapacityFact {
		return CapacityFact{AccountKey: "system", Bucket: CapacityBucketBase, RemainingPct: remaining, Source: source, Sequence: 1, ConnectionGeneration: generation, ObservedAt: at, Confidence: CapacityConfidenceAuthoritative, Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: remaining}}}
	}
	ledger.Observe(fact(CapacitySourceLiveRateLimits, 2, 2, now))
	ledger.Observe(fact(CapacitySourceHTTPHeaders, 1, 80, now.Add(time.Second)))
	windows, _ := ledger.WindowSnapshot("system")
	if windows["7d"].RemainingPct != 2 {
		t.Fatal("older connection overwrote newer window")
	}
}

func TestCodexReservePoolRoutesOtherAccountUntilExhausted(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reserve = reserve
	observe := func(account codex.AccountKey, remaining int) {
		ledger.ObserveQuotaSnapshot(account, QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: remaining, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	}
	observe("system", 2)
	observe("other", 1)
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{{AccountKey: "system", AccessToken: "fake-system", IsActive: true}, {AccountKey: "other", AccessToken: "fake-other"}}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	if err != nil || choice.AccountKey != "other" {
		t.Fatalf("choice=%+v error=%v", choice, err)
	}
	now = now.Add(time.Second)
	observe("other", 0)
	ledger.Observe(CapacityFact{AccountKey: "other", Bucket: CapacityBucketBase, Source: CapacitySourceHardLimit, Sequence: 1, ConnectionGeneration: 1, RemainingPct: 0, ObservedAt: now, ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative})
	_, err = selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	var limit *CachedUsageLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("exhausted pool error=%v", err)
	}
	if _, err = reserve.Control("disable", "", 0); err != nil {
		t.Fatal(err)
	}
	choice, err = selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	if err != nil || choice.AccountKey != "system" {
		t.Fatalf("unlocked choice=%+v error=%v", choice, err)
	}
}
func TestCodexReserveNaturalResetNeedsNewObservation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(time.Second).Unix()
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 2, ResetAtUnix: reset}}}})
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = reserve.Control("disable", "", 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if reserve.Status().Enabled {
		t.Fatal("clock alone rearmed bypass")
	}
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 100, ResetAtUnix: now.Add(7 * 24 * time.Hour).Unix()}}}})
	if !reserve.Status().Enabled {
		t.Fatal("fresh natural reset did not rearm")
	}
}
func TestCodexReserveSelectedWindowIndependentOfOtherWindows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"5h": {RemainingPct: 1, ResetAtUnix: now.Add(time.Hour).Unix()}, "7d": {RemainingPct: 10, ResetAtUnix: now.Add(7 * 24 * time.Hour).Unix()}}}})
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	if reserve.Status().Blocked {
		t.Fatal("unselected 5h window triggered 7d reserve")
	}
}

func TestCodexReserveMissingIdentitySurvivesRestart(t *testing.T) {
	now := time.Unix(1800000000, 0)
	fs := fsutil.NewMemFS()
	inventory := &reserveInventory{active: "system"}
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fs, "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: 1, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	inventory.active = ""
	if blocked, _ := reserve.Blocked("system"); !blocked {
		t.Fatal("missing active identity bypassed reserve")
	}
	restarted, err := OpenCodexReserve(fs, "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if blocked, _ := restarted.Blocked("system"); !blocked {
		t.Fatal("restart lost protected identity")
	}
	if blocked, _ := restarted.Blocked("other"); blocked {
		t.Fatal("restart blocked unrelated account")
	}
	inventory.active = "other"
	if blocked, _ := restarted.Blocked("system"); blocked {
		t.Fatal("confirmed account switch retained old target")
	}
}

func TestCodexReserveCadenceDoesNotReadInventory(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	inventory := &reserveInventory{active: "system"}
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	windows := map[quota.WindowName]quota.Window{"7d": {RemainingPct: 3, ResetAtUnix: now.Add(time.Hour).Unix()}}
	ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: windows}})
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	before := inventory.calls
	if got := reserve.RefreshInterval("system", windows); got != 5*time.Second {
		t.Fatalf("cadence=%v", got)
	}
	if inventory.calls != before {
		t.Fatal("cadence callback performed inventory I/O")
	}
}

func TestCodexReserveServiceObservationRearmsWithoutRequests(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	inventory := &reserveInventory{active: "system"}
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, inventory, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observe := func(remaining int) {
		ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: remaining, ResetAtUnix: now.Add(time.Hour).Unix()}}}})
	}
	observe(2)
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = reserve.Control("disable", "", 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	observe(100)
	reserve.ObserveInventory(codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "system", Active: true}}})
	if reserve.document.Bypass != nil {
		t.Fatal("service inventory pass did not rearm reset")
	}
	reserve.ObserveInventory(codex.Inventory{Accounts: []codex.LogicalAccount{{Key: "other", Active: true}}})
	if reserve.document.LastSystemAccount != "other" {
		t.Fatal("service inventory pass did not follow system switch")
	}
}

func TestCodexReserveDoesNotTreatSmallCorrectionAsReset(t *testing.T) {
	now := time.Unix(1800000000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	reserve, err := OpenCodexReserve(fsutil.NewMemFS(), "/state/reserve.json", ledger, &reserveInventory{active: "system"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(time.Hour).Unix()
	observe := func(remaining float64, reset int64) {
		ledger.ObserveQuotaSnapshot("system", QuotaSnapshot{FetchedAt: now, Result: quota.Result{Windows: map[quota.WindowName]quota.Window{"7d": {RemainingPct: int(remaining), RemainingPctExact: &remaining, ResetAtUnix: reset}}}})
	}
	observe(2, reset)
	if _, err = reserve.Control("set", "7d", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = reserve.Control("disable", "", 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	observe(2.1, reset+1)
	if reserve.Status().Enabled {
		t.Fatal("quota correction and timestamp jitter mistaken for reset")
	}
}
