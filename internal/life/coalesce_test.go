package life

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// The coalescer's tests are pure coordination, so they run in a synctest
// bubble (the concurrency study's R8): synctest.Wait returns once every
// goroutine is durably blocked, so "the worker is waiting" and "the run has
// parked" are one line each, and the bubble's clock moves only when all of
// them are — sys.RealClock inside it is a fake clock no test has to drive.

// runs is a Work that counts its runs and returns each one's number (1, 2,
// …), parked on gate while it is set.
type runs struct {
	mu      sync.Mutex
	started int
	gate    chan struct{}
}

func (r *runs) work(ctx context.Context, _ []struct{}) (int, error) {
	r.mu.Lock()
	r.started++
	n, gate := r.started, r.gate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return n, ctx.Err()
		}
	}
	return n, nil
}

func (r *runs) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

func (r *runs) hold() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate = make(chan struct{})
	return r.gate
}

func start(t *testing.T, r *runs, interval time.Duration) (*Coalescer[int, struct{}], *Group) {
	t.Helper()
	g := NewGroup(context.Background())
	c := &Coalescer[int, struct{}]{Work: r.work, Clock: sys.RealClock{}, Interval: interval}
	if err := c.Start(g, "work"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Wait(nil) })
	return c, g
}

// TestATriggerDuringARunGetsExactlyOneMore: tickets issued while a run is
// going are not answered by it — it may have read its input before they
// were asked — but by one more run after it, at once, however many there
// are; and the ticket issued before that run is answered by it.
func TestATriggerDuringARunGetsExactlyOneMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runs{}
		c, _ := start(t, r, 0)
		release := r.hold()
		first, _ := c.Trigger()
		synctest.Wait() // the first run is parked on the gate
		if r.count() != 1 {
			t.Fatalf("control: %d runs; want 1", r.count())
		}
		var during []Ticket
		for i := 0; i < 3; i++ {
			tk, err := c.Trigger()
			if err != nil {
				t.Fatal(err)
			}
			during = append(during, tk)
		}
		synctest.Wait()
		if r.count() != 1 {
			t.Errorf("%d runs while the first was going; want it alone", r.count())
		}
		r.mu.Lock()
		r.gate = nil
		r.mu.Unlock()
		close(release)
		synctest.Wait()
		if n, err := c.Await(context.Background(), first); n != 2 || err != nil {
			// 2, not 1: the newest result, from a run that also began
			// after first was issued.
			t.Errorf("the first ticket: %d, %v; want the newest run's result, 2", n, err)
		}
		for _, tk := range during {
			if n, err := c.Await(context.Background(), tk); n != 2 || err != nil {
				t.Errorf("a ticket issued during run 1 was answered by run %d (%v); want run 2", n, err)
			}
		}
		if r.count() != 2 {
			t.Errorf("%d runs in all; want the first and exactly one more", r.count())
		}
		// And the worker is idle: no time passing starts another.
		time.Sleep(time.Hour)
		synctest.Wait()
		if r.count() != 2 {
			t.Errorf("%d runs after an idle hour with no interval; want 2", r.count())
		}
	})
}

// TestEveryTicketIsAnsweredByARunThatBeganAfterIt: many callers trigger and
// wait at once, while runs come and go; each is answered by a run whose
// number is above every run begun when it asked.
func TestEveryTicketIsAnsweredByARunThatBeganAfterIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		started := 0
		work := func(ctx context.Context, _ []struct{}) (int, error) {
			mu.Lock()
			started++
			n := started
			mu.Unlock()
			time.Sleep(time.Second) // bubble time: a run takes a while
			return n, nil
		}
		g := NewGroup(context.Background())
		c := &Coalescer[int, struct{}]{Work: work, Clock: sys.RealClock{}}
		if err := c.Start(g, "work"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				time.Sleep(time.Duration(i%7) * 300 * time.Millisecond)
				mu.Lock()
				// A run that began before the ticket would have a number
				// no greater than this.
				begun := started
				tk, err := c.Trigger()
				mu.Unlock()
				if err != nil {
					t.Error(err)
					return
				}
				n, err := c.Await(context.Background(), tk)
				if err != nil {
					t.Error(err)
				}
				if n <= begun {
					t.Errorf("ticket %d answered by run %d, which began before it (%d had)", tk, n, begun)
				}
			}(i)
		}
		wg.Wait()
		g.Wait(nil)
		if started > 40 {
			t.Errorf("%d runs for 40 tickets", started)
		}
	})
}

// TestTriggerAndWait returns the result of a run begun after the call, and
// a caller whose context ends stops waiting without cancelling the run.
func TestTriggerAndWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runs{}
		c, _ := start(t, r, 0)
		if n, err := c.TriggerAndWait(context.Background()); n != 1 || err != nil {
			t.Errorf("TriggerAndWait: %d, %v; want run 1", n, err)
		}
		release := r.hold()
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan error)
		go func() { _, err := c.TriggerAndWait(ctx); got <- err }()
		synctest.Wait()
		cancel()
		if err := <-got; !errors.Is(err, context.Canceled) {
			t.Errorf("a caller that gave up: %v; want its context's error", err)
		}
		r.mu.Lock()
		r.gate = nil
		r.mu.Unlock()
		close(release)
		if n, err := c.TriggerAndWait(context.Background()); n != 3 || err != nil {
			t.Errorf("TriggerAndWait after: %d, %v; want run 3 (run 2 was not cancelled)", n, err)
		}
	})
}

// TestShutdownAnswersOrRefusesEveryTicket: at Stop, a ticket the running run
// covers is answered by it (cut off, with its context's error); one issued
// after it began is refused with ErrStopping, and that run never starts; a
// Trigger after Stop is refused at once. Before Start, both say so.
func TestShutdownAnswersOrRefusesEveryTicket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var unstarted Coalescer[int, struct{}]
		if _, err := unstarted.Trigger(); !errors.Is(err, ErrNotStarted) {
			t.Errorf("Trigger before Start: %v; want ErrNotStarted", err)
		}
		if _, err := unstarted.Await(context.Background(), 1); !errors.Is(err, ErrNotStarted) {
			t.Errorf("Await before Start: %v; want ErrNotStarted", err)
		}

		r := &runs{}
		c, g := start(t, r, 0)
		r.hold()
		covered, _ := c.Trigger()
		synctest.Wait()
		queued, err := c.Trigger()
		if err != nil {
			t.Fatalf("control: a Trigger before Stop: %v", err)
		}
		g.Stop()
		if _, err := c.Trigger(); !errors.Is(err, ErrStopping) {
			t.Errorf("Trigger after Stop: %v; want ErrStopping", err)
		}
		if late := g.Wait(nil); late != nil {
			t.Errorf("stragglers %v", late)
		}
		if n, err := c.Await(context.Background(), covered); n != 1 || !errors.Is(err, context.Canceled) {
			t.Errorf("the ticket the running run covered: %d, %v; want run 1, cut off", n, err)
		}
		if _, err := c.Await(context.Background(), queued); !errors.Is(err, ErrStopping) {
			t.Errorf("a ticket no run began for: %v; want ErrStopping", err)
		}
		if r.count() != 1 {
			t.Errorf("%d runs; want none after Stop", r.count())
		}

		// A coalescer started under a group already stopping runs nothing
		// and refuses everything.
		late := &Coalescer[int, struct{}]{Work: r.work, Clock: sys.RealClock{}}
		if err := late.Start(g, "late"); !errors.Is(err, ErrStopping) {
			t.Errorf("Start under a stopped group: %v; want ErrStopping", err)
		}
		if _, err := late.Trigger(); !errors.Is(err, ErrStopping) {
			t.Errorf("Trigger on a coalescer that never started: %v; want ErrStopping", err)
		}
	})
}

// TestTheIntervalRunsAndRestartsAfterEveryRun: a run nobody asked for comes
// one Interval after the worker went idle, and any run — asked for, too —
// restarts that period.
func TestTheIntervalRunsAndRestartsAfterEveryRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runs{}
		c, _ := start(t, r, 15*time.Minute)
		time.Sleep(15*time.Minute - time.Second)
		synctest.Wait()
		if r.count() != 0 {
			t.Fatalf("%d runs before the interval", r.count())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if r.count() != 1 {
			t.Fatalf("%d runs at the interval; want 1", r.count())
		}
		time.Sleep(10 * time.Minute)
		if _, err := c.TriggerAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		// The period now runs from the asked-for run, not the periodic one.
		time.Sleep(15*time.Minute - time.Second)
		synctest.Wait()
		if r.count() != 2 {
			t.Errorf("%d runs; want no periodic run 15 minutes after the last periodic one", r.count())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if r.count() != 3 {
			t.Errorf("%d runs; want a periodic run 15 minutes after the asked-for one", r.count())
		}
	})
}

// TestAPayloadReachesTheRunThatAnswersItsTicket: a payload asked while a run
// is going is not handed to that run — it may have read what the payload is
// about already — but to the next, which answers its ticket; payloads asked
// before a run are all handed to it, in order, and to no other run. The
// controls: Trigger carries nothing, and a run nobody asked for gets nothing.
func TestAPayloadReachesTheRunThatAnswersItsTicket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var got [][]string
		var gate chan struct{}
		work := func(ctx context.Context, asks []string) (int, error) {
			mu.Lock()
			got = append(got, asks)
			n, g := len(got), gate
			mu.Unlock()
			if g != nil {
				<-g
			}
			return n, nil
		}
		g := NewGroup(context.Background())
		t.Cleanup(func() { g.Wait(nil) })
		c := &Coalescer[int, string]{Work: work, Clock: sys.RealClock{}, Interval: time.Hour}
		if err := c.Start(g, "work"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		gate = make(chan struct{})
		release := gate
		mu.Unlock()
		first, _ := c.TriggerWith("a")
		synctest.Wait() // run 1 is parked, holding "a"
		during, _ := c.TriggerWith("b")
		c.Trigger()
		last, _ := c.TriggerWith("c")
		mu.Lock()
		gate = nil
		mu.Unlock()
		close(release)
		synctest.Wait() // both runs have ended
		if n, err := c.Await(context.Background(), first); n != 2 || err != nil {
			t.Fatalf("first: %d, %v; want the newest run's result, 2", n, err)
		}
		for _, tk := range []Ticket{during, last} {
			if n, err := c.Await(context.Background(), tk); n != 2 || err != nil {
				t.Errorf("ticket %d answered by run %d (%v); want run 2", tk, n, err)
			}
		}
		time.Sleep(time.Hour) // the interval: a run nobody asked for
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		want := [][]string{{"a"}, {"b", "c"}, nil}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("runs saw %q; want %q", got, want)
		}
	})
}

// TestAPayloadWhoseTicketIsRefusedReachesNoRun: at Stop, a payload asked
// during the last run is dropped with its ticket; no run sees it, and
// TriggerWith after Stop is refused. The control is the running run's own
// payload, which it saw.
func TestAPayloadWhoseTicketIsRefusedReachesNoRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var got []string
		work := func(ctx context.Context, asks []string) (int, error) {
			mu.Lock()
			got = append(got, asks...)
			mu.Unlock()
			<-ctx.Done()
			return 0, ctx.Err()
		}
		g := NewGroup(context.Background())
		c := &Coalescer[int, string]{Work: work, Clock: sys.RealClock{}}
		if err := c.Start(g, "work"); err != nil {
			t.Fatal(err)
		}
		c.TriggerWith("running")
		synctest.Wait()
		queued, _ := c.TriggerWith("queued")
		g.Wait(nil)
		if _, err := c.Await(context.Background(), queued); !errors.Is(err, ErrStopping) {
			t.Errorf("the queued ticket: %v; want ErrStopping", err)
		}
		if _, err := c.TriggerWith("late"); !errors.Is(err, ErrStopping) {
			t.Errorf("TriggerWith after Stop: %v; want ErrStopping", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !reflect.DeepEqual(got, []string{"running"}) {
			t.Errorf("runs saw %q; want the running one's payload alone", got)
		}
	})
}

// TestAwaitRefusesATicketNeverIssued: zero — what a caller holds after
// ignoring Trigger's error — and a ticket above every one issued are
// ErrNoTicket at once, never "answered". The control is a real ticket.
func TestAwaitRefusesATicketNeverIssued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runs{}
		c, _ := start(t, r, 0)
		if _, err := c.Await(context.Background(), 0); !errors.Is(err, ErrNoTicket) {
			t.Errorf("Await(0) before any Trigger: %v; want ErrNoTicket", err)
		}
		tk, err := c.Trigger()
		if err != nil {
			t.Fatal(err)
		}
		if n, err := c.Await(context.Background(), tk); n != 1 || err != nil {
			t.Fatalf("control: %d, %v", n, err)
		}
		for _, bad := range []Ticket{0, tk + 1} {
			if _, err := c.Await(context.Background(), bad); !errors.Is(err, ErrNoTicket) {
				t.Errorf("Await(%d) with %d issued: %v; want ErrNoTicket", bad, tk, err)
			}
		}
	})
}
