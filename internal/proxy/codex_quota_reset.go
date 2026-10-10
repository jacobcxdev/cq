package proxy

import (
	"errors"
	"fmt"
	"math"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

type codexQuotaResetPending struct {
	event    CodexQuotaResetEvent
	inFlight bool
}

// observeQuotaReset uses authenticated observations only. Cached usage is never
// a reset baseline, and callback delivery never holds the capacity mutex.
func (l *CodexCapacityLedger) observeQuotaReset(fact CapacityFact) {
	if !validCapacityFact(fact) || fact.Confidence != CapacityConfidenceAuthoritative ||
		(fact.Source != CapacitySourceHTTPHeaders && fact.Source != CapacitySourceLiveUsage && fact.Source != CapacitySourceLiveRateLimits) {
		return
	}
	now := l.now()
	if fact.ObservedAt.IsZero() || fact.ObservedAt.After(now) || now.Sub(fact.ObservedAt) >= l.maxAge {
		return
	}
	l.mu.Lock()
	l.observeIncludedWindowsLocked(fact)
	if l.resetWindows == nil {
		l.resetWindows = make(map[codex.AccountKey]map[quota.WindowName]codexWindowFact)
		l.resetPending = make(map[codex.AccountKey]*codexQuotaResetPending)
	}
	if l.resetWindows[fact.AccountKey] == nil {
		l.resetWindows[fact.AccountKey] = make(map[quota.WindowName]codexWindowFact)
	}
	for name, window := range fact.Windows {
		remaining := windowRemaining(window)
		if name != quota.Window5Hour && name != quota.Window7Day || window.RemainingPctExact == nil ||
			window.ResetAtUnix <= fact.ObservedAt.Unix() || math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 {
			continue
		}
		old, exists := l.resetWindows[fact.AccountKey][name]
		if exists && (!capacityCursorAfter(fact, old.fact) || !fact.ObservedAt.After(old.fact.ObservedAt)) {
			continue
		}
		if exists && now.Sub(old.fact.ObservedAt) < l.maxAge && windowRemaining(old.window) == 0 &&
			windowRemaining(window) > 0 && window.ResetAtUnix >= old.window.ResetAtUnix && l.resetPending[fact.AccountKey] == nil {
			id := codex.ResetIdempotencyKey(fact.AccountKey, fmt.Sprintf("quota:%s:%d:%d", name, old.window.ResetAtUnix, old.fact.ObservedAt.UnixNano()))
			l.resetPending[fact.AccountKey] = &codexQuotaResetPending{event: CodexQuotaResetEvent{AccountKey: fact.AccountKey, EventID: id}}
		}
		l.resetWindows[fact.AccountKey][name] = codexWindowFact{window: cloneQuotaWindow(window), fact: fact}
	}
	pending := l.resetPending[fact.AccountKey]
	if pending == nil || pending.inFlight || l.OnReset == nil {
		l.mu.Unlock()
		return
	}
	pending.inFlight = true
	l.mu.Unlock()
	err := deliverCodexQuotaReset(l.OnReset, pending.event)
	l.mu.Lock()
	if err == nil {
		delete(l.resetPending, fact.AccountKey)
	} else {
		pending.inFlight = false
	}
	l.mu.Unlock()
}

// Usage zero remains advisory for ordinary requests, but authenticates that
// redistributing a chat here cannot yet preserve extra credits.
func (l *CodexCapacityLedger) observeIncludedWindowsLocked(fact CapacityFact) {
	if l.includedWindows == nil {
		l.includedWindows = make(map[codex.AccountKey]map[quota.WindowName]codexWindowFact)
	}
	if l.includedWindows[fact.AccountKey] == nil {
		l.includedWindows[fact.AccountKey] = make(map[quota.WindowName]codexWindowFact)
	}
	for name, window := range fact.Windows {
		name = canonicalReserveWindow(name)
		remaining := windowRemaining(window)
		if quota.PeriodFor(name) <= 0 || window.RemainingPctExact == nil || !(remaining >= 0 && remaining <= 100) || window.ResetAtUnix <= fact.ObservedAt.Unix() {
			continue
		}
		old, exists := l.includedWindows[fact.AccountKey][name]
		if exists && (!capacityCursorAfter(fact, old.fact) || fact.ObservedAt.Before(old.fact.ObservedAt) || window.ResetAtUnix < old.window.ResetAtUnix) {
			continue
		}
		l.includedWindows[fact.AccountKey][name] = codexWindowFact{window: cloneQuotaWindow(window), fact: fact}
	}
}

func deliverCodexQuotaReset(deliver func(CodexQuotaResetEvent) error, event CodexQuotaResetEvent) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("Codex reset notification panic")
		}
	}()
	return deliver(event)
}
