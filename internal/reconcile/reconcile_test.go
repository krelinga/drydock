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
	for id := 1; id <= 5; id++ {
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
	if _, err := r.Run(ctx); err == nil {
		t.Fatal("a failed list was not reported")
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
// is being provisioned. Busy skips it; the control is a row in the same
// state, not busy, marked interrupted by the same run.
func TestRunLeavesThisProcessesRunsAlone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mine := e.walk(t, workspace.Cloning, workspace.Building)
	stale := e.walk(t, workspace.Cloning, workspace.Building)
	r := &Reconciler{Workspaces: e.ws, Events: e.log, Containers: fakeLister{},
		Busy: func(id string) bool { return id == mine.ID }}
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if w, _ := e.ws.Get(ctx, mine.ID); w.State != workspace.Building {
		t.Errorf("a workspace this process is provisioning was moved to %s", w.State)
	}
	if w, _ := e.ws.Get(ctx, stale.ID); w.State != workspace.Failed {
		t.Errorf("control: an interrupted workspace is %s, want failed", w.State)
	}
}
