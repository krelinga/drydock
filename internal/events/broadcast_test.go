package events

import (
	"context"
	"testing"
)

// TestBroadcastIsLiveOnly: a broadcast frame reaches a live subscriber, is
// never a row (so nothing replays it and the replay window is not spent on
// it), and a subscriber whose buffer is full skips it rather than being cut
// off — where an appended event, the control, does cut it off.
func TestBroadcastIsLiveOnly(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)
	sub := l.Subscribe()
	defer l.Cancel(sub)
	if err := l.Broadcast("resources", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	got := <-sub.C
	if got.Live != "resources" || got.ID != 0 || string(got.Data) != `{"n":1}` {
		t.Errorf("frame %+v", got)
	}
	if latest, _ := l.Latest(ctx); latest != 0 {
		t.Errorf("a broadcast wrote a row: latest id %d", latest)
	}
	if err := l.Broadcast("bad name", 1); err == nil {
		t.Error("a name with a space was accepted")
	}

	full := l.Subscribe()
	for i := 0; i < subBuffer; i++ {
		if err := l.Broadcast("resources", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Broadcast("resources", "one too many"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < subBuffer; i++ {
		<-full.C
	}
	select {
	case e, open := <-full.C:
		if !open {
			t.Fatal("a broadcast cut off a subscriber")
		}
		t.Errorf("the frame past the buffer was delivered: %+v", e)
	default:
	}
	// Control: the same backlog of appended events cuts it off.
	for i := 0; i <= subBuffer; i++ {
		if _, err := l.Emit(ctx, "", Info, "k", "m", nil); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for range full.C {
		n++
	}
	if n != subBuffer {
		t.Errorf("appended events: %d delivered before the cut-off", n)
	}
}
