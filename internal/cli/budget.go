package cli

import (
	"context"
	"errors"
	"time"
)

// Only this private cause identifies our timer. A parent's cause is arbitrary
// caller data and must never be interpreted as its cancellation error kind.
var budgetDeadlineCause = errors.New("budget deadline expired")

type budgetTimer interface{ Stop() bool }
type budgetClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) budgetTimer
}
type realBudgetClock struct{}

func (realBudgetClock) Now() time.Time { return time.Now() }
func (realBudgetClock) AfterFunc(d time.Duration, fn func()) budgetTimer {
	return time.AfterFunc(d, fn)
}

// Budget tracks one monotonic allowance. The original parent remains in force
// during consent, while the allowance itself is suspended. Methods are owned by
// the handler and must not run concurrently. Pause requires quiescent work.
// Obtain fresh Work/Cleanup contexts after every Resume.
type Budget struct {
	parent                    context.Context
	clock                     budgetClock
	remaining, reserve        time.Duration
	started                   time.Time
	work, cleanup             context.Context
	cancelWork, cancelCleanup context.CancelCauseFunc
	workTimer, cleanupTimer   budgetTimer
	paused, closed            bool
}

func BeginBudget(parent context.Context, total, cleanupReserve time.Duration) *Budget {
	return beginBudget(parent, total, cleanupReserve, realBudgetClock{})
}

func beginBudget(parent context.Context, total, cleanupReserve time.Duration, clock budgetClock) *Budget {
	total = max(total, 0)
	b := &Budget{parent: parent, clock: clock, remaining: total, reserve: min(max(cleanupReserve, 0), total), paused: true}
	b.Resume()
	return b
}

func (b *Budget) Work() context.Context    { return b.work }
func (b *Budget) Cleanup() context.Context { return b.cleanup }

func (b *Budget) Pause() {
	if b.paused || b.closed {
		return
	}
	b.remaining = max(0, b.remaining-b.clock.Now().Sub(b.started))
	b.stop()
	b.paused = true
}

func (b *Budget) Resume() {
	if !b.paused || b.closed {
		return
	}
	b.started = b.clock.Now()
	effective := b.remaining
	if parentDeadline, ok := b.parent.Deadline(); ok {
		effective = min(effective, max(0, parentDeadline.Sub(b.started)))
	}
	b.work, b.cancelWork, b.workTimer = b.deadline(max(0, effective-b.reserve))
	b.cleanup, b.cancelCleanup, b.cleanupTimer = b.deadline(effective)
	b.paused = false
}

func (b *Budget) Close() {
	if b.closed {
		return
	}
	b.stop()
	b.closed = true
}

func (b *Budget) stop() {
	if b.workTimer != nil {
		b.workTimer.Stop()
	}
	if b.cleanupTimer != nil {
		b.cleanupTimer.Stop()
	}
	b.cancelWork(context.Canceled)
	b.cancelCleanup(context.Canceled)
}

// The injected timer controls expiry rather than time.Until, which would mix
// the real clock into deterministic tests. WithCancelCause still propagates
// cancellation and values from the original parent and releases registration
// when a phase is cancelled. deadlineContext exposes normal Context.Err values.
func (b *Budget) deadline(remaining time.Duration) (context.Context, context.CancelCauseFunc, budgetTimer) {
	deadline := b.started.Add(remaining)
	if parentDeadline, ok := b.parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	ctx, cancel := context.WithCancelCause(b.parent)
	wrapped := deadlineContext{Context: ctx, deadline: deadline}
	duration := deadline.Sub(b.started)
	if duration <= 0 {
		cancel(budgetDeadlineCause)
		return wrapped, cancel, nil
	}
	timer := b.clock.AfterFunc(duration, func() { cancel(budgetDeadlineCause) })
	return wrapped, cancel, timer
}

type deadlineContext struct {
	context.Context
	deadline time.Time
}

func (c deadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

// Hide the underlying cancellation implementation from context's fast path:
// its Err is Canceled with our private expiry cause, while our public Err must
// be DeadlineExceeded. Derived request contexts must use this public contract.
// WithoutCancel hides only context's private cancellation key, retaining all
// caller values. AfterFunc provides cancellation without a waiting goroutine.
func (c deadlineContext) Value(key any) any {
	return context.WithoutCancel(c.Context).Value(key)
}
func (c deadlineContext) AfterFunc(fn func()) func() bool {
	return context.AfterFunc(c.Context, fn)
}
func (c deadlineContext) Err() error {
	err := c.Context.Err()
	if err == nil {
		return nil
	}
	if context.Cause(c.Context) == budgetDeadlineCause {
		return context.DeadlineExceeded
	}
	return err
}
