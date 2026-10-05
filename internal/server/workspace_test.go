package server

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// fakeDevcontainer writes a devcontainer CLI stand-in that answers as the
// real one does on the happy path — the recorded read-configuration and up
// results, a probe that passes and a remote that names origin — so the
// server test is about the routes and the wiring, not the CLI. The argv
// Drydock builds is asserted in internal/provision; the real CLI runs in
// test/container.
func fakeDevcontainer(t *testing.T, origin string) subproc.Runner {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		// The recording's folder was rewritten to /srv/drydock/ws/FIXTURE/repo;
		// the real CLI names the folder it was given ($3), so the fake does.
		"read-configuration) sed \"s#/srv/drydock/ws/FIXTURE/repo#$3#g\" <<'EOF'\n" + read("read-configuration-ok.json") + "\nEOF\n;;\n" +
		"up) cat <<'EOF'\n" + read("up-ok.json") + "\nEOF\n;;\n" +
		"exec) case \" $* \" in *\" remote -v \"*) printf 'origin\\t%s (fetch)\\n' '" + origin + "' ;;\n" +
		"  *\" claude --version \"*) echo '" + classify.ClaudeCodeVersion + " (Claude Code)' ;; esac ;;\n" +
		"*) exit 64 ;;\nesac\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "devcontainer")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// docker: step 4's shared credential volume, absent and then created —
	// under whatever name the config gives it, echoed from the argv, never
	// assumed; no containers listed; every stop and rm accepted. The last
	// argument of a volume command is the name (after "--", or name=<x>).
	d := filepath.Join(dir, "docker")
	if err := os.WriteFile(d, []byte(`#!/bin/sh
case "$1" in ps|stop|rm) exit 0 ;; esac
for last; do :; done
case "$1 $2" in
"volume ls") if [ -e "$0.made" ]; then cat "$0.name"; fi ;;
"volume create") echo "$6" >"$0.made"; echo "$last" >"$0.name"; echo "$last" ;;
"volume inspect")
  label=$(sed 's/=.*//' "$0.made")
  if [ "$3" = --format ]; then printf '{"%s":"true"}\n' "$label"
  else printf '[{"Name":"%s","Driver":"local","Labels":{"%s":"true"}}]\n' "$last" "$label"; fi ;;
*) exit 64 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	return subproc.Exec{Resolver: subproc.FixedResolver{"devcontainer": p, "docker": d}}
}

func body(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatal(err)
	}
}

type wsView struct {
	ID          string  `json:"id"`
	FullName    string  `json:"full_name"`
	Branch      string  `json:"branch"`
	State       string  `json:"state"`
	StateDetail *string `json:"state_detail"`
	ContainerID *string `json:"container_id"`
	Steps       map[string]struct {
		Status, Detail string
	} `json:"steps"`
	Events []struct {
		ID   int64  `json:"id"`
		Kind string `json:"kind"`
	} `json:"events"`
}

// TestWorkspaceRoutesEndToEnd drives the walking skeleton through the real
// server over its socket, signed in, against the fake GitHub and its git
// remote: POST /api/workspaces answers 202 with an id, and GET
// /api/workspaces/{id} then shows it reach running with all eight steps
// done. Around it, every refusal the contract names, each beside the request
// that succeeds.
func TestWorkspaceRoutesEndToEnd(t *testing.T) {
	// Control for app_not_configured: a server with no App lists (nothing)
	// but cannot create.
	plain := start(t)
	pc := plain.signIn(t)
	if resp := plain.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: pc,
		body: `{"repository_id":1}`}); resp.StatusCode != 503 || code(t, resp) != api.CodeAppNotConfigured {
		t.Errorf("create with no App: %d", resp.StatusCode)
	}
	if resp := plain.do(t, req{method: "GET", path: "/api/workspaces", cookie: pc}); resp.StatusCode != 200 ||
		readBody(t, resp) != `{"workspaces":[],"capacity":{"cap":10,"occupied":0}}`+"\n" {
		t.Errorf("list with no workspaces: %d", resp.StatusCode)
	}

	dir := t.TempDir()
	cfg := testConfig(t, dir)
	cfg.ContainerCap = 1
	f := githubtest.New(t, 5189455, time.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 1, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{".devcontainer/devcontainer.json"}},
		{ID: 2, FullName: "krelinga/beta", DefaultBranch: "main", PushedAt: time.Now()},
	}}}
	f.EnableGit(t)
	keyPath := filepath.Join(dir, "app.pem")
	os.WriteFile(keyPath, githubtest.KeyPEM(t), 0o400)
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 5189455, keyPath, f.URL
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	srv.Provisioner.Containers.Run = fakeDevcontainer(t, f.URL+"/krelinga/alpha.git")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
	cookie := r.signIn(t)
	post := func(path, b string) *httpResp {
		resp := r.do(t, req{method: "POST", path: path, origin: uiOrigin, cookie: cookie, body: b})
		return &httpResp{resp.StatusCode, readBody(t, resp)}
	}

	// The catalog fills at boot; a create before that is a 404.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var repos struct{ Repos []struct{ ID int64 } }
		resp := r.do(t, req{method: "GET", path: "/api/repos", cookie: cookie})
		body(t, resp.Body, &repos)
		if len(repos.Repos) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog never filled")
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, b := range []string{`{}`, `{"repository_id":"1"}`, `not json`} {
		if got := post("/api/workspaces", b); got.status != 400 || !strings.Contains(got.body, `"bad_request"`) {
			t.Errorf("body %s: %+v", b, got)
		}
	}
	if got := post("/api/workspaces", `{"repository_id":999}`); got.status != 404 || !strings.Contains(got.body, `"not_found"`) {
		t.Errorf("unknown repository: %+v", got)
	}

	created := post("/api/workspaces", `{"repository_id":1}`)
	var id struct{ ID string }
	if created.status != 202 || json.Unmarshal([]byte(created.body), &id) != nil || len(id.ID) != 26 {
		t.Fatalf("create: %+v", created)
	}
	if got := post("/api/workspaces", `{"repository_id":1}`); got.status != 409 || !strings.Contains(got.body, `"in_progress"`) {
		t.Errorf("a second workspace for the repository: %+v", got)
	}
	if got := post("/api/workspaces", `{"repository_id":2}`); got.status != 409 || !strings.Contains(got.body, `"at_capacity"`) {
		t.Errorf("past the cap of 1: %+v", got)
	}

	var v wsView
	deadline = time.Now().Add(15 * time.Second)
	for v.State != "running" && v.State != "failed" {
		if time.Now().After(deadline) {
			t.Fatalf("the workspace never settled: %+v", v)
		}
		time.Sleep(20 * time.Millisecond)
		resp := r.do(t, req{method: "GET", path: "/api/workspaces/" + id.ID, cookie: cookie})
		if resp.StatusCode != 200 {
			t.Fatalf("GET workspace = %d", resp.StatusCode)
		}
		body(t, resp.Body, &v)
	}
	if v.State != "running" || v.FullName != "krelinga/alpha" || v.Branch != "main" || v.ContainerID == nil || v.StateDetail != nil {
		t.Errorf("view %+v", v)
	}
	if len(v.Steps) != 8 {
		t.Errorf("%d steps, want 8: %+v", len(v.Steps), v.Steps)
	}
	for name, s := range v.Steps {
		if s.Status != "done" {
			t.Errorf("step %s is %s", name, s.Status)
		}
	}
	if len(v.Events) < 17 {
		t.Errorf("%d events; a full run writes at least 16 step events and the state moves", len(v.Events))
	}
	for i := 1; i < len(v.Events); i++ {
		if v.Events[i].ID >= v.Events[i-1].ID {
			t.Errorf("events are not newest first: %d after %d", v.Events[i].ID, v.Events[i-1].ID)
			break
		}
	}

	var list struct{ Workspaces []wsView }
	resp := r.do(t, req{method: "GET", path: "/api/workspaces", cookie: cookie})
	body(t, resp.Body, &list)
	if len(list.Workspaces) != 1 || list.Workspaces[0].ID != id.ID || list.Workspaces[0].Events != nil {
		t.Errorf("list %+v", list)
	}

	if got := post("/api/workspaces/"+id.ID+"/start", ``); got.status != 409 || !strings.Contains(got.body, `"in_progress"`) {
		t.Errorf("start of a running workspace: %+v", got)
	}
	if got := post("/api/workspaces/01JABCDEFGHJKMNPQRSTVWXYZ0/start", ``); got.status != 404 {
		t.Errorf("start of no workspace: %+v", got)
	}
	if resp := r.do(t, req{method: "GET", path: "/api/workspaces/01JABCDEFGHJKMNPQRSTVWXYZ0", cookie: cookie}); resp.StatusCode != 404 {
		t.Errorf("GET of no workspace: %d", resp.StatusCode)
	}
}

type httpResp struct {
	status int
	body   string
}
