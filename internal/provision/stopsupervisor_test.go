package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// stuck there has no way out at all. A container that stays paused
// (container.ErrSessionContainerPaused, and Drydock's unpause found nothing
// it could unpause — TestAPausedContainerIsUnpausedForTheStop is the one it
// can) is the same: its server cannot be signalled, and docker stop and
// docker rm --force end a paused container (measured). The control is the same fault without
// the sentinel — the Docker failure above — which still stops each at that
// sub-step.
func TestASessionServerThatSurvivedKillGoesWithItsContainer(t *testing.T) {
	for _, c := range []struct {
		name     string
		sentinel error
		note     string
	}{
		{"survived SIGKILL", container.ErrSessionSurvivedKill,
			"The session server was still running after SIGKILL, so it ends with the container, in the next step."},
		{"container paused", container.ErrSessionContainerPaused, PausedNote},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := lifecycleEnv(t)
			fault := fmt.Errorf("stop: %w", c.sentinel)
			e.p.StopSupervisor = func(context.Context, workspace.Workspace) error { return fault }
			v := e.running(t, alpha)

			if err := e.p.Stop(ctx, v.ID); err != nil {
				t.Fatal(err)
			}
			e.p.wg.Wait()
			if s := e.view(t, v.ID); s.State != workspace.Stopped || s.StateDetail != nil {
				t.Fatalf("after a stop with %s: %s (%q); actions %v",
					c.name, s.State, deref(s.StateDetail), e.actions(t, v.ID))
			}
			if got := e.actionDetail(t, v.ID, ActStop, SubSessionServer); got != c.note {
				t.Errorf("the session_server sub-step said %q, want %q", got, c.note)
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
		})
	}
}

// setStatus sets every container of the workspace to status in the fake
// docker's world.
func (e *env) setStatus(t *testing.T, ws, status string) {
	t.Helper()
	p := filepath.Join(e.cli.dir, "containers")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i, l := range lines {
		if f := strings.Fields(l); len(f) >= 3 && f[1] == ws {
			f[2] = status
			lines[i] = strings.Join(f, " ")
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A workspace stop, rebuild or delete of a paused container unpauses it
// before stopping the session server, so the server gets its SIGTERM and
// deregisters. Ended frozen with its container instead — docker stop's
// SIGKILL after the grace period, or docker rm --force — it never
// deregisters, and the next start waits minutes on the folder's registration
// (Spike 02). Each ends the container anyway, so the unpause gives nothing
// away. "between" is a pause landing after the unpause, before the server's
// stop reaches the container: the stop says paused, and the container is
// unpaused and the stop asked once more. The control is a container that is
// not paused: no unpause, and the stop as always.
func TestAPausedContainerIsUnpausedForTheStop(t *testing.T) {
	for _, action := range []string{ActStop, ActDelete, "rebuild"} {
		for _, pause := range []string{"none", "before", "between"} {
			t.Run(action+"/"+pause, func(t *testing.T) {
				ctx := context.Background()
				e := lifecycleEnv(t)
				v := e.running(t, alpha)
				var calls, stopped int
				e.p.StopSupervisor = func(_ context.Context, w workspace.Workspace) error {
					calls++
					for _, st := range e.containers(t, w.ID) {
						if st == "paused" {
							return fmt.Errorf("stop: %w", container.ErrSessionContainerPaused)
						}
					}
					if pause == "between" && calls == 1 {
						e.setStatus(t, w.ID, "paused")
						return fmt.Errorf("stop: %w", container.ErrSessionContainerPaused)
					}
					stopped++
					return nil
				}
				if pause == "before" {
					e.setStatus(t, v.ID, "paused")
				}
				switch action {
				case ActStop:
					if err := e.p.Stop(ctx, v.ID); err != nil {
						t.Fatal(err)
					}
				case ActDelete:
					if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
						t.Fatal(err)
					}
				case "rebuild":
					if err := e.p.Rebuild(ctx, v.ID); err != nil {
						t.Fatal(err)
					}
				}
				e.p.wg.Wait()

				if stopped != 1 {
					t.Errorf("the session server was stopped in a running container %d times, want once (%d asks)", stopped, calls)
				}
				unpauses := 0
				for _, a := range e.cli.callsTo(t, "docker") {
					if len(a) > 1 && a[1] == "unpause" {
						unpauses++
					}
				}
				if want := map[string]int{"none": 0, "before": 1, "between": 1}[pause]; unpauses != want {
					t.Errorf("%d unpauses, want %d", unpauses, want)
				}
				if action != "rebuild" {
					want := "Stopped the session server, SIGTERM first, so its environment is kept for the next start."
					if pause != "none" {
						want = "The workspace's container was paused, so Drydock unpaused it to stop the session server, SIGTERM first, so its environment is kept for the next start."
					}
					if got := e.actionDetail(t, v.ID, action, SubSessionServer); got != want {
						t.Errorf("the session_server sub-step said %q, want %q", got, want)
					}
				}
				switch action {
				case ActStop:
					if s := e.view(t, v.ID); s.State != workspace.Stopped {
						t.Errorf("after the stop: %s; actions %v", s.State, e.actions(t, v.ID))
					}
				case ActDelete:
					if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
						t.Errorf("the delete did not finish: %v", err)
					}
				case "rebuild":
					if s := e.view(t, v.ID); s.State != workspace.Running {
						t.Errorf("after the rebuild: %s", s.State)
					}
				}
			})
		}
	}
}
