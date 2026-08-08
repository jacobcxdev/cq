package proxy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexTurnLeaseStateTable(t *testing.T) {
	t.Parallel()
	states := []LeaseState{LeaseReserving, LeaseProvisional, LeaseBoundActive, LeaseContinuationPending, LeaseBoundQuiescent, LeaseOrphaned, LeaseSuperseded, LeaseExpired, LeaseFailedUnadmitted}
	allowed := map[[2]LeaseState]bool{
		{LeaseReserving, LeaseProvisional}: true, {LeaseReserving, LeaseFailedUnadmitted}: true,
		{LeaseProvisional, LeaseProvisional}: true, {LeaseProvisional, LeaseBoundActive}: true, {LeaseProvisional, LeaseFailedUnadmitted}: true,
		{LeaseBoundActive, LeaseContinuationPending}: true, {LeaseBoundActive, LeaseBoundQuiescent}: true, {LeaseBoundActive, LeaseOrphaned}: true,
		{LeaseContinuationPending, LeaseBoundActive}: true, {LeaseContinuationPending, LeaseOrphaned}: true, {LeaseContinuationPending, LeaseSuperseded}: true, {LeaseContinuationPending, LeaseExpired}: true,
		{LeaseBoundQuiescent, LeaseBoundActive}: true, {LeaseBoundQuiescent, LeaseOrphaned}: true, {LeaseBoundQuiescent, LeaseSuperseded}: true, {LeaseBoundQuiescent, LeaseExpired}: true,
		{LeaseOrphaned, LeaseBoundActive}: true, {LeaseOrphaned, LeaseOrphaned}: true, {LeaseOrphaned, LeaseSuperseded}: true, {LeaseOrphaned, LeaseExpired}: true,
	}
	for _, from := range states {
		for _, to := range states {
			if got := validLeaseTransition(from, to); got != allowed[[2]LeaseState{from, to}] {
				t.Fatalf("transition %s -> %s = %v", from, to, got)
			}
		}
	}
}

func TestCodexAttemptStateTable(t *testing.T) {
	t.Parallel()
	states := []CodexAttemptState{CodexAttemptPrepared, CodexAttemptDispatched, CodexAttemptStreaming, CodexAttemptProviderCompleted, CodexAttemptProviderFailed, CodexAttemptIndeterminate}
	allowed := map[[2]CodexAttemptState]bool{
		{CodexAttemptPrepared, CodexAttemptDispatched}:  true,
		{CodexAttemptDispatched, CodexAttemptStreaming}: true, {CodexAttemptDispatched, CodexAttemptProviderFailed}: true, {CodexAttemptDispatched, CodexAttemptIndeterminate}: true,
		{CodexAttemptStreaming, CodexAttemptProviderCompleted}: true, {CodexAttemptStreaming, CodexAttemptProviderFailed}: true, {CodexAttemptStreaming, CodexAttemptIndeterminate}: true,
	}
	for _, from := range states {
		for _, to := range states {
			if got := validCodexAttemptTransition(from, to); got != allowed[[2]CodexAttemptState{from, to}] {
				t.Fatalf("attempt transition %d -> %d = %v", from, to, got)
			}
		}
	}
}

func TestCodexTurnConcurrentAcquireSelectsOnce(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(7, true, nil)
	key := testCodexLeaseKey("thread", "turn")
	var calls atomic.Int32
	selectAccount := func(context.Context) (codex.AccountKey, error) {
		calls.Add(1)
		time.Sleep(time.Millisecond)
		return "account-a", nil
	}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			lease, err := manager.Acquire(context.Background(), key, selectAccount)
			if err != nil || lease.AccountKey != "account-a" {
				t.Errorf("lease = %#v, err = %v", lease, err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("selector calls = %d, want 1", calls.Load())
	}
}

func TestCodexTurnLifecycleAndSupersession(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(1, true, nil)
	first := testCodexLeaseKey("thread", "turn-001")
	lease, err := manager.Acquire(context.Background(), first, fixedCodexAccount("account-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Admit(first, lease.AccountKey, 11, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ObserveCompleted(first, boolPointer(false)); err != nil {
		t.Fatal(err)
	}
	if got, _ := manager.Get(first); got.State != LeaseContinuationPending {
		t.Fatalf("state = %s", got.State)
	}
	if err := manager.ReleaseRouting(first); err != nil {
		t.Fatal(err)
	}
	second := testCodexLeaseKey("thread", "turn-000")
	if _, err := manager.Acquire(context.Background(), second, fixedCodexAccount("account-b")); err != nil {
		t.Fatal(err)
	}
	if got, _ := manager.Get(first); got.State != LeaseSuperseded {
		t.Fatalf("predecessor state = %s", got.State)
	}
	if _, err := manager.Acquire(context.Background(), first, fixedCodexAccount("account-a")); !errors.Is(err, ErrCodexStaleTurn) {
		t.Fatalf("late predecessor error = %v", err)
	}
}

func TestCodexTurnRejectsSuccessorDuringRoutingAndNeverResurrects(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(1, true, nil)
	first := testCodexLeaseKey("thread", "turn-a")
	lease, err := manager.Acquire(context.Background(), first, fixedCodexAccount("account-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Admit(first, lease.AccountKey, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ObserveCompleted(first, nil); err != nil {
		t.Fatal(err)
	}
	second := testCodexLeaseKey("thread", "turn-b")
	if _, err := manager.Acquire(context.Background(), second, fixedCodexAccount("account-b")); !errors.Is(err, ErrCodexConcurrentTurn) {
		t.Fatalf("successor error = %v", err)
	}
	if err := manager.ReleaseRouting(first); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), second, func(context.Context) (codex.AccountKey, error) { return "", errors.New("selection failed") }); err == nil {
		t.Fatal("expected successor failure")
	}
	if got, _ := manager.Get(first); got.State != LeaseSuperseded {
		t.Fatalf("predecessor state = %s", got.State)
	}
}

func TestCodexTurnThreadLanesAndContinuity(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(1, true, nil)
	root := testCodexLeaseKey("root", "turn")
	subagent := testCodexLeaseKey("subagent", "turn")
	rootLease, err := manager.Acquire(context.Background(), root, fixedCodexAccount("account-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), subagent, fixedCodexAccount("account-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Admit(root, rootLease.AccountKey, 19, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTurnState(root, "state-a"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetTurnState(root, "state-b"); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("turn state error = %v", err)
	}
	got, _ := manager.Get(root)
	if err := got.CheckContinuation("account-a", 20, "resp", false); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("socket continuity error = %v", err)
	}
	if err := got.CheckContinuation("account-b", 19, "", true); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("encrypted continuity error = %v", err)
	}
}

func TestCodexTurnJournalFailureMakesLeaseNonMigratable(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(1, true, nil)
	key := testCodexLeaseKey("thread", "turn")
	lease, err := manager.Acquire(context.Background(), key, fixedCodexAccount("account-a"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := manager.Admit(key, lease.AccountKey, 1, func([]CodexTurnLease) error { return errors.New("ENOSPC") })
	if err == nil || !got.NonMigratable || got.State != LeaseBoundActive {
		t.Fatalf("lease = %#v, err = %v", got, err)
	}
}

func TestCodexTurnRestoreDoesNotPromoteShadowEpoch(t *testing.T) {
	t.Parallel()
	manager := NewCodexTurnLeaseManager(9, true, nil)
	shadow := CodexTurnLease{Key: testCodexLeaseKey("thread", "shadow"), State: LeaseBoundQuiescent, AccountKey: "account-a", ModeEpoch: 9, Authoritative: false}
	old := CodexTurnLease{Key: testCodexLeaseKey("thread", "old"), State: LeaseBoundQuiescent, AccountKey: "account-a", ModeEpoch: 8, Authoritative: true}
	current := CodexTurnLease{Key: testCodexLeaseKey("thread", "current"), State: LeaseBoundQuiescent, AccountKey: "account-a", ModeEpoch: 9, Authoritative: true}
	manager.Restore([]CodexTurnLease{shadow, old, current})
	if _, found := manager.Get(shadow.Key); found {
		t.Fatal("shadow lease promoted")
	}
	if _, found := manager.Get(old.Key); found {
		t.Fatal("old epoch lease promoted")
	}
	if got, found := manager.Get(current.Key); !found || got.State != LeaseOrphaned {
		t.Fatalf("current lease = %#v, found = %v", got, found)
	}
}

func TestCodexTurnLeaseEmptyAnchorPreservesKnownResponse(t *testing.T) {
	manager := NewCodexTurnLeaseManager(1, true, nil)
	key := testCodexLeaseKey("thread", "turn")
	if _, err := manager.Acquire(context.Background(), key, fixedCodexAccount("account")); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetResponseAnchor(key, "response", false); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetResponseAnchor(key, "", true); err != nil {
		t.Fatal(err)
	}
	lease, _ := manager.Get(key)
	if lease.ResponseAnchor != "response" || !lease.HasEncryptedState {
		t.Fatalf("lease=%#v", lease)
	}
}

func fixedCodexAccount(account codex.AccountKey) func(context.Context) (codex.AccountKey, error) {
	return func(context.Context) (codex.AccountKey, error) { return account, nil }
}

func testCodexLeaseKey(thread, turn string) LeaseKey {
	return LeaseKey{Lane: LaneKey{Session: "session", Thread: thread, Namespace: CodexResponsesNamespace}, Turn: turn}
}
