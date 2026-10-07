package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/secrets"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

const (
	wsA = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	wsB = "01JBBBBBBBBBBBBBBBBBBBBBBB"
)

type env struct {
	b      *Broker
	fake   *githubtest.Fake
	db     *store.DB
	dbPath string
	// sec is the secrets store GET-SECRETS reads, empty until a test puts one.
	sec *secrets.Store
}

func newEnv(t *testing.T) *env {
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
		{ID: 101, FullName: "krelinga/alpha", DefaultBranch: "main"},
		{ID: 202, FullName: "krelinga/beta", DefaultBranch: "main"},
	}}}
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (101, 77, 'krelinga/alpha', 'main')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (202, 77, 'krelinga/beta', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + wsA + `', 101, '/x', 'main', 'running')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + wsB + `', 202, '/y', 'main', 'running')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := github.ParseKey(githubtest.KeyPEM(t))
	b := &Broker{
		// Short: a Unix socket path is limited to 108 bytes, and t.TempDir
		// paths are long.
		Dir:    shortDir(t),
		GitHub: &github.Client{AppID: 4242, Key: key, BaseURL: f.URL, Clock: clock},
		DB:     db.DB, Events: events.New(db.DB, clock), Env: sys.Env{Clock: clock, Random: sys.CryptoRandom{}},
		Secrets: &secrets.Store{DB: db.DB, Key: secretsKey(t), Env: sys.Env{Clock: clock, Random: sys.CryptoRandom{}}},
	}
	t.Cleanup(b.CloseAll)
	for _, ws := range []string{wsA, wsB} {
		if err := b.Open(ctx, ws); err != nil {
			t.Fatal(err)
		}
	}
	return &env{b: b, fake: f, db: db, dbPath: dbPath, sec: b.Secrets.(*secrets.Store)}
}

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "dd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "sock")
}

// ask sends one line on a workspace's socket and returns the answer.
func (e *env) ask(t *testing.T, ws, line string) string {
	t.Helper()
	conn, err := net.Dial("unix", e.b.SocketPath(ws))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(line + "\n"))
	answer, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSuffix(answer, "\n")
}

func tokenOf(t *testing.T, answer string) string {
	t.Helper()
	tok, ok := strings.CutPrefix(answer, "OK token=")
	if !ok {
		t.Fatalf("answer %q; want a token", answer)
	}
	tok, _, _ = strings.Cut(tok, " ")
	return tok
}

func TestParse(t *testing.T) {
	for _, ok := range []string{"PING", "GET-TOKEN scope=git", "GET-TOKEN scope=gh", "GET-SECRETS"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	// No field for a repository — and an attempt to add one is refused, not
	// ignored (testing §8.3, cross-broker).
	for _, bad := range []string{
		"GET-TOKEN scope=git repo=krelinga/beta",
		"GET-TOKEN scope=git repository_id=202",
		"GET-TOKEN repo=krelinga/beta scope=git",
		"GET-TOKEN scope=git scope=gh",
		"GET-TOKEN scope=admin",
		"GET-TOKEN scope=git ",
		"GET-TOKEN  scope=git",
		"GET-TOKEN\tscope=git",
		"get-token scope=git",
		"GET-TOKEN",
		"GET-TOKEN scope=git\r",
		// GET-SECRETS takes no arguments at all (§10.3): not a name, not a
		// repository, not a trailing space.
		"GET-SECRETS ",
		"GET-SECRETS TEST_DATABASE_URL",
		"GET-SECRETS repo=krelinga/beta",
		"GET-SECRETS scope=gh",
		"get-secrets",
		"GET-SECRETS\r",
		"PING extra",
		"",
		"GET-TOKEN scope=" + strings.Repeat("g", 200),
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The socket's mode is 0666, its workspace's directory 0755 and the parent
// 0700: the parent keeps host users out, and the mount decides which
// container reaches which directory. Each workspace's directory holds its
// own socket and nothing else — not the other workspace's, and no staging
// leftover — since that directory is all its container sees.
func TestSocketPermissions(t *testing.T) {
	e := newEnv(t)
	st, _ := os.Stat(e.b.SocketPath(wsA))
	wsDir, _ := os.Stat(e.b.SocketDir(wsA))
	dir, _ := os.Stat(e.b.Dir)
	if st.Mode().Perm() != 0o666 || st.Mode()&os.ModeSocket == 0 || !wsDir.IsDir() || wsDir.Mode().Perm() != 0o755 || dir.Mode().Perm() != 0o700 {
		t.Errorf("socket %v, workspace directory %v, directory %v", st.Mode(), wsDir.Mode(), dir.Mode())
	}
	if filepath.Dir(e.b.SocketPath(wsA)) != e.b.SocketDir(wsA) || e.b.SocketDir(wsA) == e.b.SocketDir(wsB) ||
		filepath.Dir(e.b.SocketDir(wsA)) != e.b.Dir {
		t.Errorf("layout: %s, %s", e.b.SocketPath(wsA), e.b.SocketDir(wsB))
	}
	for _, ws := range []string{wsA, wsB} {
		entries, err := os.ReadDir(e.b.SocketDir(ws))
		if err != nil || len(entries) != 1 || entries[0].Name() != SocketName {
			t.Errorf("%s's directory holds %v (%v); want only %s", ws, entries, err, SocketName)
		}
	}
	all, _ := os.ReadDir(e.b.Dir)
	if len(all) != 2 {
		t.Errorf("the broker directory holds %v; want one directory per workspace and nothing else", all)
	}
}

// TestRestartKeepsTheDirectory is the bug a file mount had (#16's review): a
// container's bind mount pins an inode, so a restart must leave the
// workspace's directory the same inode and put the new socket inside it. An
// fd held on the directory stands in for the mount here — a path through
// /proc/self/fd/N resolves in the directory that fd names, as a mount does —
// and it reaches the new process's socket. The container tier repeats this
// with a real mount (test/container).
func TestRestartKeepsTheDirectory(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	held, err := os.Open(e.b.SocketDir(wsA))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	viaMount := filepath.Join("/proc/self/fd", fmt.Sprint(held.Fd()), SocketName)
	ping := func() (string, error) {
		conn, err := net.Dial("unix", viaMount)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		conn.Write([]byte("PING\n"))
		answer, _ := bufio.NewReader(conn).ReadString('\n')
		return strings.TrimSpace(answer), nil
	}
	// Control: the held directory reaches the socket before the restart.
	if got, err := ping(); got != "OK" {
		t.Fatalf("control: %q %v", got, err)
	}
	var before syscall.Stat_t
	syscall.Stat(e.b.SocketDir(wsA), &before)

	// A restart: everything closed, as at shutdown, then a fresh broker on
	// the same directory, as the next process.
	e.b.CloseAll()
	if got, err := ping(); err == nil {
		t.Fatalf("the socket answered %q after shutdown", got)
	}
	next := &Broker{Dir: e.b.Dir, GitHub: e.b.GitHub, DB: e.b.DB, Events: e.b.Events, Env: e.b.Env, Secrets: e.b.Secrets}
	t.Cleanup(next.CloseAll)
	if err := next.Open(ctx, wsA); err != nil {
		t.Fatal(err)
	}
	var after syscall.Stat_t
	syscall.Stat(next.SocketDir(wsA), &after)
	if before.Ino != after.Ino {
		t.Errorf("the workspace's directory was recreated (inode %d → %d): a running container's mount names the old one", before.Ino, after.Ino)
	}
	if got, err := ping(); got != "OK" {
		t.Errorf("after the restart, through the held directory: %q %v", got, err)
	}
}

// Remove takes the workspace's directory with its socket and whatever the
// container put beside it — a symlink as a link, never its target. What it
// cannot remove is ErrLeftover.
func TestRemoveTakesTheDirectory(t *testing.T) {
	e := newEnv(t)
	if err := e.b.Remove(wsA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(e.b.SocketDir(wsA)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory survived Remove: %v", err)
	}
	if err := e.b.Remove(wsA); err != nil {
		t.Errorf("Remove of a removed workspace: %v", err)
	}
	// Control: B is untouched and still answers.
	if got := e.ask(t, wsB, "PING"); got != "OK" {
		t.Errorf("removing A affected B: %q", got)
	}
	// What a container's root leaves: a file, and a symlink out of the
	// directory. Both go; the symlink's target stays.
	outside := filepath.Join(t.TempDir(), "precious")
	os.WriteFile(outside, []byte("keep"), 0o600)
	os.WriteFile(filepath.Join(e.b.SocketDir(wsB), "stray"), nil, 0o600)
	os.Symlink(outside, filepath.Join(e.b.SocketDir(wsB), "link"))
	if err := e.b.Remove(wsB); err != nil {
		t.Errorf("Remove with a stray file and a symlink: %v", err)
	}
	if _, err := os.Lstat(e.b.SocketDir(wsB)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory survived: %v", err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "keep" {
		t.Errorf("a symlink's target was removed: %v", err)
	}
	// One it cannot empty — here a directory it may not write, as root's
	// would be to Drydock — is ErrLeftover.
	ctx := context.Background()
	e.b.Open(ctx, wsB)
	locked := filepath.Join(e.b.SocketDir(wsB), "locked")
	os.Mkdir(locked, 0o700)
	os.WriteFile(filepath.Join(locked, "f"), nil, 0o600)
	os.Chmod(locked, 0o500)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if os.Geteuid() != 0 {
		if err := e.b.Remove(wsB); !errors.Is(err, ErrLeftover) {
			t.Errorf("Remove of a directory it cannot empty = %v, want ErrLeftover", err)
		}
		if _, err := os.Lstat(e.b.SocketPath(wsB)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the socket survived a Remove that left something: %v", err)
		}
	}
	if err := e.b.Remove("../../etc"); err == nil {
		t.Error("Remove took a non-workspace id")
	}
}

// Golden permissions (testing §8.3): each scope asks GitHub for exactly the
// §9.3 set, for exactly this workspace's repository.
func TestTokensAreScopedToTheWorkspace(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		ws    string
		scope Scope
		repo  int64
	}{{wsA, ScopeGit, 101}, {wsA, ScopeGH, 101}, {wsB, ScopeGit, 202}} {
		tok := tokenOf(t, e.ask(t, tc.ws, "GET-TOKEN scope="+string(tc.scope)))
		e.fake.Mu.Lock()
		req := e.fake.TokenRequests[len(e.fake.TokenRequests)-1]
		e.fake.Mu.Unlock()
		if len(req.RepositoryIDs) != 1 || req.RepositoryIDs[0] != tc.repo {
			t.Errorf("%s %s: repository_ids %v; want exactly [%d]", tc.ws, tc.scope, req.RepositoryIDs, tc.repo)
		}
		golden, err := os.ReadFile(filepath.Join("testdata", "scope-"+string(tc.scope)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]string
		json.Unmarshal(golden, &want)
		if !equal(req.Permissions, want) {
			t.Errorf("scope %s asked for %v; the golden set is %v", tc.scope, req.Permissions, want)
		}
		// And the token works, for that repository only.
		repos, err := e.b.GitHub.Repositories(context.Background(), github.NewToken(tok, time.Now().Add(time.Hour)))
		if err != nil || len(repos) != 1 || repos[0].ID != tc.repo {
			t.Errorf("the %s token sees %+v (%v)", tc.ws, repos, err)
		}
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// §9.2: twenty gh invocations on one cached token are one mint and one
// grant; another scope is another.
func TestCacheAndGrants(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	first := tokenOf(t, e.ask(t, wsA, "GET-TOKEN scope=gh"))
	for i := 0; i < 19; i++ {
		if tok := tokenOf(t, e.ask(t, wsA, "GET-TOKEN scope=gh")); tok != first {
			t.Fatal("a cached request got a different token")
		}
	}
	if n := e.fake.Count("POST /app/installations/77/access_tokens"); n != 1 {
		t.Errorf("20 gh requests made %d mints; want 1", n)
	}
	e.ask(t, wsA, "GET-TOKEN scope=git")
	if n := e.fake.Count("POST /app/installations/77/access_tokens"); n != 2 {
		t.Errorf("a second scope made %d mints in all; want 2", n)
	}
	var grants int
	e.db.QueryRowContext(ctx, `SELECT count(*) FROM token_grant WHERE workspace_id = ?`, wsA).Scan(&grants)
	if grants != 2 {
		t.Errorf("%d token_grant rows; want one per mint", grants)
	}
	var issued int
	e.db.QueryRowContext(ctx, `SELECT count(*) FROM event WHERE kind = 'token.issued'`).Scan(&issued)
	if issued != 2 {
		t.Errorf("%d token.issued events; want 2", issued)
	}
}

// The repository's state is read per request: archived, removed from the
// installation, or a workspace being deleted all stop tokens — and none of
// them reaches GitHub.
func TestRefusalsFromTheWorkspacesState(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, setup, want string
	}{
		{"archived", `UPDATE repository SET archived = 1 WHERE id = 101`, "ERR reason=repo_archived"},
		{"removed from the installation", `UPDATE repository SET removed_at = 'x' WHERE id = 101`, "ERR reason=revoked"},
		{"being deleted", `UPDATE workspace SET state = 'deleting' WHERE id = '` + wsA + `'`, "ERR reason=revoked"},
		{"no workspace row", `DELETE FROM workspace WHERE id = '` + wsA + `'`, "ERR reason=revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if got := e.ask(t, wsA, "GET-TOKEN scope=git"); !strings.HasPrefix(got, "OK token=") {
				t.Fatalf("control: before the change: %q", got)
			}
			e.b.GitHub = &github.Client{AppID: e.b.GitHub.AppID, Key: e.b.GitHub.Key, BaseURL: e.b.GitHub.BaseURL, Clock: e.b.GitHub.Clock} // no cache
			before := e.fake.Count("POST")
			if _, err := e.db.ExecContext(ctx, tc.setup); err != nil {
				t.Fatal(err)
			}
			if got := e.ask(t, wsA, "GET-TOKEN scope=git"); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
			if e.fake.Count("POST") != before {
				t.Error("a refused request still asked GitHub for a token")
			}
		})
	}
}

// §12: GitHub refusing the App is reported, and nothing broader is tried —
// no second request, no wider permission set, no unscoped token.
func TestNeverFallsBackToABroaderCredential(t *testing.T) {
	for _, tc := range []struct {
		status int
		msg    string
		want   string
	}{
		{403, "This installation has been suspended", "ERR reason=rate_limited"},
		{403, "API rate limit exceeded for installation", "ERR reason=rate_limited"},
		{429, "Too Many Requests", "ERR reason=rate_limited"},
		// GitHub's two 422s, word for word as the contract test pins them,
		// and a third it might one day send: the status is the same, so only
		// the message decides, and an unknown one is never a guess.
		{422, "There is at least one repository that does not exist or is not accessible to the parent installation.", "ERR reason=revoked"},
		{422, "The permissions requested are not granted to this installation.", "ERR reason=app_permission_missing"},
		{422, "Validation Failed", "ERR reason=unavailable"},
		{404, "Not Found", "ERR reason=revoked"},
		{500, "Server Error", "ERR reason=unavailable"},
	} {
		e := newEnv(t)
		e.fake.Fail = func(r *http.Request) (int, string) {
			if r.Method == "POST" {
				return tc.status, tc.msg
			}
			return 0, ""
		}
		if got := e.ask(t, wsA, "GET-TOKEN scope=git"); got != tc.want {
			t.Errorf("%d %q: %q; want %q", tc.status, tc.msg, got, tc.want)
		}
		if n := e.fake.Count("POST"); n != 1 {
			t.Errorf("%d %q: %d token requests; want exactly the one that failed", tc.status, tc.msg, n)
		}
	}
}

// The v0.3.0 deployment's bug: an App without actions:write refuses the gh
// scope with a 422, and the broker called that "revoked", so the operator
// looked for a removed repository. Driven by the fake's own enforcement of
// the App's permissions and the installation's repositories, not by Fail.
func TestAMissingAppPermissionIsNotARevocation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.fake.Mu.Lock()
	delete(e.fake.AppPermissions, "actions")
	e.fake.Mu.Unlock()

	if got := e.ask(t, wsA, "GET-TOKEN scope=gh"); got != "ERR reason=app_permission_missing" {
		t.Errorf("gh without actions:write: %q; want app_permission_missing", got)
	}
	// Control: git's scope does not ask for actions, and still works — the
	// App is otherwise fine, which is exactly what the operator sees.
	tokenOf(t, e.ask(t, wsA, "GET-TOKEN scope=git"))

	var level, msg, data string
	if err := e.db.QueryRowContext(ctx, `SELECT level, message, data FROM event WHERE kind = 'token.refused' AND workspace_id = ?`, wsA).
		Scan(&level, &msg, &data); err != nil {
		t.Fatal(err)
	}
	want := "GitHub refused a gh token for this workspace: the GitHub App lacks a permission this scope needs. " +
		"Check the App's permissions, and accept any pending permission request on its installation."
	if level != "warn" || msg != want {
		t.Errorf("event %s %q; want warn %q", level, msg, want)
	}
	var d struct {
		Scope       string            `json:"scope"`
		Reason      string            `json:"reason"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatal(err)
	}
	if d.Scope != "gh" || d.Reason != ReasonPermissionMissing || d.Permissions["actions"] != "write" || len(d.Permissions) != len(ScopeGH.Permissions()) {
		t.Errorf("event data %s", data)
	}
	// GitHub's own sentence stays out of the log: the message is ours.
	if strings.Contains(msg+data, "not granted to this installation") {
		t.Errorf("GitHub's raw message reached the event: %q %s", msg, data)
	}

	// Positive control for the distinction: the repository leaving the
	// installation, with the database not yet knowing, is still revoked.
	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = e.fake.Installations[0].Repos[1:] // drops krelinga/alpha, wsA's
	e.fake.AppPermissions["actions"] = "write"
	e.fake.Mu.Unlock()
	if got := e.ask(t, wsA, "GET-TOKEN scope=gh"); got != "ERR reason=revoked" {
		t.Errorf("a repository outside the installation: %q; want revoked", got)
	}
	var revokedMsg string
	e.db.QueryRowContext(ctx, `SELECT message FROM event WHERE kind = 'token.refused' AND data LIKE '%"revoked"%'`).Scan(&revokedMsg)
	if revokedMsg != "GitHub refused a gh token for this workspace (revoked)." {
		t.Errorf("the revoked event says %q", revokedMsg)
	}
}

func TestBadRequestsReachNothing(t *testing.T) {
	e := newEnv(t)
	if got := e.ask(t, wsA, "GET-TOKEN scope=git repo=krelinga/beta"); got != "ERR reason=bad_request" {
		t.Errorf("an injected repo field: %q", got)
	}
	if e.fake.Count("POST") != 0 {
		t.Error("a bad request reached GitHub")
	}
	if got := e.ask(t, wsA, "PING"); got != "OK" {
		t.Errorf("control: PING = %q", got)
	}
}

// Closing a socket cuts the container off at once (§9.1, Fig 3).
func TestCloseCutsAccess(t *testing.T) {
	e := newEnv(t)
	if got := e.ask(t, wsA, "PING"); got != "OK" {
		t.Fatalf("control: %q", got)
	}
	e.b.Close(wsA)
	_, err := net.Dial("unix", e.b.SocketPath(wsA))
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("after Close, connecting: %v", err)
	}
	if got := e.ask(t, wsB, "PING"); got != "OK" {
		t.Errorf("closing A affected B: %q", got)
	}
}

// TestCloseRemovesASocketItIsNotServing: a socket file left by an earlier
// process — a delete resumed at boot, whose workspace this process never
// opened — is removed by Close; a regular file at the path is not.
func TestCloseRemovesASocketItIsNotServing(t *testing.T) {
	e := newEnv(t)
	e.b.Close(wsA)
	l, _ := net.Listen("unix", e.b.SocketPath(wsA))
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Lstat(e.b.SocketPath(wsA)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := e.b.Close(wsA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(e.b.SocketPath(wsA)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stale socket survived Close: %v", err)
	}
	os.WriteFile(e.b.SocketPath(wsA), []byte("not a socket"), 0o600)
	e.b.Close(wsA)
	if _, err := os.Lstat(e.b.SocketPath(wsA)); err != nil {
		t.Errorf("Close removed something that is not a socket: %v", err)
	}
	if err := e.b.Close("../../etc/passwd"); err == nil {
		t.Error("Close took a non-workspace id")
	}
	// The directory stays: a running container's mount names it, and the
	// next Open puts a socket back in it.
	if fi, err := os.Lstat(e.b.SocketDir(wsA)); err != nil || !fi.IsDir() {
		t.Errorf("Close removed the workspace's directory: %v", err)
	}
	// A socket where a Drydock before the directory mount kept it — an
	// upgrade's leftover — is removed too.
	legacy := filepath.Join(e.b.Dir, wsA+".sock")
	l, _ = net.Listen("unix", legacy)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	e.b.Close(wsA)
	if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a legacy socket survived Close: %v", err)
	}
}

// Open replaces whatever is at the socket's name — a stale socket from a
// crash, or what root in the container put there, since the directory is
// mounted writable — and never follows a symlink: the socket lands in the
// directory, its target is untouched, and the socket's mode is set where no
// container can reach. Only a directory at the name is refused.
func TestOpenReplacesWhateverIsAtTheName(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	outside := filepath.Join(t.TempDir(), "precious")
	os.WriteFile(outside, []byte("keep"), 0o600)
	stale := func() {
		l, _ := net.Listen("unix", e.b.SocketPath(wsA))
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		l.Close()
	}
	for name, plant := range map[string]func(){
		"a stale socket": stale,
		"a regular file": func() { os.WriteFile(e.b.SocketPath(wsA), []byte("not a socket"), 0o600) },
		"a symlink":      func() { os.Symlink(outside, e.b.SocketPath(wsA)) },
	} {
		e.b.Close(wsA)
		os.Remove(e.b.SocketPath(wsA))
		plant()
		if err := e.b.Open(ctx, wsA); err != nil {
			t.Errorf("%s was not replaced: %v", name, err)
			continue
		}
		if fi, err := os.Lstat(e.b.SocketPath(wsA)); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o666 {
			t.Errorf("after %s: %v %v", name, fi.Mode(), err)
		}
		if got := e.ask(t, wsA, "PING"); got != "OK" {
			t.Errorf("after %s: PING %q", name, got)
		}
	}
	if fi, err := os.Lstat(outside); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("a symlink's target was touched: %v %v", fi.Mode(), err)
	}
	e.b.Close(wsA)
	os.Mkdir(e.b.SocketPath(wsA), 0o700)
	if err := e.b.Open(ctx, wsA); err == nil {
		t.Error("a directory at the socket path was accepted")
	}
	if entries, _ := os.ReadDir(e.b.Dir); len(entries) != 2 {
		t.Errorf("a failed Open left a staging socket: %v", entries)
	}
	if err := e.b.Open(ctx, "../../etc/passwd"); err == nil {
		t.Error("a non-workspace id became a socket path")
	}
}

// The canary sweep: every token issued is absent from the database file,
// and the grant rows that record them are present.
func TestNoTokenReachesTheDatabase(t *testing.T) {
	e := newEnv(t)
	e.ask(t, wsA, "GET-TOKEN scope=git")
	e.ask(t, wsB, "GET-TOKEN scope=gh")
	e.db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
	var raw []byte
	for _, p := range []string{e.dbPath, e.dbPath + "-wal"} {
		b, _ := os.ReadFile(p)
		raw = append(raw, b...)
	}
	if !bytes.Contains(raw, []byte("git-credential")) {
		t.Fatal("control: no grant row was written")
	}
	for _, tok := range e.fake.IssuedTokens() {
		if bytes.Contains(raw, []byte(tok)) {
			t.Error("a token reached the database")
		}
	}
}

// A socket path too long for sun_path is refused at Open: it is bound at the
// shorter staging path, so the kernel would not refuse it, and no client
// could ever connect. Control: the same broker one byte shallower opens, and
// a client reaches it.
func TestOpenRefusesAPathNoClientCouldReach(t *testing.T) {
	ctx := context.Background()
	base := shortDir(t)
	room := maxSocketPath - len(base) - len("/"+wsA+"/"+SocketName) - 1 // the "/" after base
	long := &Broker{Dir: filepath.Join(base, strings.Repeat("d", room+1))}
	if err := long.Open(ctx, wsA); err == nil {
		long.CloseAll()
		t.Fatalf("a %d-byte socket path was opened", len(long.SocketPath(wsA)))
	}
	ok := &Broker{Dir: filepath.Join(base, strings.Repeat("d", room))}
	if err := ok.Open(ctx, wsA); err != nil {
		t.Fatalf("control: a %d-byte socket path: %v", len(ok.SocketPath(wsA)), err)
	}
	defer ok.CloseAll()
	conn, err := net.Dial("unix", ok.SocketPath(wsA))
	if err != nil {
		t.Fatalf("control: connecting to a %d-byte path: %v", len(ok.SocketPath(wsA)), err)
	}
	conn.Close()
}
