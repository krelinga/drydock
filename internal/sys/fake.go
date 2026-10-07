package sys

import (
	"sync"
	"time"
)

// FakeClock is a Clock a test moves by hand. Lives beside the real one rather
// than in a test file so every package's tests can share it — the whole point
// of the seam (testing §5.2) is that timing rules are tested by advancing a
// clock, never by sleeping.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewFakeClock starts a fake clock at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// After fires when Advance moves the clock to or past now+d.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at, ch})
	return ch
}

// Advance moves the clock forward and fires any timers that are now due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !c.now.Before(w.at) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// Waiting reports how many After timers are pending. A test that advances
// the clock past a timer must first know the code under test has set it, or
// the advance lands before the wait and the timer never fires.
func (c *FakeClock) Waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// Timer is After with a stop: the returned func removes the timer if it has
// not fired, so a deadline that was not needed (sys.WithTimeout's, once its
// work is done) is not left counted by Waiting.
func (c *FakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	ch := c.After(d)
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		kept := c.waiters[:0]
		for _, w := range c.waiters {
			if (<-chan time.Time)(w.ch) != ch {
				kept = append(kept, w)
			}
		}
		c.waiters = kept
	}
}
