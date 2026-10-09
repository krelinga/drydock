//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// Stop is SIGTERM, delivered in the container: the server's clean shutdown
// runs (fakeclaude replays the recorded "Environment preserved" only on
// SIGTERM), it exits 0, and no SIGKILL is sent.
func TestStopIsSIGTERMFirst(t *testing.T) {
	r := newRig(t)
	f := r.claude(claudetest.Step{Mode: claudetest.RCServe})
	r.start()
	r.waitState(Serving, ReasonServing)
	pid := r.pid()
	if err := r.m.Stop(context.Background(), wsID); err != nil {
		t.Fatal(err)
	}
	if alive(pid) {
		t.Error("the server is still running after Stop returned")
	}
	if !strings.Contains(r.logText(), "Environment preserved") {
		t.Errorf("no clean shutdown in the log:\n%s", r.logText())
	}
	exits := claudetest.Kind(f.Events(t), claudetest.EventExit)
	if len(exits) != 1 || exits[0].Code != 0 {
		t.Errorf("exits %+v, want one clean exit", exits)
	}
	if strings.Contains(r.dockerLog(), "rc.pid KILL") {
		t.Errorf("SIGKILL sent to a server that stops on SIGTERM:\n%s", r.dockerLog())
	}
	if st, _ := r.row(); st != Exited {
		t.Errorf("state %s after a stop, want exited", st)
	}
	if n := r.invocations(); n != 1 {
		t.Errorf("%d starts: a stopped server must not be restarted", n)
	}
}

// A server that ignores SIGTERM is SIGKILLed — but only after the timeout,
// and only after SIGTERM was tried.
func TestSIGKILLOnlyAfterTheTimeout(t *testing.T) {
	const grace = 700 * time.Millisecond
	r := newRig(t, func(_ *rig, p *Policy) { p.StopTimeout = grace })
	r.script("claude", `trap '' TERM
printf 'Environment ID: env_01STUBBORN0000000000000000\r\n    Capacity: 0/4 · x\r\n'
while :; do sleep 0.05; done
`)
	r.start()
	r.waitState(Serving, ReasonServing)
	pid := r.pid()
	began := time.Now()
	if err := r.m.Stop(context.Background(), wsID); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	if alive(pid) {
		t.Fatal("the server survived SIGKILL")
	}
	log := r.dockerLog()
	term, kill := strings.Index(log, "rc.pid TERM"), strings.Index(log, "rc.pid KILL")
	if term < 0 || kill < 0 || kill < term {
		t.Errorf("want TERM then KILL in the docker log:\n%s", log)
	}
	if took < grace {
		t.Errorf("SIGKILL after %v, inside the %v grace period", took, grace)
	}
}

// A stop whose context is cancelled during the SIGTERM grace period sends no
// SIGKILL, and its log says so rather than "sending SIGKILL". The control is
// the same stubborn server stopped with a live context, whose log does say
// it and whose docker log has the KILL (TestSIGKILLOnlyAfterTheTimeout covers
// the order; this re-checks the wording against the same rig).
func TestACancelledStopSaysItSentNoSIGKILL(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		r := newRig(t, func(_ *rig, p *Policy) { p.StopTimeout = 700 * time.Millisecond })
		var mu sync.Mutex
		var logged []string
		r.m.Logf = func(format string, args ...any) {
			mu.Lock()
			logged = append(logged, fmt.Sprintf(format, args...))
			mu.Unlock()
			t.Logf(format, args...)
		}
		r.script("claude", `trap '' TERM
printf 'Environment ID: env_01STUBBORN0000000000000000\r\n    Capacity: 0/4 · x\r\n'
while :; do sleep 0.05; done
`)
		r.start()
		r.waitState(Serving, ReasonServing)
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			time.AfterFunc(200*time.Millisecond, cancel)
		}
		err := r.m.Stop(ctx, wsID)
		cancel()
		mu.Lock()
		text := strings.Join(logged, "\n")
		mu.Unlock()
		kills := strings.Contains(r.dockerLog(), "rc.pid KILL")
		if cancelled {
			if err == nil {
				t.Error("a cancelled stop reported success")
			}
			if strings.Contains(text, "sending SIGKILL") || kills {
				t.Errorf("a cancelled stop claimed or sent a SIGKILL (sent %v):\n%s", kills, text)
			}
			if !strings.Contains(text, "SIGKILL was not sent") {
				t.Errorf("a cancelled stop did not say it sent no SIGKILL:\n%s", text)
			}
		} else if !strings.Contains(text, "sending SIGKILL") || !kills {
			t.Errorf("control: a stop that timed out logged %q and sent KILL %v", text, kills)
		}
	}
}

// A Drydock restart leaves the server running in the container with no
// terminal (container.SignalSession's measurement). The next start stops that
// server first — SIGTERM, so it deregisters — and the new one comes back on
// the same environment. Without the stray stopped, the new start would be
// refused as already served for as long as the stray lived.
func TestAStrayServerIsStoppedBeforeTheNextStart(t *testing.T) {
	r := newRig(t)
	f := r.claude(claudetest.Step{Mode: claudetest.RCServe})
	cmd := exec.Command("sh", "-c", container.RemoteControlLaunch, "sh", filepath.Join(r.dir, "rc.pid"), "4")
	cmd.Env = []string{"PATH=" + r.bin + ":/usr/bin:/bin"}
	stray := claudetest.StartTerm(t, cmd, 200, 50)
	if _, err := stray.WaitFor(10*time.Second, []byte("Capacity: 1/4")); err != nil {
		t.Fatalf("the stray did not serve: %v", err)
	}
	r.start()
	r.waitState(Serving, ReasonServing)
	if exited, code, out := stray.Wait(5 * time.Second); !exited || code != 0 || !bytes.Contains(out, []byte("Environment preserved")) {
		t.Errorf("the stray was not stopped cleanly: exited %v code %d", exited, code)
	}
	if got := r.environment(); got != envFixed {
		t.Errorf("environment %q after the restart, want %q", got, envFixed)
	}
	if n := len(claudetest.Kind(f.Events(t), claudetest.EventStart)); n != 2 {
		t.Errorf("%d starts, want the stray and one more", n)
	}
	if _, n := r.row(); n != 0 {
		t.Errorf("restart_count %d: adopting is not a crash", n)
	}
}

// Restart: the same environment, the same session, nothing counted.
func TestRestartKeepsTheEnvironment(t *testing.T) {
	r := newRig(t)
	r.claude(claudetest.Step{Mode: claudetest.RCServe})
	r.start()
	r.waitState(Serving, ReasonServing)
	first := r.pid()
	if err := r.m.Restart(context.Background(), wsID); err != nil {
		t.Fatal(err)
	}
	// Restart returns once the new server has said its first state, never
	// on the stop's exited: the provisioner's job ends the press as it
	// returns (workspace.job), and the card would show a stopped server.
	evs, err := r.log.ForWorkspace(context.Background(), wsID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, ev := range evs { // newest first
		var d workspace.SupervisorData
		if ev.Kind == workspace.KindSupervisor && json.Unmarshal(ev.Data, &d) == nil {
			said = append(said, d.State)
		}
	}
	if len(said) < 2 || said[0] == string(Exited) || said[1] != string(Exited) {
		t.Errorf("supervisor states as Restart returned, newest first: %v; want the new server's after the stop's exited", said)
	}
	r.waitFor(10*time.Second, "a new server", func() bool { p := r.pid(); return p != first && p != 0 })
	r.waitState(Serving, ReasonServing)
	if got := r.environment(); got != envFixed {
		t.Errorf("environment %q, want %q", got, envFixed)
	}
	if got := r.sessions(); len(got) != 1 {
		t.Errorf("sessions %v: the reconnected session is the same row", got)
	}
	if _, n := r.row(); n != 0 {
		t.Errorf("restart_count %d after an operator restart", n)
	}
}

// A signed-out fleet starts nothing and spends nothing; signing in starts
// the server that was waiting. The control is the same rig with ok. Expired
// is not signed out (TestAnExpiredAccessTokenStillStarts).
func TestASignedOutIdentityDefersTheStart(t *testing.T) {
	for _, st := range []string{"blanked", "absent"} {
		t.Run(st, func(t *testing.T) {
			r := newRig(t)
			r.claude(claudetest.Step{Mode: claudetest.RCServe})
			if _, err := r.db.Exec(`INSERT INTO claude_identity (id, volume_name, state) VALUES (1, 'v', ?)`, st); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go r.m.Watch(ctx)
			r.start()
			r.waitState(AwaitingLogin, ReasonSignedOut)
			time.Sleep(200 * time.Millisecond)
			if n := r.invocations(); n != 0 {
				t.Fatalf("%d starts while signed out", n)
			}
			r.db.Exec(`UPDATE claude_identity SET state = 'ok' WHERE id = 1`)
			r.log.Emit(ctx, "", events.Info, "auth.identity", "Signed in.", map[string]any{})
			r.waitState(Serving, ReasonServing)
			if _, n := r.row(); n != 0 {
				t.Errorf("restart_count %d", n)
			}
		})
	}
}

// A server that exits while the credential is blanked is not restarted and
// spends nothing: the fleet fault owns it (frontend §6.6). The control: the
// same exit with the login fine is a counted crash and a restart.
func TestAnExitWhileSignedOutIsNotACrash(t *testing.T) {
	for _, blank := range []bool{true, false} {
		r := newRig(t)
		r.claude(claudetest.Step{Mode: claudetest.RCCrash, Times: 1, ExitAfter: dur(600 * time.Millisecond)},
			claudetest.Step{Mode: claudetest.RCServe})
		r.db.Exec(`INSERT INTO claude_identity (id, volume_name, state) VALUES (1, 'v', 'ok')`)
		r.start()
		r.waitFor(5*time.Second, "the start", func() bool { return r.invocations() == 1 })
		if blank {
			r.db.Exec(`UPDATE claude_identity SET state = 'blanked' WHERE id = 1`)
			r.waitState(AwaitingLogin, ReasonSignedOut)
			time.Sleep(200 * time.Millisecond)
			if _, n := r.row(); n != 0 || r.invocations() != 1 {
				t.Errorf("blanked: restart_count %d, %d starts; want 0 and 1", n, r.invocations())
			}
		} else {
			r.waitState(Serving, ReasonServing)
			if _, n := r.row(); n != 1 {
				t.Errorf("control: restart_count %d, want 1", n)
			}
		}
	}
}

// The prelude fails the start rather than launching a server whose sessions
// all lack their secrets (§8): claude never runs, and the exit is a counted
// crash naming the fetch.
func TestASecretsFailureFailsTheStart(t *testing.T) {
	r := newRig(t, func(_ *rig, p *Policy) { p.Budget = 1 })
	r.claude(claudetest.Step{Mode: claudetest.RCServe})
	r.script("drydock-secrets", "echo 'drydock: secrets unavailable: the broker did not answer' >&2\necho 'exit 69'\n")
	r.start()
	r.waitState(Degraded, ReasonBudgetSpent)
	if n := r.invocations(); n != 0 {
		t.Errorf("claude ran %d times without its secrets", n)
	}
	if d := r.last().Detail; !strings.Contains(d, "secrets") {
		t.Errorf("detail %q does not name the secrets fetch", d)
	}
}

// Redact by default: a token-shaped string and a granted secret's value the
// server printed are masked in the log, and neither reaches the database
// file or the event log. The line around them is kept (the control), so the
// sweep is not passing on an empty log.
func TestTheLogIsRedacted(t *testing.T) {
	const token = "ghs_CANARYabcdefghijklmnopqrstuvwxyz0123"
	const secret = "S3CR3T-CANARY-VALUE-41c7"
	r := newRig(t)
	r.m.Redact = func(context.Context, string) []string { return []string{secret} }
	r.script("claude", `printf 'Environment ID: env_01REDACTED000000000000000\r\n'
printf 'token `+token[:10]+`'
sleep 0.1
printf '`+token[10:]+` and secret `+secret+` end\r\n    Capacity: 0/4 · x\r\n'
while :; do sleep 0.05; done
`)
	r.start()
	r.waitState(Serving, ReasonServing)
	log := r.logText()
	if !strings.Contains(log, "token [redacted] and secret [redacted] end") {
		t.Errorf("control: the redacted line is not in the log:\n%s", log)
	}
	if strings.Contains(log, token[4:]) || strings.Contains(log, secret) {
		t.Errorf("a canary is in the log:\n%s", log)
	}
	r.m.Stop(context.Background(), wsID)
	r.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, p := range []string{"drydock.db", "drydock.db-wal"} {
		b, _ := os.ReadFile(filepath.Join(r.dir, p))
		if bytes.Contains(b, []byte(token[4:])) || bytes.Contains(b, []byte(secret)) {
			t.Errorf("a canary is in %s", p)
		}
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, "drydock.db"))
	if !bytes.Contains(b, []byte("env_01REDACTED")) {
		t.Error("control: the environment id is not in the database, so the sweep proves nothing")
	}
}

// Redact by default, to the last byte: what a server prints after its last
// line break — its parting words before it exits, the tail it was writing
// when it was stopped, the prompt it hangs at — is held back until a Flush,
// and a Flush masks the workspace's secret values exactly as a terminated
// line does. The control, in each case, is a terminated line carrying the
// same value, masked; and the unterminated line's own text must be present,
// so the test fails if the flushed line is missing as well as if it is in
// clear.
func TestTheLogIsRedactedToItsLastLine(t *testing.T) {
	const secret = "S3CR3T-CANARY-VALUE-41c7"
	const served = "printf 'Environment ID: env_01REDACTED000000000000000\\r\\n    Capacity: 0/4 · x\\r\\n'\n"
	cases := []struct {
		name   string
		opt    rigOpt
		script func(r *rig) string
		end    func(r *rig)
	}{{
		// The server exits on its own, mid-line: flushed when the run ends.
		name: "exit",
		opt:  func(_ *rig, p *Policy) { p.Budget = 0 },
		script: func(*rig) string {
			return served + "printf 'control " + secret + " terminated\\r\\n'\n" +
				"printf 'fatal: " + secret + " no newline'\nsleep 0.3\nexit 1\n"
		},
		end: func(r *rig) { r.waitState(Degraded, ReasonBudgetSpent) },
	}, {
		// Drydock stops it mid-line: flushed when the stopped run ends.
		name: "stop",
		script: func(r *rig) string {
			return served + "printf 'control " + secret + " terminated\\r\\n'\n" +
				"printf 'stopping: " + secret + " no newline'\n" +
				": > '" + filepath.Join(r.dir, "printed") + "'\nwhile :; do sleep 0.05; done\n"
		},
		end: func(r *rig) {
			r.waitState(Serving, ReasonServing)
			r.waitFor(5*time.Second, "the unterminated line printed", func() bool {
				_, err := os.Stat(filepath.Join(r.dir, "printed"))
				return err == nil
			})
			time.Sleep(200 * time.Millisecond) // and read off the terminal
			if err := r.m.Stop(context.Background(), wsID); err != nil {
				r.t.Fatal(err)
			}
		},
	}, {
		// It never announces an environment and waits mid-line: flushed at
		// the gate, so the log shows what it waits at.
		name: "gate",
		opt:  func(_ *rig, p *Policy) { p.GateTimeout = 500 * time.Millisecond; p.Budget = 0 },
		script: func(*rig) string {
			return "printf 'control " + secret + " terminated\\r\\n'\n" +
				"printf 'waiting: " + secret + " no newline'\nwhile :; do sleep 0.05; done\n"
		},
		end: func(r *rig) { r.waitState(Degraded, ReasonBudgetSpent) },
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []rigOpt
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			r := newRig(t, opts...)
			r.m.Redact = func(context.Context, string) []string { return []string{secret} }
			r.script("claude", tc.script(r))
			r.start()
			tc.end(r)
			log := r.logText()
			if !strings.Contains(log, "control [redacted] terminated") {
				t.Errorf("control: the terminated line is not in the log, masked:\n%s", log)
			}
			if !strings.Contains(log, "[redacted] no newline") {
				t.Errorf("the unterminated last line is not in the log, masked:\n%s", log)
			}
			if strings.Contains(log, secret) {
				t.Errorf("the secret's value is in the log:\n%s", log)
			}
		})
	}
}

// Park records degraded with its reason and Drydock's sentence, starts
// nothing, and is not restarted by anything but an explicit Start — which,
// the control, then serves as usual.
func TestParkStartsNothing(t *testing.T) {
	r := newRig(t)
	r.claude(claudetest.Step{Mode: claudetest.RCServe})
	const why = "Rebuild it once."
	if err := r.m.Park(context.Background(), wsID, ReasonStaleBrokerMount, why); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.row(); st != Degraded {
		t.Errorf("row state %s, want degraded", st)
	}
	if l := r.last(); l.State != string(Degraded) || l.Reason != string(ReasonStaleBrokerMount) || l.Detail != why {
		t.Errorf("event %+v", l)
	}
	time.Sleep(200 * time.Millisecond)
	if n := r.invocations(); n != 0 {
		t.Errorf("%d starts after Park", n)
	}
	r.start()
	r.waitState(Serving, ReasonServing)
	if n := r.invocations(); n != 1 {
		t.Errorf("control: %d starts after Start", n)
	}
}
