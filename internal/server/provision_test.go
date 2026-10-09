package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// awaitCatalog waits for the fake App's one repository to be listed.
func awaitCatalog(t *testing.T, r *running, cookie string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var repos struct{ Repos []struct{ ID int64 } }
		body(t, r.do(t, req{method: "GET", path: "/api/repos", cookie: cookie}).Body, &repos)
		if len(repos.Repos) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog never filled")
		}
	}
}

// TestWorkspaceRoutesRefusedOnceProvisionStops: once the provisioner's group
// in Serve's work is stopping — as it is from the moment shutdown begins,
// while the HTTP servers still drain — a request for a workspace job starts
// nothing and is answered 503 unavailable, never 202 for a job nothing would
// run, nor 500. The control is the create answered 202 before the stop,
// whose run reaches running.
func TestWorkspaceRoutesRefusedOnceProvisionStops(t *testing.T) {
	srv, serve := appServer(t, t.TempDir())
	r := serve()
	r.client.Timeout = 30 * time.Second
	cookie := r.signIn(t)
	awaitCatalog(t, r, cookie)
	call := func(method, path, b string) (int, string) {
		resp := r.do(t, req{method: method, path: path, origin: uiOrigin, cookie: cookie, body: b})
		return resp.StatusCode, readBody(t, resp)
	}

	status, b := call("POST", "/api/workspaces", `{"repository_id":1}`)
	if status != http.StatusAccepted {
		t.Fatalf("control: create = %d %s", status, b)
	}
	var created struct{ ID string }
	json.Unmarshal([]byte(b), &created)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		w, err := srv.Workspaces.Get(context.Background(), created.ID)
		if err == nil && w.State == workspace.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: the create never reached running: %+v %v", w, err)
		}
	}

	if late := srv.provisionWork.Load().Wait(time.After(30 * time.Second)); late != nil {
		t.Fatalf("the provisioner's group did not stop: %v", late)
	}
	mark, err := srv.Events.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/workspaces/" + created.ID + "/rebuild"},
		{"POST", "/api/workspaces/" + created.ID + "/stop"},
		{"POST", "/api/workspaces/" + created.ID + "/supervisor"},
		{"DELETE", "/api/workspaces/" + created.ID + "?confirm=krelinga/alpha"},
	} {
		if status, b := call(c.method, c.path, ""); status != http.StatusServiceUnavailable || !strings.Contains(b, `"unavailable"`) {
			t.Errorf("%s %s after the stop = %d %s; want 503 unavailable", c.method, c.path, status, b)
		}
	}
	if w, err := srv.Workspaces.Get(context.Background(), created.ID); err != nil || w.State != workspace.Running {
		t.Errorf("after the refusals: %+v %v; want it running, untouched", w, err)
	}
	// What a job or a request for one writes; the session server the run
	// started keeps writing its own (supervisor.state), which is not a job's.
	var wrote int
	if err := srv.DB.DB.QueryRow(`SELECT count(*) FROM event WHERE id > ? AND workspace_id = ? AND kind IN (?, ?, ?, ?)`,
		mark, created.ID, workspace.KindState, workspace.KindStep, workspace.KindAction, workspace.KindJob).Scan(&wrote); err != nil {
		t.Fatal(err)
	}
	if wrote != 0 {
		t.Errorf("the refused requests wrote %d workspace events", wrote)
	}
}

// TestShutdownEndsARunInFlight is the provisioner's half of #83: a run in
// flight is in Serve's work, which shutdown stops and waits for before the
// database closes. Its step 8 is held until the run's context ends, then
// returns slowly, asking the database as it does — so a shutdown that did
// not wait would close the database under it. After Serve returns, the
// database Serve closed holds the step failed, saying Drydock shut down, and
// the run's workspace.job end, cancelled, as the workspace's last event. The
// control is the run held at step 8 with no end before the shutdown.
func TestShutdownEndsARunInFlight(t *testing.T) {
	srv, serve := appServer(t, t.TempDir())
	entered := make(chan struct{})
	var once sync.Once
	dbAtReturn := make(chan error, 1)
	srv.Provisioner.StartSupervisor = func(ctx context.Context, w workspace.Workspace) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		time.Sleep(300 * time.Millisecond)
		dbAtReturn <- srv.DB.DB.PingContext(context.Background())
		return ctx.Err()
	}
	r := serve()
	r.client.Timeout = 30 * time.Second
	cookie := r.signIn(t)
	awaitCatalog(t, r, cookie)

	resp := r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
	var created struct{ ID string }
	body(t, resp.Body, &created)
	if resp.StatusCode != http.StatusAccepted || created.ID == "" {
		t.Fatalf("control: create = %d", resp.StatusCode)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the run never reached step 8")
	}
	var ended bool
	srv.DB.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM event WHERE workspace_id = ? AND kind = ?)`,
		created.ID, workspace.KindJob).Scan(&ended)
	if ended {
		t.Fatal("control: the run held at step 8 has ended already")
	}

	stopped := make(chan struct{})
	go func() { r.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return")
	}
	select {
	case err := <-dbAtReturn:
		if err != nil {
			t.Errorf("the database was closed while the cut-off step was returning: %v", err)
		}
	default:
		t.Fatal("Serve returned before the cut-off step did")
	}

	db, err := store.Open(context.Background(), r.cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	evs, err := events.New(db.DB, sys.RealClock{}).ForWorkspace(context.Background(), created.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("no events for the workspace")
	}
	var stepFailed bool
	for _, ev := range evs {
		var d struct{ Step, Status, Detail string }
		if ev.Kind == workspace.KindStep && json.Unmarshal(ev.Data, &d) == nil &&
			d.Step == string(workspace.StepSessionServer) && d.Status == "failed" {
			stepFailed = strings.Contains(d.Detail, "Drydock shut down")
		}
	}
	if !stepFailed {
		t.Error("step 8 was not recorded failed, saying Drydock shut down")
	}
	// ForWorkspace is newest first.
	var end workspace.JobData
	if last := evs[0]; last.Kind != workspace.KindJob || json.Unmarshal(last.Data, &end) != nil ||
		end.Kind != "create" || end.Outcome != workspace.JobCancelled {
		t.Errorf("the workspace's last event is %s %s; want the create's end, cancelled", last.Kind, last.Data)
	}
}
