package catalog

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

type env struct {
	cat    *Catalog
	group  *life.Group
	fake   *githubtest.Fake
	clock  *sys.FakeClock
	log    *events.Log
	dbPath string
}

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// newEnv is a catalog started without the refresh at once, so each test's
// refreshes are its own.
func newEnv(t *testing.T) *env { t.Helper(); return newEnvStarted(t, false) }

// newEnvBooting is a catalog started as Serve starts it: Start, with the
// refresh at once.
func newEnvBooting(t *testing.T) *env { t.Helper(); return newEnvStarted(t, true) }

func newEnvStarted(t *testing.T, boot bool) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drydock.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(t0)
	f := githubtest.New(t, 5189455, clock.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 1, FullName: "krelinga/plain", DefaultBranch: "main", PushedAt: t0.Add(-time.Hour),
			Files: []string{".devcontainer/devcontainer.json", "go.mod"}},
		{ID: 2, FullName: "krelinga/rootfile", DefaultBranch: "main", PushedAt: t0.Add(-2 * time.Hour),
			Files: []string{".devcontainer.json"}},
		{ID: 3, FullName: "krelinga/nested", DefaultBranch: "trunk", PushedAt: t0.Add(-3 * time.Hour),
			Files: []string{".devcontainer/python/devcontainer.json", ".devcontainer/README.md"}},
		{ID: 4, FullName: "krelinga/bare", DefaultBranch: "main", PushedAt: t0.Add(-4 * time.Hour),
			Files: []string{"main.go"}},
		{ID: 5, FullName: "krelinga/empty", DefaultBranch: "main"},
	}}}
	key, _ := github.ParseKey(githubtest.KeyPEM(t))
	log := events.New(db.DB, clock)
	e := &env{
		cat: &Catalog{DB: db.DB, Events: log, Clock: clock, Logf: t.Logf,
			GitHub: &github.Client{AppID: 5189455, Key: key, BaseURL: f.URL, Clock: clock}},
		group: life.NewGroup(context.Background()),
		fake:  f, clock: clock, log: log, dbPath: path,
	}
	// Stopped and waited for before the database closes, as Serve does
	// (cleanups run last-registered first).
	t.Cleanup(func() { e.group.Wait(nil) })
	if err := e.cat.start(e.group, boot); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) list(t *testing.T) map[string]RepoView {
	t.Helper()
	v, err := e.cat.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]RepoView{}
	for _, r := range v.Repos {
		out[r.FullName] = r
	}
	return out
}

func (e *env) refresh(t *testing.T) Result {
	t.Helper()
	res, err := e.cat.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRefreshListsAndProbes(t *testing.T) {
	e := newEnv(t)
	res := e.refresh(t)
	if res.Count != 5 || res.Added != 5 || res.Removed != 0 {
		t.Errorf("result %+v", res)
	}
	got := e.list(t)
	want := map[string]bool{"krelinga/plain": true, "krelinga/rootfile": true, "krelinga/nested": true,
		"krelinga/bare": false, "krelinga/empty": false}
	for name, has := range want {
		r, ok := got[name]
		if !ok || r.HasDevcontainer == nil || *r.HasDevcontainer != has {
			t.Errorf("%s: %+v; want has_devcontainer %v", name, r.HasDevcontainer, has)
		}
	}
	if got["krelinga/nested"].DefaultBranch != "trunk" {
		t.Error("default branch not recorded")
	}

	v, _ := e.cat.List(context.Background())
	if len(v.Installations) != 1 || v.Installations[0].SettingsURL != "https://github.com/settings/installations/77" ||
		v.RefreshedAt == nil || !v.RefreshedAt.Equal(t0) || v.LastRefreshError != nil {
		t.Errorf("view %+v", v)
	}
	// Newest push first: the order the home list reads in.
	if v.Repos[0].FullName != "krelinga/plain" {
		t.Errorf("first repo %s", v.Repos[0].FullName)
	}

	// The tokens asked for exactly what each call needs: metadata to list,
	// contents read to probe, and never a write.
	e.fake.Mu.Lock()
	reqs := e.fake.TokenRequests
	e.fake.Mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("token requests %+v", reqs)
	}
	for _, r := range reqs {
		for p, level := range r.Permissions {
			if level != "read" {
				t.Errorf("a catalog token asked for %s:%s", p, level)
			}
		}
	}
	if reqs[0].Permissions["metadata"] != "read" || len(reqs[0].Permissions) != 1 {
		t.Errorf("the listing token asked for %v", reqs[0].Permissions)
	}

	evs, _ := e.log.Since(context.Background(), 0)
	if len(evs) != 1 || evs[0].Kind != KindRefreshed || string(evs[0].Data) != `{"added":5,"count":5,"removed":0}` {
		t.Errorf("events %+v", evs)
	}
}

// A second refresh with nothing pushed probes nothing; a push re-probes that
// repository alone.
func TestRefreshProbesOnlyWhatChanged(t *testing.T) {
	e := newEnv(t)
	e.refresh(t)
	before := e.fake.Count("GET /repos/")
	if before == 0 {
		t.Fatal("control: the first refresh probed nothing")
	}
	e.refresh(t)
	if n := e.fake.Count("GET /repos/") - before; n != 0 {
		t.Errorf("an unchanged refresh made %d contents requests", n)
	}

	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos[3].PushedAt = t0 // krelinga/bare gains a push…
	e.fake.Installations[0].Repos[3].Files = append(e.fake.Installations[0].Repos[3].Files, ".devcontainer.json")
	e.fake.Requests = nil
	e.fake.Mu.Unlock()
	e.refresh(t)
	for _, r := range e.fake.Requests {
		if strings.HasPrefix(r, "GET /repos/") && !strings.HasPrefix(r, "GET /repos/krelinga/bare/") {
			t.Errorf("probed an unchanged repository: %s", r)
		}
	}
	if r := e.list(t)["krelinga/bare"]; r.HasDevcontainer == nil || !*r.HasDevcontainer {
		t.Error("…and its new devcontainer.json was not noticed")
	}
}

// §12: a repository the installation stops covering is dropped — unless a
// workspace holds it, when it stays, marked removed, with its workspace.
func TestRemovedRepositories(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.refresh(t)
	if _, err := e.cat.DB.ExecContext(ctx, `INSERT INTO workspace (id, repository_id, host_path, branch, state)
		VALUES ('01JAAAAAAAAAAAAAAAAAAAAAAA', 1, '/srv/drydock/ws/x/repo', 'main', 'running')`); err != nil {
		t.Fatal(err)
	}
	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = e.fake.Installations[0].Repos[2:] // drop plain (held) and rootfile (not)
	e.fake.Mu.Unlock()

	res := e.refresh(t)
	if res.Removed != 2 || res.Count != 3 {
		t.Errorf("result %+v", res)
	}
	got := e.list(t)
	if _, ok := got["krelinga/rootfile"]; ok {
		t.Error("a removed repository nobody holds is still listed")
	}
	held, ok := got["krelinga/plain"]
	if !ok || !held.Removed || held.Workspace == nil || held.Workspace.State != "running" {
		t.Errorf("the held repository: %+v (present %v)", held, ok)
	}
	if _, ok := got["krelinga/nested"]; !ok || got["krelinga/nested"].Removed {
		t.Error("control: a still-listed repository is missing or marked removed")
	}

	// And it comes back cleanly when the installation covers it again.
	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = append(e.fake.Installations[0].Repos, githubtest.Repo{ID: 1, FullName: "krelinga/plain", DefaultBranch: "main"})
	e.fake.Mu.Unlock()
	e.refresh(t)
	if e.list(t)["krelinga/plain"].Removed {
		t.Error("a re-added repository is still marked removed")
	}
}

// A dropped repository that holds a secret grant is still deleted, and its
// grant with it. secret_grant references the repository row, so before this
// the delete failed on the foreign key and so did every refresh after it.
// Control: the grant of a repository that stays is untouched.
func TestRemovedRepositoryTakesItsSecretGrants(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.refresh(t)
	for _, q := range []string{
		`INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES ('s1', 'TEST_KEY', x'00', x'00', 'a test key')`,
		`INSERT INTO secret_grant (secret_id, repository_id) VALUES ('s1', 2), ('s1', 3)`,
	} {
		if _, err := e.cat.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = e.fake.Installations[0].Repos[2:] // drops rootfile (2)
	e.fake.Mu.Unlock()
	if _, err := e.cat.Refresh(ctx); err != nil {
		t.Fatalf("a refresh dropping a granted repository failed: %v", err)
	}
	if _, ok := e.list(t)["krelinga/rootfile"]; ok {
		t.Error("the dropped repository is still listed")
	}
	var grants []int64
	rows, _ := e.cat.DB.QueryContext(ctx, `SELECT repository_id FROM secret_grant ORDER BY repository_id`)
	for rows.Next() {
		var r int64
		rows.Scan(&r)
		grants = append(grants, r)
	}
	rows.Close()
	if len(grants) != 1 || grants[0] != 3 {
		t.Errorf("grants after the drop = %v; want [3]", grants)
	}
}

// §4: a dropped repository a workspace holds keeps its row and its grants
// (§12) only while it is held. Once the workspace is gone, the next refresh
// deletes both, and a re-added repository is granted nothing. The workspace
// row is deleted here by hand, as a release before this fix (or a delete cut
// off before its transaction) left it: the refresh's sweep is what is under
// test, and internal/server tests the delete's own path. Control: the grant
// of a repository that stays installed survives every refresh, and the
// held repository's grant survives the refresh that marks it removed.
func TestReleasedRepositoryGrantsDoNotRevive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	dropped := 0
	e.cat.GrantsDropped = func() { dropped++ }
	e.refresh(t)
	for _, q := range []string{
		`INSERT INTO workspace (id, repository_id, host_path, branch, state)
			VALUES ('01JAAAAAAAAAAAAAAAAAAAAAAA', 1, '/srv/drydock/ws/x/repo', 'main', 'running')`,
		`INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES ('s1', 'TEST_KEY', x'00', x'00', 'a test key')`,
		`INSERT INTO secret_grant (secret_id, repository_id) VALUES ('s1', 1), ('s1', 3)`,
		`INSERT INTO secret_access (secret_id, workspace_id, at) VALUES ('s1', '01JAAAAAAAAAAAAAAAAAAAAAAA', 'then')`,
		`INSERT INTO token_grant (id, workspace_id, repository_id, permissions) VALUES ('t1', '01JAAAAAAAAAAAAAAAAAAAAAAA', 1, '{}')`,
	} {
		if _, err := e.cat.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	grants := func() map[int64]bool {
		out := map[int64]bool{}
		rows, err := e.cat.DB.QueryContext(ctx, `SELECT repository_id FROM secret_grant`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r int64
			rows.Scan(&r)
			out[r] = true
		}
		return out
	}
	count := func(q string) int {
		var n int
		if err := e.cat.DB.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	plain := e.fake.Installations[0].Repos[0]

	// Dropped while held: kept, marked, granted as before.
	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = e.fake.Installations[0].Repos[1:]
	e.fake.Mu.Unlock()
	e.refresh(t)
	if g := grants(); !g[1] || !g[3] {
		t.Fatalf("control: grants while held = %v; want 1 and 3", g)
	}
	if !e.list(t)["krelinga/plain"].Removed {
		t.Fatal("control: the held repository is not marked removed")
	}

	// Released: the next refresh takes the row and its grant.
	if _, err := e.cat.DB.ExecContext(ctx, `DELETE FROM workspace`); err != nil {
		t.Fatal(err)
	}
	e.refresh(t)
	if g := grants(); g[1] || !g[3] {
		t.Errorf("grants after the release = %v; want 3 only", g)
	}
	if n := count(`SELECT count(*) FROM repository WHERE id = 1`); n != 0 {
		t.Errorf("the released repository's row is still there (%d)", n)
	}
	if dropped != 1 {
		t.Errorf("GrantsDropped called %d times; want 1, after the refresh that deleted a grant", dropped)
	}

	// Re-added: granted nothing; the other repository's grant still there.
	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = append(e.fake.Installations[0].Repos, plain)
	e.fake.Mu.Unlock()
	e.refresh(t)
	e.refresh(t)
	if g := grants(); g[1] || !g[3] {
		t.Errorf("grants after the re-add = %v; want 3 only", g)
	}
	if r, ok := e.list(t)["krelinga/plain"]; !ok || r.Removed {
		t.Errorf("control: the re-added repository is not listed as installed: %+v", r)
	}
	// The history outlives both.
	if n := count(`SELECT count(*) FROM secret_access`) + count(`SELECT count(*) FROM token_grant`); n != 2 {
		t.Errorf("history rows = %d; want secret_access and token_grant kept", n)
	}
	if dropped != 1 {
		t.Errorf("GrantsDropped called %d times; want still 1: nothing more was deleted", dropped)
	}
}

// A refresh that fails changes nothing and says why — in GitHub's words,
// which name the problem without carrying a credential.
func TestFailedRefreshKeepsTheCache(t *testing.T) {
	e := newEnv(t)
	e.refresh(t)
	e.fake.Mu.Lock()
	e.fake.Fail = func(r *http.Request) (int, string) {
		if r.URL.Path == "/installation/repositories" {
			return 401, "Bad credentials"
		}
		return 0, ""
	}
	e.fake.Mu.Unlock()
	if _, err := e.cat.Refresh(context.Background()); err == nil {
		t.Fatal("a failed refresh reported success")
	}
	if n := len(e.list(t)); n != 5 {
		t.Errorf("after a failed refresh %d repositories remain, want all 5", n)
	}
	evs, _ := e.log.Since(context.Background(), 0)
	last := evs[len(evs)-1]
	if last.Kind != KindRefreshFailed || !strings.Contains(last.Message, "401 (Bad credentials)") {
		t.Errorf("last event %+v", last)
	}
	// A page loaded after the event still learns of it: the list carries the
	// failure, and a successful refresh clears it again.
	v, _ := e.cat.List(context.Background())
	if v.LastRefreshError == nil || v.LastRefreshError.Message != last.Message || !v.LastRefreshError.At.Equal(e.clock.Now()) {
		t.Errorf("after a failed refresh, last_refresh_error = %+v", v.LastRefreshError)
	}
	e.fake.Mu.Lock()
	e.fake.Fail = nil
	e.fake.Mu.Unlock()
	e.refresh(t)
	if v, _ := e.cat.List(context.Background()); v.LastRefreshError != nil {
		t.Errorf("a successful refresh left last_refresh_error = %+v", v.LastRefreshError)
	}
}

// A probe that errors leaves the answer unknown — never "no dev container" —
// and is asked again next time.
func TestFailedProbeIsUnknownAndRetried(t *testing.T) {
	e := newEnv(t)
	e.fake.Fail = func(r *http.Request) (int, string) {
		if strings.HasPrefix(r.URL.Path, "/repos/krelinga/bare/") {
			return 502, "Server Error"
		}
		return 0, ""
	}
	e.refresh(t)
	if r := e.list(t)["krelinga/bare"]; r.HasDevcontainer != nil {
		t.Errorf("a failed probe recorded %v", *r.HasDevcontainer)
	}
	e.fake.Mu.Lock()
	e.fake.Fail = nil
	e.fake.Mu.Unlock()
	e.refresh(t)
	if r := e.list(t)["krelinga/bare"]; r.HasDevcontainer == nil || *r.HasDevcontainer {
		t.Errorf("the retried probe: %v", r.HasDevcontainer)
	}
}

// One failure among the probe's requests is enough to make the answer
// unknown: here the .devcontainer listing errors while .devcontainer.json is
// merely absent, and concluding "no dev container" from the absence alone
// would badge a repository nobody actually checked.
func TestPartlyFailedProbeIsUnknown(t *testing.T) {
	e := newEnv(t)
	e.fake.Fail = func(r *http.Request) (int, string) {
		if r.URL.Path == "/repos/krelinga/bare/contents/.devcontainer" {
			return 502, "Server Error"
		}
		return 0, ""
	}
	e.refresh(t)
	if r := e.list(t)["krelinga/bare"]; r.HasDevcontainer != nil {
		t.Errorf("a half-failed probe recorded %v", *r.HasDevcontainer)
	}
	// Control: the other repositories were probed to a definite answer.
	if r := e.list(t)["krelinga/rootfile"]; r.HasDevcontainer == nil || !*r.HasDevcontainer {
		t.Error("control: an unaffected repository has no answer")
	}
}

// Refreshes asked for at once never overlap: a manual refresh during
// another is answered by one more that begins after it ends, never by the
// one running (which may have listed before it was asked), and every caller
// asking meanwhile shares that one. (Before life.Coalescer the second joined
// the first and returned an empty Result: answered by a refresh begun before
// it was asked, the join #83 and the study's R3 removed.)
func TestConcurrentRefreshesNeverOverlap(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	release := make(chan struct{})
	entered := h.arm("/app/installations", func(*http.Request) { <-release })
	first := make(chan error, 1)
	go func() { _, err := e.cat.Refresh(context.Background()); first <- err }()
	<-entered
	later := make(chan Result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := e.cat.Refresh(context.Background())
			if err != nil {
				t.Error(err)
			}
			later <- res
		}()
	}
	waitAsked(t, e, 3) // both later callers have asked
	if n := e.fake.Count("GET /app/installations"); n != 0 {
		t.Errorf("control: %d listings reached GitHub while the first was held", n)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if res := <-later; res.Count != 5 {
			t.Errorf("a caller during the first refresh got %+v; want the next refresh's result", res)
		}
	}
	if n := e.fake.Count("GET /app/installations"); n != 2 {
		t.Errorf("%d refreshes ran; want the first and one more for both later callers", n)
	}
}

// waitAsked waits until n refreshes have been asked for in all.
func waitAsked(t *testing.T, e *env, n life.Ticket) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.cat.w.Asked() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d refreshes were asked for", e.cat.w.Asked(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunRefreshesPeriodically(t *testing.T) {
	e := newEnvBooting(t)
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for e.fake.Count("GET /app/installations") < n || e.clock.Waiting() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("refresh %d never happened", n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor(1) // at once, at startup
	e.clock.Advance(14 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n := e.fake.Count("GET /app/installations"); n != 1 {
		t.Errorf("refreshed %d times before 15 minutes", n)
	}
	e.clock.Advance(time.Minute)
	waitFor(2)
	if late := e.group.Wait(nil); late != nil {
		t.Errorf("stragglers %v", late)
	}
}

// The canary sweep (testing §4.2): every token the fake issued is absent
// from the database file's raw bytes, and so is the App key.
func TestNoTokenReachesTheDatabase(t *testing.T) {
	e := newEnv(t)
	e.refresh(t)
	e.fake.Fail = func(r *http.Request) (int, string) { return 500, "boom" }
	e.cat.Refresh(context.Background()) // a failure path writes an event too
	e.cat.DB.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
	var raw []byte
	for _, p := range []string{e.dbPath, e.dbPath + "-wal"} {
		b, _ := os.ReadFile(p)
		raw = append(raw, b...)
	}
	if !bytes.Contains(raw, []byte("krelinga/nested")) {
		t.Fatal("control: the refresh wrote nothing, so the sweep proves nothing")
	}
	tokens := e.fake.IssuedTokens()
	if len(tokens) == 0 {
		t.Fatal("control: no tokens were issued")
	}
	for _, tok := range tokens {
		if bytes.Contains(raw, []byte(tok)) {
			t.Errorf("an installation token reached the database")
		}
	}
	keyBody := strings.Split(string(githubtest.KeyPEM(t)), "\n")[2]
	if bytes.Contains(raw, []byte(keyBody)) {
		t.Error("the App key reached the database")
	}
}
