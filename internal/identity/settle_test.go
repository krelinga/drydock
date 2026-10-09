package identity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/life"
)

// Every Trigger — POST /api/auth/claude/check — is answered by an event
// written after it, whatever the check found, because the UI's "Check now" is
// in flight until one arrives (frontend §4.2). A check nobody asked for stays
// silent when nothing changed (TestRecoveryIsAnnouncedEvenWithoutAChange).
// Each interleaving below is forced through the observe seam or a gated read,
// never left to the scheduler.

func (h *harness) since(t *testing.T, n int) []string {
	t.Helper()
	return h.kinds(t)[n:]
}

// storeOK stores an ok verdict through an unrequested check and returns how
// many events the log holds afterwards.
func (h *harness) storeOK(t *testing.T) int {
	t.Helper()
	h.src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	if _, err := h.w.Check(waitCtx(t)); err != nil {
		t.Fatal(err)
	}
	return len(h.kinds(t))
}

// TestARequestedCheckIsAnsweredWithoutAChange is the bug from #81's review:
// the healthy case — a check that finds the same ok as before — wrote no
// event, so the button that asked for it stayed in flight until a reload. A
// requested check with nothing to announce writes auth.identity_checked,
// carrying the view with last_checked_at moved. The control is the same
// unchanged check unrequested, which writes nothing; and a requested check
// that has something to say says it once, with no second event.
func TestARequestedCheckIsAnsweredWithoutAChange(t *testing.T) {
	h := newHarness(t)
	n := h.storeOK(t)

	// Control: unrequested and unchanged is silent.
	h.clock.Advance(time.Minute)
	if _, err := h.w.Check(waitCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got := h.since(t, n); len(got) != 0 {
		t.Fatalf("an unrequested check with no change wrote %v; want nothing", got)
	}

	// Requested and unchanged: answered.
	h.clock.Advance(time.Minute)
	if err := h.w.Trigger(); err != nil {
		t.Fatal(err)
	}
	h.settled(t, "the requested check did not end")
	evs := h.events(t)[n:]
	if len(evs) != 1 || evs[0].Kind != KindChecked {
		t.Fatalf("a requested check with no change wrote %v; want [%s]", h.since(t, n), KindChecked)
	}
	var data struct{ Identity View }
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	stateIs(t, data.Identity, OK)
	if want := h.clock.Now().UTC(); data.Identity.LastCheckedAt == nil || !data.Identity.LastCheckedAt.Equal(want) {
		t.Errorf("last_checked_at = %v; want %v, the check that answered", data.Identity.LastCheckedAt, want)
	}

	// A requested check that changed something says so once: its
	// auth.identity is the answer.
	n = len(h.kinds(t))
	h.src.set(nil, nil, nil, nil)
	h.w.Trigger()
	h.settled(t, "the requested check did not end")
	if got := h.since(t, n); strings.Join(got, ",") != KindIdentity {
		t.Errorf("a requested check that changed the verdict wrote %v; want [%s]", got, KindIdentity)
	}
	// And a requested check that failed: its auth.identity_check_failed.
	n = len(h.kinds(t))
	h.src.set(nil, nil, &ReadError{Problem: ProblemDocker, Detail: "x"}, nil)
	h.w.Trigger()
	h.settled(t, "the requested check did not end")
	if got := h.since(t, n); strings.Join(got, ",") != KindCheckFailed {
		t.Errorf("a requested check that failed wrote %v; want [%s]", got, KindCheckFailed)
	}
}

// TestATriggerDuringACheckIsAnsweredByTheNext replaces #81's join: a press
// while an unrequested check (the interval's, Check's) is running is not
// answered by it — it read the volume before the press — but by a fresh
// check after it, which answers even though, unchanged, it would have said
// nothing. The held check stays silent. The control is the same held check
// with no Trigger: it ends silent, and no second check runs.
func TestATriggerDuringACheckIsAnsweredByTheNext(t *testing.T) {
	for _, trigger := range []bool{false, true} {
		h := newHarness(t)
		n := h.storeOK(t)
		src := &gatedSource{fakeSource: h.src, in: make(chan struct{}), gate: make(chan struct{})}
		h.w.Source = src

		done := make(chan struct{})
		go func() { h.w.Check(waitCtx(t)); close(done) }() // an unrequested check
		receive(t, src.in, "the held check did not start its read")
		if trigger {
			if err := h.w.Trigger(); err != nil {
				t.Fatal(err)
			}
		}
		close(src.gate)
		receive(t, done, "the held check did not end")
		h.settled(t, "the Trigger's check did not end")

		got := h.since(t, n)
		reads := h.calls()
		switch {
		case trigger && (strings.Join(got, ",") != KindChecked || reads != 3):
			t.Errorf("a press during a check: events %v, %d reads; want [%s] from a check of its own (3 reads)", got, reads, KindChecked)
		case !trigger && (len(got) != 0 || reads != 2):
			t.Errorf("control: the held check with no Trigger: events %v, %d reads; want nothing, and no second check", got, reads)
		}
	}
}

// TestATriggerAfterTheAnnouncementIsStillAnswered: a Trigger that arrives
// after a running check wrote its auth.identity, but before the check ended,
// would be lost to a client that took its "from" after that event. It is
// answered by a check of its own, after. The seam's "announced" makes the
// Trigger synchronously, on the worker, right after the event, so it
// provably lands inside the check. The control is the same check with no
// Trigger, which writes no auth.identity_checked.
func TestATriggerAfterTheAnnouncementIsStillAnswered(t *testing.T) {
	for _, trigger := range []bool{false, true} {
		h := newHarness(t)
		h.src.set(nil, nil, nil, nil) // absent: the first check announces
		var once sync.Once
		h.w.observe = func(p string) {
			if p == "announced" && trigger {
				once.Do(func() {
					if err := h.w.Trigger(); err != nil {
						t.Error(err)
					}
				})
			}
		}
		if _, err := h.w.Check(waitCtx(t)); err != nil {
			t.Fatal(err)
		}
		h.settled(t, "the late Trigger's check did not end")
		got := strings.Join(h.kinds(t), ",")
		want := KindIdentity
		if trigger {
			want += "," + KindChecked
		}
		if got != want {
			t.Errorf("trigger=%v: events [%s]; want [%s]", trigger, got, want)
		}
	}
}

// TestACallersContextNeverCutsACheck is #85's finding 1, removed as a
// possibility rather than fixed. The handshake's LoggedIn once ran its check
// under its own five-minute bound, shorter than a first build, and a press
// that joined that check went unanswered when the bound ended it. Now every
// check runs on the worker under the watch's group: a caller whose context
// ends — LoggedIn's or Check's — stops waiting, and the check it asked for
// goes on, stores its verdict and, for LoggedIn, takes the handshake's
// moment. A press made meanwhile is answered by a check of its own. The
// control is the same cut-off wait with no press: the verdict stored, no
// event (unchanged) and no further check.
func TestACallersContextNeverCutsACheck(t *testing.T) {
	for _, trigger := range []bool{false, true} {
		h := newHarness(t)
		n := h.storeOK(t)
		src := &gatedSource{fakeSource: h.src, in: make(chan struct{}), gate: make(chan struct{})}
		h.w.Source = src
		h.clock.Advance(time.Hour)
		at := h.clock.Now().Add(-10 * time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.w.LoggedIn(ctx, at) }() // the handshake's
		receive(t, src.in, "the handshake's check did not start its read")
		if trigger {
			h.w.Trigger()
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the handshake's wait returned %v; want its context's error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the handshake's wait did not end with its context")
		}
		close(src.gate)
		h.settled(t, "the checks did not end")

		v, err := h.w.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if v.LoggedInAt == nil || !v.LoggedInAt.Equal(at) {
			t.Errorf("trigger=%v: logged_in_at = %v; want the handshake's %v — its check was cut off", trigger, v.LoggedInAt, at)
		}
		got := h.since(t, n)
		reads := h.calls()
		// The handshake's check changed logged_in_at, so it announces; the
		// press after it finds nothing new and is answered as such.
		want := KindIdentity
		wantReads := 2
		if trigger {
			want += "," + KindChecked
			wantReads = 3
		}
		if strings.Join(got, ",") != want || reads != wantReads {
			t.Errorf("trigger=%v: events %v, %d reads; want [%s], %d reads", trigger, got, reads, want, wantReads)
		}
	}
}

// TestATriggerAfterShutdownIsRefused: nothing would answer it, so it says so
// rather than being accepted; the route turns that into 503. LoggedIn and
// Check are refused alike, and no check runs. The control is a Trigger
// before the group stops, which is accepted and answered.
func TestATriggerAfterShutdownIsRefused(t *testing.T) {
	h := newHarness(t)
	h.src.set(nil, nil, nil, nil)
	if err := h.w.Trigger(); err != nil {
		t.Fatalf("control: a Trigger before shutdown = %v", err)
	}
	h.settled(t, "control: the Trigger's check did not end")
	reads := h.calls()
	if late := h.group.Wait(nil); late != nil {
		t.Fatalf("stragglers %v", late)
	}
	if err := h.w.Trigger(); !errors.Is(err, life.ErrStopping) {
		t.Errorf("a Trigger after shutdown = %v; want life.ErrStopping", err)
	}
	if err := h.w.LoggedIn(waitCtx(t), h.clock.Now()); !errors.Is(err, life.ErrStopping) {
		t.Errorf("LoggedIn after shutdown = %v; want life.ErrStopping", err)
	}
	if _, err := h.w.Check(waitCtx(t)); !errors.Is(err, life.ErrStopping) {
		t.Errorf("Check after shutdown = %v; want life.ErrStopping", err)
	}
	if n := h.calls(); n != reads {
		t.Errorf("%d reads after shutdown; want none", n-reads)
	}
}

// TestAnAnswerThatCannotBeStoredIsAFailure is #85's finding 4: a requested
// check whose verdict the database would not take, or whose answer's view
// cannot be read, is answered auth.identity_check_failed — never "nothing
// has changed", and never nothing. The controls are the same checks with the
// table intact, answered auth.identity_checked.
func TestAnAnswerThatCannotBeStoredIsAFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		// at is the seam point at which the table goes; "" before the check.
		at string
	}{{"store", ""}, {"answer", "answering"}} {
		for _, broken := range []bool{false, true} {
			h := newHarness(t)
			n := h.storeOK(t)
			drop := func() {
				if _, err := h.db.Exec(`DROP TABLE claude_identity`); err != nil {
					t.Fatal(err)
				}
			}
			var once sync.Once
			h.w.observe = func(p string) {
				if broken && c.at != "" && p == c.at {
					once.Do(drop)
				}
			}
			if broken && c.at == "" {
				drop()
			}
			h.w.Trigger()
			h.settled(t, "the requested check did not end")
			got := strings.Join(h.since(t, n), ",")
			want := KindChecked
			if broken {
				want = KindCheckFailed
			}
			if got != want {
				t.Errorf("%s, broken=%v: events [%s]; want [%s]", c.name, broken, got, want)
			}
		}
	}

	// A verdict the database will not take is a failed check even when no
	// one asked for it — said as one, like every other failure. The control
	// is the same unrequested check with the table intact: silent.
	for _, broken := range []bool{false, true} {
		h := newHarness(t)
		n := h.storeOK(t)
		if broken {
			if _, err := h.db.Exec(`DROP TABLE claude_identity`); err != nil {
				t.Fatal(err)
			}
		}
		_, err := h.w.Check(waitCtx(t))
		got := strings.Join(h.since(t, n), ",")
		switch {
		case broken && (got != KindCheckFailed || err == nil):
			t.Errorf("unrequested, unstorable: events [%s], err %v; want [%s] and an error", got, err, KindCheckFailed)
		case !broken && (got != "" || err != nil):
			t.Errorf("control: unrequested, unchanged: events [%s], err %v; want nothing", got, err)
		}
	}
}

// TestAPressSharingARunWithALoginIsAnswered: a Check-now press and a
// handshake's LoggedIn, both made during a running check, share the one
// check after it, whose asks are both of them, in either order. Over an
// absent volume that stays absent the handshake's moment dates nothing and
// the verdict does not change, so nothing is announced and the press is owed
// auth.identity_checked: the run must see that any of its asks is owed an
// answer, not only the last one. The control is the login alone, which
// writes nothing.
func TestAPressSharingARunWithALoginIsAnswered(t *testing.T) {
	for _, c := range []struct {
		name  string
		order []string
	}{
		{"press then login", []string{"press", "login"}},
		{"login then press", []string{"login", "press"}},
		{"login alone", []string{"login"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.src.set(nil, nil, nil, nil) // absent, and staying so
			if _, err := h.w.Check(waitCtx(t)); err != nil {
				t.Fatal(err)
			}
			n := len(h.kinds(t))
			src := &gatedSource{fakeSource: h.src, in: make(chan struct{}), gate: make(chan struct{})}
			h.w.Source = src

			held := make(chan struct{})
			go func() { h.w.Check(waitCtx(t)); close(held) }() // an unrequested check
			receive(t, src.in, "the held check did not start its read")
			base := h.w.c.Asked()
			login := make(chan error, 1)
			pressed := false
			for i, ask := range c.order {
				switch ask {
				case "press":
					pressed = true
					if err := h.w.Trigger(); err != nil {
						t.Fatal(err)
					}
				case "login":
					go func() { login <- h.w.LoggedIn(waitCtx(t), h.clock.Now()) }()
				}
				want := base + life.Ticket(i+1)
				waitFor(t, func() bool { return h.w.c.Asked() == want })
			}
			close(src.gate)
			receive(t, held, "the held check did not end")
			if err := <-login; err != nil {
				t.Fatal(err)
			}
			h.settled(t, "the shared check did not end")

			if reads := h.calls(); reads != 3 {
				t.Errorf("%d reads; want the first, the held one and one shared check", reads)
			}
			got := strings.Join(h.since(t, n), ",")
			want := ""
			if pressed {
				want = KindChecked
			}
			if got != want {
				t.Errorf("events [%s]; want [%s]", got, want)
			}
		})
	}
}
