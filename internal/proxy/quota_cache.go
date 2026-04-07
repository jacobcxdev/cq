package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/quota"
)

const quotaSnapshotMaxAge = 5 * time.Minute

// UsageFetchFunc fetches quota for a single Claude account.
type UsageFetchFunc func(ctx context.Context, acct keyring.ClaudeOAuth, now time.Time) (quota.Result, time.Duration, error)

// QuotaSnapshot holds a quota result and when it was fetched.
type QuotaSnapshot struct {
	Result    quota.Result
	FetchedAt time.Time
}

// QuotaReader is a read-only quota snapshot source.
type QuotaReader interface {
	Snapshot(identifier string) (QuotaSnapshot, bool)
}

// QuotaCache provides on-demand quota refresh backed by the shared cq cache.
type QuotaCache struct {
	UsageFetchFunc UsageFetchFunc
	cacheDir       string
	nowFunc        func() time.Time

	mu        sync.RWMutex
	snapshots map[string]QuotaSnapshot

	cooldownMu sync.RWMutex
	cooldowns  map[string]time.Time

	fetchMu sync.Mutex // serialises API calls in Refresh
}

// NewQuotaCache creates a quota cache backed by the shared cq cache directory.
func NewQuotaCache(fetch UsageFetchFunc, cacheDir string) *QuotaCache {
	return &QuotaCache{
		UsageFetchFunc: fetch,
		cacheDir:       cacheDir,
		nowFunc:        time.Now,
		snapshots:      make(map[string]QuotaSnapshot),
		cooldowns:      make(map[string]time.Time),
	}
}

// Snapshot returns the current quota snapshot without making API calls.
func (q *QuotaCache) Snapshot(identifier string) (QuotaSnapshot, bool) {
	if q == nil || identifier == "" {
		return QuotaSnapshot{}, false
	}
	if snap, ok := q.memorySnapshotFresh(identifier); ok {
		return snap, true
	}
	snap, ok, _ := q.loadFileSnapshot(identifier)
	if !ok {
		return QuotaSnapshot{}, false
	}
	return snap, true
}

// Refresh returns a fresh quota snapshot for the given account when possible.
func (q *QuotaCache) Refresh(ctx context.Context, acct *keyring.ClaudeOAuth) (QuotaSnapshot, bool) {
	if q == nil || acct == nil {
		return QuotaSnapshot{}, false
	}

	identifier := acctIdentifier(acct)
	if identifier == "" {
		return QuotaSnapshot{}, false
	}

	if snap, ok := q.memorySnapshotFresh(identifier); ok {
		return snap, true
	}

	fileSnap, fileOK, fileFresh := q.loadFileSnapshot(identifier)
	if fileOK && fileFresh {
		return fileSnap, true
	}

	staleSnap, staleOK := q.memorySnapshot(identifier)
	if !staleOK && fileOK {
		staleSnap, staleOK = fileSnap, true
	}

	// Serialise API calls so concurrent goroutines don't duplicate fetches
	// for the same account. After acquiring the lock, re-check memory
	// (double-check pattern — same as refreshAccount in transport.go).
	q.fetchMu.Lock()
	defer q.fetchMu.Unlock()

	if snap, ok := q.memorySnapshotFresh(identifier); ok {
		return snap, true
	}

	if q.coolingDown(identifier) {
		if staleOK {
			return staleSnap, true
		}
		return QuotaSnapshot{}, false
	}

	if q.UsageFetchFunc == nil {
		if staleOK {
			return staleSnap, true
		}
		return QuotaSnapshot{}, false
	}

	now := q.nowFunc()
	result, retryAfter, err := q.UsageFetchFunc(ctx, *acct, now)
	if err != nil {
		if staleOK {
			return staleSnap, true
		}
		return QuotaSnapshot{}, false
	}

	if result.IsUsable() {
		snap := QuotaSnapshot{Result: result, FetchedAt: now}
		q.storeSnapshot(snap)
		q.clearCooldown(identifier)
		return snap, true
	}

	if result.Error != nil && result.Error.Code == "api_error" && result.Error.HTTPStatus == http.StatusTooManyRequests && retryAfter > 0 {
		q.setCooldown(identifier, now.Add(retryAfter))
	}

	if staleOK {
		return staleSnap, true
	}
	return QuotaSnapshot{}, false
}

func (q *QuotaCache) memorySnapshotFresh(identifier string) (QuotaSnapshot, bool) {
	snap, ok := q.memorySnapshot(identifier)
	if !ok {
		return QuotaSnapshot{}, false
	}
	if q.nowFunc().Sub(snap.FetchedAt) > quotaSnapshotMaxAge {
		return QuotaSnapshot{}, false
	}
	return snap, true
}

func (q *QuotaCache) memorySnapshot(identifier string) (QuotaSnapshot, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	snap, ok := q.snapshots[identifier]
	return snap, ok
}

func (q *QuotaCache) loadFileSnapshot(identifier string) (QuotaSnapshot, bool, bool) {
	if q.cacheDir == "" {
		return QuotaSnapshot{}, false, false
	}

	path := filepath.Join(q.cacheDir, "claude.json")
	f, err := os.Open(path)
	if err != nil {
		return QuotaSnapshot{}, false, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return QuotaSnapshot{}, false, false
	}
	info, err := f.Stat()
	if err != nil {
		return QuotaSnapshot{}, false, false
	}

	var results []quota.Result
	if err := json.Unmarshal(data, &results); err != nil {
		return QuotaSnapshot{}, false, false
	}

	snapshots := make(map[string]QuotaSnapshot, len(results)*2)
	fetchedAt := info.ModTime()
	for _, result := range results {
		snap := QuotaSnapshot{Result: result, FetchedAt: fetchedAt}
		for _, key := range snapshotKeys(result) {
			snapshots[key] = snap
		}
	}
	q.mergeSnapshots(snapshots)

	snap, ok := snapshots[identifier]
	if !ok {
		return QuotaSnapshot{}, false, false
	}
	return snap, true, q.nowFunc().Sub(snap.FetchedAt) <= quotaSnapshotMaxAge
}

func (q *QuotaCache) mergeSnapshots(snapshots map[string]QuotaSnapshot) {
	if len(snapshots) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for key, snap := range snapshots {
		existing, ok := q.snapshots[key]
		if !ok || snap.FetchedAt.After(existing.FetchedAt) {
			q.snapshots[key] = snap
		}
	}
}

func (q *QuotaCache) storeSnapshot(snap QuotaSnapshot) {
	keys := snapshotKeys(snap.Result)
	if len(keys) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, key := range keys {
		q.snapshots[key] = snap
	}
}

func (q *QuotaCache) coolingDown(identifier string) bool {
	q.cooldownMu.RLock()
	defer q.cooldownMu.RUnlock()
	until, ok := q.cooldowns[identifier]
	return ok && q.nowFunc().Before(until)
}

func (q *QuotaCache) setCooldown(identifier string, until time.Time) {
	if identifier == "" {
		return
	}
	q.cooldownMu.Lock()
	defer q.cooldownMu.Unlock()
	q.cooldowns[identifier] = until
}

func (q *QuotaCache) clearCooldown(identifier string) {
	q.cooldownMu.Lock()
	defer q.cooldownMu.Unlock()
	delete(q.cooldowns, identifier)
}

func snapshotKeys(result quota.Result) []string {
	keys := make([]string, 0, 2)
	if result.AccountID != "" {
		keys = append(keys, result.AccountID)
	}
	if result.Email != "" {
		keys = append(keys, result.Email)
	}
	return keys
}
