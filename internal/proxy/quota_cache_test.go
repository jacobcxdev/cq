package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestQuotaCacheSnapshotReturnsEmptyWhenNoData(t *testing.T) {
	q := NewQuotaCache(nil, t.TempDir())
	if _, ok := q.Snapshot("missing"); ok {
		t.Fatal("expected empty snapshot")
	}
}

func TestQuotaCacheSnapshotReadsFromFileCache(t *testing.T) {
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeClaudeCache(t, dir, now, []quota.Result{quotaResult("uuid-a", "a@test.com", 42)})

	q := NewQuotaCache(nil, dir)
	q.nowFunc = func() time.Time { return now }

	snap, ok := q.Snapshot("a@test.com")
	if !ok {
		t.Fatal("expected snapshot from file cache")
	}
	if snap.Result.MinRemainingPct() != 42 {
		t.Fatalf("remaining = %d, want 42", snap.Result.MinRemainingPct())
	}
}

func TestQuotaCacheRefreshCallsAPIWhenStale(t *testing.T) {
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	q := NewQuotaCache(func(_ context.Context, acct keyring.ClaudeOAuth, gotNow time.Time) (quota.Result, time.Duration, error) {
		calls.Add(1)
		if acct.AccountUUID != "uuid-a" {
			t.Fatalf("account UUID = %q, want uuid-a", acct.AccountUUID)
		}
		if !gotNow.Equal(now) {
			t.Fatalf("now = %v, want %v", gotNow, now)
		}
		return quotaResult("uuid-a", "a@test.com", 70), 0, nil
	}, t.TempDir())
	q.nowFunc = func() time.Time { return now }
	q.snapshots["uuid-a"] = QuotaSnapshot{
		Result:    quotaResult("uuid-a", "a@test.com", 10),
		FetchedAt: now.Add(-10 * time.Minute),
	}

	snap, ok := q.Refresh(context.Background(), &keyring.ClaudeOAuth{AccountUUID: "uuid-a", Email: "a@test.com"})
	if !ok {
		t.Fatal("expected refreshed snapshot")
	}
	if snap.Result.MinRemainingPct() != 70 {
		t.Fatalf("remaining = %d, want 70", snap.Result.MinRemainingPct())
	}
	if calls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls.Load())
	}
}

func TestQuotaCacheRefreshRespectsCooldown(t *testing.T) {
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	q := NewQuotaCache(func(context.Context, keyring.ClaudeOAuth, time.Time) (quota.Result, time.Duration, error) {
		calls.Add(1)
		return quotaResult("uuid-a", "a@test.com", 80), 0, nil
	}, t.TempDir())
	q.nowFunc = func() time.Time { return now }
	q.cooldowns["uuid-a"] = now.Add(time.Minute)

	if _, ok := q.Refresh(context.Background(), &keyring.ClaudeOAuth{AccountUUID: "uuid-a", Email: "a@test.com"}); ok {
		t.Fatal("expected no snapshot during cooldown")
	}
	if calls.Load() != 0 {
		t.Fatalf("fetch calls = %d, want 0", calls.Load())
	}
}

func TestQuotaCacheRefreshUsesFileCacheWhenFresh(t *testing.T) {
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeClaudeCache(t, dir, now.Add(-2*time.Minute), []quota.Result{quotaResult("uuid-a", "a@test.com", 55)})

	var calls atomic.Int32
	q := NewQuotaCache(func(context.Context, keyring.ClaudeOAuth, time.Time) (quota.Result, time.Duration, error) {
		calls.Add(1)
		return quotaResult("uuid-a", "a@test.com", 80), 0, nil
	}, dir)
	q.nowFunc = func() time.Time { return now }

	snap, ok := q.Refresh(context.Background(), &keyring.ClaudeOAuth{AccountUUID: "uuid-a", Email: "a@test.com"})
	if !ok {
		t.Fatal("expected snapshot from file cache")
	}
	if snap.Result.MinRemainingPct() != 55 {
		t.Fatalf("remaining = %d, want 55", snap.Result.MinRemainingPct())
	}
	if calls.Load() != 0 {
		t.Fatalf("fetch calls = %d, want 0", calls.Load())
	}
}

func writeClaudeCache(t *testing.T, dir string, modTime time.Time, results []quota.Result) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}
