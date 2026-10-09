package server

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/supervisor"
)

// heldSignal is a session supervisor's Runtime whose signals are held until
// their context ends — shutdown's — and then return slowly, asking the
// database as they do: a supervisor loop still ending.
type heldSignal struct {
	supervisor.Runtime
	db      *sql.DB
	entered chan struct{}
	once    sync.Once
	at      chan error // the database, as the first held signal returned
}

func (h *heldSignal) Signal(ctx context.Context, ws string, sig container.SessionSignal, pidFile string) (bool, error) {
	h.once.Do(func() { close(h.entered) })
	<-ctx.Done()
	time.Sleep(300 * time.Millisecond)
	select {
	case h.at <- h.db.PingContext(context.Background()):
	default:
	}
	return false, ctx.Err()
}

// TestShutdownWaitsForTheSessionSupervisors: every session supervisor's loop
// is in Serve's work, so shutdown waits for it before the database closes —
// there is no separate detach step with a bound of its own. The workspace's
// supervisor is held in its first docker call (the stop of any server left
// running, before a launch) until shutdown cancels it, and then returns
// slowly, asking the database. The control is the loop held before the
// shutdown, with the workspace running.
func TestShutdownWaitsForTheSessionSupervisors(t *testing.T) {
	srv, serve := appServer(t, t.TempDir())
	h := &heldSignal{Runtime: srv.Supervisor.Runtime, db: srv.DB.DB, entered: make(chan struct{}), at: make(chan error, 1)}
	srv.Supervisor.Runtime = h
	// No verdict on the volume yet, so the loop goes on to start a server
	// rather than wait for a sign-in.
	srv.Supervisor.Identity = func(context.Context) (string, bool) { return "", false }
	r := serve()
	r.client.Timeout = 30 * time.Second
	cookie := r.signIn(t)
	awaitCatalog(t, r, cookie)
	resp := r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("control: create = %d", resp.StatusCode)
	}
	select {
	case <-h.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("control: no supervisor loop reached its first docker call")
	}

	stopped := make(chan struct{})
	go func() { r.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return")
	}
	select {
	case err := <-h.at:
		if err != nil {
			t.Errorf("the database was closed while the supervisor's loop was ending: %v", err)
		}
	default:
		t.Fatal("Serve returned before the supervisor's loop had ended")
	}
	if late := srv.supervisorWork.Load().Wait(time.After(time.Second)); late != nil {
		t.Errorf("still running after Serve returned: %v", late)
	}
}
