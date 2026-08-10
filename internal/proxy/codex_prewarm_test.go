package proxy

import (
	"errors"
	"testing"
)

func TestCodexLegacyPrewarmAdoptionFailsClosed(t *testing.T) {
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
	if _, err := prewarm.Adopt(key, "correlation-a"); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("legacy adoption error = %v", err)
	}
	if _, found := leases.Get(key); found {
		t.Fatal("legacy adoption published a live lease")
	}
	if got := prewarm.snapshot(reservation.Lane); got.State != CodexPrewarmReady {
		t.Fatalf("legacy adoption changed sentinel: %#v", got)
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
	if _, err := prewarm.Adopt(key, "wrong"); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("mismatch error = %v", err)
	}
	restarted := NewCodexPrewarmManager(leases, nil)
	restarted.Restore([]CodexPrewarmReservation{{Lane: reservation.Lane, Correlation: "correlation-a", State: CodexPrewarmReady, AccountKey: "account-a", SocketGeneration: 41, ResponseAnchor: "resp-prewarm"}})
	if _, err := restarted.Adopt(key, "correlation-a"); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
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
