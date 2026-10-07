package provision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/clone"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const (
	alpha = 101 // has .devcontainer/devcontainer.json
	plain = 102 // has none
	ghost = 999 // in the catalog, not in the installation: the token mint fails
	gone  = 103 // removed from the installation
)

const feature = "ghcr.io/krelinga/drydock/drydock:1"

const claudeVolume = "drydock-test-claude-config"

// stubBroker stands in for internal/broker: the socket is the broker's
// business and has its own tests; here what matters is that the step opens
// it and up mounts its path.
type stubBroker struct {
	mu      sync.Mutex
	opened  []string
	closed  []string
	removed []string
	// removeErr is what Remove returns, after recording the removal.
	removeErr error
	open      map[string]bool
	err       error
}

func (b *stubBroker) Close(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = append(b.closed, id)
	delete(b.open, id)
	return nil
}

// isOpen reports whether the workspace's socket is open now: opened
// successfully, and not closed since.
func (b *stubBroker) isOpen(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open[id]
}

func (b *stubBroker) closes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.closed...)
}

func (b *stubBroker) Open(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened = append(b.opened, id)
	if b.err == nil {
		if b.open == nil {
			b.open = map[string]bool{}
		}
		b.open[id] = true
	}
	return b.err
}
func (b *stubBroker) SocketDir(id string) string { return "/run/drydock/sock/" + id }

// Remove is Close plus the directory; removals records which.
func (b *stubBroker) Remove(id string) error {
	b.Close(id)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removed = append(b.removed, id)
	return b.removeErr
}

// fakeCLI is a fake devcontainer binary: it records each invocation's argv
// (one argument per line, then "--") and runs the body for its subcommand.
type fakeCLI struct {
	dir string
	// Bodies per subcommand. Defaults answer as the real CLI does on the
	// happy path, from the recorded fixtures.
	readConfig, up, exec string
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newFakeCLI(t *testing.T, origin string) *fakeCLI {
	return &fakeCLI{
		dir: t.TempDir(),
		// The recording's folder was rewritten to /srv/drydock/ws/FIXTURE/repo;
		// the real CLI names the folder it was given ($3), so the fake does.
		readConfig: "sed \"s#/srv/drydock/ws/FIXTURE/repo#$3#g\" <<'EOF'\n" + fixture(t, "read-configuration-merged-ok.json") + "\nEOF\n",
		up:         "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\necho 'a log line' >&2\n",
		// The probe answers; git prints the origin the clone left.
		exec: `case " $* " in
*" drydock-probe "*) exit 0 ;;
*" claude --version "*) echo '2.1.289 (Claude Code)' ;;
*" remote -v "*) printf 'origin\t%s (fetch)\norigin\t%s (push)\n' "` + origin + `" "` + origin + `" ;;
*) exit 9 ;;
esac`,
	}
}

// claudeAnswers is the default exec body with `claude --version` answered by
// body instead.
func claudeAnswers(e *env, body string) string {
	return strings.Replace(e.cli.exec, "echo '2.1.289 (Claude Code)'", body, 1)
}

// seedVolume puts a volume on the fake docker before a run, as inspect
// would print it.
func seedVolume(e *env, json string) {
	os.MkdirAll(filepath.Join(e.cli.dir, "vols"), 0o700)
	os.WriteFile(filepath.Join(e.cli.dir, "vols", claudeVolume+".json"), []byte(json), 0o600)
}

func (c *fakeCLI) runner(t *testing.T) subproc.Runner {
	t.Helper()
	argv := filepath.Join(c.dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" @@ >> '" + argv + "'\ncase \"$1\" in\n" +
		"read-configuration)\n" + c.readConfig + "\n;;\n" +
		"up)\n" + c.up + "\n;;\n" +
		"exec)\n" + c.exec + "\n;;\n" +
		"*) exit 64 ;;\nesac\n"
	p := filepath.Join(c.dir, "devcontainer")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(c.dir, "docker")
	if err := os.WriteFile(d, []byte(fakeDocker(c.dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	return subproc.Exec{Resolver: subproc.FixedResolver{"devcontainer": p, "docker": d}, WaitDelay: time.Second}
}

// calls returns every recorded invocation's argv.
func (c *fakeCLI) calls(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.dir, "argv"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var cur []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if l == "@@" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, l)
	}
	return out
}

func (c *fakeCLI) callsTo(t *testing.T, sub string) [][]string {
	var out [][]string
	for _, a := range c.calls(t) {
		if len(a) > 0 && a[0] == sub {
			out = append(out, a)
		}
	}
	return out
}

type env struct {
	p      *Provisioner
	cli    *fakeCLI
	fake   *githubtest.Fake
	broker *stubBroker
	log    *events.Log
	root   string
	dbPath string
}

// newEnv builds the harness. Each setup edits the fake GitHub's repositories
// before its git remote builds them, which EnableGit does at once.
func newEnv(t *testing.T, setup ...func(*githubtest.Fake)) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "drydock.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.RealClock{}
	f := githubtest.New(t, 4242, clock.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: alpha, FullName: "krelinga/alpha", DefaultBranch: "main",
			Files: []string{"README.md", ".devcontainer/devcontainer.json"}},
		{ID: plain, FullName: "krelinga/plain", DefaultBranch: "trunk", Files: []string{"README.md"}},
	}}}
	for _, fn := range setup {
		fn(f)
	}
	f.EnableGit(t)
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (101, 77, 'krelinga/alpha', 'main')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (102, 77, 'krelinga/plain', 'trunk')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (999, 77, 'krelinga/ghost', 'main')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch, removed_at) VALUES (103, 77, 'krelinga/gone', 'main', '2026-10-04T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := github.ParseKey(githubtest.KeyPEM(t))
	log := events.New(db.DB, clock)
	root := filepath.Join(dir, "ws")
	ws := &workspace.Store{DB: db.DB, Events: log, Env: sys.Env{Clock: clock, Random: sys.CryptoRandom{}}, Root: root, Cap: 10}
	b := &stubBroker{}
	cli := newFakeCLI(t, f.URL+"/krelinga/alpha.git")
	p := &Provisioner{
		Workspaces: ws, Events: log, Broker: b,
		Cloner: &clone.Cloner{DB: db.DB, GitHub: &github.Client{AppID: 4242, Key: key, BaseURL: f.URL, Clock: clock},
			Runner: subproc.Exec{}, BaseURL: f.URL},
		Containers: container.Manager{LabelPrefix: "drydock.test.provision",
			CleanupImage: "busybox:1.37.0@sha256:" + strings.Repeat("a", 64), ClaudeUID: 998, ClaudeGID: 997},
		Feature:           feature,
		FeatureOptions:    map[string]any{"botName": "krelinga-drydock-dev[bot]", "botEmail": "1+x[bot]@users.noreply.github.com"},
		ClaudeVolume:      claudeVolume,
		ClaudeCodeVersion: classify.ClaudeCodeVersion,
		Timeout:           time.Minute,
		Logf:              t.Logf,
	}
	return &env{p: p, cli: cli, fake: f, broker: b, log: log, root: root, dbPath: dbPath}
}

// wire installs the fake CLI as it stands; call it after changing a body.
func (e *env) wire(t *testing.T) { e.p.Containers.Run = e.cli.runner(t) }

// create starts a workspace and waits for its run to end.
func (e *env) create(t *testing.T, repo int64, branch string) workspace.View {
	t.Helper()
	w, err := e.p.Create(context.Background(), repo, branch)
	if err != nil {
		t.Fatalf("Create(%d): %v", repo, err)
	}
	e.p.wg.Wait()
	return e.view(t, w.ID)
}

func (e *env) view(t *testing.T, id string) workspace.View {
	t.Helper()
	v, err := e.p.Workspaces.View(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// stepEvents is the workspace's step events as "step:status", in order.
func (e *env) stepEvents(t *testing.T, id string) []string {
	t.Helper()
	evs, err := e.log.ForWorkspace(context.Background(), id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != workspace.KindStep {
			continue
		}
		var d struct{ Step, Status string }
		json.Unmarshal(evs[i].Data, &d)
		out = append(out, d.Step+":"+d.Status)
	}
	return out
}

func flag(argv []string, name string) []string {
	var vals []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == name {
			vals = append(vals, argv[i+1])
		}
	}
	return vals
}

// TestRepositoryWithAConfigReachesRunning is the walking skeleton's happy
// path, with the CLI faked so the argv Drydock builds is what is asserted:
// every step in order, the container recorded, the Feature and its bot
// identity, the broker socket and the remote env on up, the probe's two
// checks on exec — and no override config, since the repository has one.
func TestRepositoryWithAConfigReachesRunning(t *testing.T) {
	e := newEnv(t)
	e.wire(t)
	v := e.create(t, alpha, "")

	if v.State != workspace.Running {
		t.Fatalf("state %s (%v); steps %+v", v.State, deref(v.StateDetail), v.Steps)
	}
	var want []string
	for _, st := range workspace.Steps {
		want = append(want, string(st)+":started", string(st)+":done")
	}
	if got := e.stepEvents(t, v.ID); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("step events\n got %v\nwant %v", got, want)
	}
	if v.ContainerID == nil || *v.ContainerID == "" {
		t.Error("the container id was not recorded")
	}
	if v.Branch != "main" {
		t.Errorf("branch %q; the default branch is main", v.Branch)
	}
	if d := v.Steps[workspace.StepSessionServer].Detail; !strings.Contains(d, "no session server was started") {
		t.Errorf("session_server says %q; a step that does nothing must say so", d)
	}
	if d := v.Steps[workspace.StepCredentialVolume].Detail; d != "Created the shared Claude credential volume "+claudeVolume+"." {
		t.Errorf("credential_volume says %q", d)
	}
	if d := v.Steps[workspace.StepUp].Detail; d != "" {
		t.Errorf("up has a detail %q on success", d)
	}
	if _, err := os.Stat(filepath.Join(e.root, v.ID, "repo", ".git")); err != nil {
		t.Errorf("no clone: %v", err)
	}
	if len(e.broker.opened) != 1 || e.broker.opened[0] != v.ID {
		t.Errorf("broker sockets opened: %v", e.broker.opened)
	}

	ups := e.cli.callsTo(t, "up")
	if len(ups) != 1 {
		t.Fatalf("up calls: %v", ups)
	}
	up := ups[0]
	if got := flag(up, "--workspace-folder"); len(got) != 1 || got[0] != filepath.Join(e.root, v.ID, "repo") {
		t.Errorf("--workspace-folder %v", got)
	}
	if got := flag(up, "--mount"); len(got) != 2 || got[0] != "type=bind,source=/run/drydock/sock/"+v.ID+",target="+container.BrokerMountPoint ||
		got[1] != "type=volume,source="+claudeVolume+",target=/home/vscode/.claude" {
		t.Errorf("--mount %v", got)
	}
	var feats map[string]map[string]string
	if got := flag(up, "--additional-features"); len(got) != 1 || json.Unmarshal([]byte(got[0]), &feats) != nil ||
		feats[feature]["botName"] != "krelinga-drydock-dev[bot]" || feats[feature]["botEmail"] == "" || len(feats) != 1 {
		t.Errorf("--additional-features %v", got)
	}
	env := strings.Join(flag(up, "--remote-env"), " ")
	if env != "DRYDOCK_REPO=krelinga/alpha DRYDOCK_WORKSPACE="+v.ID {
		t.Errorf("--remote-env %q", env)
	}
	if got := flag(up, "--override-config"); got != nil {
		t.Errorf("a repository with its own config got --override-config %v", got)
	}
	if got := flag(e.cli.callsTo(t, "read-configuration")[0], "--override-config"); got != nil {
		t.Errorf("read-configuration got --override-config %v", got)
	}

	execs := e.cli.callsTo(t, "exec")
	if len(execs) != 3 {
		t.Fatalf("exec calls: %v", execs)
	}
	probe, git, claude := strings.Join(execs[0], " "), strings.Join(execs[1], " "), strings.Join(execs[2], " ")
	if !strings.HasSuffix(probe, "-- drydock-probe") || !strings.HasSuffix(git, "-- git -C /workspaces/repo remote -v") ||
		!strings.HasSuffix(claude, "-- claude --version") {
		t.Errorf("the probe ran\n %s\n %s\n %s", probe, git, claude)
	}
	// The remote env is given to exec again: up's does not persist.
	if !strings.Contains(git, "--remote-env DRYDOCK_WORKSPACE="+v.ID) {
		t.Errorf("exec without the remote env: %s", git)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestRepositoryWithoutAConfigGetsTheMinimalOne is §6 step 3's other path:
// no devcontainer.json is not a disqualification. Drydock's minimal config is
// written beside the clone, never in it, and every CLI call that reads the
// config — read-configuration, up, and exec — is given it.
func TestRepositoryWithoutAConfigGetsTheMinimalOne(t *testing.T) {
	e := newEnv(t)
	e.cli.readConfig = "cat <<'EOF'\n" + fixture(t, "read-configuration-merged-override.json") + "\nEOF\n"
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/workspaces/repo", "/workspaces/plain2")
	e.wire(t)
	v := e.create(t, plain, "")

	if v.State != workspace.Running {
		t.Fatalf("state %s (%s)", v.State, deref(v.StateDetail))
	}
	if v.Branch != "trunk" {
		t.Errorf("branch %q; the default branch is trunk", v.Branch)
	}
	if d := v.Steps[workspace.StepResolveConfig].Detail; !strings.Contains(d, "minimal configuration") {
		t.Errorf("resolve_config says %q; it must say the minimal config is in use", d)
	}
	override := filepath.Join(e.root, v.ID, ".drydock", "devcontainer.json")
	b, err := os.ReadFile(override)
	if err != nil || !bytes.Equal(b, DefaultConfig()) {
		t.Errorf("the minimal config at %s: %q %v", override, b, err)
	}
	for _, sub := range []string{"read-configuration", "up", "exec"} {
		calls := e.cli.callsTo(t, sub)
		if len(calls) == 0 {
			t.Errorf("no %s call", sub)
		}
		for _, c := range calls {
			if got := flag(c, "--override-config"); len(got) != 1 || got[0] != override {
				t.Errorf("%s got --override-config %v", sub, got)
			}
		}
	}
	// The repository is untouched: nothing was written into the clone.
	repo := filepath.Join(e.root, v.ID, "repo")
	if _, err := os.Lstat(filepath.Join(repo, ".devcontainer")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf(".devcontainer appeared in the clone: %v", err)
	}
	if out := gitOut(t, repo, "status", "--porcelain", "--ignored"); out != "" {
		t.Errorf("the clone has changes:\n%s", out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// TestTheCredentialVolumeIsMadeOnceAndShared is §6 step 4 against a fake
// docker that keeps its volumes: the first workspace's run creates the shared
// volume, labelled with the prefix; a second workspace and a rebuild find it
// and create nothing; and every up mounts that one volume at the Feature's
// CLAUDE_CONFIG_DIR, which is what makes one login serve them all.
func TestTheCredentialVolumeIsMadeOnceAndShared(t *testing.T) {
	e := newEnv(t)
	// Two repositories, so git's origin is whichever this exec is for.
	e.cli.exec = `case " $* " in
*" drydock-probe "*) exit 0 ;;
*" claude --version "*) echo '2.1.289 (Claude Code)' ;;
*" remote -v "*) for a; do case $a in DRYDOCK_REPO=*) r=${a#DRYDOCK_REPO=} ;; esac; done
  printf 'origin\t%s/%s.git (fetch)\n' '` + e.fake.URL + `' "$r" ;;
*) exit 9 ;;
esac`
	e.wire(t)
	ctx := context.Background()
	creates := func() (n int) {
		for _, a := range e.cli.callsTo(t, "docker") {
			if len(a) > 2 && a[1] == "volume" && a[2] == "create" {
				if got := strings.Join(a[1:], " "); got != "volume create --driver local --label drydock.test.provision.claude-config=true -- "+claudeVolume {
					t.Errorf("create argv %q", got)
				}
				n++
			}
		}
		return n
	}

	first := e.create(t, alpha, "")
	if d := first.Steps[workspace.StepCredentialVolume].Detail; d != "Created the shared Claude credential volume "+claudeVolume+"." {
		t.Errorf("first workspace's credential_volume says %q", d)
	}
	if n := creates(); n != 1 {
		t.Fatalf("after one workspace, %d creates", n)
	}
	second := e.create(t, plain, "")
	if err := e.p.Rebuild(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	first = e.view(t, first.ID)
	for _, v := range []workspace.View{first, second} {
		if v.State != workspace.Running {
			t.Fatalf("%s: %s (%s)", v.ID, v.State, deref(v.StateDetail))
		}
		if d := v.Steps[workspace.StepCredentialVolume].Detail; d != "The shared Claude credential volume "+claudeVolume+" is there." {
			t.Errorf("%s: credential_volume says %q", v.ID, d)
		}
	}
	if n := creates(); n != 1 {
		t.Errorf("after two workspaces and a rebuild, %d creates; want the first only", n)
	}
	ups := e.cli.callsTo(t, "up")
	if len(ups) != 3 {
		t.Fatalf("up calls: %d", len(ups))
	}
	for _, up := range ups {
		mounts := flag(up, "--mount")
		if len(mounts) != 2 || mounts[1] != "type=volume,source="+claudeVolume+",target="+container.ClaudeConfigMountPoint {
			t.Errorf("up mounts %v", mounts)
		}
	}
}

// TestEachRealStepNamesItsFailure extends the workspace package's "every
// step writes an event naming itself" from stubs to the real step functions:
// one scripted failure per step that can fail, each asserting the step that
// failed, Drydock's sentence for it, and the workspace's move to failed. The
// first row is the control — the same harness, nothing broken, reaches
// running — so a harness that cannot succeed cannot pass the rest.
func TestEachRealStepNamesItsFailure(t *testing.T) {
	cases := []struct {
		name   string
		repo   int64
		break_ func(e *env)
		setup  func(*githubtest.Fake) // edits the repositories before they are built
		step   workspace.Step         // "" is the control
		detail string
	}{
		{name: "control", repo: alpha},
		{name: "allocate", repo: alpha, step: workspace.StepAllocate, detail: "could not create the workspace root",
			break_: func(e *env) { os.WriteFile(e.root, []byte("a file where the root should be"), 0o600) }},
		{name: "clone", repo: ghost, step: workspace.StepClone, detail: "GitHub refused a token"},
		{name: "resolve_config refused", repo: alpha, step: workspace.StepResolveConfig,
			detail: "devcontainer could not read the dev container configuration",
			break_: func(e *env) { e.cli.readConfig = "echo 'a CLI log line' >&2; exit 1" }},
		{name: "resolve_config unparseable", repo: alpha, step: workspace.StepResolveConfig, detail: "names no image",
			break_: func(e *env) {
				e.cli.readConfig = "cat <<'EOF'\n" + fixtureBytes("read-configuration-unparseable.json") + "\nEOF\n"
			}},
		{name: "resolve_config garbage", repo: alpha, step: workspace.StepResolveConfig, detail: "could not read devcontainer's answer",
			break_: func(e *env) { e.cli.readConfig = "echo 'not json'" }},
		// A configuration file outside the clone is not read at all: its
		// paths would not resolve where they appear to (§6).
		{name: "resolve_config config outside the clone", repo: alpha, step: workspace.StepResolveConfig,
			detail: "is a symbolic link or not inside the clone",
			break_: func(e *env) {
				e.cli.readConfig = "sed \"s#/srv/drydock/ws/FIXTURE/repo/.devcontainer/devcontainer.json#/etc/hostname#g\" <<'EOF'\n" + fixtureBytes("read-configuration-merged-ok.json") + "\nEOF\n"
			}},
		{name: "resolve_config read without the merged configuration", repo: alpha, step: workspace.StepResolveConfig,
			detail: "could not check what the dev container configuration asks of the host",
			break_: func(e *env) {
				e.cli.readConfig = "sed \"s#/srv/drydock/ws/FIXTURE/repo#$3#g\" <<'EOF'\n" + fixtureBytes("read-configuration-ok.json") + "\nEOF\n"
			}},
		{name: "resolve_config container not stopped", repo: alpha, step: workspace.StepResolveConfig,
			detail: "could not stop the workspace's container before reading its configuration",
			break_: func(e *env) { os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-ps"), nil, 0o600) }},
		{name: "broker_socket", repo: alpha, step: workspace.StepBrokerSocket, detail: "GitHub access socket",
			break_: func(e *env) { e.broker.err = errors.New("listen: address in use") }},
		{name: "up failed", repo: alpha, step: workspace.StepUp, detail: "did not bring the container up",
			break_: func(e *env) {
				e.cli.up = "cat <<'EOF'\n" + fixtureBytes("up-error-postcreate.json") + "\nEOF\nexit 1"
			}},
		{name: "up unreadable", repo: alpha, step: workspace.StepUp, detail: "could not run devcontainer up",
			break_: func(e *env) { e.cli.up = "echo 'Unknown argument: json' >&2; exit 1" }},
		{name: "resolve_config lockfile pins Drydock", repo: alpha, step: workspace.StepResolveConfig,
			detail: "pins Drydock's own Feature",
			setup:  withLockfile(`{"features":{"` + feature + `":{"version":"0.0.1"}}}`)},
		{name: "resolve_config lockfile unreadable", repo: alpha, step: workspace.StepResolveConfig,
			detail: "not a lockfile the dev container CLI could use",
			setup:  withLockfile(`{"features":`)},
		{name: "verify probe", repo: alpha, step: workspace.StepVerify, detail: "socket did not answer",
			break_: func(e *env) { e.cli.exec = "case \" $* \" in *\" drydock-probe \"*) exit 1 ;; esac" }},
		{name: "verify origin", repo: alpha, step: workspace.StepVerify, detail: "origin is not the repository",
			break_: func(e *env) {
				e.cli.exec = "case \" $* \" in *\" remote -v \"*) printf 'origin\\thttps://example.com/other.git (fetch)\\n' ;; esac"
			}},
		{name: "verify claude missing", repo: alpha, step: workspace.StepVerify, detail: "Claude Code did not run",
			break_: func(e *env) { e.cli.exec = claudeAnswers(e, "echo 'claude: not found' >&2; exit 127") }},
		// The image's own Claude Code first on PATH, or a Feature that
		// installed another: the classifiers are this version's.
		{name: "verify claude version", repo: alpha, step: workspace.StepVerify,
			detail: "Claude Code is not version " + classify.ClaudeCodeVersion,
			break_: func(e *env) { e.cli.exec = claudeAnswers(e, "echo '2.1.246 (Claude Code)'") }},
		{name: "credential_volume foreign", repo: alpha, step: workspace.StepCredentialVolume,
			detail: "exists but was not made by this Drydock",
			break_: func(e *env) { seedVolume(e, `{"Name":"`+claudeVolume+`","Driver":"local","Labels":{}}`) }},
		{name: "credential_volume nfs", repo: alpha, step: workspace.StepCredentialVolume,
			detail: "is not a plain local Docker volume",
			break_: func(e *env) {
				seedVolume(e, `{"Name":"`+claudeVolume+`","Driver":"local","Labels":{"drydock.test.provision.claude-config":"true"},"Options":{"type":"nfs","device":":/claude"}}`)
			}},
		// A volume another uid has written to: the owner helper's refusal,
		// with that uid named.
		{name: "credential_volume owner", repo: alpha, step: workspace.StepCredentialVolume,
			detail: "holds files that belong to uid 1000, not to Drydock's uid",
			break_: func(e *env) {
				os.WriteFile(filepath.Join(e.cli.dir, "owner-out"), []byte("1000\n"), 0o600)
				os.WriteFile(filepath.Join(e.cli.dir, "owner-exit"), []byte("4"), 0o600)
			}},
		{name: "credential_volume docker", repo: alpha, step: workspace.StepCredentialVolume,
			detail: "could not create or check the shared Claude credential volume",
			break_: func(e *env) { os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-volume"), nil, 0o600) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var setup []func(*githubtest.Fake)
			if c.setup != nil {
				setup = append(setup, c.setup)
			}
			e := newEnv(t, setup...)
			if c.break_ != nil {
				c.break_(e)
			}
			e.wire(t)
			v := e.create(t, c.repo, "")
			if c.step == "" {
				if v.State != workspace.Running {
					t.Fatalf("control: state %s (%s)", v.State, deref(v.StateDetail))
				}
				if c := e.broker.closes(); len(c) != 0 {
					t.Errorf("control: a running workspace's socket was closed: %v", c)
				}
				return
			}
			if v.State != workspace.Failed {
				t.Fatalf("state %s, want failed", v.State)
			}
			// GitHub access follows Drydock's state (§9.1): a failed run
			// closes its socket, whichever step it failed at — the
			// container a failed postCreateCommand leaves running included.
			if c := e.broker.closes(); len(c) != 1 || c[0] != v.ID {
				t.Errorf("a failed run closed sockets %v; want its own, once", c)
			}
			got := v.Steps[c.step]
			if got.Status != "failed" || !strings.Contains(got.Detail, c.detail) {
				t.Errorf("step %s: %+v; want failed with %q", c.step, got, c.detail)
			}
			if v.StateDetail == nil || !strings.Contains(*v.StateDetail, c.detail) {
				t.Errorf("state detail %q does not carry the step's sentence", deref(v.StateDetail))
			}
			for st, o := range v.Steps {
				if st != c.step && o.Status == "failed" {
					t.Errorf("step %s also failed", st)
				}
			}
			// A failed postCreateCommand leaves its container running (§6):
			// the id is recorded so teardown can find it.
			if c.name == "up failed" && (v.ContainerID == nil || *v.ContainerID == "") {
				t.Error("a failed up that created a container did not record it")
			}
		})
	}
}

// fixtureBytes reads a fixture outside a test's helper chain.
func fixtureBytes(name string) string {
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", name))
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestCreateRefusesBeforeARowExists: a repository the installation does not
// cover, a branch the clone would refuse, and no App — each refused with no
// row written and nothing run; the control is that the same repository with
// a good branch is accepted.
func TestCreateRefusesBeforeARowExists(t *testing.T) {
	e := newEnv(t)
	e.wire(t)
	ctx := context.Background()
	for _, c := range []struct {
		repo   int64
		branch string
		want   error
	}{
		{404, "", ErrUnknownRepository},
		{gone, "", ErrUnknownRepository},
		{alpha, "--upload-pack=touch /tmp/x", ErrBadBranch},
		{alpha, "a..b", ErrBadBranch},
	} {
		if _, err := e.p.Create(ctx, c.repo, c.branch); !errors.Is(err, c.want) {
			t.Errorf("Create(%d, %q) = %v, want %v", c.repo, c.branch, err, c.want)
		}
	}
	if vs, _ := e.p.Workspaces.Views(ctx); len(vs) != 0 {
		t.Errorf("refused creates left rows: %+v", vs)
	}
	unconfigured := &Provisioner{Workspaces: e.p.Workspaces, Events: e.p.Events, Containers: e.p.Containers}
	if _, err := unconfigured.Create(ctx, alpha, ""); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Create with no App = %v", err)
	}
	if err := unconfigured.Start(ctx, "01JABCDEFGHJKMNPQRSTVWXYZ0"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Start with no App = %v", err)
	}
	if v := e.create(t, alpha, "main"); v.State != workspace.Running {
		t.Errorf("control: state %s", v.State)
	}
}

// TestStartResumesFromTheRightStep: a workspace that failed after its clone
// starts again from resolve_config and does not clone a second time; one
// whose clone failed starts from the clone. A running one, or one already
// being started, is in_progress.
func TestStartResumesFromTheRightStep(t *testing.T) {
	ctx := context.Background()

	t.Run("after the clone", func(t *testing.T) {
		e := newEnv(t)
		e.cli.up = "echo broken; exit 1"
		e.wire(t)
		v := e.create(t, alpha, "")
		if v.State != workspace.Failed || v.Steps[workspace.StepUp].Status != "failed" {
			t.Fatalf("setup: %s %+v", v.State, v.Steps)
		}
		if e.broker.isOpen(v.ID) {
			t.Error("the failed run left its socket open")
		}
		mints := len(e.fake.TokenRequests)
		e.cli.up = "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n"
		e.wire(t)
		if err := e.p.Start(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		v = e.view(t, v.ID)
		if v.State != workspace.Running {
			t.Fatalf("state %s (%s)", v.State, deref(v.StateDetail))
		}
		if n := countOf(e.stepEvents(t, v.ID), "clone:started"); n != 1 {
			t.Errorf("the clone ran %d times; a start after it must not clone again", n)
		}
		if len(e.fake.TokenRequests) != mints {
			t.Errorf("the start minted %d more tokens", len(e.fake.TokenRequests)-mints)
		}
		if n := countOf(e.stepEvents(t, v.ID), "resolve_config:started"); n != 2 {
			t.Errorf("resolve_config ran %d times, want 2", n)
		}
		if !e.broker.isOpen(v.ID) {
			t.Error("a start from failed did not reopen the socket")
		}

		// A rebuild that fails closes it again, and a rebuild from failed
		// reopens it.
		e.cli.up = "echo broken; exit 1"
		e.wire(t)
		if err := e.p.Rebuild(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		if v = e.view(t, v.ID); v.State != workspace.Failed || e.broker.isOpen(v.ID) {
			t.Errorf("a failed rebuild: %s, socket open %v", v.State, e.broker.isOpen(v.ID))
		}
		e.cli.up = "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n"
		e.wire(t)
		if err := e.p.Rebuild(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		if v = e.view(t, v.ID); v.State != workspace.Running || !e.broker.isOpen(v.ID) {
			t.Errorf("a rebuild from failed: %s, socket open %v", v.State, e.broker.isOpen(v.ID))
		}
		if err := e.p.Start(ctx, v.ID); !errors.Is(err, workspace.ErrInProgress) {
			t.Errorf("Start of a running workspace = %v, want ErrInProgress", err)
		}
		if err := e.p.Start(ctx, "01JABCDEFGHJKMNPQRSTVWXYZ0"); !errors.Is(err, workspace.ErrNotFound) {
			t.Errorf("Start of no workspace = %v", err)
		}
	})

	t.Run("after a failed clone", func(t *testing.T) {
		e := newEnv(t)
		e.wire(t)
		// The clone fails the first time: GitHub has not been told about
		// the repository yet.
		saved := e.fake.Installations[0].Repos
		e.fake.Installations[0].Repos = saved[1:]
		v := e.create(t, alpha, "")
		if v.State != workspace.Failed || v.Steps[workspace.StepClone].Status != "failed" {
			t.Fatalf("setup: %s %+v", v.State, v.Steps)
		}
		e.fake.Installations[0].Repos = saved
		if err := e.p.Start(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		v = e.view(t, v.ID)
		if v.State != workspace.Running {
			t.Fatalf("state %s (%s)", v.State, deref(v.StateDetail))
		}
		if n := countOf(e.stepEvents(t, v.ID), "clone:done"); n != 1 {
			t.Errorf("the restart did not clone: %v", e.stepEvents(t, v.ID))
		}
	})

	t.Run("at the cap", func(t *testing.T) {
		e := newEnv(t)
		e.p.Workspaces.Cap = 1
		e.cli.up = "echo broken; exit 1"
		e.wire(t)
		failed := e.create(t, alpha, "") // failed frees its slot...
		e.cli.up = "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n"
		e.wire(t)
		e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
		e.wire(t)
		if v := e.create(t, plain, ""); v.State != workspace.Running { // ...which this takes
			t.Fatalf("setup: %s %s", v.State, deref(v.StateDetail))
		}
		if err := e.p.Start(ctx, failed.ID); !errors.Is(err, workspace.ErrAtCap) {
			t.Errorf("a start past the cap = %v, want ErrAtCap", err)
		}
		if v := e.view(t, failed.ID); v.State != workspace.Failed {
			t.Errorf("a refused start moved the workspace to %s", v.State)
		}
		e.p.Workspaces.Cap = 2
		if err := e.p.Start(ctx, failed.ID); err != nil {
			t.Errorf("control: under the cap, start = %v", err)
		}
		e.p.wg.Wait()
	})

	t.Run("one run at a time", func(t *testing.T) {
		e := newEnv(t)
		e.cli.up = "echo broken; exit 1"
		e.wire(t)
		v := e.create(t, alpha, "")
		e.cli.up = "exec sleep 5"
		e.wire(t)
		if err := e.p.Start(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.p.Start(ctx, v.ID); !errors.Is(err, workspace.ErrInProgress) {
			t.Errorf("a second start during the first = %v, want ErrInProgress", err)
		}
		if !e.p.Owns(v.ID) {
			t.Error("Owns is false during a run")
		}
		e.p.Shutdown(10 * time.Second)
	})
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

// TestInterruptedRunsSayWhy: a run past its timeout, and a run cut off by
// shutdown, each fail the step they were on with a sentence naming the
// interruption — not left building, and not the cancelled subprocess's
// words. After shutdown nothing new starts.
func TestInterruptedRunsSayWhy(t *testing.T) {
	ctx := context.Background()

	t.Run("timeout", func(t *testing.T) {
		e := newEnv(t)
		e.cli.up = "exec sleep 30"
		e.wire(t)
		e.p.Timeout = 3 * time.Second
		start := time.Now()
		v := e.create(t, alpha, "")
		if v.State != workspace.Failed || v.Steps[workspace.StepUp].Status != "failed" ||
			!strings.Contains(v.Steps[workspace.StepUp].Detail, "did not finish within 3s") {
			t.Errorf("after the timeout: %s %+v", v.State, v.Steps[workspace.StepUp])
		}
		if time.Since(start) > 20*time.Second {
			t.Errorf("the timeout took %s to land", time.Since(start))
		}
	})

	t.Run("shutdown", func(t *testing.T) {
		e := newEnv(t)
		e.cli.up = "exec sleep 30"
		e.wire(t)
		w, err := e.p.Create(ctx, alpha, "")
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for len(e.cli.callsTo(t, "up")) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("up never started")
			}
			time.Sleep(20 * time.Millisecond)
		}
		e.p.Shutdown(15 * time.Second)
		v := e.view(t, w.ID)
		if v.State != workspace.Failed || !strings.Contains(v.Steps[workspace.StepUp].Detail, "Drydock shut down") {
			t.Errorf("after shutdown: %s %+v", v.State, v.Steps[workspace.StepUp])
		}
		if _, err := e.p.Create(ctx, plain, ""); !errors.Is(err, ErrShuttingDown) {
			t.Errorf("Create after shutdown = %v", err)
		}
		if !e.p.Owns(w.ID) || e.p.Owns("01JABCDEFGHJKMNPQRSTVWXYZ0") {
			t.Error("Owns does not name exactly the workspaces this process ran")
		}
	})
}

// TestNoTokenSurvivesAProvision is the canary sweep (testing §4.2) over a
// full run: every installation token the fake issued is absent from the
// workspace tree (.git/config included), the database file's raw bytes and
// the event log — in its plain, base64 and URL-encoded forms. The controls:
// the tokens exist and were presented to the git remote, and the same sweep
// does find a marker that is supposed to be there.
func TestNoTokenSurvivesAProvision(t *testing.T) {
	e := newEnv(t)
	e.wire(t)
	v := e.create(t, alpha, "")
	if v.State != workspace.Running {
		t.Fatalf("state %s", v.State)
	}
	tokens := e.fake.IssuedTokens()
	if len(tokens) == 0 {
		t.Fatal("no token was issued, so the sweep would prove nothing")
	}
	presented := false
	for _, g := range e.fake.GitAuths() {
		presented = presented || g.Token != ""
	}
	if !presented {
		t.Fatal("the clone presented no token to the remote")
	}

	var corpus bytes.Buffer
	filepath.WalkDir(filepath.Dir(e.dbPath), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			corpus.Write(b)
		}
		return nil
	})
	evs, _ := e.log.ForWorkspace(context.Background(), v.ID, 1000)
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		corpus.Write(b)
	}
	// The marker: the README's text is in the clone, and the workspace id is
	// in the database. A sweep that reads nothing finds neither.
	for _, marker := range []string{"README.md\n", v.ID} {
		if !bytes.Contains(corpus.Bytes(), []byte(marker)) {
			t.Fatalf("the sweep did not find the marker %q; it is not reading what it claims to", marker)
		}
	}
	for _, tok := range tokens {
		for _, form := range []string{tok, base64.StdEncoding.EncodeToString([]byte(tok)), url.QueryEscape(tok),
			base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))} {
			if bytes.Contains(corpus.Bytes(), []byte(form)) {
				t.Errorf("a token (as %q…) survived the provision", form[:8])
			}
		}
	}
	if cfg := gitOut(t, filepath.Join(e.root, v.ID, "repo"), "config", "--local", "--list"); strings.Contains(cfg, "helper") ||
		!strings.Contains(cfg, "remote.origin.url="+e.fake.URL+"/krelinga/alpha.git") {
		t.Errorf(".git/config:\n%s", cfg)
	}
}
