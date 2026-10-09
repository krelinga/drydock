package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/broker"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// stragglingRun is a server whose first run is held at step 8 until its
// context ends — shutdown's — and then hands the workspace to after, as a
// job still ending would: a stop's re-pause giving GitHub access back opens
// the workspace's socket exactly so. It returns the running server and the
// workspace once the run is held.
func stragglingRun(t *testing.T, srv *Server, serve func() *running, after func(id string)) (*running, string) {
	t.Helper()
	entered := make(chan string, 1)
	var once sync.Once
	srv.Provisioner.StartSupervisor = func(ctx context.Context, w workspace.Workspace) error {
		once.Do(func() { entered <- w.ID })
		<-ctx.Done()
		after(w.ID)
		return ctx.Err()
	}
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
	case id := <-entered:
		return r, id
	case <-time.After(30 * time.Second):
		t.Fatal("the run never reached step 8")
	}
	return nil, ""
}

// TestTheBrokerOutlivesEveryJob pins shutdown's order: the broker's sockets
// close only after every job in Serve's work has ended. A job still ending
// within the wait — here a cut-off run that gives the workspace its socket
// back, as a stop's re-pause does — opens it, and CloseAll then closes it,
// so no socket is left once Serve returns. With CloseAll anywhere earlier
// (before the wait) the job's Open would find the broker
// closed: that is what the Open's own success asserts.
func TestTheBrokerOutlivesEveryJob(t *testing.T) {
	srv, serve := appServer(t, t.TempDir())
	opened := make(chan error, 1)
	r, id := stragglingRun(t, srv, serve, func(id string) {
		time.Sleep(200 * time.Millisecond) // well into shutdown
		srv.Broker.Close(id)
		opened <- srv.Broker.Open(context.Background(), id)
	})
	r.stop()
	select {
	case err := <-opened:
		if err != nil {
			t.Errorf("a job ending within shutdown's wait could not open its socket: %v; the broker closed before the jobs ended", err)
		}
	default:
		t.Fatal("Serve returned before the job ended")
	}
	if _, err := os.Lstat(srv.Broker.SocketPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a socket is left after Serve returned: %v", err)
	}
}

// TestAJobOutlastingShutdownOpensNoSocket: a job still running when work's
// wait gives up — its deadline is on the server's clock, moved here — is
// left behind, and Serve closes the broker and returns. When the job then
// gives the workspace its socket back, the broker refuses: no socket exists
// after Serve has returned and the job has ended. The control is the job
// still running as Serve returns.
func TestAJobOutlastingShutdownOpensNoSocket(t *testing.T) {
	srv, serve := appServer(t, t.TempDir())
	clock := sys.NewFakeClock(time.Now())
	srv.clock = clock // only shutdown's wait for work reads it
	release := make(chan struct{})
	opened := make(chan error, 1)
	r, id := stragglingRun(t, srv, serve, func(id string) {
		<-release
		opened <- srv.Broker.Open(context.Background(), id)
	})
	stopped := make(chan struct{})
	go func() { r.stop(); close(stopped) }()
	for deadline := time.Now().Add(30 * time.Second); clock.Waiting() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("shutdown never began its wait for work")
		}
	}
	clock.Advance(workShutdownWait)
	select {
	case <-stopped:
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return once its wait gave up")
	}
	select {
	case <-opened:
		t.Fatal("control: the job ended before Serve returned")
	default:
	}
	close(release)
	select {
	case err := <-opened:
		if !errors.Is(err, broker.ErrClosed) {
			t.Errorf("Open after Serve returned = %v, want broker.ErrClosed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the job never reached its Open")
	}
	if _, err := os.Lstat(srv.Broker.SocketPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a socket exists after Serve returned: %v", err)
	}
}
