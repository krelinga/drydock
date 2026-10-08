package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/sys"
)

// slowListing holds the next GitHub listing it is armed for, in the client:
// past the moment its context ends, the way a slow step would, for a little
// while — or, if Serve returns first, until the test has looked. So a Serve
// that waits for the refresh returns after the listing has ended, and one
// that does not returns with it still held.
type slowListing struct {
	mu       sync.Mutex
	armed    bool
	entered  chan struct{}
	served   chan struct{} // Serve has returned
	looked   chan struct{} // the test has checked
	finished atomic.Bool   // the held listing has returned
}

func (s *slowListing) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	held := s.armed && req.URL.Path == "/app/installations"
	s.armed = s.armed && !held
	s.mu.Unlock()
	if !held {
		return http.DefaultTransport.RoundTrip(req)
	}
	defer s.finished.Store(true)
	close(s.entered)
	select {
	case <-req.Context().Done():
		select {
		case <-s.served:
			<-s.looked
		case <-time.After(200 * time.Millisecond):
		}
	case <-s.served:
		<-s.looked
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(req)
}

// TestShutdownEndsATriggeredCatalogRefresh: a refresh POST /api/repos/refresh
// started is ended by Serve's shutdown and waited for before the database
// closes, so it neither outlives Serve nor reaches GitHub or the database
// after it. Before the fix it ran under context.Background(), and once its
// GitHub call answered it went on to the database Serve had closed ("sql:
// database is closed"). The listing it is held in ends a moment after its
// context, in the client: a hold in the fake GitHub would see the client
// give up at once, and pass a Serve that closed the database without
// waiting for the refresh to end.
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
	slow := &slowListing{entered: make(chan struct{}), served: make(chan struct{}), looked: make(chan struct{})}
	srv.Catalog.GitHub.HTTP = &http.Client{Transport: slow}
	var looked sync.Once
	look := func() { looked.Do(func() { close(slow.looked) }) }
	t.Cleanup(look)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket), gh: f}

	// The boot refresh has started; a refresh of the test's own then either
	// joins it or runs after it, and either way returns with none running,
	// so the POST below leads a refresh of its own.
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
	slow.mu.Lock()
	slow.armed = true
	slow.mu.Unlock()
	resp := r.do(t, req{method: "POST", path: "/api/repos/refresh", origin: uiOrigin, cookie: cookie})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/repos/refresh = %d; want 202", resp.StatusCode)
	}
	select {
	case <-slow.entered:
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
	close(slow.served)
	if !slow.finished.Load() {
		// The bug: Serve closed the database with the refresh still in its
		// GitHub call, and once that returns its next step reads the closed
		// database.
		t.Error("Serve returned, and closed the database, with the triggered refresh still running")
	}
	before := f.Count("")
	look()
	if !slow.finished.Load() {
		return
	}
	if n := f.Count(""); n != before {
		t.Errorf("GitHub saw %d requests after Serve returned", n-before)
	}
}
