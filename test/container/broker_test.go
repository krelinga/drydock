package container_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/broker"
	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const botEmail = "337849865+krelinga-drydock-dev[bot]@users.noreply.github.com"

// needDevcontainer is needDocker plus the devcontainer CLI.
func needDevcontainer(t *testing.T) {
	t.Helper()
	needDocker(t)
	if _, err := exec.LookPath("devcontainer"); err != nil {
		if os.Getenv("DRYDOCK_REQUIRE_DOCKER") != "" {
			t.Fatalf("the devcontainer CLI is missing and DRYDOCK_REQUIRE_DOCKER is set")
		}
		t.Skip("the devcontainer CLI is not installed")
	}
}

// TestWorkspaceContainerReachesOnlyItsOwnRepository is Phase 3's deliverable
// in the container tier (design §14, testing §8.3): a real `devcontainer up`
// with the Feature from this checkout and the workspace's broker socket
// bind-mounted, then git and gh inside the container. It pushes a drydock/
// branch to its own repository; it cannot push outside the prefix, cannot
// read the other repository, and has no socket but its own and no Docker
// socket at all.
func TestWorkspaceContainerReachesOnlyItsOwnRepository(t *testing.T) {
	needDevcontainer(t)
	ctx := context.Background()
	p := prefix(t)

	// GitHub, faked, with its git remote; two repositories, one ours.
	f := githubtest.New(t, 4242, time.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 101, FullName: "krelinga/alpha", DefaultBranch: "main", Files: []string{"README.md"}},
		{ID: 202, FullName: "krelinga/beta", DefaultBranch: "main", Files: []string{"README.md"}},
	}}}
	f.EnableGit(t)
	host := strings.TrimPrefix(f.URL, "http://")

	// The broker, with a workspace bound to alpha.
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ws, _ := workspace.NewID(time.Now(), rand.Reader)
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (101, 77, 'krelinga/alpha', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + ws + `', 101, '/x', 'main', 'building')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := github.ParseKey(githubtest.KeyPEM(t))
	sockDir, _ := os.MkdirTemp("", "dd")
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	b := &broker.Broker{Dir: filepath.Join(sockDir, "sock"), DB: db.DB, Events: events.New(db.DB, sys.RealClock{}),
		Env:    sys.Env{Clock: sys.RealClock{}, Random: sys.CryptoRandom{}},
		GitHub: &github.Client{AppID: 4242, Key: key, BaseURL: f.URL, Clock: sys.RealClock{}}}
	if err := b.Open(ctx, ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.CloseAll)

	// The workspace folder: a devcontainer.json naming the Feature from this
	// checkout, on the host network so the container reaches the fake.
	folder := t.TempDir()
	devc := filepath.Join(folder, ".devcontainer")
	os.MkdirAll(devc, 0o755)
	if out, err := exec.Command("cp", "-r", filepath.Join("..", "..", "feature", "src", "drydock"), filepath.Join(devc, "drydock")).CombinedOutput(); err != nil {
		t.Fatalf("copying the Feature: %v: %s", err, out)
	}
	os.WriteFile(filepath.Join(devc, "devcontainer.json"), []byte(`{
  "image": "mcr.microsoft.com/devcontainers/base:debian",
  "features": {"./drydock": {"botName": "krelinga-drydock-dev[bot]", "botEmail": "`+botEmail+`"}},
  "runArgs": ["--network=host"]
}`), 0o644)

	m := container.Manager{Run: subproc.Exec{}, LabelPrefix: p}
	// The helper answers only for GitHub's host; the fake is elsewhere. Given
	// to exec as well as up: up's --remote-env does not persist (see Exec).
	remoteEnv := map[string]string{"DRYDOCK_GITHUB_HOST": host}
	upCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res, stderr, err := m.Up(upCtx, container.UpSpec{
		WorkspaceID: ws, RepositoryID: 101, FullName: "krelinga/alpha", Branch: "main", Folder: folder,
		BrokerSocket: b.SocketPath(ws),
		RemoteEnv:    remoteEnv,
	})
	if err != nil || res.Outcome != classify.ContainerRunning {
		t.Fatalf("devcontainer up: %+v %v\n%s", res, err, tail(stderr))
	}

	run := func(script string) (string, int) {
		t.Helper()
		var out bytes.Buffer
		// A login shell, as the supervisor's command is (§10.3): it is what
		// drops containerEnv's PATH and so what the /usr/local/bin links fix.
		r, err := m.Exec(ctx, ws, folder, remoteEnv, []string{"bash", "-lc", script}, &out, &out)
		if err != nil {
			t.Fatal(err)
		}
		if r.Err != nil {
			t.Fatalf("exec: %v", r.Err)
		}
		return out.String(), r.ExitCode
	}
	alpha := fmt.Sprintf("http://%s/krelinga/alpha.git", host)
	beta := fmt.Sprintf("http://%s/krelinga/beta.git", host)

	if out, code := run("drydock-probe"); code != 0 {
		t.Fatalf("the broker probe failed inside the container:\n%s", out)
	}
	// The Feature points git's helper at GitHub's URL alone, so a repository
	// naming another remote never gets the token. The fake is another
	// remote, so the test adds the same helper for it — after checking the
	// Feature's own configuration is that narrow.
	if out, code := run("git config --get-regexp '^credential\\..*helper$'"); code != 0 ||
		strings.TrimSpace(out) != "credential.https://github.com.helper /usr/local/drydock/bin/drydock-credential" {
		t.Errorf("the Feature's credential configuration (exit %d):\n%s", code, out)
	}
	if out, code := run("git config --global credential." + strings.TrimSuffix(alpha, "/krelinga/alpha.git") +
		".helper /usr/local/drydock/bin/drydock-credential"); code != 0 {
		t.Fatalf("configuring the helper for the fake: %s", out)
	}
	out, code := run("set -e; cd /tmp && git clone -q " + alpha + " a && cd a && echo e2e > e2e.txt && " +
		"git add e2e.txt && git commit -q -m e2e && git push -q origin HEAD:drydock/e2e && git log -1 --format=%ae")
	if code != 0 {
		t.Fatalf("clone, commit and push to drydock/e2e:\n%s", out)
	}
	if !strings.Contains(out, botEmail) {
		t.Errorf("the commit's author is not the bot:\n%s", out)
	}
	pushed := false
	for _, g := range f.GitAuths() {
		pushed = pushed || (g.Repo == "krelinga/alpha" && g.Service == "git-receive-pack" && g.Token != "")
	}
	if !pushed {
		t.Error("the fake saw no authenticated push to alpha")
	}

	if out, code := run("cd /tmp/a && git push origin HEAD:main"); code == 0 || !strings.Contains(out, "pushes go under refs/heads/drydock/") {
		t.Errorf("a push to main was not refused by the guard (exit %d):\n%s", code, out)
	}
	if out, code := run("git ls-remote " + beta); code == 0 {
		t.Errorf("the container read the other repository:\n%s", out)
	}
	if out, code := run("ls -A /run/drydock; test ! -e /var/run/docker.sock && ! command -v docker >/dev/null"); code != 0 || strings.TrimSpace(out) != "broker.sock" {
		t.Errorf("sockets in the container (want only its own broker socket, and no Docker socket), exit %d:\n%s", code, out)
	}
	if out, code := run("gh auth token"); code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "ghs_") {
		t.Errorf("gh did not get a token through the shim (exit %d):\n%s", code, out)
	}
}

func tail(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.Join(lines, "\n")
}
