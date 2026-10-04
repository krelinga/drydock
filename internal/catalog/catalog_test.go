package catalog

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

type env struct {
	cat    *Catalog
	fake   *githubtest.Fake
	clock  *sys.FakeClock
	log    *events.Log
	dbPath string
}

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func newEnv(t *testing.T) *env {
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
	return &env{
		cat: &Catalog{DB: db.DB, Events: log, Clock: clock,
			GitHub: &github.Client{AppID: 5189455, Key: key, BaseURL: f.URL, Clock: clock}},
		fake: f, clock: clock, log: log, dbPath: path,
	}
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

// A manual refresh during the periodic one joins it instead of racing it.
func TestConcurrentRefreshesShareOne(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var once sync.Once
	entered := make(chan struct{})
	e.fake.Fail = func(r *http.Request) (int, string) {
		if r.URL.Path == "/app/installations" {
			once.Do(func() { close(entered) })
			e.fake.Mu.Unlock() // let other requests through while this one waits
			<-release
			e.fake.Mu.Lock()
		}
		return 0, ""
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.cat.Refresh(context.Background()) }()
	<-entered
	wg.Add(1)
	go func() { defer wg.Done(); e.cat.Refresh(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // give the second a chance to (wrongly) start its own
	close(release)
	wg.Wait()
	if n := e.fake.Count("GET /app/installations"); n != 1 {
		t.Errorf("%d refreshes ran; want the second to join the first", n)
	}
}

func TestRunRefreshesPeriodically(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.cat.Run(ctx, nil); close(done) }()
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
	cancel()
	<-done
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
