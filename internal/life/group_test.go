package life

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// TestNothingStartsAfterStop: before Stop, TryGo runs its function (the
// positive control); after, Go and TryGo start nothing and TryGo says why,
// on the group and on a child, including a child asked for after Stop.
func TestNothingStartsAfterStop(t *testing.T) {
	g := NewGroup(context.Background())
	child := g.Child("c")
	ran := make(chan string, 8)
	if err := g.TryGo("before", func(context.Context) { ran <- "before" }); err != nil {
		t.Fatalf("control: TryGo before Stop: %v", err)
	}
	if err := child.TryGo("before", func(context.Context) { ran <- "child before" }); err != nil {
		t.Fatalf("control: a child's TryGo before Stop: %v", err)
	}
	if late := g.Wait(nil); late != nil {
		t.Fatalf("control: stragglers %v", late)
	}
	if len(ran) != 2 {
		t.Fatalf("control: %d functions ran before Stop; want 2", len(ran))
	}
	g.Stop()
	for _, gr := range []*Group{g, child, g.Child("late")} {
		if err := gr.TryGo("after", func(context.Context) { ran <- "after" }); !errors.Is(err, ErrStopping) {
			t.Errorf("TryGo after Stop: %v; want ErrStopping", err)
		}
		gr.Go("after", func(context.Context) { ran <- "after" })
	}
	g.Wait(nil)
	if len(ran) != 2 {
		t.Errorf("%d functions ran in all; want only the 2 before Stop", len(ran))
	}
}

// TestAGroupEndsWithItsParentContext: a group made from a context that has
// ended starts nothing, so a component's work cannot outlive Serve's ctx.
func TestAGroupEndsWithItsParentContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	g := NewGroup(parent)
	if err := g.TryGo("x", func(context.Context) {}); err != nil {
		t.Fatalf("control: %v", err)
	}
	cancel()
	if err := g.TryGo("x", func(context.Context) {}); !errors.Is(err, ErrStopping) {
		t.Errorf("TryGo after the parent ended: %v; want ErrStopping", err)
	}
	if g.Ctx().Err() == nil {
		t.Error("the group's context outlived its parent's")
	}
}

// TestWaitWaitsForChildren: Wait cancels, then waits for every goroutine the
// group and its children started, including one that takes a while to end
// after its context does.
func TestWaitWaitsForChildren(t *testing.T) {
	g := NewGroup(context.Background())
	var ended atomic.Int32
	slow := func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		ended.Add(1)
	}
	g.Go("a", slow)
	g.Child("c").Go("b", slow)
	g.Child("c").Child("d").Go("e", slow)
	if late := g.Wait(nil); late != nil {
		t.Errorf("stragglers %v", late)
	}
	if n := ended.Load(); n != 3 {
		t.Errorf("Wait returned with %d of 3 goroutines ended", n)
	}
}

// TestWaitNamesStragglers: what has not ended when the deadline — on the
// injected clock — fires is named, children's with their prefix; once they
// end, a second Wait names nothing (the control).
func TestWaitNamesStragglers(t *testing.T) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	g := NewGroup(context.Background())
	release := make(chan struct{})
	stuck := func(context.Context) { <-release }
	g.Go("loop", stuck)
	g.Child("catalog").Go("refresh", stuck)
	g.Go("quick", func(ctx context.Context) { <-ctx.Done() })
	deadline, stop := sys.NewTimer(clock, 10*time.Second)
	defer stop()
	got := make(chan []string)
	go func() { got <- g.Wait(deadline) }()
	// "quick" has ended — so Wait has begun and stopped the group — before
	// the clock moves: an Advance that landed before Wait ran fired the
	// deadline first, and a Wait reading its stragglers at once named quick
	// as well, which only a loaded machine's scheduling ever showed.
	for deadline := time.Now().Add(5 * time.Second); slices.Contains(g.stragglers(), "quick"); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("quick never ended: Wait did not stop the group")
		}
	}
	select {
	case late := <-got:
		t.Fatalf("Wait returned %v before its deadline", late)
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(10 * time.Second)
	if late, want := <-got, []string{"catalog/refresh", "loop"}; !reflect.DeepEqual(late, want) {
		t.Errorf("stragglers %q; want %q", late, want)
	}
	close(release)
	if late := g.Wait(nil); late != nil {
		t.Errorf("control: once released, stragglers %q", late)
	}
}

// TestStopWaitAndTryGoRace: TryGo from many goroutines while others Stop and
// Wait. Every TryGo that began after a Stop had returned is refused, and
// every function a TryGo accepted has ended by the time Wait returns. Run
// under -race, many times.
func TestStopWaitAndTryGoRace(t *testing.T) {
	for round := 0; round < 20; round++ {
		g := NewGroup(context.Background())
		child := g.Child("c")
		var stopped atomic.Bool
		var accepted, finished, violations atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				target := g
				if i%2 == 1 {
					target = child
				}
				for j := 0; j < 20; j++ {
					before := stopped.Load()
					err := target.TryGo("w", func(ctx context.Context) {
						<-ctx.Done()
						finished.Add(1)
					})
					if err == nil {
						accepted.Add(1)
						if before {
							violations.Add(1)
						}
					}
				}
			}(i)
		}
		waited := make(chan int32, 2)
		for k := 0; k < 2; k++ {
			go func() {
				g.Stop()
				stopped.Store(true)
				g.Wait(nil)
				waited <- 0
			}()
		}
		<-waited
		// One Wait has returned: everything accepted before it is over. The
		// accepting goroutines may still be running TryGo, which is refused.
		<-waited
		wg.Wait()
		if v := violations.Load(); v != 0 {
			t.Fatalf("round %d: %d TryGo calls begun after Stop returned were accepted", round, v)
		}
		g.Wait(nil)
		if a, f := accepted.Load(), finished.Load(); a != f {
			t.Fatalf("round %d: %d accepted, %d finished after Wait", round, a, f)
		}
	}
}

// TestWaitReturnsOnlyWhenEverythingAcceptedHasEnded: the property above, at
// the moment Wait returns rather than after the TryGo callers are done.
func TestWaitReturnsOnlyWhenEverythingAcceptedHasEnded(t *testing.T) {
	for round := 0; round < 50; round++ {
		g := NewGroup(context.Background())
		var running atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 10; j++ {
					g.TryGo("w", func(ctx context.Context) {
						running.Add(1)
						<-ctx.Done()
						running.Add(-1)
					})
				}
			}()
		}
		g.Wait(nil)
		// Anything accepted before Wait's Stop has ended; nothing is
		// accepted after it, so nothing can be running now or later.
		if n := running.Load(); n != 0 {
			t.Fatalf("round %d: %d still running when Wait returned", round, n)
		}
		wg.Wait()
		if n := running.Load(); n != 0 {
			t.Fatalf("round %d: %d started after Wait returned", round, n)
		}
	}
}

func (g *Group) childCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.children)
}

// TestAWaitedChildIsReleased: a child per job, made, used and Waited for,
// leaves nothing behind in its parent — so a thousand of them cost what one
// does. One whose Wait ran out with a straggler stays, so the parent's Wait
// still names it, and is released by its own next Wait once it ends. The
// control: a child never waited for is held.
func TestAWaitedChildIsReleased(t *testing.T) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	g := NewGroup(context.Background())
	held := g.Child("component")
	for i := 0; i < 1000; i++ {
		job := g.Child("job")
		job.Go("work", func(ctx context.Context) {})
		if late := job.Wait(nil); late != nil {
			t.Fatalf("job %d: stragglers %v", i, late)
		}
	}
	if n := g.childCount(); n != 1 {
		t.Fatalf("%d children after 1000 waited jobs; want the component's alone", n)
	}

	release := make(chan struct{})
	stuck := g.Child("stuck")
	stuck.Go("work", func(context.Context) { <-release })
	// The fake clock's channel is buffered: advancing before Wait selects
	// still fires it.
	deadline, stop := sys.NewTimer(clock, time.Second)
	defer stop()
	clock.Advance(time.Second)
	if late := stuck.Wait(deadline); !reflect.DeepEqual(late, []string{"stuck/work"}) {
		t.Fatalf("the stuck child's Wait: %q", late)
	}
	if n := g.childCount(); n != 2 {
		t.Fatalf("%d children; want the component's and the stuck one's", n)
	}
	deadline, stop = sys.NewTimer(clock, time.Second)
	defer stop()
	clock.Advance(time.Second)
	if late := g.Wait(deadline); !reflect.DeepEqual(late, []string{"stuck/work"}) {
		t.Errorf("the parent's Wait named %q; want the stuck child's straggler", late)
	}
	close(release)
	if late := stuck.Wait(nil); late != nil {
		t.Fatalf("once released: %v", late)
	}
	if n := g.childCount(); n != 1 {
		t.Errorf("%d children once the stuck one ended and was waited for; want the component's", n)
	}
	if late := held.Wait(nil); late != nil || g.childCount() != 0 {
		t.Errorf("the component's child: stragglers %v, %d children left", late, g.childCount())
	}
}
