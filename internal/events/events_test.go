package events

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

func newLog(t *testing.T) (*Log, *sys.FakeClock) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	return New(db.DB, clock), clock
}

func TestAppendAndReplay(t *testing.T) {
	ctx := context.Background()
	l, clock := newLog(t)

	a, err := l.Emit(ctx, "ws1", Info, "workspace.state", "Cloning.", map[string]string{"state": "cloning"})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	b, err := l.Emit(ctx, "", Warn, "auth.identity", "Login expires in 2 days.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID <= 0 || b.ID <= a.ID {
		t.Fatalf("ids %d then %d; want increasing", a.ID, b.ID)
	}

	got, err := l.Since(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("Since(0) = %+v", got)
	}
	if got[0].WorkspaceID != "ws1" || got[0].Level != Info || string(got[0].Data) != `{"state":"cloning"}` {
		t.Errorf("first event did not round-trip: %+v", got[0])
	}
	if got[1].WorkspaceID != "" || got[1].Data != nil || !got[1].At.Equal(clock.Now()) {
		t.Errorf("second event did not round-trip: %+v", got[1])
	}
	if after, _ := l.Since(ctx, a.ID); len(after) != 1 || after[0].ID != b.ID {
		t.Errorf("Since(first) = %+v, want only the second", after)
	}
	if none, err := l.Since(ctx, b.ID); err != nil || len(none) != 0 {
		t.Errorf("Since(latest) = %v, %v; want nothing, no error", none, err)
	}
}

// The reducer applies Data, so it must be an object it can read fields from.
func TestAppendRefusesBadInput(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)
	for name, e := range map[string]Event{
		"no kind":       {Message: "x"},
		"string data":   {Kind: "k", Data: json.RawMessage(`"cloning"`)},
		"array data":    {Kind: "k", Data: json.RawMessage(`[1]`)},
		"null data":     {Kind: "k", Data: json.RawMessage(`null`)},
		"invalid data":  {Kind: "k", Data: json.RawMessage(`{`)},
		"unknown level": {Kind: "k", Level: "debug"},
	} {
		if _, err := l.Append(ctx, e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Control: the same call with good input is accepted.
	if _, err := l.Append(ctx, Event{Kind: "k", Data: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Errorf("control: a valid event was refused: %v", err)
	}
}

// A client exactly a window behind is replayed; one event further is told to
// resync. Both sides of the boundary, so neither is vacuous.
func TestReplayWindow(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)
	l.ReplayWindow = 5
	for i := 0; i < 6; i++ {
		if _, err := l.Emit(ctx, "", Info, "k", "m", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := l.Since(ctx, 1); err != nil || len(got) != 5 {
		t.Errorf("five behind with a window of five: %d events, %v; want all five replayed", len(got), err)
	}
	if _, err := l.Since(ctx, 0); !errors.Is(err, ErrResync) {
		t.Errorf("six behind with a window of five: %v; want ErrResync", err)
	}
	// An id this log never issued: a restored or replaced database. Replaying
	// "everything after 99" would silently replay nothing.
	if _, err := l.Since(ctx, 99); !errors.Is(err, ErrResync) {
		t.Errorf("an id ahead of the log: %v; want ErrResync", err)
	}
}

func TestSubscribersSeeEventsInOrder(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)
	sub := l.Subscribe()
	defer l.Cancel(sub)
	var want []int64
	for i := 0; i < 20; i++ {
		e, err := l.Emit(ctx, "", Info, "k", "m", nil)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, e.ID)
	}
	for _, id := range want {
		if e := <-sub.C; e.ID != id {
			t.Fatalf("got event %d, want %d", e.ID, id)
		}
	}
}

// A subscriber that stops reading is cut off rather than allowed to block the
// writer — and only that subscriber: one that keeps up still gets everything.
func TestSlowSubscriberIsCutOff(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)
	stalled := l.Subscribe()
	live := l.Subscribe()
	defer l.Cancel(live)

	done := make(chan int)
	go func() {
		n := 0
		for range live.C {
			n++
			if n == subBuffer+10 {
				break
			}
		}
		done <- n
	}()
	for i := 0; i < subBuffer+10; i++ {
		if _, err := l.Emit(ctx, "", Info, "k", "m", nil); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case n := <-done:
		if n != subBuffer+10 {
			t.Errorf("the live subscriber got %d events", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control: the live subscriber did not receive every event")
	}

	n := 0
	for open := true; open; {
		select {
		case _, open = <-stalled.C:
			if open {
				n++
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the stalled subscriber was never cut off (%d buffered)", n)
		}
	}
	if n != subBuffer {
		t.Errorf("the stalled subscriber had %d buffered before being cut off, want %d", n, subBuffer)
	}
}

func TestCloseEndsSubscriptions(t *testing.T) {
	l, _ := newLog(t)
	before := l.Subscribe()
	l.Close()
	if _, ok := <-before.C; ok {
		t.Error("a subscription survived Close")
	}
	after := l.Subscribe()
	if _, ok := <-after.C; ok {
		t.Error("a subscription made after Close is open")
	}
	l.Cancel(before) // and cancelling one already closed is harmless
}

// Every way a subscription ends — Cancel, Close, falling behind, and being
// made after Close — closes C exactly once, in any order and however many
// times each happens. A second close panics, and shutdown is where they meet:
// the supervisor's Watch, started beside serving, can subscribe after the
// server has closed the log and then Cancel on its way out. That crashed the
// v0.4.0 release's test run, and would crash the server on a fast shutdown.
//
// The control is the same subscription made on an open log: it delivers an
// event before anything ends it, so "closed" is not a subscription that was
// never live.
func TestASubscriptionEndsOnceHoweverItEnds(t *testing.T) {
	ctx := context.Background()
	ends := map[string]func(*testing.T, *Log, *Sub){
		"cancel": func(_ *testing.T, l *Log, s *Sub) { l.Cancel(s) },
		"close":  func(_ *testing.T, l *Log, _ *Sub) { l.Close() },
		"lag": func(t *testing.T, l *Log, _ *Sub) {
			for i := 0; i <= subBuffer; i++ {
				if _, err := l.Emit(ctx, "", Info, "k", "m", nil); err != nil {
					t.Fatal(err)
				}
			}
		},
	}
	names := []string{"cancel", "close", "lag"}
	for _, afterClose := range []bool{false, true} {
		for _, a := range names {
			for _, b := range names {
				name := a + "," + b
				if afterClose {
					name = "subscribed-after-close," + name
				}
				t.Run(name, func(t *testing.T) {
					l, _ := newLog(t)
					if afterClose {
						l.Close()
					}
					s := l.Subscribe()
					if !afterClose {
						e, err := l.Emit(ctx, "", Info, "k", "m", nil)
						if err != nil {
							t.Fatal(err)
						}
						if got, ok := <-s.C; !ok || got.ID != e.ID {
							t.Fatalf("control: an open subscription delivered %+v (open %v), want event %d", got, ok, e.ID)
						}
					}
					ends[a](t, l, s)
					ends[b](t, l, s)
					l.Cancel(s) // what every subscriber defers
					deadline := time.After(5 * time.Second)
					for open := true; open; {
						select {
						case _, open = <-s.C:
						case <-deadline:
							t.Fatal("the subscription is still open")
						}
					}
				})
			}
		}
	}
}
