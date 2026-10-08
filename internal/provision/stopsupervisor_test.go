package provision

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/krelinga/drydock/internal/workspace"
)

// A session server that will not stop (internal/supervisor's Stop failing)
// ends every workspace action that stops it first in that action's own
// settling event, so no button waits on a supervisor.state that is not its
// own: a stop fails at its session_server sub-step and annotates the running
// row; a delete sticks there and annotates the deleting row; a rebuild says
// it in the log and carries on, since replacing the container ends the
// server. The control for each is the same action with the stop working —
// TestStopStopsTheContainerAndClosesTheSocket and the delete and rebuild
// tests beside it — plus, here, a stop asked again once it does.
func TestASessionServerThatWillNotStopSettlesEachAction(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	var failing atomic.Bool
	var calls atomic.Int32
	e.p.StopSupervisor = func(context.Context, workspace.Workspace) error {
		calls.Add(1)
		if failing.Load() {
			return errors.New("the session server did not exit after SIGKILL")
		}
		return nil
	}
	v := e.running(t, alpha)
	failing.Store(true)

	// Stop.
	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	want := StopFailedDetail("Drydock could not stop the session server.")
	got := e.view(t, v.ID)
	if got.State != workspace.Running || deref(got.StateDetail) != want {
		t.Errorf("after a failed stop: %s (%q), want running (%q)", got.State, deref(got.StateDetail), want)
	}
	if la := got.LastAction; la == nil || la.Action != ActStop || la.Step != SubSessionServer || la.Status != "failed" {
		t.Errorf("last_action %+v", la)
	}
	if s := e.states(t, v.ID); s[len(s)-1] != "running|"+want {
		t.Errorf("the stop was not settled by its annotation: %v", s)
	}

	// Rebuild: the server's stop fails, the rebuild does not.
	before := calls.Load()
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if calls.Load() == before {
		t.Error("the rebuild did not try to stop the session server first")
	}
	if r := e.view(t, v.ID); r.State != workspace.Running || r.StateDetail != nil {
		t.Errorf("after a rebuild whose session server would not stop: %s (%q)", r.State, deref(r.StateDetail))
	}

	// Delete: stuck at the session server, annotated, and resumable.
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	d := e.view(t, v.ID)
	if d.State != workspace.Deleting || !strings.Contains(deref(d.StateDetail), "Drydock could not stop the session server.") {
		t.Errorf("after a failed delete: %s (%q)", d.State, deref(d.StateDetail))
	}
	if la := d.LastAction; la == nil || la.Action != ActDelete || la.Step != SubSessionServer || la.Status != "failed" {
		t.Errorf("last_action %+v", la)
	}

	// Control: the server stops, and the delete asked again finishes.
	failing.Store(false)
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("control: the resumed delete left the workspace: %v", err)
	}
}
