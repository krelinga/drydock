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
