package server

import (
	"context"
	"database/sql"
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

// slowListing holds the next GitHub listing it is armed for, in the client,
// and watches the database while it holds: until its context has been over
// for heldAfterCancel, the way a slow step outlasts its cancel, or until the
// database closes under it. A Serve that waits for the refresh before closing
// the database keeps it open throughout, because it is waiting on this very
// listing; one that closes it first — the wait deleted, moved after the
// close, or the refresh never cancelled — closes it while the listing is
// still held.
type slowListing struct {
	db       *sql.DB
	mu       sync.Mutex
	armed    bool
	entered  chan struct{}
	pings    atomic.Int64 // pings that found the database open while held
	closed   atomic.Bool  // the database closed while the listing was held
	finished atomic.Bool  // the held listing has returned
}

// heldAfterCancel is well inside catalogShutdownWait, so a correct Serve
// waits it out rather than giving up on the refresh.
const heldAfterCancel = 2 * time.Second

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
	var cancelledAt time.Time
	for limit := time.Now().Add(time.Minute); time.Now().Before(limit); time.Sleep(5 * time.Millisecond) {
		if err := s.db.PingContext(context.Background()); err != nil {
			s.closed.Store(true)
			break
		}
		s.pings.Add(1)
		if req.Context().Err() != nil {
			if cancelledAt.IsZero() {
				cancelledAt = time.Now()
			}
			if time.Since(cancelledAt) >= heldAfterCancel {
				break
			}
		}
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
// database is closed"). The listing it is held in outlasts its context, in
// the client — a hold in the fake GitHub would see the client give up at
// once — and watches the database meanwhile, so what is pinned is the order:
// the refresh ends before the database closes, not merely before Serve
// returns.
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
	slow := &slowListing{db: srv.DB.DB, entered: make(chan struct{})}
	srv.Catalog.GitHub.HTTP = &http.Client{Transport: slow}

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
	if slow.closed.Load() {
		// The bug: the database closed under the refresh, whose next step
		// would read it ("sql: database is closed").
		t.Error("Serve closed the database while the triggered refresh was still running")
	}
	if !slow.finished.Load() {
		t.Error("Serve returned with the triggered refresh still running")
	}
	if slow.pings.Load() == 0 {
		t.Error("control: the held listing never found the database open")
	}
	if !slow.closed.Load() && slow.finished.Load() {
		before := f.Count("")
		if err := srv.DB.PingContext(context.Background()); err == nil {
			t.Error("control: the database is still open after Serve returned")
		}
		if n := f.Count(""); n != before {
			t.Errorf("GitHub saw %d requests after Serve returned", n-before)
		}
	}
}
