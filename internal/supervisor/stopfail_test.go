//go:build linux

package supervisor

import (
	"context"
	"fmt"
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
// Each cause is tested here on a supervisor with no terminal of its own —
// parked, or none in memory at all, as for a server an earlier Drydock left —
// whose stop goes through the pid file and docker alone. One holding the
// server's terminal is TestAHeldTerminalStopAsksTheContainer: under real
// Docker its terminal closing proves nothing about the server.
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
	}, {
		// The kernel in the container refuses the signal (the signal
		// script's exit 4): Docker answered, so not stop_failed, and the
		// server is still there after SIGKILL.
		name: "the container refuses SIGKILL", reason: ReasonSurvivedKill,
		setup: func(r *rig) { stray(r); r.untouch("kill-ignored"); r.touch("kill-refused", "") },
		clear: func(r *rig) { r.untouch("kill-refused") },
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
				// With no supervisor in memory the row is made here, and its
				// placeholder state is no state any server was in.
				if press == 1 && c.name == "docker exec fails, nothing in memory" && l.From != "" {
					t.Errorf("from %q for a row made just now, want none", l.From)
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
	// Written by every stop — a restart's and a workspace stop's, rebuild's
	// or delete's first sub-step — so it names none of them.
	if s := stopFailedSentence(ReasonStopFailed); !strings.Contains(s, "Ask again once Docker answers") || strings.Contains(s, "restart") {
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

// A stop of a server Drydock holds the terminal for asks the container
// whether it ended, and never takes the end of Drydock's own terminal for
// the server's: under real Docker, killing the `docker exec` client leaves
// the process it started running (the rig's "exec-detaches" mode is that).
// So a server that outlives SIGKILL there is survived_kill, and no second
// server is started beside it. Before, Stop wrote "The session server was
// stopped." and launched another over the pid file. The control is the same
// rig and a server that honours SIGTERM: one server afterwards, the new one.
func TestAHeldTerminalStopAsksTheContainer(t *testing.T) {
	for _, survives := range []bool{true, false} {
		t.Run(fmt.Sprintf("survives=%v", survives), func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) {
				p.StopTimeout, p.KillWait = 300*time.Millisecond, 300*time.Millisecond
			})
			r.touch("exec-detaches", "")
			if survives {
				r.script("claude", stubborn)
			} else {
				r.script("claude", serves)
			}
			r.start()
			r.waitState(Serving, ReasonServing)
			old := r.pid()
			if survives {
				r.touch("kill-ignored", "")
				t.Cleanup(func() { r.untouch("kill-ignored") })
			}
			err := r.m.Restart(context.Background(), wsID)
			if survives {
				if err == nil {
					t.Fatal("a restart beside a server that outlived SIGKILL reported success")
				}
				if l := r.last(); l.State != string(Degraded) || l.Reason != string(ReasonSurvivedKill) {
					t.Errorf("event %+v, want degraded/survived_kill", l)
				}
				for _, d := range r.sups() {
					if d.Reason == string(ReasonStopped) {
						t.Errorf("a server still running was said to be stopped: %+v", d)
					}
				}
				time.Sleep(300 * time.Millisecond)
				if n := r.launches(); n != 1 {
					t.Errorf("%d launches: a second server was started beside the survivor", n)
				}
				if !alive(old) || r.pid() != old {
					t.Errorf("the survivor: alive %v, pid file %d (was %d)", alive(old), r.pid(), old)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r.waitFor(10*time.Second, "a new server", func() bool { p := r.pid(); return p != old && p != 0 && alive(p) })
			r.waitState(Serving, ReasonServing)
			if alive(old) {
				t.Errorf("control: the old server %d is still running beside the new one", old)
			}
		})
	}
}

// A start finds the server an earlier run left, and stops it first; one that
// outlives SIGKILL is recorded and nothing is started — not a second server
// refused as already served, which reads as a wait that never clears, over
// a pid file that was the only way to reach the first. That start is boot
// adoption's, a rebuild's step 8 and every registration retry. The control:
// the same stray stopped by SIGKILL, and the start serves.
func TestAStartBesideASurvivorStartsNothing(t *testing.T) {
	for _, survives := range []bool{true, false} {
		t.Run(fmt.Sprintf("survives=%v", survives), func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) {
				p.StopTimeout, p.KillWait = 300*time.Millisecond, 300*time.Millisecond
			})
			r.script("claude", stubborn)
			cmd := exec.Command("sh", "-c", container.RemoteControlLaunch, "sh", filepath.Join(r.dir, "rc.pid"), "4")
			cmd.Env = []string{"PATH=" + r.bin + ":/usr/bin:/bin"}
			st := claudetest.StartTerm(t, cmd, 200, 50)
			if _, err := st.WaitFor(10*time.Second, []byte("Capacity: 0/4")); err != nil {
				t.Fatalf("the stray did not serve: %v", err)
			}
			stray := r.pid()
			if survives {
				r.touch("kill-ignored", "")
				t.Cleanup(func() { r.untouch("kill-ignored") })
			}
			r.start()
			if !survives {
				r.waitState(Serving, ReasonServing)
				if alive(stray) {
					t.Error("control: the stray is still running")
				}
				return
			}
			r.waitState(Degraded, ReasonSurvivedKill)
			if d := r.last().Detail; d != stopFailedSentence(ReasonSurvivedKill) {
				t.Errorf("detail %q", d)
			}
			time.Sleep(300 * time.Millisecond)
			if n := r.launches(); n != 0 {
				t.Errorf("%d launches beside a server that outlived SIGKILL", n)
			}
			if r.pid() != stray || !alive(stray) {
				t.Errorf("the pid file %d (was %d), alive %v", r.pid(), stray, alive(stray))
			}
		})
	}
}

// A restart whose stop worked but which a delete or shutdown cancelled
// before its start starts nothing and writes nothing more: what cancelled it
// ends the press (the delete's state events; after a shutdown, boot's
// starting). The control is the same window uncancelled.
func TestARestartCutOffAfterItsStopStartsNothing(t *testing.T) {
	for _, cut := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancelled=%v", cut), func(t *testing.T) {
			r := newRig(t)
			r.script("claude", serves)
			r.start()
			r.waitState(Serving, ReasonServing)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.m.afterStop = func() {
				if cut {
					cancel()
				}
			}
			before := len(r.sups())
			err := r.m.Restart(ctx, wsID)
			if !cut {
				if err != nil {
					t.Fatal(err)
				}
				r.waitState(Serving, ReasonServing)
				return
			}
			if err == nil {
				t.Fatal("a cancelled restart reported success")
			}
			time.Sleep(300 * time.Millisecond)
			var states []string
			for _, d := range r.sups()[before:] {
				states = append(states, d.State+"/"+d.Reason)
			}
			if got := strings.Join(states, " "); got != "exited/stopped" {
				t.Errorf("events %q, want only the stop's", got)
			}
			if n := r.launches(); n != 1 {
				t.Errorf("%d launches: a cancelled restart started a server", n)
			}
		})
	}
}

// A restart whose stop worked and whose start could not (its row unreadable)
// answers the press: the stop's exited does not, so degraded/start_failed
// does, with Restart as the card's action. The control is the same restart
// with the row there (TestRestartKeepsTheEnvironment).
func TestARestartWhoseStartFailsSaysSo(t *testing.T) {
	r := newRig(t)
	r.script("claude", serves)
	r.start()
	r.waitState(Serving, ReasonServing)
	r.m.afterStop = func() {
		if _, err := r.db.Exec(`ALTER TABLE supervisor RENAME TO supervisor_gone`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { r.db.Exec(`ALTER TABLE supervisor_gone RENAME TO supervisor`) })
	if err := r.m.Restart(context.Background(), wsID); err == nil {
		t.Fatal("a restart whose start failed reported success")
	}
	if l := r.last(); l.State != string(Degraded) || l.Reason != string(ReasonStartFailed) || l.Detail != startFailedSentence {
		t.Errorf("event %+v, want degraded/start_failed", l)
	}
}
