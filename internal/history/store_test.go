package history

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/quota"
)

const testDir = "/cache/cq"

// newTestStore returns a fresh Store backed by a MemFS.
func newTestStore(t *testing.T) (*Store, *fsutil.MemFS) {
	t.Helper()
	fs := fsutil.NewMemFS()
	s, err := New(fs, testDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, fs
}

func makeResult(accountID string, windows map[quota.WindowName]quota.Window) quota.Result {
	return quota.Result{
		AccountID: accountID,
		Status:    quota.StatusOK,
		Windows:   windows,
	}
}

func TestStoreFirstObservationNoRate(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)

	results := []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 9_000},
		}),
	}
	rates, err := s.UpdateAndGetBurnRates(ctx, results, now)
	if err != nil {
		t.Fatalf("UpdateAndGetBurnRates: %v", err)
	}
	if len(rates) != 0 {
		t.Errorf("first observation produced rates = %v, want empty", rates)
	}

	// Verify the state was persisted with Samples=1.
	state, err := s.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	acct := state.Accounts["acct1"]
	if acct == nil {
		t.Fatal("expected acct1 entry in state")
	}
	w := acct.Windows["5h"]
	if w == nil {
		t.Fatal("expected 5h window state")
	}
	if w.Samples != 1 {
		t.Errorf("Samples = %d, want 1", w.Samples)
	}
	if w.LastRemainingPct != 80 {
		t.Errorf("LastRemainingPct = %d, want 80", w.LastRemainingPct)
	}
}

func TestStoreSecondObservationEWMAUpdate(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)
	reset := now + 9_000

	// First observation: seed.
	_, err := s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: reset},
		}),
	}, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Second observation 60s later: consumed 5 percentage points.
	dt := int64(60)
	rates, err := s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 95, ResetAtUnix: reset},
		}),
	}, now+dt)
	if err != nil {
		t.Fatalf("second obs: %v", err)
	}

	// α = 1 - 2^(-60/1800) ≈ 0.02284
	// ewma = α * (5/60) + (1-α)*0 = α * 0.08333 ≈ 0.001903
	key := BurnRateKey{AccountKey: "acct1", Window: "5h"}
	got, ok := rates.Get(key)
	if !ok {
		t.Fatal("expected rate after second observation")
	}
	alpha := 1.0 - math.Pow(2, -float64(dt)/halfLifeFor(quota.Window5Hour))
	want := alpha * (5.0 / 60.0)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ewma = %.9f, want %.9f", got, want)
	}
}

func TestStoreResetUnwrap(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	period := int64(quota.PeriodFor(quota.Window5Hour).Seconds())
	now := int64(1_000_000)
	reset := now + 3_600 // reset in 1h

	// Seed at 100%.
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: reset},
		}),
	}, now)

	// 60s later: used 10%.
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 90, ResetAtUnix: reset},
		}),
	}, now+60)

	// 3700s later (past the reset): fresh window, at 95%. Reset advanced by one period.
	newNow := now + 60 + 3700
	newReset := reset + period
	rates, err := s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 95, ResetAtUnix: newReset},
		}),
	}, newNow)
	if err != nil {
		t.Fatalf("reset unwrap: %v", err)
	}

	// Delta = 90 (prev) + 100 (one reset) - 95 = 95 percentage points.
	key := BurnRateKey{AccountKey: "acct1", Window: "5h"}
	got, ok := rates.Get(key)
	if !ok {
		t.Fatal("expected rate after reset unwrap")
	}
	if got <= 0 {
		t.Errorf("ewma after reset = %.6f, want positive", got)
	}
}

func TestStoreResetAmbiguousReseed(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)
	reset := now + 3_600

	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: reset},
		}),
	}, now)

	// Reset moved by a non-clean multiple (1 hour ≠ period).
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 60, ResetAtUnix: reset + 3_600},
		}),
	}, now+60)

	state, _ := s.load()
	w := state.Accounts["acct1"].Windows["5h"]
	if w.EWMARatePctPerS != 0 {
		t.Errorf("ambiguous reset produced EWMA = %v, want 0 (reseed)", w.EWMARatePctPerS)
	}
	if w.Samples != 1 {
		t.Errorf("Samples = %d, want 1 (reseed resets sample count)", w.Samples)
	}
}

func TestStoreExhaustionCensoring(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)
	reset := now + 9_000

	// Seed at 10%, then burn down to 0, then stay at 0.
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 10, ResetAtUnix: reset},
		}),
	}, now)
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: reset},
		}),
	}, now+60)

	// Capture EWMA after the 10→0 delta.
	state, _ := s.load()
	ewmaBefore := state.Accounts["acct1"].Windows["5h"].EWMARatePctPerS
	if ewmaBefore <= 0 {
		t.Fatalf("expected positive EWMA before censoring, got %v", ewmaBefore)
	}

	// Now sit at 0 for another observation — should freeze EWMA.
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 0, ResetAtUnix: reset},
		}),
	}, now+120)

	state, _ = s.load()
	ewmaAfter := state.Accounts["acct1"].Windows["5h"].EWMARatePctPerS
	if ewmaAfter != ewmaBefore {
		t.Errorf("EWMA changed during exhaustion censoring: before=%v after=%v", ewmaBefore, ewmaAfter)
	}
}

func TestStoreSkipsBackfilledResults(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)

	// CacheAge > 0 signals backfill from stale cache — should be skipped.
	results := []quota.Result{
		{
			AccountID: "acct1",
			Status:    quota.StatusOK,
			CacheAge:  3600,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
			},
		},
	}
	_, err := s.UpdateAndGetBurnRates(ctx, results, now)
	if err != nil {
		t.Fatalf("UpdateAndGetBurnRates: %v", err)
	}

	state, _ := s.load()
	if _, ok := state.Accounts["acct1"]; ok {
		t.Error("backfilled result should not create state entry")
	}
}

func TestStoreAnonymousSkipped(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)

	results := []quota.Result{
		{
			Status: quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: 50, ResetAtUnix: now + 9_000},
			},
		},
	}
	_, err := s.UpdateAndGetBurnRates(ctx, results, now)
	if err != nil {
		t.Fatalf("UpdateAndGetBurnRates: %v", err)
	}
	state, _ := s.load()
	if len(state.Accounts) != 0 {
		t.Errorf("anonymous result produced %d accounts, want 0", len(state.Accounts))
	}
}

func TestStoreCorruptFileRecovery(t *testing.T) {
	fs := fsutil.NewMemFS()
	// Pre-populate the store path with garbage.
	_ = fs.WriteFile(filepath.Join(testDir, stateFileName), []byte("{not json"), 0o600)

	s, err := New(fs, testDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	now := int64(1_000_000)

	// Should not fail — corrupt load degrades to cold start.
	_, err = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 9_000},
		}),
	}, now)
	if err != nil {
		t.Fatalf("expected success after corrupt load, got %v", err)
	}

	// Verify the file is now valid JSON.
	data, _ := fs.ReadFile(filepath.Join(testDir, stateFileName))
	var state BurnState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Errorf("state file still corrupt after recovery: %v", err)
	}
	if state.Version != schemaVersion {
		t.Errorf("Version = %d, want %d", state.Version, schemaVersion)
	}
}

func TestStoreSchemaVersionMismatch(t *testing.T) {
	fs := fsutil.NewMemFS()
	bogus := BurnState{
		Version: 99,
		Accounts: map[string]*AccountState{
			"ghost": {Windows: map[string]*WindowState{
				"5h": {EWMARatePctPerS: 42, Samples: 5},
			}},
		},
	}
	data, _ := json.Marshal(bogus)
	_ = fs.WriteFile(filepath.Join(testDir, stateFileName), data, 0o600)

	s, err := New(fs, testDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state, err := s.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.Accounts) != 0 {
		t.Errorf("version mismatch should produce empty state, got %d accounts", len(state.Accounts))
	}
}

func TestStoreAtomicWrite(t *testing.T) {
	s, fs := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)

	_, err := s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: now + 9_000},
		}),
	}, now)
	if err != nil {
		t.Fatalf("UpdateAndGetBurnRates: %v", err)
	}

	// Verify the final file exists and no .tmp leftover.
	if _, err := fs.Stat(filepath.Join(testDir, stateFileName)); err != nil {
		t.Errorf("expected state file: %v", err)
	}
	if _, err := fs.Stat(filepath.Join(testDir, stateFileName+".tmp")); err == nil {
		t.Error("leftover .tmp file after atomic write")
	}
}

func TestStoreHalfLifeTable(t *testing.T) {
	if halfLifeFor(quota.Window5Hour) != 1800 {
		t.Errorf("5h half-life = %v, want 1800", halfLifeFor(quota.Window5Hour))
	}
	if halfLifeFor(quota.Window7Day) != 21600 {
		t.Errorf("7d half-life = %v, want 21600", halfLifeFor(quota.Window7Day))
	}
	if halfLifeFor(quota.WindowName("5h:gpt-5.3-codex-spark")) != 1800 {
		t.Errorf("5h:gpt-5.3-codex-spark half-life = %v, want 1800", halfLifeFor(quota.WindowName("5h:gpt-5.3-codex-spark")))
	}
	if halfLifeFor(quota.WindowName("7d:sonnet")) != 21600 {
		t.Errorf("7d:sonnet half-life = %v, want 21600", halfLifeFor(quota.WindowName("7d:sonnet")))
	}
}

func TestStoreClockBackward(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)
	reset := now + 9_000

	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: reset},
		}),
	}, now)

	stateBefore, _ := s.load()
	samplesBefore := stateBefore.Accounts["acct1"].Windows["5h"].Samples

	// Call again with an earlier now — Δt ≤ 0.
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 90, ResetAtUnix: reset},
		}),
	}, now-10)

	stateAfter, _ := s.load()
	samplesAfter := stateAfter.Accounts["acct1"].Windows["5h"].Samples
	if samplesAfter != samplesBefore {
		t.Errorf("Samples changed on clock-backward: before=%d after=%d", samplesBefore, samplesAfter)
	}
}

func TestStoreCoalescesMultipleWindows(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)
	r5 := now + 9_000
	r7 := now + 302_400

	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 100, ResetAtUnix: r5},
			quota.Window7Day:  {RemainingPct: 100, ResetAtUnix: r7},
		}),
	}, now)

	dt := int64(60)
	rates, _ := s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 95, ResetAtUnix: r5},
			quota.Window7Day:  {RemainingPct: 99, ResetAtUnix: r7},
		}),
	}, now+dt)

	r5Key := BurnRateKey{AccountKey: "acct1", Window: "5h"}
	r7Key := BurnRateKey{AccountKey: "acct1", Window: "7d"}
	if _, ok := rates.Get(r5Key); !ok {
		t.Error("missing 5h rate")
	}
	if _, ok := rates.Get(r7Key); !ok {
		t.Error("missing 7d rate")
	}

	// Rates should differ because half-lives differ.
	alpha5 := 1.0 - math.Pow(2, -float64(dt)/halfLifeFor(quota.Window5Hour))
	alpha7 := 1.0 - math.Pow(2, -float64(dt)/halfLifeFor(quota.Window7Day))
	if alpha5 == alpha7 {
		t.Fatal("expected different α for the two windows")
	}
}

func TestStoreUnknownAccountAbsentFromRates(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := int64(1_000_000)

	rates, _ := s.UpdateAndGetBurnRates(ctx, nil, now)
	if len(rates) != 0 {
		t.Errorf("empty results produced rates = %v, want empty", rates)
	}
}

func TestBurnRatesNilSafe(t *testing.T) {
	var rates BurnRates
	if _, ok := rates.Get(BurnRateKey{AccountKey: "x", Window: "5h"}); ok {
		t.Error("nil BurnRates.Get should return ok=false")
	}
}

func TestStoreNewCreatesDir(t *testing.T) {
	fs := fsutil.NewMemFS()
	if _, err := New(fs, "/var/data/cq"); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// Guard against a class of bug where schemaVersion drifts silently.
func TestStoreWrittenFileVersion(t *testing.T) {
	s, fs := newTestStore(t)
	ctx := context.Background()
	_, _ = s.UpdateAndGetBurnRates(ctx, []quota.Result{
		makeResult("acct1", map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: 80, ResetAtUnix: 2_000_000},
		}),
	}, 1_000_000)

	data, err := fs.ReadFile(filepath.Join(testDir, stateFileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `"version":1`) {
		t.Errorf("written file missing version:1 — got %s", string(data))
	}
}
