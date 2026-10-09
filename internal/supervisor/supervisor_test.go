//go:build linux

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// A server that serves: the argv is the design's, the terminal is a real
// one, the environment id lands on the workspace, the session the server
// announced in an OSC 8 hyperlink is upserted as the primary, and the capacity
// fraction reaches the stream.
func TestServeDiscoversTheEnvironmentAndTheSession(t *testing.T) {
	r := newRig(t)
	f := r.claude(claudetest.Step{Mode: claudetest.RCServe})
	r.start()
	r.waitState(Serving, ReasonServing)
	r.waitFor(5*time.Second, "the session", func() bool { return len(r.sessions()) == 1 })

	if got := r.environment(); got != envFixed {
		t.Errorf("workspace.environment_id = %q, want %q", got, envFixed)
	}
	if got := r.sessions(); !slices.Equal(got, []string{sessFix}) {
		t.Errorf("rc_session = %v, want [%s]", got, sessFix)
	}
	var primary int
	r.db.QueryRow(`SELECT is_primary FROM rc_session WHERE id = ?`, sessFix).Scan(&primary)
	if primary != 1 {
		t.Error("the first session is not marked primary")
	}
	// The view reads it back exactly as the stream said it.
	ws := &workspace.Store{DB: r.db.DB, Events: r.log}
	r.waitFor(5*time.Second, "capacity 1/4 in the view", func() bool {
		v, err := ws.View(context.Background(), wsID)
		return err == nil && v.Session != nil && v.Session.CapacityUsed != nil && *v.Session.CapacityUsed == 1 &&
			*v.Session.CapacityTotal == 4 && v.Session.URL == "https://claude.ai/code?environment="+envFixed &&
			v.EnvironmentID != nil && *v.EnvironmentID == envFixed && v.Supervisor != nil && v.Supervisor.State == "serving"
	})

	starts := claudetest.Kind(f.Events(t), claudetest.EventStart)
	if len(starts) != 1 {
		t.Fatalf("%d starts, want 1", len(starts))
	}
	if got := strings.Join(starts[0].Argv, " "); got != "remote-control --spawn worktree --capacity 4 --verbose" {
		t.Errorf("argv %q", got)
	}
	if !starts[0].TTY || starts[0].Width != 200 {
		t.Errorf("the server did not get Drydock's terminal: tty %v width %d", starts[0].TTY, starts[0].Width)
	}
	dc, _ := os.ReadFile(r.devc)
	if !strings.Contains(string(dc), "exec --workspace-folder "+filepath.Join(r.dir, "repo")+" --id-label drytest.workspace="+wsID+
		" --remote-env CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX=repo -- sh -c") {
		t.Errorf("devcontainer argv:\n%s", dc)
	}
	f.NoViolations(t)
}

// Discovery takes session ids from OSC 8 targets only. The model printing a
// session id in its own prose is visible text and must not become a row —
// and the announced session in the same stream must (the control).
func TestAnIDTheModelPrintedIsNotASession(t *testing.T) {
	r := newRig(t)
	r.claude(claudetest.Step{Mode: claudetest.RCServeModelOutputID, SessionDelay: dur(200 * time.Millisecond)})
	r.start()
	r.waitFor(10*time.Second, "the model's prose in the log", func() bool {
		return strings.Contains(r.logText(), "session_01SYNTHETICMODELOUTPUT00")
	})
	time.Sleep(200 * time.Millisecond)
	got := r.sessions()
	if slices.Contains(got, "session_01SYNTHETICMODELOUTPUT00") {
		t.Errorf("the model-printed id was recorded as a session: %v", got)
	}
	if !slices.Contains(got, sessFix) {
		t.Errorf("control: the announced session is missing: %v", got)
	}
}

// A session announced long after the start still upserts: the tail is
// continuous, not a scrape at startup.
func TestASessionAnnouncedLaterStillUpserts(t *testing.T) {
	r := newRig(t)
	r.claude(claudetest.Step{Mode: claudetest.RCServeDelayedSession, SessionDelay: dur(700 * time.Millisecond)})
	r.start()
	r.waitState(Serving, ReasonServing)
	if n := len(r.sessions()); n != 1 {
		t.Fatalf("%d sessions before the delay, want 1", n)
	}
	r.waitFor(10*time.Second, "the second session", func() bool { return len(r.sessions()) == 2 })
}

// All four refusals exit 1, and they lead to four different places. Exit
// status is not the discriminator; the message is.
func TestFourRefusalsOneExitCodeFourVerdicts(t *testing.T) {
	for _, tc := range []struct {
		mode   claudetest.RCMode
		state  State
		reason Reason
		retry  bool
	}{
		{claudetest.RCRefuseWaitRegistration, WaitingRegistration, ReasonWaitRegistration, true},
		{claudetest.RCRefuseNotTrusted, Degraded, ReasonNotTrusted, false},
		{claudetest.RCRefuseNoOrganization, AwaitingLogin, ReasonNoOrganization, false},
		{claudetest.RCRefuseBadCommandLine, Degraded, ReasonBadCommandLine, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			r := newRig(t)
			f := r.claude(claudetest.Step{Mode: tc.mode})
			r.start()
			r.waitState(tc.state, tc.reason)
			if tc.retry {
				r.waitFor(5*time.Second, "a second attempt", func() bool { return r.invocations() >= 3 })
			} else {
				time.Sleep(300 * time.Millisecond)
				if n := r.invocations(); n != 1 {
					t.Errorf("%d attempts; a %s refusal must not be retried", n, tc.reason)
				}
			}
			for _, e := range claudetest.Kind(f.Events(t), claudetest.EventExit) {
				if e.Code != 1 {
					t.Errorf("fakeclaude exited %d, want 1: the test depends on all four exiting alike", e.Code)
				}
			}
			if _, n := r.row(); n != 0 {
				t.Errorf("restart_count %d: a refusal is not a crash", n)
			}
		})
	}
}

// The registration wait is a wait. A registration that lapses after a span
// the test chose — no constant in the supervisor knows it — is waited out
// with a restart budget of one, and the server then serves with nothing
// spent. The control: crashes do spend that budget, and park it.
func TestTheRegistrationWaitDoesNotSpendTheBudget(t *testing.T) {
	budgetOne := func(_ *rig, p *Policy) { p.Budget = 1 }
	r := newRig(t, budgetOne)
	r.claude(claudetest.Step{Mode: claudetest.RCRefuseWaitRegistration, For: dur(1500 * time.Millisecond)},
		claudetest.Step{Mode: claudetest.RCServe})
	r.start()
	r.waitState(WaitingRegistration, ReasonWaitRegistration)
	r.waitState(Serving, ReasonServing)
	if n := r.invocations(); n < 4 {
		t.Errorf("only %d attempts: the wait should have been asked again on its interval", n)
	}
	if _, n := r.row(); n != 0 {
		t.Errorf("restart_count %d after a registration wait; want 0", n)
	}
	for _, s := range r.sups() {
		if s.State == string(Degraded) || s.Reason == string(ReasonBackoff) {
			t.Errorf("the wait was treated as a crash: %+v", s)
		}
	}

	c := newRig(t, budgetOne)
	c.claude(claudetest.Step{Mode: claudetest.RCCrash, Times: 3}, claudetest.Step{Mode: claudetest.RCServe})
	c.start()
	c.waitState(Degraded, ReasonBudgetSpent)
	if _, n := c.row(); n != 1 {
		t.Errorf("control: restart_count %d, want 1 (one restart allowed, the second crash parks it)", n)
	}
}

// A crash loop that recovers inside the budget is restarted with backoff and
// ends serving, each restart counted.
func TestACrashLoopIsRestartedWithBackoff(t *testing.T) {
	r := newRig(t)
	r.claude(claudetest.Step{Mode: claudetest.RCCrash, Times: 3}, claudetest.Step{Mode: claudetest.RCServe})
	r.start()
	r.waitState(Serving, ReasonServing)
	if _, n := r.row(); n != 3 {
		t.Errorf("restart_count %d, want 3", n)
	}
	var backoffs int
	for _, s := range r.sups() {
		if s.Reason == string(ReasonBackoff) {
			backoffs++
		}
	}
	if backoffs != 3 {
		t.Errorf("%d backoff states, want 3", backoffs)
	}
}

func TestBackoffDoublesToItsCap(t *testing.T) {
	p := Policy{Backoff: 2 * time.Second, BackoffMax: 60 * time.Second}
	var got []time.Duration
	for n := 1; n <= 7; n++ {
		got = append(got, p.BackoffFor(n))
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("BackoffFor = %v, want %v s", got, want)
		}
	}
}

// The two gates hang rather than fail, so the verdict is a timeout: nothing
// is decided before it, the right key is named after it, and nothing was ever
// typed at the prompt.
func TestAHungGateIsATimeoutNamingTheKey(t *testing.T) {
	for _, tc := range []struct {
		mode   claudetest.RCMode
		reason Reason
		key    string
	}{
		{claudetest.RCHangRemoteDialog, ReasonHangRemoteDialog, "remoteDialogSeen"},
		{claudetest.RCHangTrust, ReasonHangTrust, "hasTrustDialogAccepted"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			// The gate runs on a fake clock, moved only once the server
			// waits at its prompt: on the wall clock, a test goroutine
			// stalled past the gate (a loaded runner) saw the verdict
			// before it had looked for its absence. Nothing else on this
			// clock is set for the gate's duration: the heartbeat is an
			// hour, and the stop's bounds are not set until the gate fires.
			const gate = 1500 * time.Millisecond
			clock := sys.NewFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
			r := newRig(t, func(_ *rig, p *Policy) { p.GateTimeout, p.HeartbeatEvery = gate, time.Hour })
			r.m.Env.Clock = clock
			f := r.claude(claudetest.Step{Mode: tc.mode})
			r.start()
			r.waitFor(5*time.Second, "the server started", func() bool { return r.invocations() == 1 })
			r.waitFor(5*time.Second, "the run to set its gate", func() bool { return clock.WaitingFor(gate) == 1 })
			armed := clock.Now()
			// The prompt is in the run's tail, as the gate's verdict will
			// read it: it is what names the key.
			r.waitFor(5*time.Second, "the server to wait at its prompt", func() bool { return hangReason(r.heldBack()) == tc.reason })
			clock.Advance(gate - time.Millisecond)
			// Advance fires what is due before it returns: a gate still set
			// afterwards did not fire, so no verdict can be on its way.
			if n := clock.WaitingFor(gate); n != 1 {
				t.Fatalf("a millisecond short of the gate: %d gate timers pending; want it still set", n)
			}
			if l := r.last(); l.State != string(Starting) {
				t.Fatalf("a verdict before the timeout: %+v", l)
			}
			clock.Advance(time.Millisecond)
			r.waitState(Degraded, tc.reason)
			if el := clock.Since(armed); el < gate {
				t.Errorf("degraded after %v, before the %v timeout", el, gate)
			}
			if d := r.last().Detail; !strings.Contains(d, tc.key) {
				t.Errorf("detail does not name %s: %q", tc.key, d)
			}
			r.waitFor(5*time.Second, "the hung server gone", func() bool { return !alive(r.pid()) })
			if strings.Index(r.dockerLog(), " TERM") < 0 {
				t.Errorf("the hung server was not sent SIGTERM:\n%s", r.dockerLog())
			}
			if n := r.invocations(); n != 1 {
				t.Errorf("%d attempts: a hung gate is a config error, not retried", n)
			}
			f.NoViolations(t) // nothing typed at the gate
		})
	}
}
