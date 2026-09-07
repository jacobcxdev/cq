package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func canonicalReserveWindow(name quota.WindowName) quota.WindowName {
	return quota.WindowName(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(string(name))), "_", "-"))
}

type codexWindowFact struct {
	window quota.Window
	fact   CapacityFact
}

func cloneQuotaWindow(w quota.Window) quota.Window {
	if w.RemainingPctExact != nil {
		v := *w.RemainingPctExact
		w.RemainingPctExact = &v
	}
	return w
}
func windowRemaining(w quota.Window) float64 {
	if w.RemainingPctExact != nil {
		return *w.RemainingPctExact
	}
	return float64(w.RemainingPct)
}
func (l *CodexCapacityLedger) observeWindowsLocked(fact CapacityFact) {
	if l.windows == nil {
		l.windows = make(map[codex.AccountKey]map[quota.WindowName]codexWindowFact)
	}
	if l.windows[fact.AccountKey] == nil {
		l.windows[fact.AccountKey] = make(map[quota.WindowName]codexWindowFact)
	}
	for name, w := range fact.Windows {
		name = canonicalReserveWindow(name)
		remaining := windowRemaining(w)
		if quota.PeriodFor(name) <= 0 || math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 {
			continue
		}
		old, ok := l.windows[fact.AccountKey][name]
		if ok && fact.ConnectionGeneration > 0 && old.fact.ConnectionGeneration > 0 && !capacityCursorAfter(fact, old.fact) {
			continue
		}
		if ok && (fact.ObservedAt.Before(old.fact.ObservedAt) || (fact.ObservedAt.Equal(old.fact.ObservedAt) && (fact.Source < old.fact.Source || (fact.Source == old.fact.Source && !capacityFactAdvances(old.fact, fact))))) {
			continue
		}
		l.windows[fact.AccountKey][name] = codexWindowFact{window: cloneQuotaWindow(w), fact: fact}
	}
}

// WindowSnapshot returns independent window data and its oldest observation time.
func (l *CodexCapacityLedger) WindowSnapshot(account codex.AccountKey) (map[quota.WindowName]quota.Window, time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	windows := make(map[quota.WindowName]quota.Window)
	var oldest time.Time
	for name, entry := range l.windows[account] {
		windows[name] = cloneQuotaWindow(entry.window)
		if oldest.IsZero() || entry.fact.ObservedAt.Before(oldest) {
			oldest = entry.fact.ObservedAt
		}
	}
	return windows, oldest
}
func (l *CodexCapacityLedger) windowObservation(account codex.AccountKey, name quota.WindowName) (quota.Window, time.Time, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	entry, ok := l.windows[account][canonicalReserveWindow(name)]
	return cloneQuotaWindow(entry.window), entry.fact.ObservedAt, ok
}

type codexReserveBypass struct {
	Account    codex.AccountKey `json:"account"`
	ResetAt    int64            `json:"reset_at"`
	Remaining  float64          `json:"remaining"`
	ObservedAt time.Time        `json:"observed_at"`
}
type codexReserveDocument struct {
	LastSystemAccount codex.AccountKey    `json:"last_system_account,omitempty"`
	Window            quota.WindowName    `json:"window,omitempty"`
	Percent           float64             `json:"percent,omitempty"`
	Bypass            *codexReserveBypass `json:"bypass,omitempty"`
}

// CodexReserveStatus describes the reserve independently of any pool.
type CodexReserveStatus struct {
	Configured   bool                              `json:"configured"`
	Window       quota.WindowName                  `json:"window,omitempty"`
	Percent      float64                           `json:"percent"`
	AccountKey   codex.AccountKey                  `json:"account_key,omitempty"`
	Email        string                            `json:"email,omitempty"`
	Enabled      bool                              `json:"enabled"`
	Blocked      bool                              `json:"blocked"`
	Reason       string                            `json:"reason,omitempty"`
	RemainingPct *float64                          `json:"remaining_pct,omitempty"`
	ResetAt      int64                             `json:"reset_at,omitempty"`
	ObservedAt   time.Time                         `json:"observed_at"`
	Windows      map[quota.WindowName]quota.Window `json:"windows"`
}

// CodexReserve is owned by the service; command mutations are atomic and durable.
type CodexReserve struct {
	lastSystem codex.AccountKey
	lastEmail  string
	mu         sync.Mutex
	fs         fsutil.FileSystem
	path       string
	ledger     *CodexCapacityLedger
	inventory  codex.CredentialInventory
	now        func() time.Time
	document   codexReserveDocument
}

func OpenCodexReserve(fs fsutil.FileSystem, path string, ledger *CodexCapacityLedger, inventory codex.CredentialInventory, now func() time.Time) (*CodexReserve, error) {
	if fs == nil || path == "" || ledger == nil || inventory == nil {
		return nil, errors.New("Codex reserve dependencies unavailable")
	}
	if now == nil {
		now = time.Now
	}
	r := &CodexReserve{fs: fs, path: path, ledger: ledger, inventory: inventory, now: now}
	data, err := fs.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &r.document); err != nil {
		return nil, errors.New("invalid Codex reserve state")
	}
	if r.document.Window != "" && (quota.PeriodFor(r.document.Window) <= 0 || !validReservePercent(r.document.Percent)) {
		return nil, errors.New("invalid Codex reserve configuration")
	}
	r.lastSystem = r.document.LastSystemAccount
	return r, nil
}
func validReservePercent(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p > 0 && p < 100
}
func (r *CodexReserve) saveLocked(document codexReserveDocument) error {
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if err = r.fs.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	if err = fsutil.SecureAtomicWrite(r.fs, r.path, data); err != nil {
		return err
	}
	r.document = document
	return nil
}
func (r *CodexReserve) systemAccount() (codex.AccountKey, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inventory, err := r.inventory.List(ctx)
	if err != nil {
		return "", "", err
	}
	return reserveSystemIdentity(inventory)
}
func reserveSystemIdentity(inventory codex.Inventory) (codex.AccountKey, string, error) {
	var key codex.AccountKey
	var email string
	for _, account := range inventory.Accounts {
		if account.Active {
			if key != "" && key != account.Key {
				return "", "", errors.New("ambiguous system account")
			}
			key = account.Key
			email = account.Identity.Email
		}
	}
	return key, email, nil
}
func (r *CodexReserve) statusLocked() CodexReserveStatus {
	return r.statusForIdentityLocked(r.systemAccount())
}

// ObserveInventory updates identity and reset state during service refresh.
func (r *CodexReserve) ObserveInventory(inventory codex.Inventory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusForIdentityLocked(reserveSystemIdentity(inventory))
}
func (r *CodexReserve) statusForIdentityLocked(key codex.AccountKey, email string, err error) CodexReserveStatus {
	d := r.document
	status := CodexReserveStatus{Configured: d.Window != "", Window: d.Window, Percent: d.Percent, Enabled: d.Window != ""}
	if err != nil || key == "" {
		if err == nil {
			err = errors.New("system account missing")
		}
		key = r.lastSystem
		email = r.lastEmail
	} else {
		r.lastSystem = key
		r.lastEmail = email
	}
	status.AccountKey = key
	status.Email = email
	if err != nil {
		status.Reason = "system_account_unavailable"
		status.Blocked = status.Configured
		return status
	}
	if status.Configured && d.LastSystemAccount != key {
		d.LastSystemAccount = key
		if err := r.saveLocked(d); err != nil {
			status.Blocked = true
			status.Reason = "reserve_state_write_failed"
			return status
		}
	}
	status.Windows, _ = r.ledger.WindowSnapshot(key)
	if !status.Configured {
		return status
	}
	if key == "" {
		status.Reason = "system_account_unavailable"
		status.Blocked = true
		return status
	}
	w, observed, ok := r.ledger.windowObservation(key, d.Window)
	status.ObservedAt = observed
	status.ResetAt = w.ResetAtUnix
	remaining := windowRemaining(w)
	if ok {
		status.RemainingPct = &remaining
	}
	if d.Bypass != nil {
		b := d.Bypass
		reset := ok && observed.After(b.ObservedAt) && ((r.now().Unix() >= b.ResetAt && w.ResetAtUnix > r.now().Unix()) || (w.ResetAtUnix != b.ResetAt && remaining > b.Remaining && time.Unix(w.ResetAtUnix, 0).Add(-quota.PeriodFor(d.Window)).Unix() >= b.ObservedAt.Unix()) || (remaining == 100 && b.Remaining < 100))
		if key != b.Account || reset {
			d.Bypass = nil
			if err := r.saveLocked(d); err != nil {
				status.Blocked = true
				status.Reason = "reserve_state_write_failed"
				return status
			}
		} else {
			status.Enabled = false
			status.Reason = "disabled_until_reset"
			return status
		}
	}
	if !ok || observed.IsZero() || r.now().Sub(observed) > reserveFreshnessLimit(remaining, d.Percent) || w.ResetAtUnix <= r.now().Unix() {
		status.Blocked = true
		status.Reason = "usage_stale"
		return status
	}
	status.Blocked = remaining <= d.Percent
	if status.Blocked {
		status.Reason = "reserve_reached"
	}
	return status
}
func (r *CodexReserve) Status() CodexReserveStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusLocked()
}
func (r *CodexReserve) Blocked(account codex.AccountKey) (bool, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.document.Window == "" {
		return false, 0
	}
	status := r.statusLocked()
	// Retain the last observed system identity when inventory is temporarily unavailable.
	return status.Blocked && status.AccountKey == account, status.ResetAt
}
func (r *CodexReserve) Control(action string, window quota.WindowName, percent float64) (CodexReserveStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.statusLocked()
	d := r.document
	switch action {
	case "set":
		window = canonicalReserveWindow(window)
		if quota.PeriodFor(window) <= 0 || !validReservePercent(percent) {
			return status, errors.New("reserve requires a valid window and percentage greater than 0 and less than 100")
		}
		if _, ok := status.Windows[window]; !ok {
			return status, errors.New("selected window unavailable; inspect reserve windows")
		}
		d = codexReserveDocument{Window: window, Percent: percent, LastSystemAccount: status.AccountKey}
	case "disable":
		if !status.Configured {
			return status, errors.New("reserve is not configured")
		}
		if status.RemainingPct == nil || status.AccountKey == "" || status.ResetAt <= r.now().Unix() || r.now().Sub(status.ObservedAt) > reserveFreshnessLimit(*status.RemainingPct, d.Percent) {
			return status, errors.New("fresh usage and reset evidence required to disable reserve")
		}
		d.Bypass = &codexReserveBypass{Account: status.AccountKey, ResetAt: status.ResetAt, Remaining: *status.RemainingPct, ObservedAt: status.ObservedAt}
	case "enable":
		if !status.Configured {
			return status, errors.New("reserve is not configured")
		}
		d.Bypass = nil
	case "clear":
		d = codexReserveDocument{}
	default:
		return status, errors.New("unknown reserve action")
	}
	if err := r.saveLocked(d); err != nil {
		return status, err
	}
	return r.statusLocked(), nil
}

// Allow one service tick and one bounded usage request to complete.
func reserveFreshnessLimit(remaining, percent float64) time.Duration {
	return reserveRefreshInterval(remaining, percent) + 10*time.Second
}
func reserveRefreshInterval(remaining, percent float64) time.Duration {
	// Measure distance to the artificial exhaustion threshold.
	available := remaining - percent
	switch {
	case available <= 1:
		return 5 * time.Second
	case available <= 10:
		return 15 * time.Second
	case available <= 25:
		return 30 * time.Second
	default:
		return 60 * time.Second
	}
}
func (r *CodexReserve) RefreshInterval(account codex.AccountKey, windows map[quota.WindowName]quota.Window) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.document.Window == "" {
		return 0
	}
	if r.lastSystem != account {
		return 0
	}
	w, ok := windows[r.document.Window]
	if !ok {
		for name, candidate := range windows {
			if canonicalReserveWindow(name) == r.document.Window {
				w = candidate
				ok = true
				break
			}
		}
	}
	if !ok {
		return 5 * time.Second
	}
	return reserveRefreshInterval(windowRemaining(w), r.document.Percent)
}
