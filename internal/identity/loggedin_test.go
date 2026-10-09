package identity

import (
	"encoding/json"
	"testing"
	"time"
)

// TestLoggedInRecordsTheHandshakesMoment: a login the handshake reports is
// dated by the handshake, not by when the watch first saw it — including a
// re-login over a login that was already live, which "first live verdict"
// alone would keep dated by the old one. The change is announced. The
// controls: a check with no handshake behind it keeps the stored moment, and
// a handshake over a volume that still has no login records nothing and
// leaves no moment for a later login to take.
func TestLoggedInRecordsTheHandshakesMoment(t *testing.T) {
	ctx := waitCtx(t)
	h := newHarness(t)
	h.src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	first, err := h.w.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.LoggedInAt == nil || !first.LoggedInAt.Equal(h.clock.Now()) {
		t.Fatalf("first seen: %v", first.LoggedInAt)
	}

	// A re-login an hour later, which happened ten seconds before its check.
	h.clock.Advance(time.Hour)
	at := h.clock.Now().Add(-10 * time.Second)
	n := len(h.events(t))
	if err := h.w.LoggedIn(ctx, at); err != nil {
		t.Fatal(err)
	}
	v, _ := h.w.Read(ctx)
	if v.LoggedInAt == nil || !v.LoggedInAt.Equal(at) {
		t.Errorf("logged_in_at = %v; want the handshake's %v", v.LoggedInAt, at)
	}
	if evs := h.events(t)[n:]; len(evs) != 1 || evs[0].Kind != KindIdentity {
		t.Errorf("the new moment was not announced: %v", h.kinds(t)[n:])
	}

	// Control: an ordinary check later keeps it.
	h.clock.Advance(time.Hour)
	h.w.Check(ctx)
	if v, _ := h.w.Read(ctx); !v.LoggedInAt.Equal(at) {
		t.Errorf("a later check moved logged_in_at to %v", v.LoggedInAt)
	}

	// Control: a handshake over an absent volume records nothing.
	h.src.set(nil, fixture(t, "authstatus", "absent.json"), nil, nil)
	if err := h.w.LoggedIn(ctx, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	v, _ = h.w.Read(ctx)
	stateIs(t, v, Absent)
	h.clock.Advance(time.Minute)
	h.src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	later, _ := h.w.Check(ctx)
	if !later.LoggedInAt.Equal(h.clock.Now()) {
		t.Errorf("logged_in_at = %v; want first-seen %v, not a stale handshake's", later.LoggedInAt, h.clock.Now())
	}
}

// TestLoggedInWaitsOutARunningCheck: a check already running when the
// handshake reports read the volume before the login, so it must not take
// the handshake's moment; the check LoggedIn asked for, after it, does. Seen
// on the stream: the held check announces the login first-seen, and the
// handshake's check then re-dates it. Were the moment handed to the running
// check, it would announce the handshake's moment itself and the check after
// would find nothing to say.
func TestLoggedInWaitsOutARunningCheck(t *testing.T) {
	ctx := waitCtx(t)
	h := newHarness(t)
	h.src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	src := &gatedSource{fakeSource: h.src, in: make(chan struct{}), gate: make(chan struct{})}
	h.w.Source = src
	at := h.clock.Now().Add(-10 * time.Second)

	held := make(chan error, 1)
	go func() { _, err := h.w.Check(ctx); held <- err }()
	receive(t, src.in, "the running check did not start its read")
	done := make(chan error, 1)
	go func() { done <- h.w.LoggedIn(ctx, at) }()
	waitFor(t, func() bool { return h.w.c.Asked() == 2 })
	close(src.gate)
	receive(t, held, "the running check did not end")
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	evs := h.events(t)
	if len(evs) != 2 {
		t.Fatalf("events %v; want the running check's and the handshake's", h.kinds(t))
	}
	for i, want := range []time.Time{h.clock.Now(), at} {
		var data struct{ Identity View }
		if err := json.Unmarshal(evs[i].Data, &data); err != nil {
			t.Fatal(err)
		}
		if got := data.Identity.LoggedInAt; evs[i].Kind != KindIdentity || got == nil || !got.Equal(want) {
			t.Errorf("event %d: %s, logged_in_at %v; want %s with %v", i, evs[i].Kind, got, KindIdentity, want)
		}
	}
}
