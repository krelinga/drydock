package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/sys"
)

// TestShutdownEndsATriggeredCatalogRefresh: a refresh POST /api/repos/refresh
// started is ended by Serve's shutdown and waited for, so it neither outlives
// Serve nor reaches GitHub or the database after it. The GitHub request it is
// blocked in must have been abandoned by the time Serve returns; before the
// fix it ran under context.Background() and stayed open, and once let go it
// went on to the database Serve had closed ("sql: database is closed").
func TestShutdownEndsATriggeredCatalogRefresh(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	f := githubtest.New(t, 5189455, time.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 1, FullName: "krelinga/alpha", DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{".devcontainer/devcontainer.json"}},
	}}}
	keyPath := filepath.Join(dir, "app.pem")
	os.WriteFile(keyPath, githubtest.KeyPEM(t), 0o400)
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 5189455, keyPath, f.URL
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}

	// Armed below, once the boot refresh is over: the next listing blocks
	// until it is abandoned (its request context ends) or released.
	var (
		armed     bool
		entered   = make(chan struct{})
		abandoned = make(chan struct{})
		release   = make(chan struct{})
		once      sync.Once
	)
	f.Fail = func(r *http.Request) (int, string) { // f.Mu held
		if r.URL.Path != "/app/installations" || !armed {
			return 0, ""
		}
		armed = false
		close(entered)
		f.Mu.Unlock() // let other requests through while this one waits
		select {
		case <-r.Context().Done():
			close(abandoned)
		case <-release:
		}
		f.Mu.Lock()
		return 0, ""
	}
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket), gh: f}

	// The boot refresh has started; a refresh of the test's own then either
	// joins it or runs after it, and either way returns with none running,
	// so the POST below leads a refresh of its own rather than joining.
	deadline := time.Now().Add(10 * time.Second)
	for f.Count("GET /app/installations") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the boot refresh never started")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := srv.Catalog.Refresh(context.Background()); err != nil {
		t.Fatalf("a refresh before shutdown: %v", err)
	}
	// Positive control: a refresh before shutdown completes and writes.
	v, err := srv.Catalog.List(context.Background())
	if err != nil || len(v.Repos) != 1 {
		t.Fatalf("control: the catalog before shutdown is %+v, %v; want one repository", v, err)
	}

	cookie := r.signIn(t)
	f.Mu.Lock()
	armed = true
	f.Mu.Unlock()
	resp := r.do(t, req{method: "POST", path: "/api/repos/refresh", origin: uiOrigin, cookie: cookie})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/repos/refresh = %d; want 202", resp.StatusCode)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("control: the triggered refresh never reached GitHub")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return")
	}
	before := len(requests(f))
	select {
	case <-abandoned:
	case <-time.After(10 * time.Second):
		// The bug: the refresh is still in GitHub, under a context nothing
		// ends, and once let go its next step reads the closed database.
		t.Fatal("Serve returned with the triggered refresh still in its GitHub request")
	}
	once.Do(func() { close(release) })
	if got := requests(f)[before:]; len(got) != 0 {
		t.Errorf("GitHub saw %v after Serve returned", got)
	}
}

func requests(f *githubtest.Fake) []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]string(nil), f.Requests...)
}
