package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const (
	wsA = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	wsB = "01JBBBBBBBBBBBBBBBBBBBBBBB"
)

func row(id string, s workspace.State, container string) workspace.Workspace {
	return workspace.Workspace{ID: id, RepositoryID: 1, Branch: "main", State: s, ContainerID: container}
}

func ctr(ws, id string, running bool) container.Found {
	status := "exited"
	if running {
		status = "running"
	}
	return container.Found{ContainerID: id, WorkspaceID: ws, RepositoryID: 9, Repo: "krelinga/orphan",
		Branch: "dev", Running: running, Status: status}
}

func kinds(plan []Action) string {
	var s []string
	for _, a := range plan {
		s = append(s, fmt.Sprintf("%s:%s:%s", a.Kind, a.WorkspaceID[len(a.WorkspaceID)-1:], a.ContainerID))
	}
	return strings.Join(s, " ")
}

// The table in design §6, row by row, plus the cases it leaves implicit.
func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  []workspace.Workspace
		found []container.Found
		want  string
	}{
		{"running, and Docker agrees: adopt",
			[]workspace.Workspace{row(wsA, workspace.Running, "c1")}, []container.Found{ctr(wsA, "c1", true)},
			"adopt:A:c1"},
		{"running, but the container exited: stopped, not restarted",
			[]workspace.Workspace{row(wsA, workspace.Running, "c1")}, []container.Found{ctr(wsA, "c1", false)},
			"mark_stopped:A:c1"},
		{"running, but the container is gone: stopped, id cleared",
			[]workspace.Workspace{row(wsA, workspace.Running, "c1")}, nil,
			"mark_stopped:A:"},
		{"no row, a running container: adopt the orphan",
			nil, []container.Found{ctr(wsA, "c1", true)},
			"adopt_orphan:A:c1"},
		{"deleting, whatever Docker says: resume the delete",
			[]workspace.Workspace{row(wsA, workspace.Deleting, "c1"), row(wsB, workspace.Deleting, "")},
			[]container.Found{ctr(wsA, "c1", true)},
			"resume_delete:A:c1 resume_delete:B:"},

		{"mid-build at shutdown: failed, never resumed",
			[]workspace.Workspace{row(wsA, workspace.Building, ""), row(wsB, workspace.Cloning, "")},
			[]container.Found{ctr(wsA, "c1", true)},
			"mark_interrupted:A:c1 mark_interrupted:B:"},
		{"stopped with its container: nothing to do",
			[]workspace.Workspace{row(wsA, workspace.Stopped, "c1")}, []container.Found{ctr(wsA, "c1", false)},
			""},
		{"stopped, and it was rebuilt under a new id: sync the cache only",
			[]workspace.Workspace{row(wsA, workspace.Stopped, "old")}, []container.Found{ctr(wsA, "new", false)},
			"sync_container:A:new"},
		{"failed, container gone: clear the cache",
			[]workspace.Workspace{row(wsA, workspace.Failed, "c1")}, nil,
			"sync_container:A:"},
		{"two containers for one workspace: the running one, and the other left alone",
			[]workspace.Workspace{row(wsA, workspace.Running, "")},
			[]container.Found{ctr(wsA, "z-exited", false), ctr(wsA, "a-running", true)},
			"adopt:A:a-running extra_container:A:z-exited"},
		{"an orphan with incomplete labels is left, never removed",
			nil, []container.Found{{ContainerID: "c1", WorkspaceID: wsA, Running: true}},
			"leave_orphan:A:c1"},
		{"an orphan whose id this instance could not have issued is left",
			nil, []container.Found{ctr("not-a-ulid-but-ends-in-Z", "c1", true)},
			"leave_orphan:Z:c1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kinds(Plan(tc.rows, tc.found)); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// ---- Run, against a real store ------------------------------------------

type fakeLister struct {
	found []container.Found
	err   error
}

func (f fakeLister) List(context.Context) ([]container.Found, error) { return f.found, f.err }

type env struct {
	ws    *workspace.Store
	log   *events.Log
	repos *int64
}

func newEnv(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for id := 1; id <= 10; id++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (?, 1, ?, 'main')`,
			id, fmt.Sprintf("krelinga/r%d", id)); err != nil {
			t.Fatal(err)
		}
	}
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	log := events.New(db.DB, clock)
	return env{repos: new(int64), ws: &workspace.Store{DB: db.DB, Events: log, Env: sys.Env{Clock: clock, Random: sys.CryptoRandom{}},
		Root: "/srv/drydock/ws", Cap: 10}, log: log}
}

// walk creates a workspace and moves it through the given states.
func (e env) walk(t *testing.T, path ...workspace.State) workspace.Workspace {
	t.Helper()
	ctx := context.Background()
	*e.repos++ // one repository each: Create refuses a second for the same one
	w, err := e.ws.Create(ctx, *e.repos, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range path {
		if w, err = e.ws.Move(ctx, w.ID, s, ""); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func TestRunAppliesThePlan(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	running := e.walk(t, workspace.Cloning, workspace.Building, workspace.Running)
	e.ws.SetContainer(ctx, running.ID, "c-live")
	died := e.walk(t, workspace.Cloning, workspace.Building, workspace.Running)
	e.ws.SetContainer(ctx, died.ID, "c-dead")
	building := e.walk(t, workspace.Cloning, workspace.Building)
	e.ws.Move(ctx, building.ID, workspace.Failed, "")
	e.ws.Move(ctx, building.ID, workspace.Building, "")
	const orphan = "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	const stoppedOrphan = "01JYYYYYYYYYYYYYYYYYYYYYYY"

	var adopted []string
	r := &Reconciler{Workspaces: e.ws, Events: e.log,
		Containers: fakeLister{found: []container.Found{
			ctr(running.ID, "c-live", true),
			ctr(died.ID, "c-dead", false),
			ctr(orphan, "c-orphan", true),
			ctr(stoppedOrphan, "c-orphan-exited", false),
		}},
		OnAdopt: func(_ context.Context, w workspace.Workspace) error { adopted = append(adopted, w.ID); return nil },
	}
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}

	check := func(id string, state workspace.State, container string) {
		t.Helper()
		w, err := e.ws.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if w.State != state || w.ContainerID != container {
			t.Errorf("%s: %s %q; want %s %q", id, w.State, w.ContainerID, state, container)
		}
	}
	check(running.ID, workspace.Running, "c-live")
	check(died.ID, workspace.Stopped, "c-dead")
	check(building.ID, workspace.Failed, "")
	check(orphan, workspace.Running, "c-orphan")
	check(stoppedOrphan, workspace.Stopped, "c-orphan-exited") // adopted, and not started
	if len(adopted) != 1 || adopted[0] != running.ID {
		t.Errorf("supervisors restarted for %v; want only %s", adopted, running.ID)
	}

	// The orphan's row is rebuilt from its labels, repository and all.
	o, _ := e.ws.Get(ctx, orphan)
	if o.RepositoryID != 9 || o.Branch != "dev" || o.HostPath != "/srv/drydock/ws/"+orphan+"/repo" {
		t.Errorf("orphan row %+v", o)
	}

	// Every change wrote an event, and the interrupted one says why.
	evs, _ := e.log.Since(ctx, 0)
	var detail string
	for _, ev := range evs {
		if ev.WorkspaceID == building.ID && ev.Kind == workspace.KindState {
			var d struct{ Detail string }
			json.Unmarshal(ev.Data, &d)
			detail = d.Detail
		}
	}
	if !strings.Contains(detail, "restarted while this workspace was building") {
		t.Errorf("the interrupted workspace's last event says %q", detail)
	}

	// A second run finds nothing left to do except re-adopting what runs:
	// reconciliation converges.
	plan, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan {
		if a.Kind != Adopt {
			t.Errorf("second run still wants %s for %s", a.Kind, a.WorkspaceID)
		}
	}
}

// If Docker cannot be listed, every running row would look absent and be
// marked stopped. So a failed list changes nothing at all.
func TestRunChangesNothingWhenDockerCannotBeListed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	w := e.walk(t, workspace.Cloning, workspace.Building, workspace.Running)
	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{err: errors.New("daemon down")}}
	if _, err := r.Run(ctx); !errors.Is(err, ErrNothingChanged) {
		t.Fatalf("a failed list was reported as %v; want ErrNothingChanged", err)
	}
	if got, _ := e.ws.Get(ctx, w.ID); got.State != workspace.Running {
		t.Errorf("a failed list moved the workspace to %s", got.State)
	}
	// Control: the same row with an empty, successful list is marked stopped,
	// so the assertion above is about the error and not an inert reconciler.
	r.Containers = fakeLister{}
	r.Run(ctx)
	if got, _ := e.ws.Get(ctx, w.ID); got.State != workspace.Stopped {
		t.Errorf("control: with Docker reporting it absent, the workspace is %s", got.State)
	}
}

// TestAPartialFailureIsNotNothingChanged: one action failing (a resumed
// delete that sticks) is reported as a *Partial naming the count, never as
// ErrNothingChanged — the same run applied everything else, and the control
// is that it did.
func TestAPartialFailureIsNotNothingChanged(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	stuck := e.walk(t, workspace.Deleting)
	died := e.walk(t, workspace.Cloning, workspace.Building, workspace.Running)
	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{},
		Delete: func(context.Context, workspace.Workspace, string) error { return errors.New("root-owned files") }}
	_, err := r.Run(ctx)
	var p *Partial
	if !errors.As(err, &p) || len(p.Errs) != 1 || p.Applied != 1 || errors.Is(err, ErrNothingChanged) {
		t.Fatalf("Run = %v; want a *Partial with one failure and one applied, not ErrNothingChanged", err)
	}
	if !strings.Contains(err.Error(), stuck.ID) {
		t.Errorf("the error does not name the workspace that failed: %v", err)
	}
	// Control: the rest of the plan was applied.
	if got, _ := e.ws.Get(ctx, died.ID); got.State != workspace.Stopped {
		t.Errorf("the other row is %s; the partial run should have marked it stopped", got.State)
	}
}

// Without a Delete function a deleting workspace stays deleting — persisted,
// so a later run with deletion wired in finishes it.
func TestResumeDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	w := e.walk(t, workspace.Deleting)
	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{}}
	r.Run(ctx)
	if got, _ := e.ws.Get(ctx, w.ID); got.State != workspace.Deleting {
		t.Errorf("without a deleter: %s", got.State)
	}
	var deleted string
	r.Delete = func(_ context.Context, w workspace.Workspace, _ string) error { deleted = w.ID; return nil }
	r.Run(ctx)
	if deleted != w.ID {
		t.Errorf("the delete was not resumed")
	}
}

// TestRunLeavesThisProcessesRunsAlone: reconciliation runs beside serving,
// so a workspace this process is provisioning is mid-provision because it
// is being provisioned. Exclusive declines it; the control is a row in the
// same state, not busy, marked interrupted by the same run — inside its
// Exclusive call, not after it.
func TestRunLeavesThisProcessesRunsAlone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mine := e.walk(t, workspace.Cloning, workspace.Building)
	stale := e.walk(t, workspace.Cloning, workspace.Building)
	var inside workspace.State // the stale row's state as its act returned
	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{},
		Exclusive: func(id string, act func() error) (bool, error) {
			if id == mine.ID {
				return false, nil
			}
			err := act()
			if w, gerr := e.ws.Get(ctx, id); gerr == nil && id == stale.ID {
				inside = w.State
			}
			return true, err
		}}
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if w, _ := e.ws.Get(ctx, mine.ID); w.State != workspace.Building {
		t.Errorf("a workspace this process is provisioning was moved to %s", w.State)
	}
	if w, _ := e.ws.Get(ctx, stale.ID); w.State != workspace.Failed {
		t.Errorf("control: an interrupted workspace is %s, want failed", w.State)
	}
	if inside != workspace.Failed {
		t.Errorf("the interrupted workspace was %q when its Exclusive call returned: the act ran outside the lock", inside)
	}
}

// ---- Steps an earlier process died inside -----------------------------------

// step writes a workspace.step event, as Provision does.
func (e env) step(t *testing.T, id string, st workspace.Step, status string) int64 {
	t.Helper()
	ev, err := e.log.Emit(context.Background(), id, events.Info, workspace.KindStep, "x",
		map[string]any{"step": st, "status": status})
	if err != nil {
		t.Fatal(err)
	}
	return ev.ID
}

// ran writes a run's step events, every step done up to last, which is
// written as started and left there.
func (e env) ran(t *testing.T, id string, last workspace.Step) {
	t.Helper()
	for _, st := range workspace.Steps {
		e.step(t, id, st, "started")
		if st == last {
			return
		}
		e.step(t, id, st, "done")
	}
}

type stepEv struct {
	id                   int64
	step, status, detail string
}

// stepEvents is the workspace's step events, oldest first.
func (e env) stepEvents(t *testing.T, id string) []stepEv {
	t.Helper()
	evs, err := e.log.ForWorkspace(context.Background(), id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []stepEv
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != workspace.KindStep {
			continue
		}
		var d struct{ Step, Status, Detail string }
		json.Unmarshal(evs[i].Data, &d)
		out = append(out, stepEv{evs[i].ID, d.Step, d.Status, d.Detail})
	}
	return out
}

const closedStep8 = "The session server step failed: " + InterruptedStep

// TestRunClosesAStepLeftStarted: a running workspace whose step 8 was left
// started by a process that died inside it — the hard-kill case nothing else
// writes an end for — has the step failed with Drydock's sentence, published
// live, and stays running: a step-8 failure never fails the workspace (§6).
// The controls, all in the same run: a step 8 that ended done; one whose
// started was followed by its failed; an earlier run's started that a newer
// run's started and done followed; and a workspace this process has a job
// for, whose step 8 is started because it is running. Each is untouched.
func TestRunClosesAStepLeftStarted(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	walk := func(last workspace.Step) workspace.Workspace {
		w := e.walk(t, workspace.Cloning, workspace.Building, workspace.Running)
		e.ws.SetContainer(ctx, w.ID, "c-"+w.ID)
		e.ran(t, w.ID, last)
		return w
	}
	dangling := walk(workspace.StepSessionServer)
	done := walk(workspace.StepSessionServer)
	e.step(t, done.ID, workspace.StepSessionServer, "done")
	ended := walk(workspace.StepSessionServer)
	e.step(t, ended.ID, workspace.StepSessionServer, "failed")
	reran := walk(workspace.StepSessionServer)
	e.step(t, reran.ID, workspace.StepSessionServer, "started")
	e.step(t, reran.ID, workspace.StepSessionServer, "done")
	mine := walk(workspace.StepSessionServer)
	// A killed run's allocate, and then a later run that never redid it:
	// the started is the newest of its step but not of the workspace, so
	// its run is over. Closing it would append the newest step event and
	// take the later run's timeline over.
	movedPast := walk(workspace.StepAllocate)
	for _, st := range workspace.Steps[1:] {
		e.step(t, movedPast.ID, st, "started")
		e.step(t, movedPast.ID, st, "done")
	}

	var found []container.Found
	for _, w := range []workspace.Workspace{dangling, done, ended, reran, mine, movedPast} {
		found = append(found, ctr(w.ID, "c-"+w.ID, true))
	}
	controls := map[string]workspace.Workspace{"done": done, "ended failed": ended, "rerun": reran, "owned": mine,
		"moved past": movedPast}
	before := map[string]int{}
	for _, w := range controls {
		before[w.ID] = len(e.stepEvents(t, w.ID))
	}

	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{found: found},
		Exclusive: func(id string, act func() error) (bool, error) {
			if id == mine.ID {
				return false, nil
			}
			return true, act()
		}}
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}

	v, err := e.ws.View(ctx, dangling.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Steps[workspace.StepSessionServer]; got.Status != "failed" || got.Detail != closedStep8 {
		t.Errorf("step 8 after reconciliation: %+v; want failed, %q", got, closedStep8)
	}
	if v.State != workspace.Running {
		t.Errorf("the workspace is %s; a step-8 failure leaves it running", v.State)
	}
	for st, o := range v.Steps {
		if st != workspace.StepSessionServer && o.Status != "done" {
			t.Errorf("step %s is %s; only the dangling step is closed", st, o.Status)
		}
	}
	// Published, as a live failure is: the reducer applies it from the stream.
	live := false
	for len(sub.C) > 0 {
		ev := <-sub.C
		var d struct{ Step, Status, Detail string }
		json.Unmarshal(ev.Data, &d)
		if ev.WorkspaceID == dangling.ID && ev.Kind == workspace.KindStep &&
			d.Step == string(workspace.StepSessionServer) && d.Status == "failed" && d.Detail == closedStep8 {
			live = true
		}
	}
	if !live {
		t.Error("the closed step was not published to subscribers")
	}

	for name, w := range controls {
		if got := e.stepEvents(t, w.ID); len(got) != before[w.ID] {
			t.Errorf("%s: step events went from %d to %d: %+v", name, before[w.ID], len(got), got[before[w.ID]:])
		}
	}
	if v, _ := e.ws.View(ctx, mine.ID); v.Steps[workspace.StepSessionServer].Status != "started" {
		t.Errorf("owned: step 8 is %+v", v.Steps[workspace.StepSessionServer])
	}

	// Idempotent: a second boot finds nothing left started.
	n := len(e.stepEvents(t, dangling.ID))
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.stepEvents(t, dangling.ID); len(got) != n {
		t.Errorf("a second run wrote more step events: %+v", got[n:])
	}
}

// TestRunClosesTheStepOfAnInterruptedBuild: a workspace a crash left
// building, with the container start step started, is marked failed by the
// plan (§6) — and its step is failed too, before the move, in the order a
// live failure writes them, so the timeline names the step the failed state
// is about. The control is the same row with its step ended: marked failed,
// and no step written.
func TestRunClosesTheStepOfAnInterruptedBuild(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	w := e.walk(t, workspace.Cloning, workspace.Building)
	e.ran(t, w.ID, workspace.StepUp)
	control := e.walk(t, workspace.Cloning, workspace.Building)
	e.ran(t, control.ID, workspace.StepUp)
	e.step(t, control.ID, workspace.StepUp, "done")
	before := len(e.stepEvents(t, control.ID))

	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{}}
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := e.ws.View(ctx, w.ID)
	if v.State != workspace.Failed || !strings.Contains(deref(v.StateDetail), "restarted while this workspace was building") {
		t.Errorf("state %s %q", v.State, deref(v.StateDetail))
	}
	want := "The container start step failed: " + InterruptedStep
	if got := v.Steps[workspace.StepUp]; got.Status != "failed" || got.Detail != want {
		t.Errorf("step up: %+v; want failed, %q", got, want)
	}
	// The step's failure is older than the move it explains.
	evs, _ := e.log.ForWorkspace(ctx, w.ID, 1000)
	var stepID, moveID int64
	for _, ev := range evs { // newest first
		var d struct{ Step, Status, State string }
		json.Unmarshal(ev.Data, &d)
		if ev.Kind == workspace.KindStep && d.Status == "failed" && stepID == 0 {
			stepID = ev.ID
		}
		if ev.Kind == workspace.KindState && d.State == string(workspace.Failed) && moveID == 0 {
			moveID = ev.ID
		}
	}
	if stepID == 0 || moveID == 0 || stepID > moveID {
		t.Errorf("step failed at event %d, moved to failed at %d; want the step first", stepID, moveID)
	}

	if v, _ := e.ws.View(ctx, control.ID); v.State != workspace.Failed {
		t.Errorf("control: %s", v.State)
	}
	if got := e.stepEvents(t, control.ID); len(got) != before {
		t.Errorf("control: a step that ended was written again: %+v", got[before:])
	}
}

// TestRunClosesADeletingWorkspacesStep: a resumed delete that sticks leaves
// the row deleting, and /ws/:id still shows its timeline, so a deleting row's
// dangling step is closed too — before the delete is resumed, which takes
// the lock itself. The control is a deleting row this process owns: left
// alone, and its delete not resumed by reconciliation either.
func TestRunClosesADeletingWorkspacesStep(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	del := e.walk(t, workspace.Cloning, workspace.Building, workspace.Running)
	e.ran(t, del.ID, workspace.StepSessionServer)
	e.ws.Move(ctx, del.ID, workspace.Deleting, "")
	mine := e.walk(t, workspace.Cloning, workspace.Building, workspace.Running)
	e.ran(t, mine.ID, workspace.StepSessionServer)
	e.ws.Move(ctx, mine.ID, workspace.Deleting, "")
	var closedAtDelete workspace.StepOutcome
	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{},
		Exclusive: func(id string, act func() error) (bool, error) {
			if id == mine.ID {
				return false, nil
			}
			return true, act()
		},
		Delete: func(ctx context.Context, w workspace.Workspace, _ string) error {
			v, _ := e.ws.View(ctx, w.ID)
			closedAtDelete = v.Steps[workspace.StepSessionServer]
			return errors.New("stuck")
		}}
	r.Run(ctx) // the delete sticks: a *Partial
	if v, _ := e.ws.View(ctx, del.ID); v.State != workspace.Deleting ||
		v.Steps[workspace.StepSessionServer].Status != "failed" || v.Steps[workspace.StepSessionServer].Detail != closedStep8 {
		t.Errorf("a stuck delete's row: %s, step 8 %+v", v.State, v.Steps[workspace.StepSessionServer])
	}
	if closedAtDelete.Status != "failed" {
		t.Errorf("step 8 was %q when the delete was resumed; want it closed first", closedAtDelete.Status)
	}
	if v, _ := e.ws.View(ctx, mine.ID); v.Steps[workspace.StepSessionServer].Status != "started" {
		t.Errorf("owned: step 8 is %+v", v.Steps[workspace.StepSessionServer])
	}
}

// TestRunNeverClosesAStepALaterRunMovedPast: an older release killed during
// verify; a later start failed at resolve config, never reaching verify
// again. Verify's started is the newest of its step but not the workspace's,
// so it is left: closing it would append the newest step event, and the
// failed card and timeline would blame verify instead of resolve config.
// The control is the same history with the start killed inside resolve
// config: that started is the newest overall, and it is closed.
func TestRunNeverClosesAStepALaterRunMovedPast(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seed := func(lastStatus string) workspace.Workspace {
		w := e.walk(t, workspace.Cloning, workspace.Building)
		e.ran(t, w.ID, workspace.StepVerify)
		e.ws.Move(ctx, w.ID, workspace.Failed, "Drydock restarted while this workspace was building.")
		e.ws.Move(ctx, w.ID, workspace.Building, "")
		e.step(t, w.ID, workspace.StepResolveConfig, "started")
		if lastStatus != "" {
			e.step(t, w.ID, workspace.StepResolveConfig, lastStatus)
			e.ws.Move(ctx, w.ID, workspace.Failed, "The resolve config step failed.")
		}
		return w
	}
	past := seed("failed")
	killed := seed("")
	before := len(e.stepEvents(t, past.ID))

	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{}}
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.stepEvents(t, past.ID); len(got) != before {
		t.Errorf("a step a later run moved past was closed: %+v", got[before:])
	}
	if v, _ := e.ws.View(ctx, killed.ID); v.Steps[workspace.StepResolveConfig].Status != "failed" ||
		v.Steps[workspace.StepVerify].Status != "started" {
		t.Errorf("control: resolve config %+v, verify %+v; want only the newest closed",
			v.Steps[workspace.StepResolveConfig], v.Steps[workspace.StepVerify])
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
