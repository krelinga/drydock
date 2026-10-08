package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/workspace"
)

// runEvent is one of the detail view's events, as far as settled reads it.
type runEvent struct {
	ID   int64           `json:"id"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// runMarks reads, from a workspace's events, the id of the newest move into
// running and of the newest end (done or failed) of step 8. An annotation
// (a workspace.state event whose from is its state) is not a move.
func runMarks(evs []runEvent) (moved, ended int64) {
	for _, e := range evs {
		var d struct{ State, From, Step, Status string }
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		switch {
		case e.Kind == "workspace.state" && d.State == "running" && d.From != "running":
			moved = max(moved, e.ID)
		case e.Kind == "workspace.step" && d.Step == "session_server" && (d.Status == "done" || d.Status == "failed"):
			ended = max(ended, e.ID)
		}
	}
	return moved, ended
}

// settled reports whether a provisioning run has ended: failed, or running
// with *this run's* step 8 ended — an end written after the newest move into
// running.
//
// Running is entered when the probe passes, before step 8 hands the
// workspace to the supervisor (design §6: step 8 runs in running, and its
// failure leaves the workspace there), and until step 8 returns the run is in
// flight, so a stop or a rebuild is refused in_progress. The view's steps are
// the newest event per step across the workspace's whole history, so on a
// start or a rebuild a read between the move into running and step 8's
// started event shows running beside the *previous* run's step 8 done: the
// step's status alone cannot say whose end it is, and the event ids can.
//
// test/container's settled holds the same rule; change both together.
func settled(state string, evs []runEvent) bool {
	if state == "failed" {
		return true
	}
	moved, ended := runMarks(evs)
	return state == "running" && moved != 0 && ended > moved
}

// TestSettledRule pins the rule's cases, the two no hold at the
// StartSupervisor seam can show among them: step 8 not yet begun, and the
// previous run's step 8 end read beside this run's move into running.
func TestSettledRule(t *testing.T) {
	ev := func(id int64, kind, data string) runEvent { return runEvent{id, kind, json.RawMessage(data)} }
	move := func(id int64, from, to string) runEvent {
		return ev(id, "workspace.state", `{"state":"`+to+`","from":"`+from+`"}`)
	}
	s8 := func(id int64, status string) runEvent {
		return ev(id, "workspace.step", `{"step":"session_server","status":"`+status+`"}`)
	}
	verify := ev(9, "workspace.step", `{"step":"verify","status":"done"}`)
	first := []runEvent{move(1, "", "pending"), move(5, "cloning", "building"), verify, move(10, "building", "running")}
	ended := append(first[:len(first):len(first)], s8(11, "started"), s8(12, "done"))
	// A stop, then a start up to its move into running and no further.
	restarted := append(ended[:len(ended):len(ended)], move(13, "running", "stopped"), move(14, "stopped", "building"),
		ev(19, "workspace.step", `{"step":"verify","status":"done"}`), move(20, "building", "running"))
	for _, c := range []struct {
		name  string
		state string
		evs   []runEvent
		want  bool
	}{
		{"step 8 done", "running", ended, true},
		{"step 8 failed", "running", append(first[:len(first):len(first)], s8(11, "started"), s8(12, "failed")), true},
		{"step 8 ended, then an annotation", "running", append(ended[:len(ended):len(ended)], move(13, "running", "running")), true},
		{"failed", "failed", first, true},
		{"step 8 not yet begun", "running", first, false},
		{"step 8 started", "running", append(first[:len(first):len(first)], s8(11, "started")), false},
		{"a start's move, beside the last run's step 8 done", "running", restarted, false},
		{"that start's step 8 started", "running", append(restarted[:len(restarted):len(restarted)], s8(21, "started")), false},
		{"that start's step 8 done", "running", append(restarted[:len(restarted):len(restarted)], s8(21, "started"), s8(22, "done")), true},
		{"building", "building", first[:2], false},
		{"no events", "running", nil, false},
	} {
		if got := settled(c.state, c.evs); got != c.want {
			t.Errorf("%s: settled = %v, want %v", c.name, got, c.want)
		}
	}
}

// step8Hold holds every step 8 at the StartSupervisor seam until the test
// releases it, so a test acts inside the window between running and step 8's
// end on every run rather than only on the runs a loaded machine produces.
type step8Hold struct {
	t       *testing.T
	limit   time.Duration
	mu      sync.Mutex
	waiting []chan struct{} // held runs, oldest first
	arrived chan struct{}   // a run joined waiting
	all     chan struct{}   // closed by cleanup: let everything through
}

// holdStep8 installs the hold. Each release lets exactly one held run
// through — waiting for one to arrive, so a token is never banked for a run
// that has not come yet, and a run that left (its context ended) takes no
// release with it. A step 8 nobody releases within the limit is let through
// with a test error rather than held for ever: a StartSupervisor call no test
// is waiting for (boot's ResumeSupervisors, which holds the provisioner's
// lock) must fail the test loudly, not hang the binary.
func holdStep8(t *testing.T, srv *Server) *step8Hold {
	t.Helper()
	h := &step8Hold{t: t, limit: 10 * time.Second, arrived: make(chan struct{}, 1), all: make(chan struct{})}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(h.all) }) })
	startSupervisor := srv.Provisioner.StartSupervisor
	srv.Provisioner.StartSupervisor = func(ctx context.Context, w workspace.Workspace) error {
		if err := h.hold(ctx, w.ID); err != nil {
			return err
		}
		return startSupervisor(ctx, w)
	}
	return h
}

func (h *step8Hold) hold(ctx context.Context, id string) error {
	open := make(chan struct{})
	h.mu.Lock()
	h.waiting = append(h.waiting, open)
	h.mu.Unlock()
	select {
	case h.arrived <- struct{}{}:
	default:
	}
	timer := time.NewTimer(h.limit)
	defer timer.Stop()
	select {
	case <-open:
		return nil
	case <-h.all:
		return nil
	case <-ctx.Done():
		if h.leave(open) {
			return ctx.Err()
		}
		return nil // released as it was cancelled: the release was this run's
	case <-timer.C:
		if h.leave(open) {
			h.t.Errorf("workspace %s: step 8 held %s and never released: a StartSupervisor call no test waits for", id, h.limit)
		}
		return nil
	}
}

// leave takes a run out of waiting, and reports whether it was still there
// (not yet released).
func (h *step8Hold) leave(open chan struct{}) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.waiting {
		if c == open {
			h.waiting = append(h.waiting[:i], h.waiting[i+1:]...)
			return true
		}
	}
	return false
}

// release lets the oldest held run through, waiting for one to be held.
func (h *step8Hold) release() {
	h.t.Helper()
	deadline := time.NewTimer(h.limit)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		if len(h.waiting) > 0 {
			open := h.waiting[0]
			h.waiting = h.waiting[1:]
			h.mu.Unlock()
			close(open)
			return
		}
		h.mu.Unlock()
		select {
		case <-h.arrived:
		case <-deadline.C:
			h.t.Fatalf("no step 8 was held within %s", h.limit)
		}
	}
}

// staleControl is the positive control for settled's run check, taken from a
// real view while this run's step 8 is held: the events a read between the
// move into running and step 8's started event would have seen are those up
// to that move. When an earlier run's step 8 ended among them — a start or a
// rebuild — the step's status there says done, and settled must still say
// no. It reports whether there was such an earlier end.
func staleControl(t *testing.T, evs []runEvent) bool {
	t.Helper()
	moved, _ := runMarks(evs)
	var before []runEvent
	for _, e := range evs {
		if e.ID <= moved {
			before = append(before, e)
		}
	}
	if _, ended := runMarks(before); ended == 0 {
		return false
	}
	if settled("running", before) {
		t.Errorf("running, beside the last run's step 8 end, is called settled: %+v", before)
	}
	return true
}
