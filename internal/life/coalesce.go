package life

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// ErrNotStarted is a Coalescer's Trigger or Await before Start.
var ErrNotStarted = errors.New("not started")

// ErrNoTicket is Await of a ticket this coalescer never issued: zero, which
// no Trigger returns (it is what a caller holds after ignoring Trigger's
// error), or one above every ticket issued so far. Never "answered".
var ErrNoTicket = errors.New("life: no such ticket")

// Ticket is one request for a run: the value of the asked counter the
// Trigger that made it left behind. It is answered once a run that began
// after it has ended. Zero is no ticket.
type Ticket uint64

// Coalescer runs Work on one goroutine it owns: every Interval, and whenever
// Trigger asks, with requests made while a run is going answered by one more
// run after it. Set the exported fields, then Start it under a Group.
//
// A request may carry a payload of type P (TriggerWith): what the run that
// answers it must know about it — that an answer is owed, a fact to record.
// Each payload is handed to exactly one run, the one that answers its
// ticket, so it never reaches a run that began before it was asked; one
// whose ticket no run answers (the worker ended first) is dropped. A
// coalescer whose requests carry nothing uses struct{} and Trigger.
type Coalescer[R, P any] struct {
	// Work is one run. Its context is the group's; it is never called
	// concurrently with itself. asks are the payloads of the requests this
	// run answers, in the order they were made: empty for a run nobody
	// asked for, and for requests made with Trigger.
	Work func(ctx context.Context, asks []P) (R, error)
	// Clock times Interval.
	Clock sys.Clock
	// Interval is the period between runs that nobody asked for, timed from
	// the end of the last run. Zero or less: none.
	Interval time.Duration

	mu      sync.Mutex
	ctx     context.Context // the group's; nil before Start
	asked   uint64          // the newest ticket issued
	done    uint64          // every ticket up to this one is answered
	exited  bool            // the worker has ended, or never began
	last    R               // the newest run's result
	lastErr error
	asks    []P           // payloads of tickets above done that no run has begun for
	poke    chan struct{} // capacity 1: "asked moved", never blocks Trigger
	changed chan struct{} // closed and replaced whenever done or exited moves
}

// Start runs the worker under g, as the goroutine called name. A group
// already stopping starts nothing, and the coalescer then refuses every
// request with ErrStopping. Start must be called once.
func (c *Coalescer[R, P]) Start(g *Group, name string) error {
	c.mu.Lock()
	if c.ctx != nil {
		c.mu.Unlock()
		panic("life: Coalescer started twice")
	}
	c.ctx = g.Ctx()
	c.poke = make(chan struct{}, 1)
	c.changed = make(chan struct{})
	c.mu.Unlock()
	if err := g.TryGo(name, c.loop); err != nil {
		c.mu.Lock()
		c.exited = true
		c.broadcastLocked()
		c.mu.Unlock()
		return err
	}
	return nil
}

// Trigger asks for a run and returns at once with the ticket that run will
// answer: one that begins after this call, never one already going. It is
// ErrNotStarted before Start and ErrStopping once the group is stopping.
func (c *Coalescer[R, P]) Trigger() (Ticket, error) { return c.trigger(nil) }

// TriggerWith is Trigger carrying p, which Work receives in asks on the run
// that answers the ticket — never on one that began before this call — and
// which is dropped if the worker ends with the ticket unanswered.
func (c *Coalescer[R, P]) TriggerWith(p P) (Ticket, error) { return c.trigger(&p) }

func (c *Coalescer[R, P]) trigger(p *P) (Ticket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.ctx == nil:
		return 0, ErrNotStarted
	case c.exited || c.ctx.Err() != nil:
		return 0, ErrStopping
	}
	c.asked++
	if p != nil {
		// Under the lock asked moves under: the run that reads asked as its
		// generation takes exactly the payloads asked up to it.
		c.asks = append(c.asks, *p)
	}
	select {
	case c.poke <- struct{}{}:
	default: // a poke is already waiting; the worker reads asked, not pokes
	}
	return Ticket(c.asked), nil
}

// Await waits until t is answered and returns the newest run's result — a
// run that began after t was issued. It returns ErrStopping if the worker
// ends with t unanswered, and ctx's error if ctx ends first; a caller that
// gives up cancels nothing, since the run is shared. A ticket this
// coalescer never issued, zero above all, is ErrNoTicket.
func (c *Coalescer[R, P]) Await(ctx context.Context, t Ticket) (R, error) {
	var zero R
	for {
		c.mu.Lock()
		switch {
		case c.ctx == nil:
			c.mu.Unlock()
			return zero, ErrNotStarted
		case t == 0 || uint64(t) > c.asked:
			c.mu.Unlock()
			return zero, ErrNoTicket
		case uint64(t) <= c.done:
			r, err := c.last, c.lastErr
			c.mu.Unlock()
			return r, err
		case c.exited:
			c.mu.Unlock()
			return zero, ErrStopping
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}

// TriggerAndWait is Trigger then Await: a run that begins after the call,
// and its result.
func (c *Coalescer[R, P]) TriggerAndWait(ctx context.Context) (R, error) {
	t, err := c.Trigger()
	if err != nil {
		var zero R
		return zero, err
	}
	return c.Await(ctx, t)
}

// Asked is the newest ticket issued: how many runs have been asked for in
// all. A test's way to know its callers have asked, without sleeping.
func (c *Coalescer[R, P]) Asked() Ticket {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Ticket(c.asked)
}

func (c *Coalescer[R, P]) broadcastLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// loop is the worker: it waits for a poke, the interval or the end of ctx,
// and runs while anything is asked. It never runs once ctx has ended, and as
// it exits every ticket no run began for is refused.
func (c *Coalescer[R, P]) loop(ctx context.Context) {
	defer func() {
		c.mu.Lock()
		c.exited = true
		c.asks = nil // their tickets are refused: no run will see them
		c.broadcastLocked()
		c.mu.Unlock()
	}()
	// The period is timed from when the worker goes idle: armed as it
	// starts to wait, stopped once a run begins, so every run — asked for
	// or periodic — restarts it, and a test that sees the timer set knows
	// no run is going.
	var tick <-chan time.Time
	stopTick := func() {}
	defer func() { stopTick() }()
	for {
		c.mu.Lock()
		pending := c.asked > c.done
		c.mu.Unlock()
		if !pending {
			if tick == nil && c.Interval > 0 {
				tick, stopTick = sys.NewTimer(c.Clock, c.Interval)
			}
			select {
			case <-ctx.Done():
				return
			case <-c.poke:
				continue // read asked again
			case <-tick:
			}
		}
		if ctx.Err() != nil {
			return
		}
		stopTick()
		tick, stopTick = nil, func() {}
		// gen is read as the run begins: it answers only tickets issued
		// before this point. One issued during the run is above gen, so the
		// loop goes round again at once. The payloads are taken with it:
		// every one in asks was asked at or before gen, so each reaches the
		// run that answers its ticket and no other.
		c.mu.Lock()
		gen := c.asked
		asks := c.asks
		c.asks = nil
		c.mu.Unlock()
		r, err := c.Work(ctx, asks)
		c.mu.Lock()
		c.done = gen
		c.last, c.lastErr = r, err
		c.broadcastLocked()
		c.mu.Unlock()
	}
}
