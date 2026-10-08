package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/sys"
)

// TestDiskFullThroughTheServer: the server wires the env's disk into both
// the pre-flight and the sampler. With the injected filesystem at the limit,
// POST /api/workspaces is 507 disk_full naming the figures, before any row
// exists, and the list's `disk` says over; with room — the control, the same
// request on the same server — it is accepted.
func TestDiskFullThroughTheServer(t *testing.T) {
	dir := t.TempDir()
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

	disk := &sys.FakeDisk{}
	disk.Set(93, 100)
	env := sys.Production()
	env.Disk = disk
	srv, err := New(context.Background(), cfg, env)
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
	r.client.Timeout = 30 * time.Second
	cookie := r.signIn(t)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var repos struct{ Repos []struct{ ID int64 } }
		resp := r.do(t, req{method: "GET", path: "/api/repos", cookie: cookie})
		body(t, resp.Body, &repos)
		if len(repos.Repos) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog never filled")
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp := r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
	if got := readBody(t, resp); resp.StatusCode != 507 || !strings.Contains(got, `"code":"`+api.CodeDiskFull+`"`) ||
		!strings.Contains(got, "93% full; Drydock refuses at 90%") {
		t.Fatalf("create on a full disk: %d %s", resp.StatusCode, got)
	}
	var list struct {
		Workspaces []any
		Disk       *struct{ Over bool }
	}
	for {
		resp := r.do(t, req{method: "GET", path: "/api/workspaces", cookie: cookie})
		body(t, resp.Body, &list)
		if list.Disk != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list.Workspaces) != 0 || list.Disk == nil || !list.Disk.Over {
		t.Errorf("after the refusal: %d rows, disk %+v", len(list.Workspaces), list.Disk)
	}

	disk.Set(50, 100)
	resp = r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
	if got := readBody(t, resp); resp.StatusCode != 202 {
		t.Errorf("control, with room: %d %s", resp.StatusCode, got)
	}
}
