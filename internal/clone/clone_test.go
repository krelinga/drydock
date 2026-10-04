package clone

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const repoID = 101

type env struct {
	cloner *Cloner
	ws     *workspace.Store
	log    *events.Log
	fake   *githubtest.Fake
	dbPath string
	// argv is where the git wrapper records the argv of git and, through
	// GIT_TRACE, of every process git itself starts.
	argv, trace string
}

// newEnv builds a fake GitHub with its git remote, a database holding the
// repository, and a Cloner whose git is a wrapper that records its argv
// before exec'ing the real one. root is the workspace root.
func newEnv(t *testing.T, root string) *env {
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
		{ID: repoID, FullName: "krelinga/alpha", DefaultBranch: "main", Files: []string{"README.md", "src/fixture-marker.txt"}},
		{ID: 202, FullName: "krelinga/beta", DefaultBranch: "main"},
	}}}
	f.EnableGit(t)
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (101, 77, 'krelinga/alpha', 'main')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (202, 77, 'krelinga/beta', 'main')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	argv, trace := filepath.Join(dir, "argv"), filepath.Join(dir, "trace")
	wrapper := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + argv + "'\nGIT_TRACE='" + trace + "' exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	key, _ := github.ParseKey(githubtest.KeyPEM(t))
	log := events.New(db.DB, clock)
	return &env{
		cloner: &Cloner{
			DB:      db.DB,
			GitHub:  &github.Client{AppID: 4242, Key: key, BaseURL: f.URL, Clock: clock},
			Runner:  subproc.Exec{Resolver: subproc.FixedResolver{"git": wrapper}},
			BaseURL: f.URL,
		},
		ws:  &workspace.Store{DB: db.DB, Events: log, Env: sys.Env{Clock: clock, Random: sys.CryptoRandom{}}, Root: root},
		log: log, fake: f, dbPath: dbPath, argv: argv, trace: trace,
	}
}

// provision runs the clone through the real step runner, the rest of the
// steps stubbed, so what reaches the event log is what production writes.
func (e *env) provision(t *testing.T, branch string) (workspace.Workspace, error) {
	t.Helper()
	ctx := context.Background()
	w, err := e.ws.Create(ctx, repoID, branch)
	if err != nil {
		t.Fatal(err)
	}
	run := map[workspace.Step]workspace.StepFunc{}
	for _, st := range workspace.Steps {
		run[st] = func(context.Context, workspace.Workspace) error { return nil }
	}
	run[workspace.StepAllocate] = func(_ context.Context, w workspace.Workspace) error {
		return os.MkdirAll(filepath.Dir(w.HostPath), 0o755)
	}
	run[workspace.StepClone] = e.cloner.Step
	return w, e.ws.Provision(ctx, w.ID, workspace.StepAllocate, run)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// sweep returns every file under root, and the paths of those containing
// any of needles (testing §4.2, the canary sweep).
func sweep(t *testing.T, root string, needles []string) (files []string, hits []string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, p)
		for _, n := range needles {
			if bytes.Contains(b, []byte(n)) {
				hits = append(hits, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, hits
}

func (e *env) dbBytes(t *testing.T) []byte {
	t.Helper()
	var raw []byte
	for _, p := range []string{e.dbPath, e.dbPath + "-wal"} {
		b, _ := os.ReadFile(p)
		raw = append(raw, b...)
	}
	return raw
}

// The clone works, with exactly the token minted for it, and that token is
// nowhere afterwards: not under the workspace (the working tree and .git/),
// not in the database, not in any argv. Testing §8.3, "No token in
// .git/config or the tree".
func TestCloneLeavesNoTokenBehind(t *testing.T) {
	root := t.TempDir()
	e := newEnv(t, root)
	w, err := e.provision(t, "main")
	if err != nil {
		t.Fatal(err)
	}

	// Positive controls: a real clone, on the branch, with the fixture's
	// files, whose history git can read.
	if b, err := os.ReadFile(filepath.Join(w.HostPath, "src", "fixture-marker.txt")); err != nil || string(b) != "src/fixture-marker.txt\n" {
		t.Fatalf("the clone lacks the fixture file: %q, %v", b, err)
	}
	if out := git(t, w.HostPath, "log", "--format=%s"); strings.TrimSpace(out) != "fixture" {
		t.Errorf("git log: %q", out)
	}
	if b := strings.TrimSpace(git(t, w.HostPath, "rev-parse", "--abbrev-ref", "HEAD")); b != "main" {
		t.Errorf("checked out %q; want main", b)
	}

	// The token was scoped to this one repository, read-only.
	e.fake.Mu.Lock()
	reqs := append([]githubtest.TokenRequest(nil), e.fake.TokenRequests...)
	e.fake.Mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("%d token requests; want 1", len(reqs))
	}
	if r := reqs[0]; len(r.RepositoryIDs) != 1 || r.RepositoryIDs[0] != repoID ||
		len(r.Permissions) != 2 || r.Permissions["contents"] != "read" || r.Permissions["metadata"] != "read" {
		t.Errorf("token request %+v; want contents:read+metadata:read for [%d] only", r, repoID)
	}

	// Observed, not inferred: what git presented is what the fake issued.
	issued := e.fake.IssuedTokens()
	if len(issued) != 1 {
		t.Fatalf("issued %d tokens; want 1", len(issued))
	}
	presented := false
	for _, a := range e.fake.GitAuths() {
		if a.Token != "" && a.Token != issued[0] {
			t.Errorf("git presented %q, which the fake did not issue for this clone", a.Token)
		}
		if a.Token == issued[0] && a.Repo == "krelinga/alpha" && a.Service == "git-upload-pack" {
			presented = true
		}
	}
	if !presented {
		t.Fatalf("git never presented the issued token: %+v", e.fake.GitAuths())
	}

	// The canary sweep, over the whole workspace directory including .git/.
	files, hits := sweep(t, filepath.Dir(w.HostPath), issued)
	if len(files) < 10 || !contains(files, filepath.Join(w.HostPath, "src", "fixture-marker.txt")) ||
		!contains(files, filepath.Join(w.HostPath, ".git", "config")) {
		t.Fatalf("control: the sweep saw %d files, not the clone", len(files))
	}
	if len(hits) > 0 {
		t.Errorf("the token is on disk in %v", hits)
	}

	// And in the database, where the events went.
	e.ws.DB.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
	raw := e.dbBytes(t)
	if !bytes.Contains(raw, []byte("workspace.step")) {
		t.Fatal("control: no step event in the database, so its sweep proves nothing")
	}
	if bytes.Contains(raw, []byte(issued[0])) {
		t.Error("the token reached the database")
	}

	// argv: git's own, and through GIT_TRACE every child git started —
	// the remote helper and the credential helper among them.
	argv, _ := os.ReadFile(e.argv)
	trace, _ := os.ReadFile(e.trace)
	if !bytes.Contains(argv, []byte("clone")) {
		t.Fatalf("control: the argv record lacks `clone`: %q", argv)
	}
	if !bytes.Contains(trace, []byte("remote-http")) || !bytes.Contains(trace, []byte("printf")) {
		t.Fatalf("control: the trace did not see the remote helper and the credential helper start:\n%s", trace)
	}
	for name, b := range map[string][]byte{"argv": argv, "trace": trace} {
		if bytes.Contains(b, []byte(issued[0])) {
			t.Errorf("the token is in a process's argv (%s record)", name)
		}
	}

	// .git/config: the plain remote, and no credential configuration left
	// for the container to inherit.
	cfg, err := os.ReadFile(filepath.Join(w.HostPath, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "url = " + e.fake.URL + "/krelinga/alpha.git\n"; !bytes.Contains(cfg, []byte(want)) {
		t.Errorf(".git/config lacks %q:\n%s", want, cfg)
	}
	for _, bad := range []string{"credential", "extraheader", "x-access-token", "helper"} {
		if bytes.Contains(cfg, []byte(bad)) {
			t.Errorf(".git/config mentions %q:\n%s", bad, cfg)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// An http.extraheader in a repository enclosing the workspace root is sent
// ahead of the credential helper and wins. `git clone` itself does not read
// an enclosing repository's config (git 2.52), but the git that runs in
// that directory for anything else does, so env's GIT_CEILING_DIRECTORIES is
// tested on a command that would: ls-remote, with and without it.
func TestEnclosingRepositoryConfigIsIgnored(t *testing.T) {
	const bogusToken = "ghs_EnclosingBogus"
	enclosing := t.TempDir()
	git(t, enclosing, "init", "-q")
	bogus := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + bogusToken))
	git(t, enclosing, "config", "http.extraheader", "Authorization: Basic "+bogus)
	root := filepath.Join(enclosing, "ws")

	e := newEnv(t, root)
	w, err := e.provision(t, "main")
	if err != nil {
		t.Fatalf("clone under an enclosing repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.HostPath, "README.md")); err != nil {
		t.Fatal(err)
	}

	sentBogus := func() int {
		n := 0
		for _, a := range e.fake.GitAuths() {
			if a.Token == bogusToken {
				n++
			}
		}
		return n
	}
	if n := sentBogus(); n != 0 {
		t.Errorf("the clone sent the enclosing repository's extraheader %d times", n)
	}

	dir := filepath.Dir(w.HostPath) // the clone's working directory
	env := e.cloner.env(e.fake.URL, dir, e.fake.IssuedTokens()[0])
	lsRemote := func(env []string) {
		cmd := exec.Command("git", "ls-remote", e.fake.URL+"/krelinga/alpha.git")
		cmd.Dir, cmd.Env = dir, env
		cmd.Run()
	}
	// Control: the trap is armed. Without the ceiling, git in that
	// directory finds the enclosing repository and sends its header.
	var noCeiling []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_CEILING_DIRECTORIES=") {
			noCeiling = append(noCeiling, kv)
		}
	}
	lsRemote(noCeiling)
	armed := sentBogus()
	if armed == 0 {
		t.Fatal("control: without the ceiling git did not send the enclosing header, so this test proves nothing")
	}
	lsRemote(env)
	if sentBogus() != armed {
		t.Error("with the clone's environment, git still read the enclosing repository's config")
	}
}

// A failed clone fails the clone step with Drydock's sentence; git's stderr
// reaches the caller, not the event log.
func TestFailedCloneKeepsGitOutputOutOfEvents(t *testing.T) {
	e := newEnv(t, t.TempDir())
	w, err := e.provision(t, "no-such-branch")
	var se *workspace.StepError
	if !errors.As(err, &se) || se.Step != workspace.StepClone {
		t.Fatalf("got %v; want the clone step to fail", err)
	}
	// Control: the caller has git's own words.
	if !strings.Contains(err.Error(), "not found in upstream") {
		t.Fatalf("control: the caller's error lacks git's message: %v", err)
	}
	all, _ := e.log.ForWorkspace(context.Background(), w.ID, 100)
	sawSentence := false
	for _, ev := range all {
		text := ev.Message + string(ev.Data)
		if strings.Contains(text, "git could not clone the repository.") {
			sawSentence = true
		}
		for _, leak := range append([]string{"not found in upstream", e.fake.URL, "exited"}, e.fake.IssuedTokens()...) {
			if strings.Contains(text, leak) {
				t.Errorf("event %s %q carries %q", ev.Kind, text, leak)
			}
		}
	}
	if !sawSentence {
		t.Error("control: no event carries the public sentence")
	}
	for _, tok := range e.fake.IssuedTokens() {
		if strings.Contains(err.Error(), tok) {
			t.Error("the token is in the returned error")
		}
	}
}

// A repository of the installation other than the workspace's is not
// reachable with the clone's token: the scope is enforced by GitHub (here,
// the fake), and this proves the request named only the one repository.
func TestTheTokenReachesOnlyItsRepository(t *testing.T) {
	e := newEnv(t, t.TempDir())
	if _, err := e.provision(t, "main"); err != nil {
		t.Fatal(err)
	}
	tok := e.fake.IssuedTokens()[0]
	dir := t.TempDir()
	try := func(repo string) error {
		cmd := exec.Command("git", "ls-remote", e.fake.URL+"/"+repo+".git")
		cmd.Dir = dir
		cmd.Env = (&Cloner{BaseURL: e.fake.URL}).env(e.fake.URL, dir, tok)
		return cmd.Run()
	}
	if err := try("krelinga/alpha"); err != nil {
		t.Fatalf("control: the token does not reach its own repository: %v", err)
	}
	if err := try("krelinga/beta"); err == nil {
		t.Error("the clone token reached another repository")
	}
}

// Names that could be read as an option, or escape refs/heads, are refused
// before git runs.
func TestRefusesHostileInputBeforeGitRuns(t *testing.T) {
	for _, b := range []string{"main", "feature/x", "v1.2", "drydock/fix-1"} {
		if !validBranch(b) {
			t.Errorf("%q refused", b)
		}
	}
	for _, b := range []string{"", "-u", "--upload-pack=touch /tmp/x", "a..b", "a b", "a\nb", "x.lock", "/x", "x/", "a@{1}", "a:b", "a~1"} {
		if validBranch(b) {
			t.Errorf("%q accepted", b)
		}
	}

	e := newEnv(t, t.TempDir())
	w, err := e.ws.Create(context.Background(), repoID, "--upload-pack=touch /tmp/pwned")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cloner.Step(context.Background(), w); err == nil {
		t.Error("a branch that is an option was cloned")
	}
	if _, err := os.Stat(e.argv); !os.IsNotExist(err) {
		t.Error("git ran for a hostile branch")
	}
	if n := len(e.fake.IssuedTokens()); n != 0 {
		t.Errorf("a token was minted (%d) for a clone that could not run", n)
	}
	// Control: the same workspace with a sane branch clones.
	w.Branch = "main"
	os.MkdirAll(filepath.Dir(w.HostPath), 0o755)
	if err := e.cloner.Step(context.Background(), w); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// A clone never deletes what is already at its path.
func TestRefusesAnExistingDirectory(t *testing.T) {
	e := newEnv(t, t.TempDir())
	w, err := e.ws.Create(context.Background(), repoID, "main")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(w.HostPath, 0o755)
	keep := filepath.Join(w.HostPath, "uncommitted.txt")
	os.WriteFile(keep, []byte("work"), 0o644)
	if err := e.cloner.Step(context.Background(), w); err == nil {
		t.Error("cloned over an existing directory")
	}
	if b, _ := os.ReadFile(keep); string(b) != "work" {
		t.Error("the existing directory's contents were touched")
	}
	// Control.
	os.RemoveAll(w.HostPath)
	if err := e.cloner.Step(context.Background(), w); err != nil {
		t.Fatalf("control: %v", err)
	}
}
