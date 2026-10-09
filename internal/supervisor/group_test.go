//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
)

// heldRuntime holds every SIGTERM asked of the container, once armed, until
// released — ignoring its context, as a docker call that does not end with
// one would: what a shutdown must still wait for, or name.
type heldRuntime struct {
	Runtime
	mu      sync.Mutex
	armed   bool
	entered chan struct{} // closed when the first signal is held
	enterMu sync.Once
	release chan struct{}
	relOnce sync.Once
}

func holdTerms(r *rig) *heldRuntime {
	h := &heldRuntime{Runtime: r.m.Runtime, entered: make(chan struct{}), release: make(chan struct{})}
	r.m.Runtime = h
	// Registered after the rig's own cleanup, so it runs first: the rig's
	// Stop must not be held.
	r.t.Cleanup(h.free)
	return h
}

func (h *heldRuntime) arm() {
	h.mu.Lock()
	h.armed = true
	h.mu.Unlock()
}

func (h *heldRuntime) free() { h.relOnce.Do(func() { close(h.release) }) }

func (h *heldRuntime) Signal(ctx context.Context, ws string, sig container.SessionSignal, pidFile string) (bool, error) {
	h.mu.Lock()
	armed := h.armed
	h.mu.Unlock()
	if armed && sig == container.SessionTerm {
		h.enterMu.Do(func() { close(h.entered) })
		<-h.release
	}
	return h.Runtime.Signal(ctx, ws, sig, pidFile)
}

// Once the supervisor's group has stopped — shutdown — nothing starts: not
// a Start (step 8, a restart's second half, boot adoption), not a Park, not
// a sign-in's Resume, and
// no loop, launch, row change or event follows either. The control is the
// same Start before the stop, which serves.
func TestNothingStartsOnceTheGroupStops(t *testing.T) {
	r := newRig(t)
	r.touch("exec-detaches", "")
	r.script("claude", serves)
	r.start()
	r.waitState(Serving, ReasonServing)
	if late := r.detach(5 * time.Second); late != nil {
		t.Fatalf("still running after shutdown: %v", late)
	}
	launches, events := r.launches(), len(r.sups())
	st, restarts := r.row()

	if err := r.m.Start(context.Background(), wsID); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after shutdown: %v, want ErrClosed", err)
	}
	if err := r.m.Park(context.Background(), wsID, ReasonContainerPaused, "parked"); !errors.Is(err, ErrClosed) {
		t.Errorf("Park after shutdown: %v, want ErrClosed", err)
	}
	if err := r.m.Resume(context.Background(), wsID); !errors.Is(err, ErrClosed) {
		t.Errorf("Resume after shutdown: %v, want ErrClosed", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := r.launches(); n != launches {
		t.Errorf("%d launches after shutdown", n-launches)
	}
	if n := len(r.sups()); n != events {
		t.Errorf("%d supervisor events after shutdown: %+v", n-events, r.sups()[events:])
	}
	if s, n := r.row(); s != st || n != restarts {
		t.Errorf("row %s/%d after shutdown, was %s/%d", s, n, st, restarts)
	}
	r.m.mu.Lock()
	s := r.m.sups[wsID]
	r.m.mu.Unlock()
	if s != nil && s.running() {
		t.Error("a supervisor loop is running after shutdown")
	}
}

// The stop of a server hung at a gate is the group's, so shutdown waits for
// it and names it when it outlasts the bound, rather than leaving it to run
// on unseen after the database has closed. And it is cut off by shutdown
// like the rest: once released it signals nothing, so the hung server — apart
// from Drydock, as under real Docker — is left where it is, for the next
// process to stop. The control is the gate stop itself: before the shutdown
// it is under way, held at its SIGTERM.
func TestTheGateStopIsTheGroups(t *testing.T) {
	r := newRig(t, func(_ *rig, p *Policy) { p.GateTimeout = 500 * time.Millisecond })
	r.touch("exec-detaches", "")
	// No environment, ever: a gate.
	r.script("claude", "printf 'Enable Remote Control? (y/n) '\nwhile :; do sleep 0.05; done\n")
	h := holdTerms(r)
	r.start()
	r.waitFor(5*time.Second, "the server launched", func() bool { return r.launches() == 1 && r.pid() != 0 })
	server := r.pid()
	// The pre-launch stop has asked its question; the next SIGTERM is the
	// gate's.
	h.arm()
	terms := strings.Count(r.dockerLog(), " TERM")
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the gate stop never began")
	}
	late := r.detach(time.Second)
	want := "gate stop " + wsID
	if fmt.Sprint(late) != fmt.Sprint([]string{want}) {
		t.Errorf("shutdown's stragglers %v, want [%s]: the gate stop is not the group's", late, want)
	}
	h.free()
	if late := r.g.Wait(time.After(5 * time.Second)); late != nil {
		t.Errorf("still running once released: %v", late)
	}
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(r.dockerLog(), " TERM"); n != terms {
		t.Errorf("%d SIGTERMs sent after shutdown:\n%s", n-terms, r.dockerLog())
	}
	if !alive(server) {
		t.Error("the hung server was signalled after shutdown")
	}
}

// A start replaces whatever the card was showing at once — a parked
// container_paused or stale_broker_mount, a stop's exited — with starting,
// or with awaiting_login when the fleet is signed out: written before Start
// returns, not when the loop first gets as far as writing, which is after an
// identity read and a docker exec (held here, so the loop has written
// nothing). The control, in each case, is the parked state standing before
// the start, and the loop going on to serve, or to wait for a login, once
// released.
func TestAStartReplacesAParkedStateAtOnce(t *testing.T) {
	for _, signedOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("signed out=%v", signedOut), func(t *testing.T) {
			r := newRig(t)
			if signedOut {
				r.m.Identity = func(context.Context) (string, bool) { return "blanked", true }
			}
			r.script("claude", serves)
			if err := r.m.Park(context.Background(), wsID, ReasonContainerPaused, "paused at boot"); err != nil {
				t.Fatal(err)
			}
			if l := r.last(); l.State != string(Degraded) || l.Reason != string(ReasonContainerPaused) {
				t.Fatalf("control: parked as %+v", l)
			}
			h := holdTerms(r)
			h.arm()
			r.start()
			want, reason := Starting, ReasonLaunching
			if signedOut {
				want, reason = AwaitingLogin, ReasonSignedOut
			}
			if l := r.last(); l.State != string(want) || l.Reason != string(reason) || l.From != string(Degraded) {
				t.Errorf("as Start returned, the card's event is %+v; want %s/%s from degraded", l, want, reason)
			}
			if st, _ := r.row(); st != want {
				t.Errorf("as Start returned, the row says %s; want %s", st, want)
			}
			h.free()
			if signedOut {
				time.Sleep(300 * time.Millisecond)
				if n := r.launches(); n != 0 {
					t.Errorf("control: %d launches with the fleet signed out", n)
				}
				if l := r.last(); l.State != string(AwaitingLogin) {
					t.Errorf("control: %+v, want awaiting_login", l)
				}
				return
			}
			r.waitState(Serving, ReasonServing)
			select {
			case <-h.entered:
			default:
				t.Error("control: the loop's pre-launch stop was never held")
			}
			if n := len(r.sups()); n != 3 {
				t.Errorf("events %+v: want parked, starting, serving — no second starting", r.sups())
			}
			var pid int
			r.db.QueryRow(`SELECT coalesce(pid, 0) FROM supervisor WHERE workspace_id = ?`, wsID).Scan(&pid)
			if pid == 0 {
				t.Error("the launched server's pid is not on the row")
			}
		})
	}
}
