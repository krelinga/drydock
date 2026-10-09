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
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/provision"
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
	pullImage(t, provision.DefaultImage)
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
	// Not a test of the disk: the runner's own fill must never refuse its creates (design §12).
	cfg.DiskLimitPercent = 100
	cfg.UIOrigin, cfg.UIHost = "https://drydock.test", "drydock.test"
	cfg.DatabasePath = filepath.Join(dir, "drydock.db")
	cfg.APISocket, cfg.PreviewSocket = filepath.Join(dir, "http.sock"), filepath.Join(dir, "preview.sock")
	cfg.BrokerDir, cfg.WorkspaceRoot = filepath.Join(dir, "sock"), filepath.Join(dir, "ws")
	cfg.SocketGroup, cfg.LabelPrefix = g.Name, p
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 4242, key, f.URL
	cfg.BotName, cfg.BotEmail = "krelinga-drydock-dev[bot]", botEmail
	cfg.Feature, cfg.ClaudeVolume = newFeatureRegistry(t).Drydock, claudeVolume(p)

	srv, err := newServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	srv.Provisioner.Config = []byte(hostNetConfig)
	// The runArgs the test needs (above) is host access: approved, as the
	// operator would, before anything runs (design §6).
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
		ContainerID *string `json:"container_id"`
		Events      []runEvent
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
				// Running counts once the run has ended (settled): until
				// step 8 returns, a stop or rebuild is refused.
				if ok && v.State == w && (w != "running" || settled(t, v.State, v.Events)) {
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

	// The attack on PR #25: from inside the running container, the agent
	// rewrites the clone's devcontainer.json with an initializeCommand that
	// would run on the host, and the operator rebuilds. The rebuild stops
	// the container, stops at resolve_config for an approval naming the
	// field, and never runs up — so the host canary never appears. Then the
	// operator approves, and it runs: approval is the operator's decision to
	// let it (design §6), so the canary appears. Putting the file back is less
	// than was approved, so it runs without asking — and without the canary.
	canary := filepath.Join(dir, "canary-initialize")
	hostile := `{"image":"` + provision.DefaultImage + `","runArgs":["--network=host"],` +
		`"initializeCommand":"touch ` + canary + `"}`
	rewrite := exec.Command("docker", "exec", "-i", "-u", "vscode", first, "sh", "-c",
		"cat > /workspaces/repo/.devcontainer/devcontainer.json")
	rewrite.Stdin = strings.NewReader(hostile)
	if out, err := rewrite.CombinedOutput(); err != nil {
		t.Fatalf("rewriting the config from inside the container: %v: %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(clone, ".devcontainer", "devcontainer.json")); string(b) != hostile {
		t.Fatalf("setup: the container's write did not reach the clone: %q", b)
	}
	if status, body := c.post("/api/workspaces/"+id+"/rebuild", ""); status != 202 {
		t.Fatalf("rebuild of the rewritten config: %d %s", status, body)
	}
	type setting struct{ Field, Source string }
	type pending struct {
		State    string
		Steps    map[string]struct{ Status, Detail string }
		Approval *struct {
			Hash                    string
			Added, Changed, Removed []setting
		}
	}
	awaitApproval := func(what string, noCanary bool) pending {
		t.Helper()
		deadline := time.Now().Add(5 * time.Minute)
		for {
			var v pending
			_, body, _ := c.do("GET", "/api/workspaces/"+id, "")
			json.Unmarshal([]byte(body), &v)
			if v.State == "failed" || v.State == "running" || (v.State == "stopped" && v.Approval != nil) {
				if _, err := os.Lstat(canary); noCanary && err == nil {
					t.Fatalf("%s: initializeCommand ran on the host before an approval (%s)", what, v.State)
				}
				if v.State != "stopped" || v.Approval == nil || v.Steps["resolve_config"].Status != "needs_approval" {
					t.Fatalf("%s: %s, resolve_config %+v; want stopped, waiting for an approval", what, v.State, v.Steps["resolve_config"])
				}
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: never stopped for an approval: %s", what, v.State)
			}
			time.Sleep(time.Second)
		}
	}
	asked := awaitApproval("the rewritten config", true)
	if _, err := os.Lstat(canary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initializeCommand ran on the host before an approval: %v", err)
	}
	if a := asked.Approval; len(a.Added) != 1 || a.Added[0] != (setting{"initializeCommand", "repository"}) ||
		!strings.Contains(asked.Steps["resolve_config"].Detail, "initializeCommand") {
		t.Errorf("the request: %+v, %q", a, asked.Steps["resolve_config"].Detail)
	}
	if got := labelled(id); len(got) != 1 || got[0] != first || inspectRunning(first) != "false" {
		t.Errorf("while waiting, docker lists %v (running=%s); want %s, stopped", got, inspectRunning(first), first)
	}
	// A stale hash is refused and changes nothing.
	if status, body := c.post("/api/workspaces/"+id+"/config-approval", `{"hash":"sha256:`+strings.Repeat("0", 64)+`"}`); status != 409 || !strings.Contains(body, "approval_stale") {
		t.Errorf("a stale approval: %d %s", status, body)
	}
	// Approved: the rebuild runs, initializeCommand with it.
	if status, body := c.post("/api/workspaces/"+id+"/config-approval", `{"hash":"`+asked.Approval.Hash+`"}`); status != 202 {
		t.Fatalf("approve: %d %s", status, body)
	}
	v = await(id, "running")
	if _, err := os.Lstat(canary); err != nil {
		t.Errorf("the approved initializeCommand did not run: %v", err)
	}
	// The committed file back: less than was approved — runArgs alone, a
	// subset of what was — so it runs without asking, and the approval
	// stays as it was (design §6).
	if err := os.WriteFile(filepath.Join(clone, ".devcontainer", "devcontainer.json"), []byte(hostNetConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(canary); err != nil {
		t.Fatal(err)
	}
	if status, body := c.post("/api/workspaces/"+id+"/rebuild", ""); status != 202 {
		t.Fatalf("rebuild with the file put back: %d %s", status, body)
	}
	v = await(id, "running")
	if _, err := os.Lstat(canary); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("initializeCommand ran with the file put back: %v", err)
	}
	first = *v.ContainerID

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

	// The images up built for this workspace, by the names the CLI gives
	// them. Control: up built at least one (the Feature's), so "none after
	// the delete" below is not vacuous.
	built, err := container.BuiltImages(clone)
	if err != nil {
		t.Fatal(err)
	}
	images := func() []string {
		args := []string{"image", "ls", "--format", "{{.Repository}}"}
		for _, n := range built {
			args = append(args, "--filter", "reference="+n)
		}
		return strings.Fields(docker(t, args...))
	}
	if got := images(); len(got) == 0 {
		t.Fatalf("control: docker lists none of %q before the delete: the CLI's names have moved", built)
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
	if got := images(); len(got) != 0 {
		t.Errorf("after delete, the images up built for it are still there: %v", got)
	}
	if got := docker(t, "image", "ls", "-q", provision.DefaultImage); got == "" {
		t.Errorf("the delete took the base image %s, which is not the workspace's own", provision.DefaultImage)
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
