package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/sys"
)

// blankedSource serves the Spike 00 tombstone: present, both tokens empty.
type blankedSource struct {
	mu    sync.Mutex
	calls int
	creds []byte
}

func (b *blankedSource) Credentials(context.Context) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.creds, nil
}

func (b *blankedSource) AuthStatus(context.Context) ([]byte, error) {
	return []byte(`{"loggedIn":false}`), nil
}

// TestClaudeIdentityEndToEnd: GET /api/auth/claude through the real gate and
// socket. The watch runs at boot, so the stored verdict appears without
// anyone asking; the body has both halves, `login` null until the handshake
// exists; and POST /api/auth/claude/check runs another check (the counter
// moves). Unauthenticated, both are 401 like every route.
func TestClaudeIdentityEndToEnd(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "credentials", "blanked.json"))
	if err != nil {
		t.Fatal(err)
	}
	src := &blankedSource{creds: b}
	srv.Identity.Source = src
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}

	if resp := r.do(t, req{method: "GET", path: "/api/auth/claude"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("signed out: GET /api/auth/claude = %d; want 401", resp.StatusCode)
	}
	cookie := r.signIn(t)

	var body map[string]json.RawMessage
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := r.do(t, req{method: "GET", path: "/api/auth/claude", cookie: cookie})
		if resp.StatusCode != 200 {
			t.Fatalf("GET /api/auth/claude = %d", resp.StatusCode)
		}
		raw, _ := io.ReadAll(resp.Body)
		body = nil
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var id struct{ State *string }
		json.Unmarshal(body["identity"], &id)
		if id.State != nil {
			if *id.State != "blanked" {
				t.Fatalf("state = %s; want blanked", *id.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no verdict within 5s: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The supervisor reads the same verdict, through the watch: blanked is
	// a fleet under which no session server is started (§7.3, §8).
	if st, known := srv.Supervisor.Identity(context.Background()); !known || st != "blanked" {
		t.Errorf("the supervisor sees identity %q (known %v); want blanked", st, known)
	}
	if login, ok := body["login"]; !ok || string(login) != "null" {
		t.Errorf("login = %s (present %v); want null until the handshake exists", login, ok)
	}
	var id map[string]json.RawMessage
	json.Unmarshal(body["identity"], &id)
	for _, k := range []string{"state", "account_email", "expires_at", "logged_in_at", "last_checked_at", "volume", "check_error"} {
		if _, ok := id[k]; !ok {
			t.Errorf("identity lacks %q: %v", k, id)
		}
	}
	if string(id["volume"]) != `"`+cfg.ClaudeVolume+`"` {
		t.Errorf("volume = %s", id["volume"])
	}

	if resp := r.do(t, req{method: "POST", path: "/api/auth/claude/check", cookie: cookie}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("check without Origin = %d; want 403", resp.StatusCode)
	}
	before := func() int { src.mu.Lock(); defer src.mu.Unlock(); return src.calls }()
	if resp := r.do(t, req{method: "POST", path: "/api/auth/claude/check", cookie: cookie, origin: uiOrigin}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("check = %d; want 202", resp.StatusCode)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		src.mu.Lock()
		n := src.calls
		src.mu.Unlock()
		if n > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("POST /api/auth/claude/check ran no check")
		}
	}
	// The check found the same blanked login it found at boot, and is
	// still answered: an auth.identity_checked, the event "Check now"
	// settles on (frontend §4.2). Without it the button never comes back.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		evs, err := srv.Events.Since(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		var checked, changed int
		for _, e := range evs {
			switch e.Kind {
			case identity.KindChecked:
				checked++
			case identity.KindIdentity:
				changed++
			}
		}
		if checked == 1 && changed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after an unchanged requested check: %d %s and %d %s; want 1 and 1",
				checked, identity.KindChecked, changed, identity.KindIdentity)
		}
	}
	// Once the watch has shut down nothing would answer a check, so the
	// route refuses it rather than accepting it; the 202 above is the control.
	if late := srv.identityWork.Load().Wait(time.After(5 * time.Second)); late != nil {
		t.Fatalf("the watch did not stop: %v", late)
	}
	if resp := r.do(t, req{method: "POST", path: "/api/auth/claude/check", cookie: cookie, origin: uiOrigin}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("check after the watch shut down = %d; want 503", resp.StatusCode)
	}
}

// slowCredentials holds the next credential read it is armed for, as a
// helper container a cancelled `docker run` client leaves behind outlasts
// its context, and watches the database while it holds — until its context
// has been over for heldAfterCancel, or until the database closes under it.
// Unarmed it answers "no file": absent.
type slowCredentials struct {
	db       *sql.DB
	mu       sync.Mutex
	armed    bool
	entered  chan struct{}
	pings    atomic.Int64
	closed   atomic.Bool
	finished atomic.Bool
}

func (s *slowCredentials) Credentials(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	held := s.armed
	s.armed = false
	s.mu.Unlock()
	if !held {
		return nil, nil
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
		if ctx.Err() != nil {
			if cancelledAt.IsZero() {
				cancelledAt = time.Now()
			}
			if time.Since(cancelledAt) >= heldAfterCancel {
				break
			}
		}
	}
	return nil, &identity.ReadError{Problem: identity.ProblemDocker, Detail: "held"}
}

func (s *slowCredentials) AuthStatus(context.Context) ([]byte, error) {
	return []byte(`{"loggedIn":false}`), nil
}

// TestShutdownEndsATriggeredIdentityCheck is the identity watch's half of
// #83: a check POST /api/auth/claude/check asked for runs in Serve's work,
// which shutdown stops and waits for before the database closes, so the
// check neither outlives Serve nor touches the database after it. The read
// it is held in outlasts its context and watches the database meanwhile, so
// what is pinned is the order: the check ends before the database closes.
// The control is the boot check, which stored absent with the database open.
func TestShutdownEndsATriggeredIdentityCheck(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	slow := &slowCredentials{db: srv.DB.DB, entered: make(chan struct{})}
	srv.Identity.Source = slow
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if v, err := srv.Identity.Read(context.Background()); err == nil && v.State != nil {
			if *v.State != identity.Absent {
				t.Fatalf("control: the boot check stored %s; want absent", *v.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control: the boot check stored nothing")
		}
	}

	cookie := r.signIn(t)
	slow.mu.Lock()
	slow.armed = true
	slow.mu.Unlock()
	if resp := r.do(t, req{method: "POST", path: "/api/auth/claude/check", cookie: cookie, origin: uiOrigin}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/auth/claude/check = %d; want 202", resp.StatusCode)
	}
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("control: the triggered check never read the volume")
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
		t.Error("Serve closed the database while the triggered check was still running")
	}
	if !slow.finished.Load() {
		t.Error("Serve returned with the triggered check still running")
	}
	if slow.pings.Load() == 0 {
		t.Error("control: the held read never found the database open")
	}
}
