package proxy

import (
	"context"
	"sync"
)

// codexLeasePersistenceGate queues contending mutations instead of polling.
// Releasing the buffered token hands its place to an already waiting sender,
// so successive mutations cannot overtake a queued request.
type codexLeasePersistenceGate struct {
	once  sync.Once
	token chan struct{}
}

func (gate *codexLeasePersistenceGate) Lock() {
	_ = gate.LockContext(context.Background())
}

func (gate *codexLeasePersistenceGate) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gate.once.Do(func() {
		gate.token = make(chan struct{}, 1)
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case gate.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			gate.Unlock()
			return err
		}
		return nil
	}
}

func (gate *codexLeasePersistenceGate) Unlock() {
	select {
	case <-gate.token:
	default:
		panic("unlock of unlocked Codex lease persistence gate")
	}
}
