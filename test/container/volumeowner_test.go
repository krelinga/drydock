package container_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/secrets"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/sys"
)

// TestTheFirstWorkspaceDoesNotDecideTheVolumesOwner is #38's review finding,
// end to end: the real server, the real devcontainer CLI, Drydock's Feature
// from this checkout, and a fresh shared credential volume.
//
// The first workspace created is one whose remote user is not vscode — dev,
// made at uid 1500 in its image, so not Drydock's uid wherever this runs —
// and whose uid the CLI updates to Drydock's, as it does by default. The
// Feature makes /home/vscode/.claude in that image as dev's *build-time* uid,
// and the CLI's update re-owns only /home/dev. Docker copies an image
// directory's owner into a volume mounted while empty, so before the fix the
// volume became uid 1500's and this workspace failed the Feature's preflight
// ("belongs to uid 1500, but the remote user dev is uid 1000", measured on
// the code before it) — every time, since the failed workspace leaves the
// volume empty for the next mount to copy into again — and a login started
// meanwhile was refused for the same owner.
//
// Now step 4 gives the empty volume to Drydock's uid and leaves a marker in
// it before any workspace mounts it, so:
//
//   - the dev workspace runs, as dev, at Drydock's uid, and the volume is
//     Drydock's uid with the marker in it;
//   - a vscode workspace created after it runs too;
//   - a dev workspace with the uid update turned off — whose remote user
//     really is not Drydock's uid — is refused by the Feature's preflight,
//     alone, and the volume's owner is unchanged: one repository that cannot
//     share the login does not take the others with it.
func TestTheFirstWorkspaceDoesNotDecideTheVolumesOwner(t *testing.T) {
	needDevcontainer(t)
	if os.Getuid() == 1500 {
		t.Skip("this test's remote user is uid 1500, which is this process's")
	}
	if out, err := exec.Command("docker", "pull", "--quiet", provision.DefaultImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v: %s", provision.DefaultImage, err, out)
	}
	if out, err := exec.Command("docker", "pull", "--quiet", config.DefaultCleanupImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull: %v: %s", err, out)
	}
	p := prefix(t)
	vol := claudeVolume(p)
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })

	f := githubtest.New(t, 4242, time.Now)
	// A bare Debian, not the devcontainers base: there uid 1000 is vscode's,
	// and the CLI does not update a remote user to a uid another user has —
	// so on a host where Drydock is uid 1000 the update would silently not
	// happen. Here nothing holds Drydock's uid, as on a real deployment,
	// where Drydock is a system user.
	dockerfile := "FROM " + image + "\nRUN useradd --create-home --uid 1500 --shell /bin/bash dev\n"
	devRepo := func(id int64, name string, updateUID bool) githubtest.Repo {
		cfg := `{"build":{"dockerfile":"Dockerfile"},"remoteUser":"dev","runArgs":["--network=host"]`
		if !updateUID {
			cfg += `,"updateRemoteUserUID":false`
		}
		cfg += `}`
		return githubtest.Repo{ID: id, FullName: name, DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{"README.md", ".devcontainer/devcontainer.json", ".devcontainer/Dockerfile"},
			Contents: map[string]string{".devcontainer/devcontainer.json": cfg,
				".devcontainer/Dockerfile": dockerfile}}
	}
	hostNetConfig := `{"image":"` + provision.DefaultImage + `","runArgs":["--network=host"]}`
	reg := newFeatureRegistry(t, "1.0.0")
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		devRepo(201, "krelinga/dev", true),
		{ID: 202, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files:    []string{"README.md", ".devcontainer/devcontainer.json"},
			Contents: map[string]string{".devcontainer/devcontainer.json": hostNetConfig}},
		devRepo(203, "krelinga/dev-fixed-uid", false),
	}}}
	f.EnableGit(t)

	dir, err := os.MkdirTemp("", "ddo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	u, _ := user.Current()
	g, _ := user.LookupGroupId(u.Gid)
	key := filepath.Join(dir, "app.pem")
	os.WriteFile(key, githubtest.KeyPEM(t), 0o400)
	secretsKey := filepath.Join(dir, "secrets.key")
	masterKey := make([]byte, secrets.KeySize)
	rand.Read(masterKey)
	os.WriteFile(secretsKey, masterKey, 0o400)
	cfg := config.Default()
	cfg.SecretsKey = secretsKey
	cfg.UIOrigin, cfg.UIHost = "https://drydock.test", "drydock.test"
	cfg.DatabasePath = filepath.Join(dir, "drydock.db")
	cfg.APISocket, cfg.PreviewSocket = filepath.Join(dir, "http.sock"), filepath.Join(dir, "preview.sock")
	cfg.BrokerDir, cfg.WorkspaceRoot = filepath.Join(dir, "sock"), filepath.Join(dir, "ws")
	cfg.SocketGroup, cfg.LabelPrefix = g.Name, p
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 4242, key, f.URL
	cfg.BotName, cfg.BotEmail = "krelinga-drydock-dev[bot]", botEmail
	cfg.Feature = reg.Drydock
	cfg.ClaudeVolume = vol

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
		if len(repos.Repos) == 3 {
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
	}
	// create makes one workspace and waits for it to settle: one at a
	// time, so the first mount is the one the test says it is.
	create := func(repo string) (string, view) {
		t.Helper()
		status, body := c.post("/api/workspaces", `{"repository_id":`+repo+`}`)
		var created struct{ ID string }
		if status != 202 || json.Unmarshal([]byte(body), &created) != nil || created.ID == "" {
			t.Fatalf("create %s: %d %s", repo, status, body)
		}
		deadline := time.Now().Add(15 * time.Minute)
		for {
			var v view
			c.get("/api/workspaces/"+created.ID, &v)
			if v.State == "running" || v.State == "failed" {
				return created.ID, v
			}
			if time.Now().After(deadline) {
				t.Fatalf("repository %s never settled: %+v", repo, v)
			}
			time.Sleep(time.Second)
		}
	}
	// owner is the volume's owner and what is in it, as Docker sees them.
	owner := func() string {
		t.Helper()
		return docker(t, "run", "--rm", "--network", "none", "--mount", "type=volume,source="+vol+",target=/v,readonly",
			"--entrypoint", "sh", config.DefaultCleanupImage, "-c", "stat -c '%u %a' /v; ls -A /v | grep -x .drydock-volume; true")
	}
	want := strconv.Itoa(os.Getuid()) + " 700\n.drydock-volume"
	execIn := func(id, script string) (string, error) {
		out, err := exec.Command("devcontainer", "exec", "--workspace-folder", filepath.Join(cfg.WorkspaceRoot, id, "repo"),
			"--id-label", p+".workspace="+id, "--", "sh", "-c", script).Output()
		return string(out), err
	}

	devID, dev := create("201")
	if dev.State != "running" {
		t.Errorf("the first workspace, remote user dev: %s (%s); steps %+v", dev.State, deref(dev.StateDetail), dev.Steps)
	}
	if got := owner(); got != want {
		t.Errorf("after the dev workspace the volume is %q; want %q", got, want)
	}
	if out, err := execIn(devID, `id -un; id -u; stat -c %u "$CLAUDE_CONFIG_DIR"`); err != nil ||
		out != "dev\n"+strconv.Itoa(os.Getuid())+"\n"+strconv.Itoa(os.Getuid())+"\n" {
		t.Errorf("in the dev workspace: %v\n%s", err, out)
	}

	_, alpha := create("202")
	if alpha.State != "running" {
		t.Errorf("a vscode workspace after the dev one: %s (%s)", alpha.State, deref(alpha.StateDetail))
	}

	_, fixed := create("203")
	if fixed.State != "failed" || fixed.Steps["up"].Status != "failed" {
		t.Errorf("a dev workspace with the uid update off: %s; up %+v", fixed.State, fixed.Steps["up"])
	}
	if got := owner(); got != want {
		t.Errorf("after the refused workspace the volume is %q; want %q", got, want)
	}
}
