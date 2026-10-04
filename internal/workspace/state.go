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

import "fmt"

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
