package identity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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
	if _, err := h.w.Check(context.Background()); err != nil {
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
	ctx := context.Background()
	n := h.storeOK(t)

	// Control: unrequested and unchanged is silent.
	h.clock.Advance(time.Minute)
	if _, err := h.w.Check(ctx); err != nil {
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
	drained(t, &h.w.triggers, "the requested check did not end")
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
	drained(t, &h.w.triggers, "the requested check did not end")
	if got := h.since(t, n); strings.Join(got, ",") != KindIdentity {
		t.Errorf("a requested check that changed the verdict wrote %v; want [%s]", got, KindIdentity)
	}
	// And a requested check that failed: its auth.identity_check_failed.
	n = len(h.kinds(t))
	h.src.set(nil, nil, &ReadError{Problem: ProblemDocker, Detail: "x"}, nil)
	h.w.Trigger()
	drained(t, &h.w.triggers, "the requested check did not end")
	if got := h.since(t, n); strings.Join(got, ",") != KindCheckFailed {
		t.Errorf("a requested check that failed wrote %v; want [%s]", got, KindCheckFailed)
	}
}

// TestAJoinedTriggerIsAnsweredByTheCheckItJoined: a press while the
// interval's check is running joins it rather than starting another, and is
// answered when that check ends — even though the check, unrequested and
// unchanged, would have said nothing. The "joined" seam proves the Trigger is
// waiting on the held check before it is let go. The control is the same
// held check with no Trigger, which ends silent.
func TestAJoinedTriggerIsAnsweredByTheCheckItJoined(t *testing.T) {
	for _, trigger := range []bool{false, true} {
		h := newHarness(t)
		n := h.storeOK(t)
		src := &gatedSource{fakeSource: h.src, in: make(chan struct{}), gate: make(chan struct{})}
		h.w.Source = src
		joined := make(chan struct{}, 1)
		h.w.observe = func(p string) {
			if p == "joined" {
				joined <- struct{}{}
			}
		}

		done := make(chan struct{})
		go func() { h.w.Check(context.Background()); close(done) }() // the interval's check
		receive(t, src.in, "the held check did not start its read")
		if trigger {
			h.w.Trigger()
			receive(t, joined, "the Trigger did not join the held check")
		}
		close(src.gate)
		receive(t, done, "the held check did not end")
		drained(t, &h.w.triggers, "the joined Trigger did not end")

		got := h.since(t, n)
		switch {
		case trigger && strings.Join(got, ",") != KindChecked:
			t.Errorf("a joined Trigger: events %v; want [%s]", got, KindChecked)
		case !trigger && len(got) != 0:
			t.Errorf("control: the held check with no Trigger wrote %v; want nothing", got)
		}
		if reads := h.calls(); reads != 2 { // storeOK's, and the held one
			t.Errorf("trigger=%v: %d reads; want the held check's alone after storeOK's", trigger, reads)
		}
	}
}

// TestATriggerAfterTheAnnouncementIsStillAnswered: a Trigger that arrives
// after a running check wrote its auth.identity, but before the check ended,
// joins a check whose answer is already written — and a client that had seen
// that event took its "from" after it. end answers it with its own event. The
// seam's "announced" makes the Trigger synchronously, right after the event,
// while the check still holds running, so it provably lands inside the check.
// (Its goroutine may then join or, once the check has ended, run a spare
// unrequested check, which is silent: either way the answer is end's.) The
// control is the same check with no Trigger, which writes no
// auth.identity_checked.
func TestATriggerAfterTheAnnouncementIsStillAnswered(t *testing.T) {
	for _, trigger := range []bool{false, true} {
		h := newHarness(t)
		h.src.set(nil, nil, nil, nil) // absent: the first check announces
		var once sync.Once
		h.w.observe = func(p string) {
			if p == "announced" && trigger {
				once.Do(func() {
					h.w.mu.Lock()
					inside := h.w.running
					h.w.mu.Unlock()
					if !inside {
						t.Error("the seam's announced came after the check let go of running")
					}
					h.w.Trigger()
				})
			}
		}
		if _, err := h.w.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		drained(t, &h.w.triggers, "the late Trigger did not end")
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

// TestAJoinedTriggerOutlivesTheCallersContext is round 1's finding 1. The
// handshake's LoggedIn runs a check under its own five-minute bound, shorter
// than a first build; a press that joins that check must not go unanswered
// when that bound — not the watch — ends it. The check it joined checked
// nothing, so it answers nothing; the press gets a fresh check under the
// watch's own context, which answers. The control is the same cut-off check
// with no Trigger: no event and no second check.
func TestAJoinedTriggerOutlivesTheCallersContext(t *testing.T) {
	for _, trigger := range []bool{false, true} {
		h := newHarness(t)
		n := h.storeOK(t)
		src := &gatedSource{fakeSource: h.src, in: make(chan struct{}), gate: make(chan struct{})}
		h.w.Source = src
		joined := make(chan struct{}, 1)
		h.w.observe = func(p string) {
			if p == "joined" {
				joined <- struct{}{}
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := h.w.Check(ctx); done <- err }() // the handshake's check
		receive(t, src.in, "the handshake's check did not start its read")
		if trigger {
			h.w.Trigger()
			receive(t, joined, "the Trigger did not join the handshake's check")
		}
		cancel()
		close(src.gate)
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the cut-off check returned %v; want its context's error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the cut-off check did not end")
		}
		drained(t, &h.w.triggers, "the press's check did not end")

		got := h.since(t, n)
		reads := h.calls()
		switch {
		case trigger && (strings.Join(got, ",") != KindChecked || reads != 3):
			t.Errorf("a press joined to a cut-off check: events %v, %d reads; want [%s] from a fresh check (3 reads)", got, reads, KindChecked)
		case !trigger && (len(got) != 0 || reads != 2):
			t.Errorf("control: events %v, %d reads; want nothing, and no second check", got, reads)
		}
	}
}

// TestATriggerAfterShutdownIsRefused: nothing would answer it, so it says so
// rather than being accepted; the route turns that into 503. The control is
// a Trigger before Shutdown, which is accepted.
func TestATriggerAfterShutdownIsRefused(t *testing.T) {
	h := newHarness(t)
	h.src.set(nil, nil, nil, nil)
	if err := h.w.Trigger(); err != nil {
		t.Fatalf("control: a Trigger before Shutdown = %v", err)
	}
	h.w.Shutdown(5 * time.Second)
	if err := h.w.Trigger(); !errors.Is(err, ErrShutdown) {
		t.Fatalf("a Trigger after Shutdown = %v; want ErrShutdown", err)
	}
}

// TestAnAnswerThatCannotBeStoredIsAFailure is round 1's finding 4: a requested
// check whose verdict the database would not take, or whose answer's view
// cannot be read, is answered auth.identity_check_failed — never end's
// "nothing has changed", and never nothing. The controls are the same checks
// with the table intact, answered auth.identity_checked.
func TestAnAnswerThatCannotBeStoredIsAFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		// at is the seam point at which the table goes; "" before the check.
		at string
	}{{"store", ""}, {"answer", "ending"}} {
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
			drained(t, &h.w.triggers, "the requested check did not end")
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
	// one asked for it — said as one, like every other failure, rather than
	// left for end's answer to cover. The control is the same unrequested
	// check with the table intact: silent.
	for _, broken := range []bool{false, true} {
		h := newHarness(t)
		n := h.storeOK(t)
		if broken {
			if _, err := h.db.Exec(`DROP TABLE claude_identity`); err != nil {
				t.Fatal(err)
			}
		}
		_, err := h.w.Check(context.Background())
		got := strings.Join(h.since(t, n), ",")
		switch {
		case broken && (got != KindCheckFailed || err == nil):
			t.Errorf("unrequested, unstorable: events [%s], err %v; want [%s] and an error", got, err, KindCheckFailed)
		case !broken && (got != "" || err != nil):
			t.Errorf("control: unrequested, unchanged: events [%s], err %v; want nothing", got, err)
		}
	}
}
