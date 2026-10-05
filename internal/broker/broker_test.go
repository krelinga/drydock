package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// The socket's mode is 0666 and its directory's 0700: the directory keeps
// host users out, and the mount decides which container reaches it.
func TestSocketPermissions(t *testing.T) {
	e := newEnv(t)
	st, _ := os.Stat(e.b.SocketPath(wsA))
	dir, _ := os.Stat(e.b.Dir)
	if st.Mode().Perm() != 0o666 || st.Mode()&os.ModeSocket == 0 || dir.Mode().Perm() != 0o700 {
		t.Errorf("socket %v, directory %v", st.Mode(), dir.Mode())
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
		{422, "There is at least one repository that does not exist", "ERR reason=revoked"},
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
}

func TestOpenReplacesAStaleSocketButNothingElse(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.b.Close(wsA)
	// A socket left behind by a crash: a listener that is gone.
	l, _ := net.Listen("unix", e.b.SocketPath(wsA))
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := e.b.Open(ctx, wsA); err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	e.b.Close(wsA)
	os.WriteFile(e.b.SocketPath(wsA), []byte("not a socket"), 0o600)
	if err := e.b.Open(ctx, wsA); err == nil {
		t.Error("a regular file at the socket path was replaced")
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
