package sys

import (
	"context"
	"errors"
	"time"
)

// ErrTimedOut is the cause WithTimeout gives a context it ended because its
// time ran out, so a caller can tell its own deadline from its parent's end
// with context.Cause.
var ErrTimedOut = errors.New("timed out")

// WithTimeout is context.WithTimeout on the injected clock: the returned
// context ends when parent does, when cancel is called, or when c.After(d)
// fires — whichever is first — and in the last case its cause is ErrTimedOut.
// A deadline built from context.WithTimeout runs on the wall clock, which a
// test cannot move; this one a FakeClock's Advance expires at once.
//
// d <= 0 means no timeout: the context ends only with its parent or cancel.
func WithTimeout(parent context.Context, c Clock, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if d <= 0 {
		return ctx, func() { cancel(context.Canceled) }
	}
	var fired <-chan time.Time
	stop := func() {}
	if t, ok := c.(timerClock); ok {
		fired, stop = t.Timer(d)
	} else {
		fired = c.After(d)
	}
	go func() {
		select {
		case <-fired:
			cancel(ErrTimedOut)
		case <-ctx.Done():
			stop()
		}
	}()
	// Stopped here too, not only by the goroutine: a caller that cancels
	// and then reads the clock's Waiting must not see this timer.
	return ctx, func() { cancel(context.Canceled); stop() }
}

// timerClock is a Clock whose timers can be stopped; WithTimeout stops the
// one it no longer needs.
type timerClock interface {
	Timer(d time.Duration) (<-chan time.Time, func())
}

// Timer implements timerClock.
func (RealClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// TimedOut reports whether ctx ended because a WithTimeout ran out.
func TimedOut(ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(context.Cause(ctx), ErrTimedOut)
}

// Cleanup returns a context for bookkeeping or cleanup that must still run
// after parent has ended. It keeps parent's values but not its cancellation
// (context.WithoutCancel), and it ends when d passes on c, as WithTimeout's
// does. A record of what happened, a helper container's removal, a sweep
// after a cut-off check: each is owed whether or not the work before it was
// cancelled, and each must still end.
//
// It is rule 3 of the context rule (CLAUDE.md, *Working conventions*). Code
// outside this package should call Cleanup rather than WithoutCancel: the
// meta-test in contextrule_test.go fails on any WithoutCancel there that its
// allowlist does not name. Unlike WithTimeout's, a d <= 0 here does not mean
// "no timeout": a cleanup that never ends is the bug this exists to prevent,
// so Cleanup panics.
func Cleanup(parent context.Context, c Clock, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		panic("sys.Cleanup: a cleanup needs a positive bound")
	}
	return WithTimeout(context.WithoutCancel(parent), c, d)
}
