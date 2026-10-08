package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/workspace"
)

// A session server whose stop fails because Docker could not be asked
// (internal/supervisor's stop_failed) ends every workspace action that stops
// it first in that action's own settling event, so no button waits on a
// supervisor.state that is not its own: a stop fails at its session_server
// sub-step and annotates the running row, with Stop again the button; a
// delete sticks there and annotates the deleting row, with Delete again; a
// rebuild says it in the log and carries on, since replacing the container
// ends the server. The control is each action asked again once the fault
// is gone, which finishes.
func TestASessionServerThatWillNotStopSettlesEachAction(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	var failing atomic.Bool
	var calls atomic.Int32
	e.p.StopSupervisor = func(context.Context, workspace.Workspace) error {
		calls.Add(1)
		if failing.Load() {
			return errors.New("docker exec: Cannot connect to the Docker daemon")
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

// A session server that outlived SIGKILL (container.ErrSessionSurvivedKill)
// never stops however often it is asked, and only its container going ends
// it — which is the very next sub-step of a stop (docker stop) and of a
// delete (docker rm --force). So both carry on, the session_server sub-step
// done with a note saying why, and both finish against the fault that never
// clears: a stop that failed there would offer Stop for good, and a delete
// stuck there has no way out at all. The control is the same fault without
// the sentinel — the Docker failure above — which still stops each at that
// sub-step.
func TestASessionServerThatSurvivedKillGoesWithItsContainer(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	survivor := fmt.Errorf("stop: %w", container.ErrSessionSurvivedKill)
	e.p.StopSupervisor = func(context.Context, workspace.Workspace) error { return survivor }
	v := e.running(t, alpha)
	const note = "The session server was still running after SIGKILL, so it ends with the container, in the next step."

	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if s := e.view(t, v.ID); s.State != workspace.Stopped || s.StateDetail != nil {
		t.Fatalf("after a stop with a server that outlived SIGKILL: %s (%q); actions %v",
			s.State, deref(s.StateDetail), e.actions(t, v.ID))
	}
	if got := e.actionDetail(t, v.ID, ActStop, SubSessionServer); got != note {
		t.Errorf("the session_server sub-step said %q, want %q", got, note)
	}
	for id, status := range e.containers(t, v.ID) {
		if status != "exited" {
			t.Errorf("container %s is %s: the stop did not reach docker stop", id, status)
		}
	}

	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("the delete did not finish: %v; actions %v", err, e.actions(t, v.ID))
	}
	if n := len(e.containers(t, v.ID)); n != 0 {
		t.Errorf("%d containers left after the delete", n)
	}
}
