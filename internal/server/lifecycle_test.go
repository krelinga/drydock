package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// fakeCLIs is fakeDevcontainer plus a docker that lists no containers and
// accepts every stop and rm: the server tests are about the routes and the
// wiring; what docker does is internal/provision's fake and test/container's
// real daemon.
func fakeCLIs(t *testing.T, origin string) subproc.Runner {
	t.Helper()
	// fakeDevcontainer's docker answers ps, stop and rm as well as step 4's
	// volume commands.
	return fakeDevcontainer(t, origin)
}

// appServer is a server with the fake GitHub as its App and the fake CLIs,
// not yet serving: the caller may seed rows first.
func appServer(t *testing.T, dir string) (*Server, func() *running) {
	t.Helper()
	cfg := testConfig(t, dir)
	f := githubtest.New(t, 5189455, time.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 1, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{".devcontainer/devcontainer.json"}},
	}}}
	f.EnableGit(t)
	keyPath := filepath.Join(dir, "app.pem")
	os.WriteFile(keyPath, githubtest.KeyPEM(t), 0o400)
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 5189455, keyPath, f.URL
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	run := fakeCLIs(t, f.URL+"/krelinga/alpha.git")
	srv.Provisioner.Cloner.BaseURL = f.URL
	srv.Provisioner.Containers.Run = run
	srv.Reconciler.Containers = container.Manager{Run: run, LabelPrefix: cfg.LabelPrefix}
	return srv, func() *running {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		return &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
	}
}

// TestWorkspaceLifecycleThroughTheServer drives stop, start, rebuild and
// delete through the real server over its socket: each answers 202 {}, the
// state follows, the broker socket is closed by a stop and reopened by a
// start, and a delete takes the directory, the socket and the row. Every
// refusal is beside the request that succeeds.
func TestWorkspaceLifecycleThroughTheServer(t *testing.T) {
	srv, serve := appServer(t, t.TempDir())
	hold := holdStep8(t, srv)
	r := serve()
	// A hang fails with a message rather than at go test's timeout.
	r.client.Timeout = 30 * time.Second
	cookie := r.signIn(t)
	call := func(method, path string) *httpResp {
		resp := r.do(t, req{method: method, path: path, origin: uiOrigin, cookie: cookie})
		return &httpResp{resp.StatusCode, readBody(t, resp)}
	}
	view := func(id string) wsView {
		resp := r.do(t, req{method: "GET", path: "/api/workspaces/" + id, cookie: cookie})
		if resp.StatusCode == 404 {
			return wsView{State: "gone"}
		}
		var v wsView
		body(t, resp.Body, &v)
		return v
	}
	state := func(id string) string { return view(id).State }
	// reached is the state, and for running also the run's end: until step
	// 8 returns the run is in flight and a stop or rebuild is refused.
	reached := func(v wsView, want string) bool {
		if want == "running" {
			return settled(t, v.State, v.Events)
		}
		return v.State == want
	}
	// await waits for want. Every step 8 is held (holdStep8), and await lets
	// one through only once it has seen it held — running, step 8 started —
	// so each wait for running is proved past that window: an await that
	// returned on the state alone would leave the run held, and the stop or
	// rebuild after it would be refused.
	reruns := 0 // runs whose held view had an earlier run's step 8 end in it
	await := func(id, want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		released := false
		for v := view(id); !reached(v, want); v = view(id) {
			if !released && v.State == "running" && v.Steps["session_server"].Status == "started" {
				if staleControl(t, v.Events) {
					reruns++
				}
				hold.release()
				released = true
			}
			if time.Now().After(deadline) {
				t.Fatalf("workspace never reached %s; it is %+v", want, v)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if s := state(id); s != want {
			t.Fatalf("workspace settled %s, want %s", s, want)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var repos struct{ Repos []struct{ ID int64 } }
		body(t, r.do(t, req{method: "GET", path: "/api/repos", cookie: cookie}).Body, &repos)
		if len(repos.Repos) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog never filled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Boot's work (reconciliation, then resuming session servers through
	// the held seam) is over before the first create, so no StartSupervisor
	// call but the runs' own reaches the hold.
	select {
	case <-srv.reconciled:
	case <-time.After(15 * time.Second):
		t.Fatal("boot reconciliation never finished")
	}
	resp := r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
	var id struct{ ID string }
	body(t, resp.Body, &id)
	// Control: the window between running and step 8's end is real, and a
	// stop inside it is refused, which is why await waits past it.
	deadline = time.Now().Add(15 * time.Second)
	for v := view(id.ID); !(v.State == "running" && v.Steps["session_server"].Status == "started"); v = view(id.ID) {
		if v.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("never running with step 8 started: %+v", v)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := call("POST", "/api/workspaces/"+id.ID+"/stop"); got.status != 409 || !strings.Contains(got.body, `"in_progress"`) {
		t.Errorf("stop while step 8 runs: %+v", got)
	}
	await(id.ID, "running")
	// The cap and the occupied count (frontend §4.5 #17), from the list
	// itself: a running workspace counts, a stopped one does not, and a
	// start counts it again.
	occupied := func() string {
		var l struct{ Capacity struct{ Cap, Occupied int } }
		body(t, r.do(t, req{method: "GET", path: "/api/workspaces", cookie: cookie}).Body, &l)
		return fmt.Sprintf("%d of %d", l.Capacity.Occupied, l.Capacity.Cap)
	}
	if got := occupied(); got != fmt.Sprintf("1 of %d", r.cfg.ContainerCap) {
		t.Errorf("running: %s", got)
	}
	sock := srv.Broker.SocketPath(id.ID)
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("control: a running workspace has no socket: %v", err)
	}

	const nobody = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	for _, p := range []string{"/stop", "/rebuild"} {
		if got := call("POST", "/api/workspaces/"+nobody+p); got.status != 404 || !strings.Contains(got.body, `"not_found"`) {
			t.Errorf("%s of no workspace: %+v", p, got)
		}
	}
	if got := call("DELETE", "/api/workspaces/"+nobody+"?confirm=krelinga/alpha"); got.status != 404 {
		t.Errorf("delete of no workspace: %+v", got)
	}

	if got := call("POST", "/api/workspaces/"+id.ID+"/stop"); got.status != 202 || strings.TrimSpace(got.body) != `{}` {
		t.Fatalf("stop: %+v", got)
	}
	await(id.ID, "stopped")
	if got := occupied(); got != fmt.Sprintf("0 of %d", r.cfg.ContainerCap) {
		t.Errorf("stopped: %s", got)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stopped workspace kept its socket: %v", err)
	}
	if got := call("POST", "/api/workspaces/"+id.ID+"/stop"); got.status != 409 || !strings.Contains(got.body, `"in_progress"`) {
		t.Errorf("stop of a stopped workspace: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.WorkspaceRoot, id.ID, "repo", ".git")); err != nil {
		t.Errorf("the clone did not survive the stop: %v", err)
	}

	if got := call("POST", "/api/workspaces/"+id.ID+"/start"); got.status != 202 {
		t.Fatalf("start: %+v", got)
	}
	await(id.ID, "running")
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("start did not reopen the socket: %v", err)
	}
	if got := occupied(); got != fmt.Sprintf("1 of %d", r.cfg.ContainerCap) {
		t.Errorf("started again: %s", got)
	}

	if got := call("POST", "/api/workspaces/"+id.ID+"/rebuild"); got.status != 202 || strings.TrimSpace(got.body) != `{}` {
		t.Fatalf("rebuild: %+v", got)
	}
	await(id.ID, "running")

	for _, confirm := range []string{"", "?confirm=", "?confirm=krelinga%2FAlpha", "?confirm=alpha", "?confirm=krelinga/alpha%20"} {
		got := call("DELETE", "/api/workspaces/"+id.ID+confirm)
		if got.status != 400 || !strings.Contains(got.body, `"`+api.CodeConfirmMismatch+`"`) {
			t.Errorf("delete with %q: %+v", confirm, got)
		}
	}
	if s := state(id.ID); s != "running" {
		t.Fatalf("a refused delete moved the workspace to %s", s)
	}
	if got := call("DELETE", "/api/workspaces/"+id.ID+"?confirm="+url.QueryEscape("krelinga/alpha")); got.status != 202 ||
		strings.TrimSpace(got.body) != `{}` {
		t.Fatalf("delete: %+v", got)
	}
	await(id.ID, "gone")
	// The start and the rebuild each ran the stale-step-8 control.
	if reruns != 2 {
		t.Errorf("%d runs found an earlier run's step 8 end, want 2 (the start and the rebuild)", reruns)
	}
	if _, err := os.Lstat(filepath.Join(r.cfg.WorkspaceRoot, id.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace directory survived: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket survived the delete: %v", err)
	}
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		t.Error("the deleted workspace's socket still answers")
	}
	if got := call("DELETE", "/api/workspaces/"+id.ID+"?confirm=krelinga/alpha"); got.status != 404 {
		t.Errorf("a second delete: %+v", got)
	}
	// The repository is free for a new workspace: one per repository until
	// delete, and delete has finished.
	resp = r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
	if resp.StatusCode != 202 {
		t.Errorf("create after delete: %d", resp.StatusCode)
	}
}

// TestDeleteResumesAtBoot is the wiring of §6's "deleting: resume the
// delete": a row left deleting by an earlier process — its directory and a
// stale socket file still there — is finished by boot reconciliation through
// the same delete the route runs. Beside it, a stopped workspace with its own
// directory is left exactly as it was.
func TestDeleteResumesAtBoot(t *testing.T) {
	const deleting, stopped = "01JDE1ETEAAAAAAAAAAAAAAAAA", "01JST0PPEDAAAAAAAAAAAAAAAA"
	srv, serve := appServer(t, t.TempDir())
	cfg := srv.Workspaces
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (1, 77, 'krelinga/alpha', 'main')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (2, 77, 'krelinga/beta', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + deleting + `', 1, '` +
			filepath.Join(cfg.Root, deleting, "repo") + `', 'main', 'deleting')`,
		`UPDATE workspace SET state_detail = 'The delete stopped part-way: docker could not remove the workspace''s containers. Delete again to retry.' WHERE id = '` + deleting + `'`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + stopped + `', 2, '` +
			filepath.Join(cfg.Root, stopped, "repo") + `', 'main', 'stopped')`,
	} {
		if _, err := srv.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{deleting, stopped} {
		os.MkdirAll(filepath.Join(cfg.Root, id, "repo"), 0o700)
		os.WriteFile(filepath.Join(cfg.Root, id, "repo", "unpushed.txt"), []byte("work"), 0o600)
	}
	// A socket file an earlier process left behind.
	os.MkdirAll(filepath.Dir(srv.Broker.SocketPath(deleting)), 0o700)
	ln, err := net.Listen("unix", srv.Broker.SocketPath(deleting))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	serve()
	select {
	case <-srv.reconciled:
	case <-time.After(30 * time.Second):
		t.Fatal("boot reconciliation did not finish")
	}
	if _, err := srv.Workspaces.Get(ctx, deleting); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("the deleting row survived boot: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cfg.Root, deleting)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its directory survived boot: %v", err)
	}
	if _, err := os.Lstat(srv.Broker.SocketPath(deleting)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its stale socket survived boot: %v", err)
	}
	evs, _ := srv.Events.ForWorkspace(ctx, deleting, 100)
	if len(evs) == 0 || evs[0].Kind != workspace.KindGone {
		b, _ := json.Marshal(evs)
		t.Errorf("the last event is not workspace.gone: %s", b)
	}
	// The row came in stuck, annotated; the resume cleared that first, on
	// the stream, before its first sub-step (frontend §4.5 #16).
	if n := len(evs); n < 2 || evs[n-1].Kind != workspace.KindState ||
		string(evs[n-1].Data) != `{"from":"deleting","state":"deleting"}` || evs[n-2].Kind != provision.KindAction {
		b, _ := json.Marshal(evs)
		t.Errorf("the resume did not begin by clearing the stuck note: %s", b)
	}
	// The control: reconciliation is not deleting everything.
	if w, err := srv.Workspaces.Get(ctx, stopped); err != nil || w.State != workspace.Stopped {
		t.Errorf("the stopped workspace: %+v %v", w, err)
	}
	if b, err := os.ReadFile(filepath.Join(cfg.Root, stopped, "repo", "unpushed.txt")); err != nil || string(b) != "work" {
		t.Errorf("the stopped workspace's clone: %v", err)
	}
	if _, err := os.Stat(srv.Broker.SocketPath(stopped)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("boot opened a socket for a stopped workspace: %v", err)
	}
}
