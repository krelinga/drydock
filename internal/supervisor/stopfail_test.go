//go:build linux

package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/container"
)

// stubborn serves and ignores SIGTERM: only SIGKILL ends it.
const stubborn = `trap '' TERM
printf 'Environment ID: env_01STUBBORN0000000000000000\r\n    Capacity: 0/4 · x\r\n'
while :; do sleep 0.05; done
`

const serves = `printf 'Environment ID: env_01STUBBORN0000000000000000\r\n    Capacity: 0/4 · x\r\n'
while :; do sleep 0.05; done
`

func (r *rig) touch(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) untouch(name string) { os.Remove(filepath.Join(r.dir, name)) }

// A restart whose stop fails ends in a supervisor.state event all the same
// — degraded, with the reason that names the cause and the sentence that
// names the one action that can fix it — so the card's Restart session
// server settles (frontend §4.2) and offers that action. Before this, Stop
// returned its error with no event, and the button spun until a reload. The
// control, in each case, is the same press once the fault is gone: a normal
// restart, exited then serving.
//
// Each cause is tested where it can happen: on a supervisor with no
// terminal of its own — parked, or none in memory at all, as for a server an
// earlier Drydock left — whose stop goes through the pid file and docker
// alone. One holding the server's terminal does not fail this way: after
// SIGKILL Drydock ends its own end of it, which hangs the terminal up.
func TestARestartWhoseStopFailsSaysSo(t *testing.T) {
	type tc struct {
		name   string
		reason Reason
		// setup leaves a supervisor (or none) and a server for the restart
		// to stop, and arms the fault.
		setup func(r *rig)
		// clear disarms it.
		clear func(r *rig)
	}
	parked := func(fault, what string) func(r *rig) {
		return func(r *rig) {
			r.script("claude", "exit 1\n")
			r.start()
			r.waitState(Degraded, ReasonBudgetSpent)
			r.touch(fault, what)
		}
	}
	stray := func(r *rig) {
		r.script("claude", stubborn)
		cmd := exec.Command("sh", "-c", container.RemoteControlLaunch, "sh", filepath.Join(r.dir, "rc.pid"), "4")
		cmd.Env = []string{"PATH=" + r.bin + ":/usr/bin:/bin"}
		s := claudetest.StartTerm(r.t, cmd, 200, 50)
		if _, err := s.WaitFor(10*time.Second, []byte("Capacity: 0/4")); err != nil {
			r.t.Fatalf("the stray did not serve: %v", err)
		}
		r.touch("kill-ignored", "")
	}
	cases := []tc{{
		name: "docker ps fails, parked", reason: ReasonStopFailed,
		setup: parked("docker-fails", "ps"),
		clear: func(r *rig) { r.untouch("docker-fails"); r.script("claude", serves) },
	}, {
		name: "docker exec fails, parked", reason: ReasonStopFailed,
		setup: parked("docker-fails", "exec"),
		clear: func(r *rig) { r.untouch("docker-fails"); r.script("claude", serves) },
	}, {
		name: "docker exec fails, nothing in memory", reason: ReasonStopFailed,
		setup: func(r *rig) { r.script("claude", serves); r.touch("docker-fails", "exec") },
		clear: func(r *rig) { r.untouch("docker-fails") },
	}, {
		// SIGTERM is delivered and ignored; then Docker fails. Not a server
		// seen surviving SIGKILL, so not Rebuild's.
		name: "SIGKILL cannot be sent", reason: ReasonStopFailed,
		setup: func(r *rig) { stray(r); r.untouch("kill-ignored"); r.touch("docker-fails", "KILL") },
		clear: func(r *rig) { r.untouch("docker-fails") },
	}, {
		// SIGKILL goes out (and is ignored), but whether the server is
		// still there cannot be asked.
		name: "whether SIGKILL worked cannot be asked", reason: ReasonStopFailed,
		setup: func(r *rig) { stray(r); r.touch("docker-fails", "0") },
		clear: func(r *rig) { r.untouch("docker-fails"); r.untouch("kill-ignored") },
	}, {
		name: "survives SIGKILL, a server an earlier Drydock left", reason: ReasonSurvivedKill,
		setup: stray,
		clear: func(r *rig) { r.untouch("kill-ignored") },
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) {
				p.Budget = 1
				p.StopTimeout, p.KillWait = 300*time.Millisecond, 300*time.Millisecond
			})
			t.Cleanup(func() { c.clear(r) }) // before the rig's own Stop
			c.setup(r)
			before := len(r.sups())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			for press := 1; press <= 2; press++ {
				if err := r.m.Restart(ctx, wsID); err == nil {
					t.Fatalf("press %d: the restart reported success", press)
				}
				if ctx.Err() != nil {
					t.Fatalf("press %d: the restart did not return before its deadline", press)
				}
				got := r.sups()
				// Each press is answered, the second too, though it says
				// exactly what the first did: a press with no event is a
				// button that spins.
				if len(got) != before+press {
					t.Fatalf("press %d: %d supervisor.state events after the restart, want %d: %+v",
						press, len(got)-before, press, got[before:])
				}
				l := got[len(got)-1]
				if l.State != string(Degraded) || l.Reason != string(c.reason) || l.Detail != stopFailedSentence(c.reason) {
					t.Errorf("press %d: event %+v, want degraded/%s with its sentence", press, l, c.reason)
				}
				if st, _ := r.row(); st != Degraded {
					t.Errorf("press %d: row state %s, want degraded", press, st)
				}
			}
			// The control: the fault gone, the same press restarts as usual.
			c.clear(r)
			before = len(r.sups())
			if err := r.m.Restart(context.Background(), wsID); err != nil {
				t.Fatalf("control: %v", err)
			}
			r.waitState(Serving, ReasonServing)
			var states []string
			for _, d := range r.sups()[before:] {
				states = append(states, d.State+"/"+d.Reason)
			}
			// The failed stop left the supervisor registered — even where
			// there was none in memory before — so the retry stops the same
			// server and says so.
			want := "exited/stopped starting/launching serving/connected"
			if got := strings.Join(states, " "); got != want {
				t.Errorf("control: events %q, want %q", got, want)
			}
		})
	}
}

// The sentences name the action that can work, and the reasons are the
// ones the card keys on.
func TestStopFailureSentences(t *testing.T) {
	if s := stopFailedSentence(ReasonStopFailed); !strings.Contains(s, "Restart the session server again") {
		t.Errorf("stop_failed: %q", s)
	}
	if s := stopFailedSentence(ReasonSurvivedKill); !strings.Contains(s, "Rebuild") || !strings.Contains(s, "clone is kept") {
		t.Errorf("survived_kill: %q", s)
	}
}

// A stop cut off by its caller — a delete cancelling the restart, Drydock
// shutting down — is not a failed stop: what cancelled it says what happens
// next, and no degraded is written. The control is TestARestartWhoseStopFailsSaysSo.
func TestACancelledStopWritesNoFailure(t *testing.T) {
	r := newRig(t, func(_ *rig, p *Policy) { p.StopTimeout = 2 * time.Second })
	r.script("claude", stubborn)
	r.start()
	r.waitState(Serving, ReasonServing)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if err := r.m.Restart(ctx, wsID); err == nil {
		t.Fatal("a cancelled restart reported success")
	}
	for _, d := range r.sups() {
		if d.State == string(Degraded) {
			t.Errorf("a cancelled stop wrote %+v", d)
		}
	}
}
