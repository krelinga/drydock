package container_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/sys"
)

// TestDeleteRemovesRootOwnedFiles: a process running as root inside the
// container — sudo, a build tool, a postCreateCommand — leaves root-owned
// files in the bind-mounted clone, which the drydock user on the host cannot
// remove. Here they are made for real, with `docker exec -u 0`: a root-owned
// file, and a root-owned 0700 directory with content. Then a delete through
// the real server.
//
//   - Precondition: the host user really cannot remove them, so the test is
//     not vacuous.
//   - Control: with the helper unavailable (no cleanup image), the delete
//     sticks in deleting, its files sub-step failed — which is what every
//     delete of such a workspace did before the helper existed.
//   - With the helper: asking again finishes it — no directory, no row, no
//     container — and the files sub-step says the helper was used.
//
// Around it, the helper's own reach. A sentinel beside the workspace, in the
// workspace root, survives: a helper that mounted a parent would have
// emptied it. A stray helper an earlier attempt left (made here by hand,
// carrying the helper label) is invisible to reconciliation's listing — with
// the workspace's container listed beside it as the control — and is removed
// by the delete that runs the helper.
func TestDeleteRemovesRootOwnedFiles(t *testing.T) {
	needDevcontainer(t)
	for _, img := range []string{provision.DefaultImage, config.DefaultCleanupImage} {
		if out, err := exec.Command("docker", "pull", "--quiet", img).CombinedOutput(); err != nil {
			t.Fatalf("docker pull %s: %v: %s", img, err, out)
		}
	}
	p := prefix(t)
	ctx := context.Background()

	f := githubtest.New(t, 4242, time.Now)
	hostNetConfig := `{"image":"` + provision.DefaultImage + `","runArgs":["--network=host"]}`
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 101, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files:    []string{"README.md", ".devcontainer/devcontainer.json"},
			Contents: map[string]string{".devcontainer/devcontainer.json": hostNetConfig}},
	}}}
	f.EnableGit(t)

	dir, err := os.MkdirTemp("", "ddr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// What root made, if the test stopped before the delete removed it.
		exec.Command("docker", "run", "--rm", "--network", "none", "--mount", "type=bind,source="+dir+",target=/d",
			config.DefaultCleanupImage, "find", "/d", "-mindepth", "1", "-delete").Run()
		os.RemoveAll(dir)
	})
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

	srv, err := server.New(ctx, cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	srv.Provisioner.Config = []byte(hostNetConfig)
	// The runArgs the test needs (above) is host access: approved, as the
	// operator would, before anything runs (design §6).
	approveHostNetwork(t, srv.DB.DB, 101)
	srv.Provisioner.RemoteEnv = map[string]string{"DRYDOCK_GITHUB_HOST": strings.TrimPrefix(f.URL, "http://")}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx) }()
	t.Cleanup(func() { cancel(); <-done })

	c := &client{t: t, sock: cfg.APISocket}
	if err := srv.Auth.SetPassword(ctx, "correct horse battery staple"); err != nil {
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
	// files is the files sub-step's last outcome: "status: detail".
	files := func(id string) string {
		evs, err := srv.Events.ForWorkspace(ctx, id, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs { // newest first
			var d struct{ Step, Status, Detail string }
			json.Unmarshal(ev.Data, &d)
			if ev.Kind == provision.KindAction && d.Step == provision.SubFiles && d.Status != "started" {
				return d.Status + ": " + d.Detail
			}
		}
		return ""
	}
	del := func(id string) {
		t.Helper()
		if status, body, _ := c.do("DELETE", "/api/workspaces/"+id+"?confirm="+url.QueryEscape("krelinga/alpha"), ""); status != 202 {
			t.Fatalf("delete: %d %s", status, body)
		}
	}

	status, body := c.post("/api/workspaces", `{"repository_id":101}`)
	var created struct{ ID string }
	if status != 202 || json.Unmarshal([]byte(body), &created) != nil {
		t.Fatalf("create: %d %s", status, body)
	}
	id := created.ID
	var v view
	for deadline := time.Now().Add(15 * time.Minute); ; {
		var ok bool
		if v, ok = read(id); ok && v.State == "running" {
			break
		}
		if v.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("workspace never reached running: %s (%s)", v.State, deref(v.StateDetail))
		}
		time.Sleep(time.Second)
	}
	cid := *v.ContainerID
	clone := filepath.Join(cfg.WorkspaceRoot, id, "repo")
	folder := docker(t, "inspect", "--format",
		`{{range .Mounts}}{{if eq .Source "`+clone+`"}}{{.Destination}}{{end}}{{end}}`, cid)
	if folder == "" {
		t.Fatalf("the clone %s is not mounted in %s", clone, cid)
	}
	docker(t, "exec", "-u", "0", cid, "sh", "-c",
		`mkdir -p "$1/target/root-dir/deep" && echo built > "$1/target/root-dir/deep/artifact" &&
		 chmod 0700 "$1/target/root-dir" && echo cache > "$1/root-file"`, "sh", folder)

	// Precondition: the host user cannot remove what root made.
	if err := os.Remove(filepath.Join(clone, "target", "root-dir", "deep", "artifact")); err == nil {
		t.Fatal("precondition: the host user removed a file root made; the test would be vacuous")
	}
	if fi, err := os.Stat(filepath.Join(clone, "root-file")); err != nil {
		t.Fatalf("precondition: %v", err)
	} else if owner := fi.Sys().(*syscall.Stat_t).Uid; owner != 0 {
		t.Fatalf("precondition: root-file is owned by %d, not root", owner)
	}

	// A sentinel beside the workspace, and a stray helper.
	sentinel := filepath.Join(cfg.WorkspaceRoot, "sentinel")
	os.WriteFile(sentinel, []byte("keep"), 0o600)
	stray := docker(t, "run", "-d", "--label", p+".cleanup="+id, config.DefaultCleanupImage, "sleep", "600")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", stray).Run() })
	found, err := manager(p).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, fd := range found {
		listed[fd.ContainerID] = true
	}
	if !listed[cid] {
		t.Errorf("control: reconciliation's listing misses the workspace's container %s: %+v", cid, found)
	}
	if listed[stray] {
		t.Errorf("reconciliation's listing includes the cleanup helper %s", stray)
	}

	// Control: no helper, and the delete sticks naming files.
	image := srv.Provisioner.Containers.CleanupImage
	srv.Provisioner.Containers.CleanupImage = ""
	del(id)
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if strings.HasPrefix(files(id), "failed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: the delete never failed at files; files %q", files(id))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v, ok := read(id); !ok || v.State != "deleting" {
		t.Fatalf("control: after a failed files step: %+v %v", v, ok)
	}
	if _, err := os.Stat(filepath.Join(clone, "target", "root-dir")); err != nil {
		t.Errorf("control: root's directory went without the helper: %v", err)
	}

	// With the helper: asking again finishes it.
	srv.Provisioner.Containers.CleanupImage = image
	del(id)
	for deadline := time.Now().Add(5 * time.Minute); ; {
		if _, ok := read(id); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the delete never finished; files %q", files(id))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d := files(id); !strings.HasPrefix(d, "done") || !strings.Contains(d, "helper container") {
		t.Errorf("files %q: the event must say the helper was used", d)
	}
	if _, err := os.Lstat(filepath.Join(cfg.WorkspaceRoot, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace directory survived: %v", err)
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep" {
		t.Errorf("the sentinel beside the workspace went: %v", err)
	}
	if got := strings.Fields(docker(t, "ps", "-aq", "--no-trunc", "--filter", "label="+p+".workspace="+id)); len(got) != 0 {
		t.Errorf("containers left: %v", got)
	}
	if got := strings.Fields(docker(t, "ps", "-aq", "--no-trunc", "--filter", "label="+p+".cleanup="+id)); len(got) != 0 {
		t.Errorf("cleanup helpers left, the stray included: %v", got)
	}
}
