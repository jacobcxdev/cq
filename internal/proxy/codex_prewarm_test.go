package proxy

import (
	"errors"
	"testing"
)

func TestCodexPrewarmAdoption(t *testing.T) {
	t.Parallel()
	leases := NewCodexTurnLeaseManager(3, true, nil)
	prewarm := NewCodexPrewarmManager(leases, nil)
	metadata := CodexTurnMetadata{SessionID: "session", ThreadID: "thread", RequestKind: CodexRequestPrewarm}
	reservation, err := prewarm.Create(metadata, "correlation-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prewarm.Bind(reservation.Lane, "account-a", 41); err != nil {
		t.Fatal(err)
	}
	if _, err := prewarm.Ready(reservation.Lane, "resp-prewarm", "state-prewarm"); err != nil {
		t.Fatal(err)
	}
	key := LeaseKey{Lane: reservation.Lane, Turn: "turn-real"}
	lease, err := prewarm.Adopt(key, "correlation-a")
	if err != nil {
		t.Fatal(err)
	}
	if lease.AccountKey != "account-a" || lease.UpstreamSocketGeneration != 41 || lease.ResponseAnchor != "resp-prewarm" || lease.TurnState != "state-prewarm" || lease.State != LeaseBoundQuiescent {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestCodexPrewarmMismatchAndRestartCannotAdopt(t *testing.T) {
	t.Parallel()
	leases := NewCodexTurnLeaseManager(3, true, nil)
	prewarm := NewCodexPrewarmManager(leases, nil)
	metadata := CodexTurnMetadata{SessionID: "session", ThreadID: "thread", RequestKind: CodexRequestPrewarm}
	reservation, err := prewarm.Create(metadata, "correlation-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prewarm.Bind(reservation.Lane, "account-a", 41); err != nil {
		t.Fatal(err)
	}
	if _, err := prewarm.Ready(reservation.Lane, "resp-prewarm", "state-prewarm"); err != nil {
		t.Fatal(err)
	}
	key := LeaseKey{Lane: reservation.Lane, Turn: "turn-real"}
	if _, err := prewarm.Adopt(key, "wrong"); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("mismatch error = %v", err)
	}
	restarted := NewCodexPrewarmManager(leases, nil)
	restarted.Restore([]CodexPrewarmReservation{{Lane: reservation.Lane, Correlation: "correlation-a", State: CodexPrewarmReady, AccountKey: "account-a", SocketGeneration: 41, ResponseAnchor: "resp-prewarm"}})
	if _, err := restarted.Adopt(key, "correlation-a"); !errors.Is(err, ErrCodexContinuity) {
		t.Fatalf("restart error = %v", err)
	}
}

func TestCodexPrewarmRejectsUntypedEmptyTurn(t *testing.T) {
	t.Parallel()
	manager := NewCodexPrewarmManager(NewCodexTurnLeaseManager(1, false, nil), nil)
	_, err := manager.Create(CodexTurnMetadata{SessionID: "session", ThreadID: "thread", RequestKind: CodexRequestTurn}, "correlation")
	if err == nil {
		t.Fatal("expected untyped empty turn error")
	}
}
