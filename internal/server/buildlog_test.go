package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// TestBuildLogThroughTheServer: the server wires the provisioner's held
// build logs into the route. A create whose `up` fails is served, signed in,
// as held with the build's last line; the same request without a cookie is
// 401 — the control that the route is the gated one, answering for real.
func TestBuildLogThroughTheServer(t *testing.T) {
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
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	// The happy-path fake, but with an `up` that prints a build and fails.
	happy := fakeDevcontainer(t, f.URL+"/krelinga/alpha.git").(subproc.Exec).Resolver.(subproc.FixedResolver)
	errJSON, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", "up-error-postcreate.json"))
	if err != nil {
		t.Fatal(err)
	}
	wrap := filepath.Join(t.TempDir(), "devcontainer")
	os.WriteFile(wrap, []byte("#!/bin/sh\nif [ \"$1\" = up ]; then\n"+
		"echo 'Step 7/9 : RUN make the-build-line-7f2c' >&2\n"+
		"cat <<'EOF'\n"+string(errJSON)+"\nEOF\nexit 1\nfi\nexec "+happy["devcontainer"]+" \"$@\"\n"), 0o755)
	srv.Provisioner.Containers.Run = subproc.Exec{Resolver: subproc.FixedResolver{"devcontainer": wrap, "docker": happy["docker"]}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
	r.client.Timeout = 30 * time.Second
	cookie := r.signIn(t)

	deadline := time.Now().Add(10 * time.Second)
	var id string
	for id == "" {
		resp := r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
		var created struct{ ID string }
		if resp.StatusCode == 202 {
			body(t, resp.Body, &created)
			id = created.ID
		} else {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("the create was never accepted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		var v struct{ State string }
		resp := r.do(t, req{method: "GET", path: "/api/workspaces/" + id, cookie: cookie})
		body(t, resp.Body, &v)
		if v.State == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %s, never failed", v.State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp := r.do(t, req{method: "GET", path: "/api/workspaces/" + id + "/build-log", cookie: cookie})
	got := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(got, `"held":true`) || !strings.Contains(got, "the-build-line-7f2c") {
		t.Errorf("build log, signed in: %d %s", resp.StatusCode, got)
	}
	resp = r.do(t, req{method: "GET", path: "/api/workspaces/" + id + "/build-log"})
	if got := readBody(t, resp); resp.StatusCode != 401 || strings.Contains(got, "the-build-line-7f2c") {
		t.Errorf("build log, no cookie: %d %s", resp.StatusCode, got)
	}
}
