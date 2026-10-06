package container_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/secrets"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/sys"
)

// TestCreateWorkspaceThroughTheServer is Phase 2's deliverable in the
// container tier (design §14: "the button produces a running container you
// can devcontainer exec into"): POST /api/workspaces through the real server,
// over its Unix socket and signed in, against the fake GitHub and its git
// remote, with the real devcontainer CLI and Drydock's Feature from this
// checkout, served by a local registry. Two repositories at once — one with
// a devcontainer.json, one without — and both reach running, with the
// container found again by its label.
//
// Three more exercise the repository's devcontainer-lock.json (design §6),
// with a Feature from the same registry whose versions are told apart inside
// the container: one commits a lockfile pinning the older version and gets
// it; the control, the same config with no lockfile, gets the newer. The
// pinned one's lockfile is in sync, as VS Code writes it, and `up` leaves it
// byte for byte — Drydock's Feature has no dependsOn to write into it — so
// those clones are left as cloned. The third commits a stale lockfile, which
// `up` rewrites as VS Code would; Drydock leaves the rewrite and says so.
//
// Two test-only settings, both because the fake GitHub listens on the
// host's loopback: the containers run with --network=host (in the repository's
// config and in the minimal one), and DRYDOCK_GITHUB_HOST is set through the
// remote env. Neither changes what is under test.
func TestCreateWorkspaceThroughTheServer(t *testing.T) {
	needDevcontainer(t)
	if out, err := exec.Command("docker", "pull", "--quiet", provision.DefaultImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v: %s", provision.DefaultImage, err, out)
	}
	p := prefix(t)

	f := githubtest.New(t, 4242, time.Now)
	hostNetConfig := `{"image":"` + provision.DefaultImage + `","runArgs":["--network=host"]}`
	reg := newFeatureRegistry(t, "1.0.0", "1.1.0")
	markerConfig := `{"image":"` + provision.DefaultImage + `","runArgs":["--network=host"],"features":{"` + reg.Ref + `":{}}}`
	locked := func(id int64, name, lock string) githubtest.Repo {
		r := githubtest.Repo{ID: id, FullName: name, DefaultBranch: "main", PushedAt: time.Now(),
			Files:    []string{"README.md", ".devcontainer/devcontainer.json"},
			Contents: map[string]string{".devcontainer/devcontainer.json": markerConfig}}
		if lock != "" {
			r.Files = append(r.Files, ".devcontainer/devcontainer-lock.json")
			r.Contents[".devcontainer/devcontainer-lock.json"] = lock
		}
		return r
	}
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 101, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files:    []string{"README.md", ".devcontainer/devcontainer.json"},
			Contents: map[string]string{".devcontainer/devcontainer.json": hostNetConfig}},
		{ID: 102, FullName: "krelinga/plain", DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{"README.md"}},
		locked(103, "krelinga/pinned", reg.lockfile("1.0.0")),
		locked(104, "krelinga/unpinned", ""),
		// Stale: written before the configuration declared the marker.
		locked(105, "krelinga/stale", "{\n  \"features\": {}\n}\n"),
	}}}
	f.EnableGit(t)

	// Short paths: a Unix socket's path is limited to 108 bytes.
	dir, err := os.MkdirTemp("", "ddp")
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
	cfg.ClaudeVolume = claudeVolume(p)

	srv, err := server.New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	srv.Provisioner.Config = []byte(hostNetConfig)
	srv.Provisioner.RemoteEnv = map[string]string{"DRYDOCK_GITHUB_HOST": strings.TrimPrefix(f.URL, "http://")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	c := &client{t: t, sock: cfg.APISocket}
	if err := srv.Auth.SetPassword(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	c.cookie = c.signIn("correct horse battery staple")

	// The catalog fills at boot.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var repos struct{ Repos []struct{ ID int64 } }
		c.get("/api/repos", &repos)
		if len(repos.Repos) == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog never filled")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A secret granted to the configured repository alone, so a provisioned
	// container's broker socket answers GET-SECRETS as well as GET-TOKEN.
	canary := fmt.Sprintf("Pv%x", time.Now().UnixNano())
	if _, err := srv.Secrets.Put(context.Background(), "PROVISION_CANARY", canary, "nothing; a test value", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Secrets.SetGrants(context.Background(), "PROVISION_CANARY", []int64{101}, false); err != nil {
		t.Fatal(err)
	}

	ids := map[string]string{}
	for _, repo := range []string{"101", "102", "103", "104", "105"} {
		status, body := c.post("/api/workspaces", `{"repository_id":`+repo+`}`)
		var created struct{ ID string }
		if status != 202 || json.Unmarshal([]byte(body), &created) != nil || created.ID == "" {
			t.Fatalf("create %s: %d %s", repo, status, body)
		}
		ids[repo] = created.ID
	}

	type view struct {
		State       string
		StateDetail *string `json:"state_detail"`
		ContainerID *string `json:"container_id"`
		Steps       map[string]struct{ Status, Detail string }
	}
	views := map[string]view{}
	deadline = time.Now().Add(15 * time.Minute)
	for len(views) < len(ids) {
		if time.Now().After(deadline) {
			t.Fatalf("not all settled in time: %+v", views)
		}
		time.Sleep(time.Second)
		for repo, id := range ids {
			var v view
			c.get("/api/workspaces/"+id, &v)
			if v.State == "running" || v.State == "failed" {
				views[repo] = v
			}
		}
	}
	for repo, v := range views {
		if v.State != "running" {
			t.Errorf("repository %s: %s (%s); steps %+v", repo, v.State, deref(v.StateDetail), v.Steps)
			continue
		}
		// Docker is the truth: the container the row names is the one
		// carrying this workspace's label, and it is running.
		found := docker(t, "ps", "-q", "--no-trunc", "--filter", "label="+p+".workspace="+ids[repo])
		if v.ContainerID == nil || found != *v.ContainerID {
			t.Errorf("repository %s: row says %v, docker says %q", repo, v.ContainerID, found)
		}
	}
	if t.Failed() {
		return
	}
	if d := views["102"].Steps["resolve_config"].Detail; !strings.Contains(d, "minimal configuration") {
		t.Errorf("the plain repository's resolve_config says %q", d)
	}
	if d := views["101"].Steps["resolve_config"].Detail; d != "" {
		t.Errorf("the configured repository's resolve_config says %q", d)
	}

	// Usable: devcontainer exec into each, as the remote user, in the clone.
	execIn := func(repo, script string) (string, error) {
		id := ids[repo]
		args := []string{"exec", "--workspace-folder", filepath.Join(cfg.WorkspaceRoot, id, "repo"),
			"--id-label", p + ".workspace=" + id}
		if repo == "102" {
			args = append(args, "--override-config", filepath.Join(cfg.WorkspaceRoot, id, ".drydock", "devcontainer.json"))
		}
		out, err := exec.Command("devcontainer", append(args, "--", "sh", "-c", script)...).Output()
		return string(out), err
	}
	for repo := range views {
		out, err := execIn(repo, "whoami; git log -1 --format=%s; git config user.email")
		if err != nil || !strings.Contains(out, "vscode\nfixture\n"+botEmail) {
			t.Errorf("repository %s: exec %v:\n%s", repo, err, out)
		}
	}
	// The Feature installed is this checkout's, which records where gh came
	// from (the published one before 0.3.0 does not), and gh is there.
	if out, err := execIn("101", "grep GH_INSTALLED_BY= /usr/local/drydock/etc/feature.env && /usr/local/drydock/real/gh --version 2>/dev/null || /usr/bin/gh --version"); err != nil || !strings.Contains(out, "gh version") {
		t.Errorf("the workspace's Feature: exec %v:\n%s", err, out)
	}

	// Claude Code (design §7.1, §11). In every workspace: the pinned version
	// is the claude found, a login shell included; CLAUDE_CONFIG_DIR is the
	// one mount at the Feature's path; and postCreate wrote the two keys
	// whose absence hangs a headless remote-control, for the workspace
	// folder (step 7's, /workspaces/repo, which every one of these has).
	wantClaude := classify.ClaudeCodeVersion + " (Claude Code)\n" + classify.ClaudeCodeVersion + " (Claude Code)\n" +
		"1\n/home/vscode/.claude\n1\n[true,true]\n"
	for repo := range views {
		out, err := execIn(repo, `claude --version; bash -lc 'claude --version'; echo "$DISABLE_AUTOUPDATER"; echo "$CLAUDE_CONFIG_DIR"
awk '$5 == "/home/vscode/.claude"' /proc/self/mountinfo | wc -l
jq -c '[.remoteDialogSeen, .projects["/workspaces/repo"].hasTrustDialogAccepted]' "$CLAUDE_CONFIG_DIR/.claude.json"`)
		if err != nil || out != wantClaude {
			t.Errorf("repository %s: Claude Code in the container: exec %v:\n%s\nwant:\n%s", repo, err, out, wantClaude)
		}
	}
	// And it is one volume, the configured one, in all of them — Docker's
	// answer, not the container's — and local and labelled as step 4 made
	// it. The sharing itself, seen from inside: what one workspace writes
	// there, another reads.
	for repo, id := range ids {
		cid := docker(t, "ps", "-q", "--no-trunc", "--filter", "label="+p+".workspace="+id)
		if got := docker(t, "inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/home/vscode/.claude"}}{{.Type}} {{.Name}}{{end}}{{end}}`, cid); got != "volume "+claudeVolume(p) {
			t.Errorf("repository %s: /home/vscode/.claude is %q", repo, got)
		}
	}
	if got := docker(t, "volume", "inspect", "-f", `{{.Driver}} {{index .Labels "`+p+`.claude-config"}} {{len .Options}}`, claudeVolume(p)); got != "local true 0" {
		t.Errorf("the shared volume: %q", got)
	}
	shared := fmt.Sprintf("shared-%x", time.Now().UnixNano())
	if _, err := execIn("101", `echo `+shared+` > "$CLAUDE_CONFIG_DIR/drydock-test-shared"`); err != nil {
		t.Fatal(err)
	}
	if out, err := execIn("102", `cat "$CLAUDE_CONFIG_DIR/drydock-test-shared"`); err != nil || strings.TrimSpace(out) != shared {
		t.Errorf("what one workspace wrote to the shared volume, another read as %q (%v)", out, err)
	}

	// Secrets reach a provisioned container the way Claude Code would run a
	// command: the Feature's CLAUDE_ENV_FILE, then the command. The granted
	// repository's container gets the value; the other's prelude succeeds,
	// silently, with nothing — its socket is its own, and the grant is not.
	prelude := `eval "$(cat "$CLAUDE_ENV_FILE")" && printf '[%s]' "${PROVISION_CANARY-unset}"`
	if out, err := execIn("101", prelude); err != nil || out != "["+canary+"]" {
		t.Errorf("the granted workspace: exec %v, got %q", err, out)
	}
	if out, err := execIn("102", prelude); err != nil || out != "[unset]" {
		t.Errorf("the ungranted workspace: exec %v, got %q", err, out)
	}

	// The committed lockfile was honoured: the pinned version is installed,
	// where the same configuration without a lockfile gets the newest. The
	// control is what makes the pin's assertion mean something — a registry
	// whose major tag served 1.0.0 would pass it vacuously.
	for repo, want := range map[string]string{"103": "1.0.0", "104": "1.1.0", "105": "1.1.0"} {
		if out, err := execIn(repo, "cat "+markerPath); err != nil || strings.TrimSpace(out) != want {
			t.Errorf("repository %s: the Feature installed %q (%v); want %s", repo, strings.TrimSpace(out), err, want)
		}
	}
	if d := views["103"].Steps["resolve_config"].Detail; !strings.Contains(d, "lockfile") {
		t.Errorf("the pinned repository's resolve_config says %q", d)
	}

	// No clone was touched but the stale one's. For the plain repository the
	// minimal config is beside the clone, never in it; where no lockfile is
	// committed `up` created none (it does with no lockfile flag, measured);
	// and an in-sync lockfile is byte for byte as cloned, with nothing put
	// back — Drydock no longer puts anything back. The stale lockfile is the
	// control: the same comparison does see a rewrite, which `up` left and
	// the up step names.
	for repo, id := range ids {
		clone := filepath.Join(cfg.WorkspaceRoot, id, "repo")
		want := ""
		if repo == "105" {
			want = " M .devcontainer/devcontainer-lock.json\n"
		}
		if out, err := exec.Command("git", "-C", clone, "status", "--porcelain", "--ignored").CombinedOutput(); err != nil || string(out) != want {
			t.Errorf("repository %s: the clone's status is %q (%v); want %q", repo, out, err, want)
		}
		d := views[repo].Steps["up"].Detail
		if (repo == "105") != strings.Contains(d, `rewrote ".devcontainer/devcontainer-lock.json"`) {
			t.Errorf("repository %s: the up step says %q", repo, d)
		}
	}
	if b, err := os.ReadFile(filepath.Join(cfg.WorkspaceRoot, ids["105"], "repo", ".devcontainer", "devcontainer-lock.json")); err != nil || !strings.Contains(string(b), reg.Ref) {
		t.Errorf("the stale lockfile was rewritten to %q (%v); want an entry for %s", b, err, reg.Ref)
	}

	// The canary sweep (testing §4.2): no installation token in the
	// workspace tree, its .git/config, or the database's raw bytes — the
	// event log lives there. The control is that the same sweep finds the
	// marker it should: the README in the clone, and a workspace id in the
	// database.
	tokens := f.IssuedTokens()
	if len(tokens) == 0 {
		t.Fatal("no token was issued, so the sweep would prove nothing")
	}
	var corpus bytes.Buffer
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(path)
			corpus.Write(b)
		}
		return nil
	})
	for _, marker := range []string{"README.md\n", ids["101"]} {
		if !bytes.Contains(corpus.Bytes(), []byte(marker)) {
			t.Fatalf("the sweep missed the marker %q", marker)
		}
	}
	for _, tok := range tokens {
		if bytes.Contains(corpus.Bytes(), []byte(tok)) {
			t.Errorf("an installation token survived in the workspace tree or the database")
		}
	}
	if bytes.Contains(corpus.Bytes(), []byte(canary)) {
		t.Errorf("the secret's value is in plain text in the workspace tree or the database")
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// client is a signed-in browser, minus the browser: requests over the API
// socket with the Host, Origin and forwarded address Caddy would send.
type client struct {
	t      *testing.T
	sock   string
	cookie string
}

func (c *client) do(method, path, body string) (int, string, *http.Response) {
	c.t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", c.sock)
	}}}
	req, _ := http.NewRequest(method, "http://drydock.test"+path, strings.NewReader(body))
	req.Header.Set("Origin", "https://drydock.test")
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: c.cookie})
	}
	resp, err := hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func (c *client) signIn(password string) string {
	c.t.Helper()
	status, body, resp := c.do("POST", "/api/auth/session", fmt.Sprintf(`{"password":%q}`, password))
	if status != 204 {
		c.t.Fatalf("sign-in: %d %s", status, body)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "__Host-drydock" {
			return ck.Value
		}
	}
	c.t.Fatal("no session cookie")
	return ""
}

func (c *client) get(path string, v any) {
	c.t.Helper()
	status, body, _ := c.do("GET", path, "")
	if status != 200 || json.Unmarshal([]byte(body), v) != nil {
		c.t.Fatalf("GET %s: %d %s", path, status, body)
	}
}

func (c *client) post(path, body string) (int, string) {
	c.t.Helper()
	status, b, _ := c.do("POST", path, body)
	return status, b
}
