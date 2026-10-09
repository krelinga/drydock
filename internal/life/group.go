// Package life is the lifecycle of Drydock's long-lived work: a Group that
// owns goroutines and is stopped and waited for as one, and a Coalescer, the
// one worker behind "periodic, plus on demand, joined, every request
// answered".
//
// Both exist because each component used to build its own: a context rooted
// at context.Background, a closed flag, a WaitGroup, a wall-clock bound on
// the wait. Every one of them needed a review to prove the same three
// properties by hand (#81, #83, #85): no work is added once the wait has
// begun, nothing starts after the cancel, and a queued follow-up never runs
// after shutdown. Here they are proved once.
//
// # Rules and details
//
// A Group's context descends from the one it was made from — never from
// context.Background — so it ends when its parent does, and the context
// rule's meta-test scans this package like any other. TryGo adds under the
// same mutex Stop takes, and refuses once Stop has begun or the context has
// ended, so no work starts after Stop and nothing is added to a WaitGroup
// being waited on. Wait stops first, then waits for the group and every
// child, until a deadline the caller makes on the injected clock
// (sys.NewTimer), and names what was still running.
//
// A Coalescer's requests are a generation counter, not flags. Trigger bumps
// asked and returns it as a Ticket; the worker reads asked as a run starts and
// answers every ticket up to it when the run ends, so a ticket is answered
// only by a run that began after it was issued, and a Trigger during a run
// gets one more run, at once. When the worker exits (its group stopped),
// tickets no run started for are refused with ErrStopping; it never starts a
// run after its context has ended.
package life

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrStopping is TryGo after Stop, or a Coalescer's Trigger or Await once its
// worker has ended or is ending.
var ErrStopping = errors.New("shutting down")

// Group owns goroutines that end with its context. Make one with NewGroup or
// Child; the zero value is not usable.
type Group struct {
	name   string
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	stopping bool
	wg       sync.WaitGroup
	next     uint64
	running  map[uint64]string
	children []*Group
}

// NewGroup returns a group whose context descends from parent: it ends when
// parent does, or at Stop.
func NewGroup(parent context.Context) *Group {
	ctx, cancel := context.WithCancel(parent)
	return &Group{ctx: ctx, cancel: cancel, running: map[uint64]string{}}
}

// Child returns a group whose context descends from g's, which g's Stop
// stops and g's Wait waits for. name prefixes its goroutines' names in
// Wait's answer. A child asked for after g's Stop is already stopped.
func (g *Group) Child(name string) *Group {
	c := NewGroup(g.ctx)
	c.name = name
	if g.name != "" {
		c.name = g.name + "/" + name
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		c.Stop()
		return c
	}
	g.children = append(g.children, c)
	return c
}

// Ctx is the group's context, for work that must end with the group — and
// what a component derives its own contexts from, never context.Background.
func (g *Group) Ctx() context.Context { return g.ctx }

// Go starts a long-lived loop under the group, or nothing once it is
// stopping: a loop asked for during shutdown has nothing left to do.
func (g *Group) Go(name string, f func(ctx context.Context)) { _ = g.TryGo(name, f) }

// TryGo starts f under the group's context and returns nil, or starts nothing
// and returns ErrStopping once Stop has begun or the context has ended.
func (g *Group) TryGo(name string, f func(ctx context.Context)) error {
	g.mu.Lock()
	// Under the mutex Stop takes: once Stop has set stopping, nothing is
	// added, so Wait's wg.Wait never races an Add. The context check also
	// refuses a child whose parent has cancelled but not yet reached it.
	if g.stopping || g.ctx.Err() != nil {
		g.mu.Unlock()
		return ErrStopping
	}
	id := g.next
	g.next++
	g.running[id] = name
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.running, id)
			g.mu.Unlock()
			g.wg.Done()
		}()
		f(g.ctx)
	}()
	return nil
}

// Stop refuses new work, cancels the group's context, and stops every child.
// It does not wait; Wait does. Safe to call more than once and from any
// goroutine.
func (g *Group) Stop() {
	g.mu.Lock()
	g.stopping = true
	g.cancel()
	kids := append([]*Group(nil), g.children...)
	g.mu.Unlock()
	for _, k := range kids {
		k.Stop()
	}
}

// Wait stops the group, then waits for every goroutine it and its children
// started, or until deadline fires — made by the caller on the injected
// clock (sys.NewTimer); nil waits for ever. It returns the names of what was
// still running when the deadline fired, sorted, children's prefixed with
// their name; nil when everything ended.
func (g *Group) Wait(deadline <-chan time.Time) []string {
	g.Stop()
	done := make(chan struct{})
	go func() {
		g.waitAll()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-deadline:
	}
	late := g.stragglers()
	sort.Strings(late)
	return late
}

// waitAll waits for g's goroutines and then each child's. g is stopped, so
// its children are fixed and none of them adds work.
func (g *Group) waitAll() {
	g.wg.Wait()
	g.mu.Lock()
	kids := append([]*Group(nil), g.children...)
	g.mu.Unlock()
	for _, k := range kids {
		k.waitAll()
	}
}

func (g *Group) stragglers() []string {
	g.mu.Lock()
	var out []string
	for _, n := range g.running {
		if g.name != "" {
			n = g.name + "/" + n
		}
		out = append(out, n)
	}
	kids := append([]*Group(nil), g.children...)
	g.mu.Unlock()
	for _, k := range kids {
		out = append(out, k.stragglers()...)
	}
	return out
}
