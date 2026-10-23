package proxy

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestCodexLeasePersistenceWaiterPrecedesRelockingHolder(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	for _, test := range []struct {
		name    string
		acquire func(*CodexLeaseRuntime, *CodexContinuityCoordinator) (func(), error)
	}{
		{
			name: "runtime lifecycle",
			acquire: func(lease *CodexLeaseRuntime, _ *CodexContinuityCoordinator) (func(), error) {
				return lease.beginLifecycleMutationContext(context.Background())
			},
		},
		{
			name: "coordinator persistence",
			acquire: func(_ *CodexLeaseRuntime, coordinator *CodexContinuityCoordinator) (func(), error) {
				return coordinator.beginCodexLeasePersistenceContext(context.Background())
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
			lease := newCodexLeaseRuntimeTest(t, coordinator)
			gate := &coordinator.leases.lifecycle.persistence
			gate.Lock()
			started := make(chan struct{})
			order := make(chan string, 2)
			errors := make(chan error, 1)
			go func() {
				close(started)
				release, err := test.acquire(lease, coordinator)
				if err != nil {
					errors <- err
					return
				}
				order <- "waiter"
				release()
				errors <- nil
			}()
			<-started
			// With one P, yielding lets the waiter park on the held gate before
			// this holder immediately starts its next lifecycle transition.
			runtime.Gosched()
			gate.Unlock()
			gate.Lock()
			order <- "holder"
			gate.Unlock()
			if err := <-errors; err != nil {
				t.Fatal(err)
			}
			if first := <-order; first != "waiter" {
				t.Fatalf("first persistence acquisition = %q, want queued waiter", first)
			}
		})
	}
}

func TestCodexLeasePersistenceGateCancelledWaiterReleasesQueue(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	var gate codexLeasePersistenceGate
	gate.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		err := gate.LockContext(ctx)
		if err == nil {
			gate.Unlock()
		}
		result <- err
	}()
	<-started
	runtime.Gosched()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled persistence wait = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled persistence wait remained blocked")
	}
	if len(gate.token) != 1 {
		t.Fatal("cancelled waiter released the current holder")
	}
	gate.Unlock()
	assertCodexLeasePersistenceGateAvailable(t, &gate)
}

func TestCodexLeasePersistenceGateCancellationDuringHandoff(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	cancelledHandoffs := 0
	for range 100 {
		var gate codexLeasePersistenceGate
		gate.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			close(started)
			err := gate.LockContext(ctx)
			if err == nil {
				gate.Unlock()
			}
			result <- err
		}()
		<-started
		runtime.Gosched()
		// Hand ownership to the queued sender, then cancel before it resumes.
		// Either a successful acquisition or cancellation must release its token.
		gate.Unlock()
		cancel()
		if err := <-result; errors.Is(err, context.Canceled) {
			cancelledHandoffs++
		} else if err != nil {
			t.Fatalf("persistence handoff = %v, want success or context canceled", err)
		}
		assertCodexLeasePersistenceGateAvailable(t, &gate)
	}
	if cancelledHandoffs == 0 {
		t.Fatal("cancelled persistence handoffs were accepted")
	}
}

func TestCodexLeasePersistenceGateRejectsCancelledContext(t *testing.T) {
	var gate codexLeasePersistenceGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.LockContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("already cancelled persistence wait = %v, want context canceled", err)
	}
	assertCodexLeasePersistenceGateAvailable(t, &gate)
}

func assertCodexLeasePersistenceGateAvailable(t *testing.T, gate *codexLeasePersistenceGate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.LockContext(ctx); err != nil {
		t.Fatalf("persistence gate retained a cancelled acquisition: %v", err)
	}
	gate.Unlock()
}
