package proxy

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

// QuotaFetchFunc fetches quota results for all accounts of a provider.
type QuotaFetchFunc func(ctx context.Context, now time.Time) ([]quota.Result, error)

// QuotaSnapshot holds a quota result and when it was fetched.
type QuotaSnapshot struct {
	Result    quota.Result
	FetchedAt time.Time
}

// QuotaMonitor periodically fetches quota data for proactive account selection.
// A nil *QuotaMonitor is safe to call methods on — all methods return zero values.
type QuotaMonitor struct {
	fetch    QuotaFetchFunc
	interval time.Duration
	nowFunc  func() time.Time // for testing

	mu        sync.RWMutex
	snapshots map[string]QuotaSnapshot
}

// NewQuotaMonitor creates a monitor that polls quota at the given interval.
// Call Start to begin polling.
func NewQuotaMonitor(fetch QuotaFetchFunc, interval time.Duration) *QuotaMonitor {
	return &QuotaMonitor{
		fetch:     fetch,
		interval:  interval,
		nowFunc:   time.Now,
		snapshots: make(map[string]QuotaSnapshot),
	}
}

// Start performs an immediate fetch, then polls at the configured interval.
// It blocks until ctx is cancelled.
func (m *QuotaMonitor) Start(ctx context.Context) {
	m.poll(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(ctx)
		}
	}
}

// Snapshot returns the most recent quota snapshot for the given account identifier
// (AccountID or Email). Returns false if no snapshot exists or the receiver is nil.
func (m *QuotaMonitor) Snapshot(identifier string) (QuotaSnapshot, bool) {
	if m == nil {
		return QuotaSnapshot{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap, ok := m.snapshots[identifier]
	return snap, ok
}

// poll fetches quota for all accounts and stores results dual-keyed by
// AccountID and Email so lookups work regardless of which identifier the
// caller has (matching acctIdentifier which prefers UUID > Email > Token).
func (m *QuotaMonitor) poll(ctx context.Context) {
	now := m.nowFunc()
	results, err := m.fetch(ctx, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: quota monitor: %v\n", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear stale entries and rebuild from fresh results.
	m.snapshots = make(map[string]QuotaSnapshot, len(results)*2)
	for _, r := range results {
		if !r.IsUsable() {
			continue
		}
		snap := QuotaSnapshot{Result: r, FetchedAt: now}
		if r.AccountID != "" {
			m.snapshots[r.AccountID] = snap
		}
		if r.Email != "" {
			m.snapshots[r.Email] = snap
		}
	}
}
