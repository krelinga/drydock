package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
}
