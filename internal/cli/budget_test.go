package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeBudgetTimer struct {
	at      time.Time
	fn      func()
	stopped bool
}

func (t *fakeBudgetTimer) Stop() bool { active := !t.stopped; t.stopped = true; return active }

type fakeBudgetClock struct {
	now    time.Time
	timers []*fakeBudgetTimer
}

func newBudgetClock() *fakeBudgetClock {
	return &fakeBudgetClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
}
func (c *fakeBudgetClock) Now() time.Time { return c.now }
func (c *fakeBudgetClock) AfterFunc(d time.Duration, fn func()) budgetTimer {
	t := &fakeBudgetTimer{at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}
func (c *fakeBudgetClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
	for _, timer := range c.timers {
		if !timer.stopped && !timer.at.After(c.now) {
			timer.stopped = true
			timer.fn()
		}
	}
}
func checkBudgetDeadline(t *testing.T, ctx context.Context, want time.Time) {
	t.Helper()
	got, ok := ctx.Deadline()
	if !ok || !got.Equal(want) {
		t.Fatalf("deadline=%v %t want %v", got, ok, want)
	}
}
func checkBudgetError(t *testing.T, ctx context.Context, want error) {
	t.Helper()
	if !errors.Is(ctx.Err(), want) {
		t.Fatalf("context error=%v want %v", ctx.Err(), want)
	}
	if want != nil {
		select {
		case <-ctx.Done():
		default:
			t.Fatal("expired context not done")
		}
	}
}

func TestCLIV2BudgetConsentReserveInsideTotal(t *testing.T) {
	c := newBudgetClock()
	start := c.Now()
	b := beginBudget(context.Background(), 30*time.Second, 5*time.Second, c)
	defer b.Close()
	checkBudgetDeadline(t, b.Work(), start.Add(25*time.Second))
	checkBudgetDeadline(t, b.Cleanup(), start.Add(30*time.Second))
	c.advance(20 * time.Second)
	checkBudgetError(t, b.Work(), nil)
	oldWork, oldCleanup := b.Work(), b.Cleanup()
	b.Pause()
	b.Pause()
	checkBudgetError(t, oldWork, context.Canceled)
	checkBudgetError(t, oldCleanup, context.Canceled)
	for _, timer := range c.timers {
		if !timer.stopped {
			t.Fatal("pause left timer running")
		}
	}
	c.advance(60 * time.Second)
	b.Resume()
	b.Resume()
	if b.Work() == oldWork || b.Cleanup() == oldCleanup {
		t.Fatal("resume reused cancelled contexts")
	}
	checkBudgetDeadline(t, b.Work(), start.Add(85*time.Second))
	checkBudgetDeadline(t, b.Cleanup(), start.Add(90*time.Second))
	c.advance(5 * time.Second)
	checkBudgetError(t, b.Work(), context.DeadlineExceeded)
	checkBudgetError(t, b.Cleanup(), nil)
	c.advance(5 * time.Second)
	checkBudgetError(t, b.Cleanup(), context.DeadlineExceeded)
	b.Pause()
	c.advance(time.Hour)
	b.Resume()
	checkBudgetError(t, b.Work(), context.DeadlineExceeded)
	checkBudgetError(t, b.Cleanup(), context.DeadlineExceeded)
}

func TestCLIV2BudgetOriginalParentDuringConsent(t *testing.T) {
	c := newBudgetClock()
	start := c.Now()
	parent := beginBudget(context.Background(), 40*time.Second, 0, c)
	defer parent.Close()
	b := beginBudget(parent.Work(), 30*time.Second, 5*time.Second, c)
	defer b.Close()
	c.advance(20 * time.Second)
	b.Pause()
	c.advance(60 * time.Second)
	b.Resume()
	checkBudgetError(t, b.Work(), context.DeadlineExceeded)
	checkBudgetError(t, b.Cleanup(), context.DeadlineExceeded)
	checkBudgetDeadline(t, b.Cleanup(), start.Add(40*time.Second))
}

func TestCLIV2BudgetParentBoundsAndValues(t *testing.T) {
	type key struct{}
	c := newBudgetClock()
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
	b := beginBudget(parent, time.Minute, 5*time.Second, c)
	defer b.Close()
	if b.Work().Value(key{}) != "value" || b.Cleanup().Value(key{}) != "value" {
		t.Fatal("lost parent values")
	}
	cancel()
	// Parent cancellation propagation is asynchronous in context itself. Done,
	// rather than a wall-clock sleep, synchronises the observable cancellation.
	<-b.Work().Done()
	<-b.Cleanup().Done()
	checkBudgetError(t, b.Work(), context.Canceled)
	checkBudgetError(t, b.Cleanup(), context.Canceled)
	b.Pause()
	b.Resume()
	checkBudgetError(t, b.Work(), context.Canceled)
	parentBudget := beginBudget(context.Background(), 10*time.Second, 0, c)
	defer parentBudget.Close()
	child := beginBudget(parentBudget.Work(), time.Minute, 5*time.Second, c)
	defer child.Close()
	checkBudgetDeadline(t, child.Work(), c.Now().Add(5*time.Second))
	checkBudgetDeadline(t, child.Cleanup(), c.Now().Add(10*time.Second))
	c.advance(5 * time.Second)
	checkBudgetError(t, child.Work(), context.DeadlineExceeded)
	checkBudgetError(t, child.Cleanup(), nil)
	// An earlier parent can consume the entire work allowance in reserve.
	reserved := beginBudget(parentBudget.Work(), time.Minute, 8*time.Second, c)
	defer reserved.Close()
	checkBudgetError(t, reserved.Work(), context.DeadlineExceeded)
	checkBudgetError(t, reserved.Cleanup(), nil)
	checkBudgetDeadline(t, reserved.Cleanup(), c.Now().Add(5*time.Second))
}

func TestCLIV2BudgetExpiryAndClose(t *testing.T) {
	for _, tc := range []struct{ total, reserve time.Duration }{{0, 0}, {-time.Second, 0}, {time.Second, 2 * time.Second}, {time.Second, -time.Second}} {
		c := newBudgetClock()
		b := beginBudget(context.Background(), tc.total, tc.reserve, c)
		if tc.total <= 0 || tc.reserve >= tc.total {
			checkBudgetError(t, b.Work(), context.DeadlineExceeded)
		}
		if tc.total > 0 {
			c.advance(tc.total)
		}
		checkBudgetError(t, b.Cleanup(), context.DeadlineExceeded)
		b.Close()
		b.Close()
		b.Resume()
		checkBudgetError(t, b.Cleanup(), context.DeadlineExceeded)
	}
	c := newBudgetClock()
	b := beginBudget(context.Background(), time.Minute, time.Second, c)
	b.Close()
	b.Resume()
	checkBudgetError(t, b.Work(), context.Canceled)
	checkBudgetError(t, b.Cleanup(), context.Canceled)
	for _, timer := range c.timers {
		if !timer.stopped {
			t.Fatal("close leaked timer")
		}
	}
	// Exercise the production clock without sleeping.
	real := BeginBudget(context.Background(), 0, 0)
	defer real.Close()
	checkBudgetError(t, real.Work(), context.DeadlineExceeded)
}

func TestCLIV2BudgetDerivedRequestContext(t *testing.T) {
	c := newBudgetClock()
	b := beginBudget(context.Background(), time.Minute, 5*time.Second, c)
	defer b.Close()
	request, cancel := context.WithCancel(b.Work())
	defer cancel()
	c.advance(55 * time.Second)
	<-request.Done()
	checkBudgetError(t, request, context.DeadlineExceeded)
	if context.Cause(request) != context.DeadlineExceeded {
		t.Fatalf("request cause: %v", context.Cause(request))
	}
}

func TestCLIV2BudgetParentErrorKind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent func() (context.Context, func())
		want   error
	}{
		{"deadline with custom cause", func() (context.Context, func()) {
			parent, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), errors.New("custom deadline cause"))
			t.Cleanup(cancel)
			return parent, func() {}
		}, context.DeadlineExceeded},
		{"cancel with deadline cause", func() (context.Context, func()) {
			parent, cancel := context.WithCancelCause(context.Background())
			t.Cleanup(func() { cancel(nil) })
			return parent, func() { cancel(context.DeadlineExceeded) }
		}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, trigger := tc.parent()
			budget := BeginBudget(parent, time.Minute, 5*time.Second)
			defer budget.Close()
			workRequest, cancelWork := context.WithCancel(budget.Work())
			defer cancelWork()
			cleanupRequest, cancelCleanup := context.WithCancel(budget.Cleanup())
			defer cancelCleanup()
			trigger()
			for name, ctx := range map[string]context.Context{"work": budget.Work(), "cleanup": budget.Cleanup(), "work request": workRequest, "cleanup request": cleanupRequest} {
				<-ctx.Done()
				if ctx.Err() != tc.want {
					t.Errorf("%s Err=%v want parent Err=%v (cause %v)", name, ctx.Err(), tc.want, context.Cause(parent))
				}
			}
		})
	}
}
