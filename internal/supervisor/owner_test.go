//go:build linux

package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/subproc"
)

// counting serves and counts every SIGTERM it receives in the file "terms",
// and otherwise ignores it: only SIGKILL ends it. What it counts is what was
// delivered, which a docker log of the signal script's argv cannot tell
// from a SIGTERM that found no server.
func counting(r *rig) string {
	return fmt.Sprintf(`trap 'echo t >> %q' TERM
printf 'Environment ID: env_01STUBBORN0000000000000000\r\n    Capacity: 0/4 · x\r\n'
while :; do sleep 0.05; done
`, filepath.Join(r.dir, "terms"))
}

// terms is how many SIGTERMs the counting server has received.
func (r *rig) terms() int {
	b, _ := os.ReadFile(filepath.Join(r.dir, "terms"))
	return strings.Count(string(b), "t\n")
}

// kills is how many SIGKILLs were asked of the container.
func (r *rig) kills() int { return strings.Count(r.dockerLog(), "rc.pid KILL") }

// stray starts a server an earlier Drydock left: in the "container", with
// no terminal of Drydock's, recorded in the pid file.
func (r *rig) stray() int {
	r.t.Helper()
	cmd := exec.Command("sh", "-c", container.RemoteControlLaunch, "sh", filepath.Join(r.dir, "rc.pid"), "4")
	cmd.Env = []string{"PATH=" + r.bin + ":/usr/bin:/bin"}
	st := claudetest.StartTerm(r.t, cmd, 200, 50)
	if _, err := st.WaitFor(10*time.Second, []byte("Capacity: 0/4")); err != nil {
		r.t.Fatalf("the stray did not serve: %v", err)
	}
	return r.pid()
}

// Two stoppers of one server never both signal it: every signal is sent by
// the workspace's owner — its supervision loop, or a sup born stopped — and a
// second stop asked while the first is under way is answered by its outcome,
// with nothing more sent. The server here ignores SIGTERM and counts it, so a
// second stopper beside the first shows as a second SIGTERM delivered (and a
// second SIGKILL), and the second stop is asked once the first's SIGTERM has
// landed. Before, Stop signalled from its caller's goroutine: a Restart
// beside a Stop found no supervisor (the Stop had taken it) and stopped the
// server again through the pid file, and a Stop beside the loop's stop of a
// leftover server sent its own SIGTERM beside the loop's. In each case the
// control is the one stop that was asked: exactly one SIGTERM delivered and
// one SIGKILL, and the server gone.
func TestTwoStoppersNeverBothSignal(t *testing.T) {
	cases := []struct {
		name string
		// first begins a stop of the server and returns what waits for it;
		// second is the stop asked beside it.
		first, second func(r *rig) error
		setup         func(r *rig) int // the server's pid
		launches      int              // servers launched in all
		check         func(t *testing.T, r *rig)
	}{{
		name: "a Stop beside a Restart",
		setup: func(r *rig) int {
			r.script("claude", counting(r))
			r.start()
			r.waitState(Serving, ReasonServing)
			return r.pid()
		},
		first:  func(r *rig) error { return r.m.Stop(context.Background(), wsID) },
		second: func(r *rig) error { return r.m.Restart(context.Background(), wsID) },
		// The restart's own start, once the stop it shared has worked.
		launches: 2,
	}, {
		name: "a Stop beside the loop's stop of a leftover server",
		setup: func(r *rig) int {
			r.script("claude", counting(r))
			return r.stray()
		},
		first: func(r *rig) error {
			// The loop's pre-launch stop of the stray is the first stopper.
			return r.m.Start(context.Background(), wsID)
		},
		second: func(r *rig) error { return r.m.Stop(context.Background(), wsID) },
		// The stop was asked before the loop could launch: nothing is.
		launches: 0,
	}, {
		name: "two Stops of a server no supervisor holds",
		setup: func(r *rig) int {
			r.script("claude", counting(r))
			return r.stray()
		},
		first:    func(r *rig) error { return r.m.Stop(context.Background(), wsID) },
		second:   func(r *rig) error { return r.m.Stop(context.Background(), wsID) },
		launches: 0,
	}, {
		// A Start replaces a supervisor that is stopping, and the new loop's
		// stop of a leftover server is the second stopper: it waits for the
		// replaced one to be quiet, then finds nothing to send.
		name: "a Start beside a Stop",
		setup: func(r *rig) int {
			r.script("claude", counting(r))
			r.start()
			r.waitState(Serving, ReasonServing)
			return r.pid()
		},
		first:    func(r *rig) error { return r.m.Stop(context.Background(), wsID) },
		second:   func(r *rig) error { return r.m.Start(context.Background(), wsID) },
		launches: 2,
		// The replaced supervisor's exited, settled after the Start wrote
		// its starting, is not recorded over it: the card goes from the
		// new starting to serving.
		check: func(t *testing.T, r *rig) {
			got := r.sups()
			var tail []string
			for _, d := range got[len(got)-2:] {
				tail = append(tail, d.State+"/"+d.Reason)
			}
			if s := strings.Join(tail, " "); s != "starting/launching serving/connected" {
				t.Errorf("last events %q, want the new starting and serving", s)
			}
		},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) {
				p.StopTimeout, p.KillWait = 800*time.Millisecond, 500*time.Millisecond
			})
			server := c.setup(r)
			var wg sync.WaitGroup
			var firstErr, secondErr error
			wg.Add(2)
			go func() { defer wg.Done(); firstErr = c.first(r) }()
			r.waitFor(5*time.Second, "the first stop's SIGTERM delivered", func() bool { return r.terms() >= 1 })
			go func() { defer wg.Done(); secondErr = c.second(r) }()
			wg.Wait()
			if firstErr != nil || secondErr != nil {
				t.Fatalf("first %v, second %v", firstErr, secondErr)
			}
			if alive(server) {
				t.Fatal("control: the server is still running after both stops")
			}
			if n := r.terms(); n != 1 {
				t.Errorf("%d SIGTERMs delivered to one server by two stoppers, want 1", n)
			}
			if n := r.kills(); n != 1 {
				t.Errorf("%d SIGKILLs asked of the container, want 1:\n%s", n, r.dockerLog())
			}
			if c.launches > 0 {
				r.waitState(Serving, ReasonServing)
			}
			time.Sleep(300 * time.Millisecond)
			if n := r.launches(); n != c.launches {
				t.Errorf("%d launches, want %d", n, c.launches)
			}
			if c.check != nil {
				c.check(t, r)
			}
		})
	}
}

// A Stop when no supervision loop holds the server is answered by a sup born
// stopped, whose owner stops it through the pid file and replies: with
// nothing in memory and nothing running, with a stray an earlier Drydock
// left, beside a Park (whose sup only records), and for a loop that has
// parked and ended. Each answers, and the parked loop's stop records exited
// as a stop of a held supervisor always has; one with nothing held records
// nothing and leaves nothing registered. The control in each is the server,
// where there is one, gone.
func TestABornStoppedOwnerAnswers(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(r *rig) int // the server's pid, or 0
		record bool             // the stop says exited
	}{{
		name:  "nothing held, nothing running",
		setup: func(*rig) int { return 0 },
	}, {
		name: "nothing held, a stray",
		setup: func(r *rig) int {
			r.script("claude", serves)
			return r.stray()
		},
	}, {
		name: "beside a Park",
		setup: func(r *rig) int {
			r.script("claude", serves)
			pid := r.stray()
			if err := r.m.Park(context.Background(), wsID, ReasonStaleBrokerMount, "parked"); err != nil {
				r.t.Fatal(err)
			}
			return pid
		},
	}, {
		name: "a loop that parked",
		setup: func(r *rig) int {
			r.script("claude", "exit 1\n")
			r.start()
			r.waitState(Degraded, ReasonBudgetSpent)
			return 0
		},
		record: true,
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) { p.Budget = 1 })
			server := c.setup(r)
			before := len(r.sups())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := r.m.Stop(ctx, wsID); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if server != 0 && alive(server) {
				t.Error("control: the server is still running after the stop")
			}
			got := r.sups()[before:]
			switch {
			case c.record && (len(got) != 1 || got[0].State != string(Exited) || got[0].Reason != string(ReasonStopped)):
				t.Errorf("events %+v, want one exited/stopped", got)
			case !c.record && len(got) != 0:
				t.Errorf("events %+v for a stop of a server no supervisor held", got)
			}
			r.m.mu.Lock()
			held := r.m.sups[wsID]
			r.m.mu.Unlock()
			if held != nil {
				t.Errorf("a supervisor is still registered after a stop that worked")
			}
			// Asked again, it answers again.
			if err := r.m.Stop(ctx, wsID); err != nil {
				t.Errorf("a second stop: %v", err)
			}
		})
	}
}

// The loop's last look before a launch: a shutdown that lands after the
// workspace's spec is read and before the exec starts nothing. Before, the
// launch went ahead after shutdown, and the run's leave then abandoned a
// server just started. The control is the same held spec released with no
// shutdown: one launch, serving.
func TestNothingIsLaunchedAfterShutdown(t *testing.T) {
	for _, shut := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shut), func(t *testing.T) {
			r := newRig(t)
			r.script("claude", serves)
			spec := r.m.Spec
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			r.m.Spec = func(ctx context.Context, ws string) (container.SessionSpec, error) {
				once.Do(func() { close(entered); <-release })
				// What the server's real Spec reads under the loop's
				// context is not what is tested here: it answers.
				return spec(context.WithoutCancel(ctx), ws)
			}
			// Counted at the Runtime, not by the fake devcontainer's log: a
			// launch after shutdown is left at once, and can be killed before
			// it has written anything.
			starts := &countStarts{Runtime: r.m.Runtime}
			r.m.Runtime = starts
			r.start()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the loop never asked for the spec")
			}
			stopped := make(chan []string, 1)
			if shut {
				go func() { stopped <- r.detach(5 * time.Second) }()
				<-r.g.Ctx().Done()
			}
			close(release)
			if !shut {
				r.waitState(Serving, ReasonServing)
				if n := starts.n.Load(); n != 1 {
					t.Errorf("control: %d launches", n)
				}
				return
			}
			if late := <-stopped; late != nil {
				t.Errorf("still running after shutdown: %v", late)
			}
			if n := starts.n.Load(); n != 0 {
				t.Errorf("%d launches after shutdown", n)
			}
		})
	}
}

// countStarts counts the servers the supervisor asks its Runtime to start.
type countStarts struct {
	Runtime
	n atomic.Int32
}

func (c *countStarts) Start(ctx context.Context, spec container.SessionSpec, cols, rows int) (subproc.Process, *os.File, error) {
	c.n.Add(1)
	return c.Runtime.Start(ctx, spec, cols, rows)
}

// While the database refuses what the server announced, the run says so
// once, not on every chunk a chatty server prints, and tries again — on the
// next chunk, and on the heartbeat tick, so a server that has gone quiet
// still has its environment recorded once the database answers. The
// controls: the record lands once the refusal goes (so the log line was not
// silenced by nothing being tried), in the quiet case with no output after
// it.
func TestAFailingDiscoveryIsSaidOnceAndHealsQuietly(t *testing.T) {
	for _, chatty := range []bool{true, false} {
		t.Run(fmt.Sprintf("chatty=%v", chatty), func(t *testing.T) {
			r := newRig(t, func(_ *rig, p *Policy) { p.HeartbeatEvery = 300 * time.Millisecond })
			var mu sync.Mutex
			var logged []string
			r.m.Logf = func(format string, args ...any) {
				mu.Lock()
				logged = append(logged, fmt.Sprintf(format, args...))
				mu.Unlock()
				t.Logf(format, args...)
			}
			said := func() int {
				mu.Lock()
				defer mu.Unlock()
				n := 0
				for _, l := range logged {
					if strings.Contains(l, "recording what the session server announced") {
						n++
					}
				}
				return n
			}
			if _, err := r.db.Exec(`CREATE TRIGGER refuse_env BEFORE UPDATE OF environment_id ON workspace
				BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`); err != nil {
				t.Fatal(err)
			}
			const env = "env_01QUIET000000000000000000"
			body := "printf 'Environment ID: " + env + "\\r\\n    Capacity: 0/4 · x\\r\\n'\n"
			if chatty {
				body += "while :; do printf 'tick\\r\\n'; sleep 0.05; done\n"
			} else {
				body += ": > '" + filepath.Join(r.dir, "printed") + "'\nwhile :; do sleep 0.05; done\n"
			}
			r.script("claude", body)
			r.start()
			r.waitState(Serving, ReasonServing)
			if chatty {
				time.Sleep(time.Second) // twenty chunks, each a retry
				if n := said(); n != 1 {
					t.Errorf("the failing write was logged %d times in one run, want once", n)
				}
			} else {
				r.waitFor(5*time.Second, "the server's last output", func() bool {
					_, err := os.Stat(filepath.Join(r.dir, "printed"))
					return err == nil
				})
				time.Sleep(200 * time.Millisecond)
			}
			if got := r.environment(); got != "" {
				t.Fatalf("control: environment %q recorded while refused", got)
			}
			if _, err := r.db.Exec(`DROP TRIGGER refuse_env`); err != nil {
				t.Fatal(err)
			}
			r.waitFor(5*time.Second, "the environment recorded", func() bool { return r.environment() == env })
		})
	}
}
