//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/container"
)

// Shutdown (the supervisor's group stopping: the detach) closes every
// supervisor's terminal promptly and signals no server, including a
// supervisor whose restart failed because the container is paused: that
// stop kept Drydock's end of the frozen server's terminal and cancelled the
// run's context itself, so shutdown's cancel was news to no one and the
// loop read on until the bound ran out — the whole of it, after which
// shutdown stopped waiting for every other supervisor too. The control is a
// serving supervisor, which shutdown has always ended at once. In both, the server — apart from Drydock, as under
// real Docker ("exec-detaches") — is still running afterwards.
func TestDetachClosesEveryTerminalAndSignalsNobody(t *testing.T) {
	for _, stuck := range []bool{false, true} {
		t.Run(fmt.Sprintf("after a failed stop in a paused container=%v", stuck), func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) {
				p.StopTimeout, p.KillWait = 300*time.Millisecond, 300*time.Millisecond
			})
			t.Cleanup(func() { r.untouch("container-paused") }) // before the rig's own Stop
			r.touch("exec-detaches", "")
			r.script("claude", serves)
			r.start()
			r.waitState(Serving, ReasonServing)
			server := r.pid()
			r.m.mu.Lock()
			s := r.m.sups[wsID]
			r.m.mu.Unlock()
			if stuck {
				r.touch("container-paused", "")
				if err := r.m.Restart(context.Background(), wsID); !errors.Is(err, container.ErrSessionContainerPaused) {
					t.Fatalf("restart: %v, want ErrSessionContainerPaused", err)
				}
				time.Sleep(200 * time.Millisecond)
				if !s.running() {
					t.Fatal("the loop ended after the failed stop: nothing is left for shutdown to close")
				}
			}
			const bound = 10 * time.Second
			began := time.Now()
			late := r.detach(bound)
			if took := time.Since(began); took > 3*time.Second {
				t.Errorf("shutdown took %v of its %v bound", took, bound)
			}
			if late != nil || s.running() {
				t.Errorf("still running after shutdown: %v (loop running %v)", late, s.running())
			}
			if !alive(server) || r.pid() != server {
				t.Errorf("the server: alive %v, pid file %d (was %d); shutdown must signal no server", alive(server), r.pid(), server)
			}
		})
	}
}

// A container paused while a stop waits for its server to exit — SIGTERM
// delivered, the server not yet gone — ends the wait at once: Docker will
// not exec into it, so whether the server exited cannot be asked, and
// SIGKILL cannot be sent. The stop is the paused stop_failed straight away,
// rather than polling a container that cannot answer for the rest of the
// grace period and then trying SIGKILL. The stand-in server pauses its
// "container" when SIGTERM arrives, and otherwise ignores it. The control is
// the same server with no pause: SIGKILL after the grace period ends it.
func TestAPauseDuringTheStopEndsTheWait(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%v", pause), func(t *testing.T) {
			const grace = 3 * time.Second
			r := newRig(t, func(_ *rig, p *Policy) {
				p.StopTimeout, p.KillWait = grace, 500*time.Millisecond
			})
			t.Cleanup(func() { r.untouch("container-paused") }) // before the rig's own Stop
			onTerm := "''"
			if pause {
				onTerm = fmt.Sprintf("': > %s'", filepath.Join(r.dir, "container-paused"))
			}
			r.script("claude", "trap "+onTerm+" TERM\n"+serves)
			// A server an earlier Drydock left: no terminal here, so the
			// stop asks the container whether it has exited.
			cmd := exec.Command("sh", "-c", container.RemoteControlLaunch, "sh", filepath.Join(r.dir, "rc.pid"), "4")
			cmd.Env = []string{"PATH=" + r.bin + ":/usr/bin:/bin"}
			st := claudetest.StartTerm(t, cmd, 200, 50)
			if _, err := st.WaitFor(10*time.Second, []byte("Capacity: 0/4")); err != nil {
				t.Fatalf("the stray did not serve: %v", err)
			}
			server := r.pid()
			began := time.Now()
			err := r.m.Stop(context.Background(), wsID)
			took := time.Since(began)
			if !pause {
				if err != nil || alive(server) {
					t.Errorf("control: %v, server alive %v; want SIGKILL to end it", err, alive(server))
				}
				return
			}
			if !errors.Is(err, container.ErrSessionContainerPaused) {
				t.Fatalf("stop: %v, want ErrSessionContainerPaused", err)
			}
			if took >= grace {
				t.Errorf("the stop took %v: it waited out the %v grace period on a paused container", took, grace)
			}
			if l := r.last(); l.State != string(Degraded) || l.Reason != string(ReasonStopFailed) || l.Detail != pausedSentence {
				t.Errorf("event %+v, want degraded/stop_failed with the paused sentence", l)
			}
			r.untouch("container-paused")
		})
	}
}

// Shutdown in the middle of a stop leaves the terminal to the stop until it
// has decided: a stop of a server Drydock holds the terminal for waits on
// that terminal (`devcontainer exec` exits when the server does), so closing
// it under the stop would read as the server ending, and "The session server
// was stopped." would be written over a server still running. The server
// here ignores SIGTERM, and shutdown comes during the grace period. Either the
// stop says it stopped and the server is gone (SIGKILL, after the grace
// period), or it says it did not; never stopped with the server alive. The
// control is the same stop with no shutdown.
func TestDetachDuringAStopLetsTheStopDecide(t *testing.T) {
	for _, detach := range []bool{false, true} {
		t.Run(fmt.Sprintf("detach=%v", detach), func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) {
				p.StopTimeout, p.KillWait = time.Second, 500*time.Millisecond
			})
			r.touch("exec-detaches", "")
			r.script("claude", stubborn)
			r.start()
			r.waitState(Serving, ReasonServing)
			server := r.pid()
			stopped := make(chan error, 1)
			go func() { stopped <- r.m.Stop(context.Background(), wsID) }()
			if detach {
				time.Sleep(300 * time.Millisecond)
				r.detach(10 * time.Second)
			}
			err := <-stopped
			if err == nil && alive(server) {
				t.Errorf("the stop said stopped and the server is alive; last event %+v", r.last())
			}
			if !detach && (err != nil || alive(server)) {
				t.Errorf("control: %v, server alive %v", err, alive(server))
			}
		})
	}
}
