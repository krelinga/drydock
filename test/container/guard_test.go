package container_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/provision"
)

// TestTheGuardRefusesAMovedTag is the attack the docker guard closes (design
// §6, "The docker guard"), against the real devcontainer CLI and real
// Docker. A repository declares a Feature from a registry its author
// controls, by a tag. Step 3's read-configuration fetches its metadata and
// finds nothing to approve; then, before up, the tag moves to a version that
// declares privileged (the registry's MovedFlag, set by a devcontainer
// wrapper on the first up — deterministic, where waiting for step 3 to end
// would be a race). Up fetches the moved Feature and runs `docker run
// --privileged` — through the guard, which refuses it: the workspace fails at
// up naming privileged, and no container carrying its label exists.
//
// The control is the same workspace with privileged approved for the
// repository: started again, it reads the moved metadata, finds it within
// the approval, and comes up with HostConfig.Privileged true.
func TestTheGuardRefusesAMovedTag(t *testing.T) {
	needDevcontainer(t)
	pullImage(t, provision.DefaultImage)
	p := prefix(t)
	reg := newFeatureRegistry(t)

	// The devcontainer CLI, wrapped: the first up moves the tag, then runs
	// the real CLI unchanged.
	real, err := exec.LookPath("devcontainer")
	if err != nil {
		t.Fatal(err)
	}
	// Only the first up moves it: the wrapper is armed once.
	wrap := t.TempDir()
	arm := filepath.Join(wrap, "armed")
	os.WriteFile(arm, nil, 0o600)
	if err := os.WriteFile(filepath.Join(wrap, "devcontainer"), []byte("#!/bin/sh\n"+
		"[ \"$1\" = up ] && [ -e '"+arm+"' ] && rm '"+arm+"' && touch '"+reg.MovedFlag+"'\nexec '"+real+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))

	f := githubtest.New(t, 4242, time.Now)
	cfgJSON := `{"image":"` + provision.DefaultImage + `","runArgs":["--network=host"],"features":{"` + reg.Shifty + `":{}}}`
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 101, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files:    []string{"README.md", ".devcontainer/devcontainer.json"},
			Contents: map[string]string{".devcontainer/devcontainer.json": cfgJSON}},
	}}}
	f.EnableGit(t)

	dir, err := os.MkdirTemp("", "ddg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	u, _ := user.Current()
	g, _ := user.LookupGroupId(u.Gid)
	key := filepath.Join(dir, "app.pem")
	os.WriteFile(key, githubtest.KeyPEM(t), 0o400)
	cfg := config.Default()
	cfg.UIOrigin, cfg.UIHost = "https://drydock.test", "drydock.test"
	cfg.DatabasePath = filepath.Join(dir, "drydock.db")
	cfg.APISocket, cfg.PreviewSocket = filepath.Join(dir, "http.sock"), filepath.Join(dir, "preview.sock")
	cfg.BrokerDir, cfg.WorkspaceRoot = filepath.Join(dir, "sock"), filepath.Join(dir, "ws")
	cfg.SocketGroup, cfg.LabelPrefix = g.Name, p
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 4242, key, f.URL
	cfg.BotName, cfg.BotEmail = "krelinga-drydock-dev[bot]", botEmail
	cfg.Feature, cfg.ClaudeVolume = reg.Drydock, claudeVolume(p)

	srv, err := newServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	// The tier's own runArgs, approved as the operator would (approval_test.go).
	approveHostNetwork(t, srv.DB.DB, 101)
	srv.Provisioner.RemoteEnv = map[string]string{"DRYDOCK_GITHUB_HOST": strings.TrimPrefix(f.URL, "http://")}
	noLogin(t, srv.DB.DB, cfg.ClaudeVolume)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	c := &client{t: t, sock: cfg.APISocket}
	if err := srv.Auth.SetPassword(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	c.cookie = c.signIn("correct horse battery staple")
	deadline := time.Now().Add(10 * time.Second)
	for {
		var repos struct{ Repos []struct{ ID int64 } }
		c.get("/api/repos", &repos)
		if len(repos.Repos) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog never filled")
		}
		time.Sleep(50 * time.Millisecond)
	}

	type view struct {
		State       string
		StateDetail *string `json:"state_detail"`
		Steps       map[string]struct{ Status, Detail string }
		Approval    any
		Events      []runEvent
	}
	await := func(id string, want ...string) view {
		t.Helper()
		deadline := time.Now().Add(15 * time.Minute)
		for {
			var v view
			c.get("/api/workspaces/"+id, &v)
			for _, w := range want {
				if v.State == w && (w != "running" || settled(t, v.State, v.Events)) {
					return v
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("workspace never reached %v: %+v", want, v)
			}
			time.Sleep(time.Second)
		}
	}
	containers := func(id string) []string {
		out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+p+".workspace="+id).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(out))
	}

	status, body := c.post("/api/workspaces", `{"repository_id":101}`)
	var created struct{ ID string }
	if status != 202 || json.Unmarshal([]byte(body), &created) != nil {
		t.Fatalf("create: %d %s", status, body)
	}
	v := await(created.ID, "failed", "running", "stopped")
	up := v.Steps["up"]
	if v.State != "failed" || up.Status != "failed" || !strings.Contains(up.Detail, "privileged") ||
		!strings.Contains(up.Detail, "no container was created") {
		t.Fatalf("the moved tag: %s, up %+v, resolve_config %+v", v.State, up, v.Steps["resolve_config"])
	}
	if rc := v.Steps["resolve_config"]; rc.Status != "done" || v.Approval != nil {
		t.Errorf("step 3 saw the move: %+v %+v", rc, v.Approval)
	}
	if reg.MovedServed.Load() == 0 {
		t.Error("up never fetched the moved Feature: the attack was not reproduced")
	}
	if ids := containers(created.ID); len(ids) != 0 {
		for _, id := range ids {
			priv, _ := exec.Command("docker", "inspect", "--format", "{{.HostConfig.Privileged}}", id).Output()
			t.Errorf("a container exists after the refusal: %s privileged=%s", id, strings.TrimSpace(string(priv)))
		}
	}

	// The control: privileged approved for the repository.
	approveSettings(t, srv.DB.DB, 101,
		container.HostSetting{Field: "privileged", Source: container.SourceFeature, Value: json.RawMessage(`true`)},
		container.HostSetting{Field: "runArgs", Source: container.SourceRepository, Value: json.RawMessage(`["--network=host"]`)})
	if status, body := c.post("/api/workspaces/"+created.ID+"/start", ""); status != 202 {
		t.Fatalf("start: %d %s", status, body)
	}
	v = await(created.ID, "running", "failed", "stopped")
	if v.State != "running" {
		t.Fatalf("with privileged approved: %s %+v", v.State, v.Steps)
	}
	ids := containers(created.ID)
	if len(ids) != 1 {
		t.Fatalf("containers %v", ids)
	}
	if priv := docker(t, "inspect", "--format", "{{.HostConfig.Privileged}}", ids[0]); strings.TrimSpace(priv) != "true" {
		t.Errorf("the approved container: Privileged=%s", priv)
	}

	// The approval narrows and the tag moves back: step 3 now finds nothing
	// beyond runArgs, and up would start the existing container — created
	// privileged — rather than create one. The guard reads it back and
	// refuses the start; it stays stopped.
	if status, body := c.post("/api/workspaces/"+created.ID+"/stop", ""); status != 202 {
		t.Fatalf("stop: %d %s", status, body)
	}
	await(created.ID, "stopped")
	approveSettings(t, srv.DB.DB, 101,
		container.HostSetting{Field: "runArgs", Source: container.SourceRepository, Value: json.RawMessage(`["--network=host"]`)})
	os.Remove(reg.MovedFlag)
	if status, body := c.post("/api/workspaces/"+created.ID+"/start", ""); status != 202 {
		t.Fatalf("start: %d %s", status, body)
	}
	v = await(created.ID, "failed", "running", "stopped")
	up = v.Steps["up"]
	if v.State != "failed" || !strings.Contains(up.Detail, "privileged") || v.Steps["resolve_config"].Status != "done" {
		t.Fatalf("the start of a container created privileged, approval narrowed: %s, up %+v, resolve_config %+v",
			v.State, up, v.Steps["resolve_config"])
	}
	if run := docker(t, "inspect", "--format", "{{.State.Running}}", ids[0]); strings.TrimSpace(run) != "false" {
		t.Errorf("the refused container is running: %s", run)
	}
}

// approveSettings records an approval of exactly settings for a repository,
// superseding its current one, as POST …/config-approval does.
func approveSettings(t *testing.T, db *sql.DB, repo int64, s ...container.HostSetting) {
	t.Helper()
	b, _ := json.Marshal(s)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE config_approval SET superseded_at = ? WHERE repository_id = ? AND superseded_at IS NULL`, now, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO config_approval (repository_id, hash, settings, approved_by, approved_at)
		VALUES (?, ?, ?, 'test', ?)`, repo, container.HashSettings(s), string(b), now); err != nil {
		t.Fatal(err)
	}
}
