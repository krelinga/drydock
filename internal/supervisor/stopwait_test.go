//go:build linux

package supervisor

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// heldTerminal is a Runtime whose one server is a fake: its terminal is a
// pipe the test holds the writing end of, and Drydock's local end of it (the
// `devcontainer exec` client) is a fakeProc. So a test decides on its own
// when the terminal closes and when the local process exits, which a real
// process ties together: an orphaned `docker exec` client can hold the
// terminal open after Drydock's end has gone, and a local client can linger
// after its terminal has closed. Signals in the container are recorded and
// end the server; nothing ends the local process but its own exit (exit) or
// a SIGKILL sent to it.
type heldTerminal struct {
	mu      sync.Mutex
	sigs    []container.SessionSignal // asked of the container, in order
	serving bool                      // a server is running in the container
	// exitOnTerm: the container's SIGTERM ends the local process too, as a
	// server's exit ends `devcontainer exec` — but the terminal stays open.
	exitOnTerm bool
	proc       *fakeProc
	w          *os.File // the terminal's far end
	started    chan struct{}
}

func (h *heldTerminal) Start(ctx context.Context, spec container.SessionSpec, cols, rows int) (subproc.Process, *os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	h.serving, h.w = true, w
	h.proc = &fakeProc{exited: make(chan struct{})}
	p := h.proc
	h.mu.Unlock()
	close(h.started)
	return p, r, nil
}

func (h *heldTerminal) Signal(ctx context.Context, ws string, sig container.SessionSignal, pidFile string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sigs = append(h.sigs, sig)
	found := h.serving
	switch sig {
	case container.SessionTerm, container.SessionKill:
		h.serving = false
		if found && h.exitOnTerm {
			h.proc.exit()
		}
	}
	return found, nil
}

// since is what was asked of the container from the n-th signal on.
func (h *heldTerminal) since(n int) []container.SessionSignal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sigs[n:])
}

func (h *heldTerminal) asked() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sigs)
}

// closeTerminal is the server exiting: EOF on Drydock's end of its
// terminal, and nothing left in the container to signal.
func (h *heldTerminal) closeTerminal() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serving = false
	if h.w != nil {
		h.w.Close()
	}
}

// fakeProc is Drydock's local end of a server. It exits when told to, or on
// SIGKILL, and records every signal it is sent.
type fakeProc struct {
	mu     sync.Mutex
	sigs   []subproc.Signal
	exited chan struct{}
	once   sync.Once
}

func (p *fakeProc) Pid() int { return 4242 }

func (p *fakeProc) Signal(sig subproc.Signal) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, sig)
	p.mu.Unlock()
	if sig == subproc.SignalKill {
		p.exit()
	}
	return nil
}

func (p *fakeProc) Wait() subproc.Result { <-p.exited; return subproc.Result{} }

func (p *fakeProc) exit() { p.once.Do(func() { close(p.exited) }) }

func (p *fakeProc) signals() []subproc.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.sigs)
}

// A Stop's wait is bounded on both of Drydock's ends of the server, by
// KillWait on the injected clock, and neither bound ever reaches the server
// itself: it is sent the one SIGTERM sequence and nothing more.
//
//   - The terminal held open: the stop works (the server and Drydock's local
//     process exit) but something else still holds the terminal, as an
//     orphaned `docker exec` client can. The run closes the master KillWait
//     after the stop, and the Stop returns — not before.
//   - The local process lingering: the terminal closes (the server has
//     exited) but Drydock's local client does not, and a Stop asked then
//     waits on the run's end. The run SIGKILLs the local process KillWait
//     after the terminal closed, and the Stop returns — not before.
//
// Remove either bound and its case never returns. The controls are each
// case's own "not before": the Stop is still waiting a millisecond short of
// the bound, so the return is the bound's doing and not an accident of the
// fake.
func TestAStopsWaitIsBounded(t *testing.T) {
	const killWait = 500 * time.Millisecond
	for _, tc := range []struct {
		name string
		// exitOnTerm: the stop's SIGTERM ends the local process (the
		// terminal is held); otherwise the terminal closes first and the
		// local process lingers.
		exitOnTerm bool
	}{
		{name: "terminal held open", exitOnTerm: true},
		{name: "local process lingers", exitOnTerm: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) {
				// Only KillWait may fire as the clock is moved: the gate and
				// the heartbeat are far beyond it.
				p.StopTimeout, p.KillWait = 2*time.Second, killWait
				p.GateTimeout, p.HeartbeatEvery = time.Hour, time.Hour
			})
			clock := sys.NewFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
			r.m.Env.Clock = clock
			r.m.Identity = func(context.Context) (string, bool) { return "", false }
			h := &heldTerminal{exitOnTerm: tc.exitOnTerm, started: make(chan struct{})}
			r.m.Runtime = h
			r.start()
			select {
			case <-h.started:
			case <-time.After(5 * time.Second):
				t.Fatal("no server was launched")
			}
			// The pre-launch stop of a leftover asked once and found none.
			before := h.asked()
			h.mu.Lock()
			proc := h.proc
			h.mu.Unlock()
			// Whatever happens, free both ends at the end, so a bound that
			// is missing fails this test rather than hanging the rig's
			// cleanup behind it.
			t.Cleanup(func() { h.closeTerminal(); proc.exit() })

			// pending is how many timers the run should have set by the
			// time it waits on the bound: the gate and the heartbeat,
			// and the bound itself — plus, where the stop's SIGTERM
			// waited on the local process, that wait's StopTimeout timer,
			// which it leaves behind once the process has ended.
			pending := 3
			if tc.exitOnTerm {
				pending = 4
			} else {
				h.closeTerminal()
			}
			stopped := make(chan error, 1)
			if tc.exitOnTerm {
				go func() { stopped <- r.m.Stop(context.Background(), wsID) }()
			}
			// A run with no bound sets no timer here, and fails at this wait.
			r.waitFor(5*time.Second, "the run to set its KillWait bound", func() bool { return clock.Waiting() >= pending })
			if !tc.exitOnTerm {
				// Asked while the run waits for its local process, which
				// has stopped reading the queue.
				go func() { stopped <- r.m.Stop(context.Background(), wsID) }()
			}
			notYet := func(when string) {
				t.Helper()
				select {
				case err := <-stopped:
					t.Fatalf("control: the Stop returned %s (err %v)", when, err)
				case <-time.After(200 * time.Millisecond):
				}
			}
			notYet("before the clock moved")
			clock.Advance(killWait - time.Millisecond)
			notYet("a millisecond short of KillWait")
			clock.Advance(time.Millisecond)
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatalf("the Stop failed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the Stop did not return once KillWait had passed")
			}

			// The server: the one SIGTERM, from the stop — through the
			// terminal the run held, or (the run having ended) through the
			// pid file, finding the server already gone. Never a SIGKILL.
			if got, want := h.since(before), []container.SessionSignal{container.SessionTerm}; !slices.Equal(got, want) {
				t.Errorf("the container was asked %v after the launch, want %v", got, want)
			}
			// Drydock's local end: SIGKILLed only where it lingered.
			var want []subproc.Signal
			if !tc.exitOnTerm {
				want = []subproc.Signal{subproc.SignalKill}
			}
			if got := proc.signals(); !slices.Equal(got, want) {
				t.Errorf("the local process was sent %v, want %v", got, want)
			}
			r.waitState(Exited, ReasonStopped)
		})
	}
}
