package provision

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/reconcile"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestACreateCutOffInsideAStepIsClosedAtBoot is the crash test for a run: a
// create is cut off *inside* a step — the process gone with the step started,
// as a kill, an OOM or an unrecovered panic leaves it, with nothing to write
// its end — and then a new process boots on the same database and disk, in
// the order Serve runs it: reconciliation, the sockets, the session servers.
//
//   - Inside step 8, the workspace is already running (§6). It stays running,
//     adopted; step 8 is failed with reconciliation's sentence; and boot's
//     ResumeSupervisors then starts the session server, its events newer than
//     the close. A rebuild's new step 8 then ends done, newer still.
//   - Inside the container start, the workspace is building. Reconciliation
//     marks it failed (§6), and the step is failed with it; a start from
//     failed then runs through to running.
//
// Before the restart, the control: reconciliation in the process that is
// still running the step does not touch it — its step is started because it
// is running.
func TestACreateCutOffInsideAStepIsClosedAtBoot(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		step  workspace.Step
		state workspace.State // after the restart's reconciliation
	}{
		{workspace.StepSessionServer, workspace.Running},
		{workspace.StepUp, workspace.Failed},
	} {
		t.Run(string(c.step), func(t *testing.T) {
			e := lifecycleEnv(t)
			hang := filepath.Join(e.cli.dir, "hang-up")
			e.cli.up = `if [ -e '` + hang + `' ]; then sleep 30; exit 1; fi
` + registeringUp(e.cli.dir)
			e.wire(t)
			hold := make(chan struct{})
			if c.step == workspace.StepUp {
				os.WriteFile(hang, nil, 0o600)
			} else {
				e.p.StartSupervisor = func(ctx context.Context, w workspace.Workspace) error {
					select {
					case <-hold:
					case <-ctx.Done():
					}
					return nil
				}
			}
			// The first process is never told it crashed; at the end it is
			// shut down, so nothing it runs outlives the test.
			t.Cleanup(func() { close(hold); e.shutdown(time.Minute) })

			w, err := e.p.Create(ctx, alpha, "")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(20 * time.Second)
			for e.view(t, w.ID).Steps[c.step].Status != "started" {
				if time.Now().After(deadline) {
					t.Fatalf("the run never reached %s: %v", c.step, e.stepEvents(t, w.ID))
				}
				time.Sleep(10 * time.Millisecond)
			}

			reconciler := func(p *Provisioner) *reconcile.Reconciler {
				return &reconcile.Reconciler{Workspaces: e.p.Workspaces, Events: e.log,
					Containers: e.p.Containers, Exclusive: p.Unowned}
			}
			// Control: the live process owns the run.
			if _, err := reconciler(e.p).Run(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.view(t, w.ID).Steps[c.step]; got.Status != "started" {
				t.Fatalf("reconciliation in the process running the step closed it: %+v", got)
			}

			// The restart.
			os.Remove(hang)
			b2 := &stubBroker{}
			var supervisorEvent int64
			p2 := &Provisioner{Workspaces: e.p.Workspaces, Events: e.log, Broker: b2,
				Cloner: e.p.Cloner, Containers: e.p.Containers, Feature: e.p.Feature,
				FeatureOptions: e.p.FeatureOptions, ClaudeVolume: e.p.ClaudeVolume,
				ClaudeCodeVersion: e.p.ClaudeCodeVersion, Timeout: time.Minute, Logf: t.Logf,
				StartSupervisor: func(ctx context.Context, w workspace.Workspace) error {
					// The supervisor's first event, as Supervisor.Start writes it.
					ev, err := e.log.Emit(ctx, w.ID, events.Info, workspace.KindSupervisor, "Starting.",
						map[string]any{"state": "starting"})
					if supervisorEvent == 0 {
						supervisorEvent = ev.ID
					}
					return err
				}}
			runIn(t, p2)
			if _, err := reconciler(p2).Run(ctx); err != nil {
				t.Fatal(err)
			}
			if err := p2.ReopenSockets(ctx); err != nil {
				t.Fatal(err)
			}
			if err := p2.ResumeSupervisors(ctx); err != nil {
				t.Fatal(err)
			}

			v := e.view(t, w.ID)
			if v.State != c.state {
				t.Errorf("after the restart: %s (%s); want %s", v.State, deref(v.StateDetail), c.state)
			}
			want := map[workspace.Step]string{workspace.StepSessionServer: "The session server step failed: ",
				workspace.StepUp: "The container start step failed: "}[c.step] + reconcile.InterruptedStep
			if got := v.Steps[c.step]; got.Status != "failed" || got.Detail != want {
				t.Errorf("step %s after the restart: %+v; want failed, %q", c.step, got, want)
			}
			closed := e.lastStepEvent(t, w.ID, c.step)

			if c.state == workspace.Running {
				if supervisorEvent == 0 {
					t.Fatal("boot did not start the adopted workspace's session server")
				}
				if closed >= supervisorEvent {
					t.Errorf("step 8 was closed at event %d, after the restarted supervisor's %d", closed, supervisorEvent)
				}
				if err := p2.Rebuild(ctx, w.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if supervisorEvent != 0 {
					t.Error("boot started a session server for a failed workspace")
				}
				if err := p2.Start(ctx, w.ID); err != nil {
					t.Fatal(err)
				}
			}
			p2.idle()
			v = e.view(t, w.ID)
			if v.State != workspace.Running || v.Steps[workspace.StepSessionServer].Status != "done" {
				t.Errorf("the next run: %s, steps %+v", v.State, v.Steps)
			}
			if got := e.lastStepEvent(t, w.ID, workspace.StepSessionServer); got <= closed {
				t.Errorf("the next run's step 8 (event %d) is not newer than the close (%d)", got, closed)
			}
		})
	}
}

// lastStepEvent is the id of the newest workspace.step event for st.
func (e *env) lastStepEvent(t *testing.T, id string, st workspace.Step) int64 {
	t.Helper()
	var n int64
	err := e.p.Workspaces.DB.QueryRow(`SELECT coalesce(max(id), 0) FROM event
		WHERE workspace_id = ? AND kind = ? AND json_extract(data, '$.step') = ?`,
		id, workspace.KindStep, string(st)).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
