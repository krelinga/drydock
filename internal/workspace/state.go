// Package workspace owns the workspace row's lifecycle: its states, the legal
// moves between them, the eight provisioning steps of design §6, and the
// events each of those writes.
//
// The state machine lives here and nowhere else. The frontend owns none
// (frontend §2.1) and renders whatever the last workspace.state event said;
// the container manager and reconciliation ask this package to move a
// workspace rather than writing the column themselves. So an illegal move — a
// deleted workspace coming back as running, say — is refused in one place.
package workspace

import (
	"fmt"
	"strings"
)

// State is workspace.state; the database's CHECK constraint holds the same set.
type State string

const (
	Pending  State = "pending"
	Cloning  State = "cloning"
	Building State = "building"
	Running  State = "running"
	Stopped  State = "stopped"
	Failed   State = "failed"
	Deleting State = "deleting"
)

// States is every state, in lifecycle order.
var States = []State{Pending, Cloning, Building, Running, Stopped, Failed, Deleting}

// transitions is the whole state machine. Anything not listed is refused.
//
// Three rules shape it:
//   - Deleting is a sink. Nothing comes back from it; the row is removed when
//     the delete finishes, and reconciliation resumes an interrupted one
//     (§6) — which is why it is a persisted state and not an in-memory flag.
//   - Every state but Deleting can fail, and every state can be deleted:
//     delete is the one action always offered.
//   - Nothing starts itself. Stopped and Failed leave only by an operator's
//     start or retry; no transition here is taken on a timer (§1: no idle
//     reaper, and reconciliation never auto-starts what was stopped).
var transitions = map[State][]State{
	Pending:  {Cloning, Failed, Deleting},
	Cloning:  {Building, Failed, Deleting},
	Building: {Running, Failed, Stopped, Deleting},
	// Running → Building is a rebuild; → Stopped is a stop, or a container
	// found exited or absent at boot.
	Running: {Building, Stopped, Failed, Deleting},
	Stopped: {Building, Deleting},
	// A failed clone is retried from the clone; anything later from the build.
	Failed:   {Cloning, Building, Deleting},
	Deleting: {},
}

// CanMove reports whether from → to is a legal transition.
func CanMove(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// ErrIllegalMove is a refused transition.
type ErrIllegalMove struct{ From, To State }

func (e ErrIllegalMove) Error() string {
	return fmt.Sprintf("workspace: cannot move from %s to %s", e.From, e.To)
}

// Occupying reports whether a workspace in s counts against the
// concurrent-container cap (§6 step 1). Building counts: it is the most
// expensive state there is. Failed does not: nothing is being built or
// served. (A failed up can still leave a container running, §6, which
// teardown has to account for — but that is cleanup, not capacity.)
func Occupying(s State) bool {
	switch s {
	case Pending, Cloning, Building, Running:
		return true
	}
	return false
}

// occupyingSQL is Occupying as an SQL list — 'pending','cloning',… — built
// from Occupying itself, so the count Create checks inside its transaction
// and the count a start or rebuild checks (Occupied) are not two spellings of
// one rule that can drift from each other or from the Go function.
var occupyingSQL = func() string {
	var in []string
	for _, s := range States {
		if Occupying(s) {
			in = append(in, "'"+string(s)+"'")
		}
	}
	return strings.Join(in, ",")
}()

// Capacity is the concurrent-container cap and how many workspaces count
// against it, as GET /api/workspaces reports them (frontend §4.5 #17). Cap is
// nil when there is none — a Store.Cap of 0, which only a test has: config
// refuses a cap below 1.
type Capacity struct {
	Cap      *int `json:"cap"`
	Occupied int  `json:"occupied"`
}

// CapacityOf counts views by Occupying — the rule create, start and rebuild
// enforce — against the store's cap. It counts the rows the caller already
// read, so the number and the list it is served beside are one snapshot (one
// SELECT): a client that counts the list by the same rule gets the same
// answer, which is what lets the UI keep the count live from the stream.
func (s *Store) CapacityOf(vs []View) Capacity {
	c := Capacity{}
	if s.Cap > 0 {
		n := s.Cap
		c.Cap = &n
	}
	for _, v := range vs {
		if Occupying(v.State) {
			c.Occupied++
		}
	}
	return c
}
