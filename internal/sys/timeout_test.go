package sys

import (
	"context"
	"testing"
	"time"
)

// TestWithTimeoutRunsOnTheInjectedClock: the deadline is the fake clock's,
// so advancing it ends the context with ErrTimedOut as its cause — and a
// parent's end or a cancel is never mistaken for a timeout (the controls).
func TestWithTimeoutRunsOnTheInjectedClock(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	ctx, cancel := WithTimeout(context.Background(), c, time.Minute)
	defer cancel()
	c.Advance(59 * time.Second)
	if ctx.Err() != nil {
		t.Fatal("ended before its time")
	}
	c.Advance(time.Second)
	<-ctx.Done()
	if !TimedOut(ctx) {
		t.Fatalf("cause = %v; want ErrTimedOut", context.Cause(ctx))
	}

	parent, stop := context.WithCancel(context.Background())
	ctx2, cancel2 := WithTimeout(parent, c, time.Minute)
	defer cancel2()
	stop()
	<-ctx2.Done()
	if TimedOut(ctx2) {
		t.Fatal("a parent's end read as a timeout")
	}
	ctx3, cancel3 := WithTimeout(context.Background(), c, time.Minute)
	cancel3()
	if TimedOut(ctx3) {
		t.Fatal("a cancel read as a timeout")
	}
	ctx4, cancel4 := WithTimeout(context.Background(), c, 0)
	defer cancel4()
	c.Advance(time.Hour)
	if ctx4.Err() != nil {
		t.Fatal("d <= 0 must mean no timeout")
	}
}

type cleanupKey struct{}

// TestCleanupOutlivesItsParentOnTheInjectedClock: a cleanup context keeps
// running after its parent is cancelled — the point of it — keeps the
// parent's values, and still ends, when the fake clock passes its bound,
// with ErrTimedOut as its cause. The control: before the bound, with the
// parent long gone, it has not ended.
func TestCleanupOutlivesItsParentOnTheInjectedClock(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	parent, stop := context.WithCancel(context.WithValue(context.Background(), cleanupKey{}, "v"))
	stop()
	ctx, cancel := Cleanup(parent, c, time.Minute)
	defer cancel()
	if ctx.Err() != nil {
		t.Fatal("a cleanup context ended with its cancelled parent")
	}
	if got := ctx.Value(cleanupKey{}); got != "v" {
		t.Fatalf("value = %v; a cleanup keeps its parent's values", got)
	}
	if c.Waiting() != 1 {
		t.Fatalf("Waiting = %d; the bound must be on the injected clock", c.Waiting())
	}
	c.Advance(59 * time.Second)
	if ctx.Err() != nil {
		t.Fatal("ended before its bound")
	}
	c.Advance(time.Second)
	<-ctx.Done()
	if !TimedOut(ctx) {
		t.Fatalf("cause = %v; want ErrTimedOut", context.Cause(ctx))
	}
}

// TestCleanupCancelReleasesItsTimer: cancel ends it at once, not as a
// timeout, and takes its timer off the fake clock.
func TestCleanupCancelReleasesItsTimer(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	ctx, cancel := Cleanup(context.Background(), c, time.Minute)
	cancel()
	<-ctx.Done()
	if TimedOut(ctx) {
		t.Fatal("a cancel read as a timeout")
	}
	if c.Waiting() != 0 {
		t.Fatalf("Waiting = %d after cancel; want 0", c.Waiting())
	}
}

// TestCleanupRefusesAnUnboundedCleanup: d <= 0 is a bug, not "no timeout".
func TestCleanupRefusesAnUnboundedCleanup(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Cleanup(%v) did not panic", d)
				}
			}()
			Cleanup(context.Background(), NewFakeClock(time.Unix(0, 0)), d)
		}()
	}
}
