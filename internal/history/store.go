// Package history persists per-(account, window) exponentially weighted
// moving averages of the observed burn rate (percentage-points consumed per
// second). These rates drive the phase-invariant gauge severity math in
// internal/aggregate — see computeGaugeInfo for the consumer.
//
// The store uses a delta-based EWMA: each call computes the wall-clock
// difference between the previous remaining percentage (with reset-unwrap)
// and the current remaining percentage, divides by the elapsed time between
// observations, and blends it with the prior rate using a window-specific
// half-life. First observations seed state without producing a rate;
// subsequent observations update it. Exhausted-zero samples are censored
// (the EWMA is frozen, not driven toward zero by artificial quiescence).
//
// Anonymous results (empty AccountID and empty Email) bypass the store —
// they have no stable identity across runs and always cold-start.
//
// The store is expected to be invoked from serial code. internal/app.Runner
// calls UpdateAndGetBurnRates from buildReport AFTER wg.Wait() completes all
// provider goroutines, so no mutex is needed on Store internals.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/quota"
)

// schemaVersion is the on-disk BurnState schema version. Any mismatch on
// load is treated as cold start (empty state).
const schemaVersion = 1

// stateFileName is the sidecar file name under the cache/history directory.
const stateFileName = "burn_state_v1.json"

// BurnRateKey identifies a single (provider, account, window) burn-rate entry.
// All fields are value types so the struct is comparable and usable as a map
// key.
type BurnRateKey struct {
	ProviderID string
	AccountKey string
	Window     string // quota.WindowName as string
}

// BurnRates is the read-side snapshot returned by the store after processing
// a batch of results. Nil-safe: a nil BurnRates returns (0, false) for any
// Get, which downstream code treats as cold-start.
type BurnRates map[BurnRateKey]float64

// Get returns the EWMA rate in own-pct/s for the given key, or (0, false)
// if no rate is available (cold start, insufficient samples, or nil map).
func (b BurnRates) Get(k BurnRateKey) (float64, bool) {
	if b == nil {
		return 0, false
	}
	r, ok := b[k]
	return r, ok
}

// BurnState is the on-disk persistence schema.
type BurnState struct {
	Version  int                      `json:"version"`
	Accounts map[string]*AccountState `json:"accounts"` // key = "<accountKey>"
}

// AccountState holds all window states for a single (provider, account).
type AccountState struct {
	Windows map[string]*WindowState `json:"windows"` // key = window name
}

// WindowState captures the EWMA plus the minimum metadata needed to compute
// the next delta sample.
type WindowState struct {
	EWMARatePctPerS  float64 `json:"ewma_rate_pct_per_s"`
	LastSeenUnix     int64   `json:"last_seen_unix"`
	LastRemainingPct int     `json:"last_remaining_pct"`
	LastResetAtUnix  int64   `json:"last_reset_at_unix"`
	Samples          int     `json:"samples"`
}

// Store persists EWMA burn state using fsutil.FileSystem for dependency
// injection. The tmp+rename pattern mirrors internal/cache.Cache.Put.
type Store struct {
	fs   fsutil.FileSystem
	dir  string
	path string
}

// New creates a history store rooted at dir. The directory is created with
// 0o700 if it does not exist. Callers that pass a nil store into
// internal/app.Runner get cold-start behaviour throughout the gauge math.
func New(fs fsutil.FileSystem, dir string) (*Store, error) {
	if err := fs.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create history dir: %w", err)
	}
	return &Store{
		fs:   fs,
		dir:  dir,
		path: filepath.Join(dir, stateFileName),
	}, nil
}

// load reads the on-disk BurnState. Missing file, corrupt JSON, or a schema
// version mismatch all return an empty (but non-nil) BurnState — cold start.
// I/O errors other than NotExist are surfaced so callers can log them.
func (s *Store) load() (*BurnState, error) {
	data, err := s.fs.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyState(), nil
		}
		return emptyState(), fmt.Errorf("read history: %w", err)
	}
	var state BurnState
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupt file: degrade to cold start rather than failing.
		fmt.Fprintf(os.Stderr, "cq: history file corrupt, starting fresh: %v\n", err)
		return emptyState(), nil
	}
	if state.Version != schemaVersion {
		return emptyState(), nil
	}
	if state.Accounts == nil {
		state.Accounts = make(map[string]*AccountState)
	}
	return &state, nil
}

// save writes state to disk using the atomic tmp+rename pattern so readers
// never see a torn file.
func (s *Store) save(state *BurnState) error {
	state.Version = schemaVersion
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal history: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := s.fs.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write history tmp: %w", err)
	}
	if err := s.fs.Rename(tmp, s.path); err != nil {
		s.fs.Remove(tmp)
		return fmt.Errorf("rename history: %w", err)
	}
	return nil
}

// UpdateAndGetBurnRates processes the current batch of results, updates the
// persistent EWMA state, writes it atomically, and returns the current rates.
// Results that are not usable, backfilled from stale cache (CacheAge > 0), or
// anonymous (no AccountID and no Email) are skipped.
//
// The returned BurnRates only contains entries with Samples >= 2, i.e. at
// least one delta has been observed. First observations seed state but
// produce no rate, so downstream code cold-starts for that account+window
// until the next run.
//
// Must be called from serial code.
func (s *Store) UpdateAndGetBurnRates(
	ctx context.Context,
	results []quota.Result,
	nowEpoch int64,
) (BurnRates, error) {
	_ = ctx // reserved for future cancellation support

	state, err := s.load()
	if err != nil {
		// load already degraded to empty on parse/missing errors, so a
		// non-nil error here means a real I/O failure. Keep going with the
		// empty state and surface the error to the caller for logging.
		state = emptyState()
	}

	for _, r := range results {
		if !r.IsUsable() || len(r.Windows) == 0 {
			continue
		}
		// Backfilled-from-stale-cache results would poison the EWMA: a tiny
		// Δt over data that's actually hours old.
		if r.CacheAge > 0 {
			continue
		}
		accountKey := r.AccountID
		if accountKey == "" {
			accountKey = r.Email
		}
		if accountKey == "" {
			continue
		}
		// Provider is not carried on quota.Result directly, so the store
		// keys on account identity only. Collisions across providers would
		// only happen for identical account keys across Claude/Codex/Gemini,
		// which is unlikely for a local CLI — and even then the window name
		// prevents cross-window contamination. Downstream code builds the
		// BurnRateKey with the provider ID from aggregate context so the
		// read path is still provider-scoped.
		accountFullKey := accountKey
		acct, ok := state.Accounts[accountFullKey]
		if !ok {
			acct = &AccountState{Windows: make(map[string]*WindowState)}
			state.Accounts[accountFullKey] = acct
		} else if acct.Windows == nil {
			acct.Windows = make(map[string]*WindowState)
		}

		for winName, w := range r.Windows {
			windowKey := string(winName)
			prev, have := acct.Windows[windowKey]
			if !have {
				// First observation: seed state, no rate sample yet.
				acct.Windows[windowKey] = &WindowState{
					EWMARatePctPerS:  0,
					LastSeenUnix:     nowEpoch,
					LastRemainingPct: w.RemainingPct,
					LastResetAtUnix:  w.ResetAtUnix,
					Samples:          1,
				}
				continue
			}

			dt := nowEpoch - prev.LastSeenUnix
			if dt <= 0 {
				// Clock went backward or duplicate observation — preserve
				// prior state unchanged. This is deliberately strict: even
				// a zero-delta would produce a divide-by-zero below.
				continue
			}

			deltaPct, resetOK := computeDelta(prev, w, winName)
			if !resetOK {
				// Ambiguous reset (e.g. synthetic epoch fallback or reset
				// time went backwards) — reseed snapshot, no EWMA update.
				prev.LastSeenUnix = nowEpoch
				prev.LastRemainingPct = w.RemainingPct
				prev.LastResetAtUnix = w.ResetAtUnix
				continue
			}

			// Censor negative deltas (the API occasionally reports a small
			// upward adjustment on refresh).
			if deltaPct < 0 {
				deltaPct = 0
			}

			// Exhaustion censoring: if the account is sitting at zero and
			// was at zero before, this sample is censored data, not evidence
			// of zero demand. Freeze the EWMA and update only LastSeenUnix.
			if w.RemainingPct == 0 && deltaPct == 0 && prev.LastRemainingPct == 0 {
				prev.LastSeenUnix = nowEpoch
				prev.LastResetAtUnix = w.ResetAtUnix
				continue
			}

			instantRate := float64(deltaPct) / float64(dt)
			halfLife := halfLifeFor(winName)
			alpha := 1.0 - math.Pow(2.0, -float64(dt)/halfLife)
			prev.EWMARatePctPerS = alpha*instantRate + (1-alpha)*prev.EWMARatePctPerS
			prev.LastSeenUnix = nowEpoch
			prev.LastRemainingPct = w.RemainingPct
			prev.LastResetAtUnix = w.ResetAtUnix
			prev.Samples++
		}
	}

	// Persist — log and continue on write failure so this run's gauge still
	// benefits from the in-memory state.
	var saveErr error
	if err := s.save(state); err != nil {
		saveErr = err
	}

	rates := make(BurnRates)
	for accountFullKey, acct := range state.Accounts {
		if acct == nil {
			continue
		}
		for windowKey, ws := range acct.Windows {
			if ws == nil || ws.Samples < 2 {
				continue
			}
			// The store does not know the provider ID — it's keyed by account
			// identity alone. Downstream code in aggregate constructs the
			// BurnRateKey with the provider from aggregate context and looks
			// up using the account-only key. To support that, we publish the
			// rate under a key where ProviderID is empty, and the lookup site
			// does the same. This keeps the package surface small.
			rates[BurnRateKey{
				ProviderID: "",
				AccountKey: accountFullKey,
				Window:     windowKey,
			}] = ws.EWMARatePctPerS
		}
	}

	return rates, saveErr
}

// computeDelta returns the amount consumed since the previous observation in
// percentage points, unwrapping window resets. If the reset metadata is
// ambiguous (non-clean multiple of the window period, backwards, or synthetic)
// it returns ok=false and the caller reseeds without an EWMA update.
func computeDelta(prev *WindowState, w quota.Window, winName quota.WindowName) (int, bool) {
	if w.ResetAtUnix == prev.LastResetAtUnix {
		return prev.LastRemainingPct - w.RemainingPct, true
	}
	if w.ResetAtUnix < prev.LastResetAtUnix {
		// Reset time moved backwards — can happen if the API stops reporting
		// the reset and we fall back to a synthetic epoch. Ambiguous.
		return 0, false
	}

	period := int64(quota.PeriodFor(winName).Seconds())
	if period <= 0 {
		return 0, false
	}
	diff := w.ResetAtUnix - prev.LastResetAtUnix
	// Accept clean non-negative multiples of period within ±60s tolerance.
	const tolerance = int64(60)
	nResets := (diff + period/2) / period
	if nResets <= 0 {
		return 0, false
	}
	residual := diff - nResets*period
	if residual < -tolerance || residual > tolerance {
		return 0, false
	}
	// Unwrap: each reset restores 100 percentage points.
	return prev.LastRemainingPct + int(100*nResets) - w.RemainingPct, true
}

// halfLifeFor returns the EWMA half-life in seconds for the given window.
// These are hand-picked ergonomic values, not derived from first principles:
//   - 5h window: 30 minutes — adapts within a single session
//   - 7d window:  6 hours — smooths across sessions but reacts to genuine
//     workload shifts within a fraction of a day
//
// Tuning is an open question; revisit after observing real-world usage.
func halfLifeFor(win quota.WindowName) float64 {
	switch quota.BaseWindow(win) {
	case quota.Window5Hour:
		return 30 * 60 // 1800s
	case quota.Window7Day:
		return 6 * 3600 // 21600s
	default:
		return 30 * 60
	}
}

// HalfLifeForTesting exposes the half-life table for cross-package tests.
// Not part of the public API.
func HalfLifeForTesting(win quota.WindowName) float64 { return halfLifeFor(win) }

func emptyState() *BurnState {
	return &BurnState{
		Version:  schemaVersion,
		Accounts: make(map[string]*AccountState),
	}
}
