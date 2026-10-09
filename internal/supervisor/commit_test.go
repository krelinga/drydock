//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// traceCommits logs every committed state of the supervisor row, with the
// detail its last_error carries, in commit order.
func traceCommits(t *testing.T, r *rig) func() []string {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE commit_trace (n INTEGER PRIMARY KEY AUTOINCREMENT, state TEXT, detail TEXT)`,
		`CREATE TRIGGER commit_trace_supervisor AFTER UPDATE OF state ON supervisor BEGIN
			INSERT INTO commit_trace (state, detail) VALUES (NEW.state, NEW.last_error); END`,
	} {
		if _, err := r.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return func() []string {
		rows, err := r.db.Query(`SELECT state, coalesce(detail, '') FROM commit_trace ORDER BY n`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var st, d string
			rows.Scan(&st, &d)
			out = append(out, st+"/"+d)
		}
		return out
	}
}

// detached is a supervisor with no loop for the rig's workspace, as Park
// makes one.
func detached(t *testing.T, r *rig) *sup {
	t.Helper()
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	s, err := r.m.detachedLocked(context.Background(), wsID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Three writers of one workspace's supervisor state at once — two on the
// same sup, as the loop and a Stop are, and answer's, which makes a sup of
// its own each time — publish in the order their rows committed, and each
// event's from is the state the commit before it left. Commit order is
// measured by a trigger on the row; publish order is the stream's. Written
// as three steps (memory, row, Emit), the orders part and from names a state
// the row never held just before.
func TestStateWritesPublishInCommitOrder(t *testing.T) {
	r := newRig(t)
	s := detached(t, r)
	ctx := context.Background()
	s.set(ctx, Degraded, ReasonStopFailed, "first", 0)
	committed := traceCommits(t, r)

	const each = 40
	total := 3 * each
	sub := r.log.Subscribe()
	defer r.log.Cancel(sub)
	got := make(chan []events.Event, 1)
	go func() {
		var out []events.Event
		for ev := range sub.C {
			if ev.Kind == workspace.KindSupervisor {
				out = append(out, ev)
			}
			if len(out) == total {
				break
			}
		}
		got <- out
	}()
	var wg sync.WaitGroup
	for w := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				detail := fmt.Sprintf("w%d-%d", w, i)
				// States whose detail the row keeps (last_error), so the
				// trace names each commit.
				st := []State{Degraded, AwaitingLogin, WaitingRegistration}[w]
				if w == 2 {
					r.m.answer(ctx, wsID, st, ReasonWaitRegistration, detail)
				} else {
					s.announce(ctx, st, ReasonStopFailed, detail)
				}
			}
		}()
	}
	wg.Wait()
	var published []events.Event
	select {
	case published = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the writes' events were not all published")
	}

	trace := committed()
	if len(trace) != total || len(published) != total {
		t.Fatalf("%d commits and %d events, want %d of each (the positive control)", len(trace), len(published), total)
	}
	prev := string(Degraded)
	for i, ev := range published {
		var d workspace.SupervisorData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if said := d.State + "/" + d.Detail; said != trace[i] {
			t.Fatalf("event %d says %s, but commit %d wrote %s: publish order is not commit order", i, said, i, trace[i])
		}
		if d.From != prev {
			t.Fatalf("event %d (%s) is from %q, but the commit before it left %q", i, d.Detail, d.From, prev)
		}
		prev = d.State
	}
	if st, _ := r.row(); string(st) != prev {
		t.Errorf("row %s, last event %s", st, prev)
	}
}

// A state write that does not commit leaves memory where the row is: the
// next write of that state is then not taken for a repeat, and is written.
// This is #108's review's nit: a launch's starting whose row write failed
// left memory at starting, so the loop's own starting was deduplicated to a
// pid and no starting event was ever written.
func TestAFailedStateWriteLeavesMemoryAtTheRow(t *testing.T) {
	r := newRig(t)
	s := detached(t, r)
	ctx := context.Background()
	s.set(ctx, Degraded, ReasonStaleBrokerMount, "parked", 0)
	before := len(r.sups())
	if _, err := r.db.Exec(`CREATE TRIGGER refuse_starting BEFORE UPDATE OF state ON supervisor
		WHEN NEW.state = 'starting' BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`); err != nil {
		t.Fatal(err)
	}
	// launchLocked's starting, refused.
	s.set(ctx, Starting, ReasonLaunching, launchingSentence, 0)
	if st := s.current(); st != Degraded {
		t.Errorf("memory %s after a write that did not commit, want degraded (the row's)", st)
	}
	if st, _ := r.row(); st != Degraded {
		t.Errorf("row %s, want degraded", st)
	}
	if n := len(r.sups()); n != before {
		t.Errorf("%d events for a write that did not commit", n-before)
	}
	if _, err := r.db.Exec(`DROP TRIGGER refuse_starting`); err != nil {
		t.Fatal(err)
	}
	// The loop's own starting, with the process it launched: written in
	// full, not taken for a repeat of the one that failed.
	s.set(ctx, Starting, ReasonLaunching, launchingSentence, 4242)
	l := r.last()
	if l.State != string(Starting) || l.From != string(Degraded) || len(r.sups()) != before+1 {
		t.Errorf("last event %+v (%d new), want one starting from degraded", l, len(r.sups())-before)
	}
	var pid int
	if err := r.db.QueryRow(`SELECT coalesce(pid, 0) FROM supervisor WHERE workspace_id = ?`, wsID).Scan(&pid); err != nil || pid != 4242 {
		t.Errorf("pid %d (%v), want 4242", pid, err)
	}
	if st := s.current(); st != Starting {
		t.Errorf("memory %s, want starting", st)
	}
}
