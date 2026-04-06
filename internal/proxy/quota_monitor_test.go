package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func fakeResults(remaining int) []quota.Result {
	return []quota.Result{
		{
			AccountID: "uuid-a",
			Email:     "a@test.com",
			Status:    quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: remaining},
			},
		},
		{
			AccountID: "uuid-b",
			Email:     "b@test.com",
			Status:    quota.StatusOK,
			Windows: map[quota.WindowName]quota.Window{
				quota.Window5Hour: {RemainingPct: remaining + 10},
			},
		},
	}
}

func TestQuotaMonitor_ImmediateFetch(t *testing.T) {
	var called atomic.Int32
	fetch := func(_ context.Context, _ time.Time) ([]quota.Result, error) {
		called.Add(1)
		return fakeResults(50), nil
	}

	m := NewQuotaMonitor(fetch, time.Hour) // long interval — only initial fires
	ctx, cancel := context.WithCancel(context.Background())

	go m.Start(ctx)
	// Give the goroutine time to run the initial poll.
	time.Sleep(50 * time.Millisecond)
	cancel()

	if called.Load() < 1 {
		t.Fatal("expected at least one fetch on start")
	}

	snap, ok := m.Snapshot("uuid-a")
	if !ok {
		t.Fatal("expected snapshot for uuid-a")
	}
	if snap.Result.MinRemainingPct() != 50 {
		t.Fatalf("expected 50%%, got %d%%", snap.Result.MinRemainingPct())
	}
}

func TestQuotaMonitor_PeriodicRefresh(t *testing.T) {
	var calls atomic.Int32
	fetch := func(_ context.Context, _ time.Time) ([]quota.Result, error) {
		calls.Add(1)
		return fakeResults(50), nil
	}

	m := NewQuotaMonitor(fetch, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	go m.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()

	if c := calls.Load(); c < 2 {
		t.Fatalf("expected at least 2 fetches (initial + periodic), got %d", c)
	}
}

func TestQuotaMonitor_Shutdown(t *testing.T) {
	fetch := func(_ context.Context, _ time.Time) ([]quota.Result, error) {
		return fakeResults(50), nil
	}

	m := NewQuotaMonitor(fetch, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.Start(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestQuotaMonitor_FetchError(t *testing.T) {
	first := true
	fetch := func(_ context.Context, _ time.Time) ([]quota.Result, error) {
		if first {
			first = false
			return fakeResults(50), nil
		}
		return nil, fmt.Errorf("network error")
	}

	m := NewQuotaMonitor(fetch, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	go m.Start(ctx)
	// Wait for initial + at least one failed poll.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Snapshots should still have data from the first successful fetch.
	snap, ok := m.Snapshot("uuid-a")
	if !ok {
		t.Fatal("expected snapshot to survive after fetch error")
	}
	if snap.Result.MinRemainingPct() != 50 {
		t.Fatalf("expected 50%%, got %d%%", snap.Result.MinRemainingPct())
	}
}

func TestQuotaMonitor_DualKeyLookup(t *testing.T) {
	fetch := func(_ context.Context, _ time.Time) ([]quota.Result, error) {
		return fakeResults(42), nil
	}

	m := NewQuotaMonitor(fetch, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Lookup by UUID.
	snap, ok := m.Snapshot("uuid-a")
	if !ok || snap.Result.MinRemainingPct() != 42 {
		t.Fatalf("UUID lookup failed: ok=%v, pct=%d", ok, snap.Result.MinRemainingPct())
	}

	// Lookup by email.
	snap, ok = m.Snapshot("a@test.com")
	if !ok || snap.Result.MinRemainingPct() != 42 {
		t.Fatalf("email lookup failed: ok=%v, pct=%d", ok, snap.Result.MinRemainingPct())
	}
}

func TestQuotaMonitor_MissingIdentifier(t *testing.T) {
	fetch := func(_ context.Context, _ time.Time) ([]quota.Result, error) {
		return fakeResults(50), nil
	}

	m := NewQuotaMonitor(fetch, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	_, ok := m.Snapshot("nonexistent")
	if ok {
		t.Fatal("expected false for unknown identifier")
	}
}

func TestQuotaMonitor_NilReceiver(t *testing.T) {
	var m *QuotaMonitor
	_, ok := m.Snapshot("anything")
	if ok {
		t.Fatal("expected false from nil receiver")
	}
}

func TestQuotaMonitor_SkipsUnusableResults(t *testing.T) {
	fetch := func(_ context.Context, _ time.Time) ([]quota.Result, error) {
		return []quota.Result{
			{
				AccountID: "uuid-err",
				Email:     "err@test.com",
				Status:    quota.StatusError,
				Error:     &quota.ErrorInfo{Code: "fetch_error"},
			},
		}, nil
	}

	m := NewQuotaMonitor(fetch, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	_, ok := m.Snapshot("uuid-err")
	if ok {
		t.Fatal("expected unusable result to be skipped")
	}
}
