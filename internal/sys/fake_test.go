package sys

import (
	"testing"
	"time"
)

// TestWaitingForCountsOnlyTimersOfThatDuration: two timers on one clock, an
// interval and a read's timeout. Waiting counts both, so it cannot say the
// interval is set; WaitingFor counts the interval alone — and drops it when
// it fires or is stopped, while the other stays counted (the control).
func TestWaitingForCountsOnlyTimersOfThatDuration(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	const interval, timeout = 30 * time.Second, time.Hour
	_, stopTimeout := c.Timer(timeout)
	if n, w := c.WaitingFor(interval), c.Waiting(); n != 0 || w != 1 {
		t.Fatalf("only the timeout set: WaitingFor(interval) = %d, Waiting = %d; want 0 and 1", n, w)
	}
	fired := c.After(interval)
	if n := c.WaitingFor(interval); n != 1 {
		t.Fatalf("WaitingFor(interval) = %d once it is set; want 1", n)
	}
	if n := c.WaitingFor(timeout); n != 1 {
		t.Errorf("control: WaitingFor(timeout) = %d; want 1", n)
	}
	c.Advance(interval)
	<-fired
	if n, w := c.WaitingFor(interval), c.WaitingFor(timeout); n != 0 || w != 1 {
		t.Errorf("after the interval fired: %d intervals and %d timeouts pending; want 0 and 1", n, w)
	}
	// Measured from when it was set, not from now: the same duration again
	// is counted as its own timer.
	_, stopAgain := c.Timer(interval)
	if n := c.WaitingFor(interval); n != 1 {
		t.Errorf("a second interval set: WaitingFor = %d; want 1", n)
	}
	stopAgain()
	stopTimeout()
	if n := c.Waiting(); n != 0 {
		t.Errorf("%d timers left after both were stopped", n)
	}
}
