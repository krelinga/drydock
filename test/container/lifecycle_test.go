package container_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/sys"
)

// TestStopStartRebuildDelete is Phase 6's lifecycle in the container tier:
// through the real server, signed in, with the real devcontainer CLI and the
// Feature from this checkout on the devcontainer's Docker. One workspace goes
// provision → stop → start → rebuild → delete, and at each point Docker is
// asked what is true rather than the row:
//
//   - stop: the container exists and has exited; the clone is intact; the
//     broker socket is gone, so a token request on its path fails.
//   - start: the same container, running again; the socket answers.
//   - rebuild: a new container id, the old one gone, the clone intact.
//   - delete: no container carries the label, the directory and the row are
//     gone, and so is the socket — a token request on the old path fails.
//
// Each "fails" has its control: the same token request succeeding while the
// workspace runs.
func TestStopStartRebuildDelete(t *testing.T) {
	needDevcontainer(t)
	if out, err := exec.Command("docker", "pull", "--quiet", provision.DefaultImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v: %s", provision.DefaultImage, err, out)
	}
	p := prefix(t)

	f := githubtest.New(t, 4242, time.Now)
	hostNetConfig := `{"image":"` + provision.DefaultImage + `","runArgs":["--network=host"]}`
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 101, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files:    []string{"README.md", ".devcontainer/devcontainer.json"},
			Contents: map[string]string{".devcontainer/devcontainer.json": hostNetConfig}},
	}}}
	f.EnableGit(t)

	dir, err := os.MkdirTemp("", "ddl")
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
	cfg.Feature, cfg.ClaudeVolume = newFeatureRegistry(t).Drydock, claudeVolume(p)

	srv, err := server.New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	srv.Provisioner.Config = []byte(hostNetConfig)
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
		ContainerID *string `json:"container_id"`
	}
	read := func(id string) (view, bool) {
		status, body, _ := c.do("GET", "/api/workspaces/"+id, "")
		if status == 404 {
			return view{}, false
		}
		var v view
		if status != 200 || json.Unmarshal([]byte(body), &v) != nil {
			t.Fatalf("GET workspace: %d %s", status, body)
		}
		return v, true
	}
	await := func(id string, want ...string) view {
		t.Helper()
		deadline := time.Now().Add(15 * time.Minute)
		for {
			v, ok := read(id)
			if !ok && len(want) == 0 {
				return v
			}
			for _, w := range want {
				if ok && v.State == w {
					return v
				}
			}
			if ok && v.State == "failed" {
				t.Fatalf("workspace failed: %s", deref(v.StateDetail))
			}
			if time.Now().After(deadline) {
				t.Fatalf("workspace never reached %v: %+v", want, v)
			}
			time.Sleep(time.Second)
		}
	}
	labelled := func(id string) []string {
		return strings.Fields(docker(t, "ps", "-aq", "--no-trunc", "--filter", "label="+p+".workspace="+id))
	}
	inspectRunning := func(cid string) string {
		return docker(t, "inspect", "--format", "{{.State.Running}}", cid)
	}
	token := func(id string) error {
		conn, err := net.DialTimeout("unix", srv.Broker.SocketPath(id), 5*time.Second)
		if err != nil {
			return err
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte("GET-TOKEN scope=git\n")); err != nil {
			return err
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, "OK token=") {
			return errors.New("broker answered " + strings.Fields(line)[0])
		}
		return nil
	}

	status, body := c.post("/api/workspaces", `{"repository_id":101}`)
	var created struct{ ID string }
	if status != 202 || json.Unmarshal([]byte(body), &created) != nil {
		t.Fatalf("create: %d %s", status, body)
	}
	id := created.ID
	v := await(id, "running")
	first := *v.ContainerID
	clone := filepath.Join(cfg.WorkspaceRoot, id, "repo")
	marker := filepath.Join(clone, "unpushed.txt")
	if err := os.WriteFile(marker, []byte("work in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := token(id); err != nil {
		t.Fatalf("control: a running workspace's socket refused a token request: %v", err)
	}

	// Stop.
	if status, body := c.post("/api/workspaces/"+id+"/stop", ""); status != 202 {
		t.Fatalf("stop: %d %s", status, body)
	}
	v = await(id, "stopped")
	if got := labelled(id); len(got) != 1 || got[0] != first {
		t.Errorf("after stop, docker lists %v; want only %s", got, first)
	}
	if r := inspectRunning(first); r != "false" {
		t.Errorf("after stop the container is running=%s", r)
	}
	if err := token(id); err == nil {
		t.Error("a stopped workspace's socket still issued a token")
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "work in progress" {
		t.Errorf("the clone did not survive the stop: %v", err)
	}

	// Start: the same container.
	if status, body := c.post("/api/workspaces/"+id+"/start", ""); status != 202 {
		t.Fatalf("start: %d %s", status, body)
	}
	v = await(id, "running")
	if *v.ContainerID != first || inspectRunning(first) != "true" {
		t.Errorf("start from stopped: container %s running=%s; want %s again", *v.ContainerID, inspectRunning(first), first)
	}
	if err := token(id); err != nil {
		t.Errorf("start did not bring the socket back: %v", err)
	}

	// Rebuild: a new container, the clone intact.
	if status, body := c.post("/api/workspaces/"+id+"/rebuild", ""); status != 202 {
		t.Fatalf("rebuild: %d %s", status, body)
	}
	if v, _ := read(id); v.State != "building" {
		t.Errorf("straight after rebuild: %s", v.State)
	}
	v = await(id, "running")
	second := *v.ContainerID
	if second == first {
		t.Errorf("rebuild kept container %s", first)
	}
	if got := labelled(id); len(got) != 1 || got[0] != second {
		t.Errorf("after rebuild, docker lists %v; want only %s", got, second)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "work in progress" {
		t.Errorf("the clone did not survive the rebuild: %v", err)
	}
	if err := token(id); err != nil {
		t.Errorf("after rebuild the socket refused: %v", err)
	}

	// Delete: a wrong confirm first, then the right one.
	if status, body, _ := c.do("DELETE", "/api/workspaces/"+id+"?confirm=krelinga/alph", ""); status != 400 {
		t.Errorf("delete with a near-miss confirm: %d %s", status, body)
	}
	if got := labelled(id); len(got) != 1 {
		t.Fatalf("a refused delete touched the container: %v", got)
	}
	if status, body, _ := c.do("DELETE", "/api/workspaces/"+id+"?confirm="+url.QueryEscape("krelinga/alpha"), ""); status != 202 {
		t.Fatalf("delete: %d %s", status, body)
	}
	await(id)
	if got := labelled(id); len(got) != 0 {
		t.Errorf("after delete, docker still lists %v", got)
	}
	if _, err := os.Lstat(filepath.Join(cfg.WorkspaceRoot, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace directory survived: %v", err)
	}
	if _, err := os.Lstat(srv.Broker.SocketPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the broker socket survived: %v", err)
	}
	if err := token(id); err == nil {
		t.Error("a token request on the deleted workspace's socket path succeeded")
	}
	// The shared credential volume is every workspace's login: a delete
	// never takes it (§7.1).
	if got := docker(t, "volume", "ls", "-q", "--filter", "label="+p+".claude-config"); got != claudeVolume(p) {
		t.Errorf("after delete, the shared credential volume: %q", got)
	}
}
