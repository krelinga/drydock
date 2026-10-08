package api

import "testing"

// TestStreamCarriesBroadcastsUnnumbered: a broadcast arrives as a named frame
// with no id line, so the browser's Last-Event-ID stays the last real event's;
// the events around it keep their ids and their order.
func TestStreamCarriesBroadcastsUnnumbered(t *testing.T) {
	env := newStreamEnv(t)
	s := env.connect(t, "")
	s.expectComment("connected")
	a := env.emit(t, "workspace.state")
	if err := env.log.Broadcast("resources", map[string]string{"at": "now"}); err != nil {
		t.Fatal(err)
	}
	b := env.emit(t, "workspace.step")
	s.expectEvent(a)
	f, ok := s.next()
	if !ok || f.name != "resources" || f.id != "" || f.data != `{"at":"now"}` {
		t.Fatalf("broadcast frame %+v", f)
	}
	s.expectEvent(b)
}
