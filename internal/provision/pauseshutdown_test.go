package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/broker"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestARePauseOutlastingShutdownOpensNoSocket: a stop of a paused container,
// cut off by shutdown after its unpause, pauses the container again and then
// gives GitHub access back. Here the re-pause is still running when the
// group's wait gives up, and the broker is closed (Serve's CloseAll) before
// it ends — so its reopen comes after CloseAll, and must open nothing: a
// socket left now would be served by no one and handed to the next boot's
// container, which this process paused. The controls: the stop closed the
// socket before its unpause, and the wait did give up on the stop.
func TestARePauseOutlastingShutdownOpensNoSocket(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	dir, err := os.MkdirTemp("", "dd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	b := &broker.Broker{Dir: filepath.Join(dir, "sock")}
	t.Cleanup(b.CloseAll)
	e.p.Broker = b
	if err := b.Open(ctx, v.ID); err != nil { // as step 5 opened it
		t.Fatal(err)
	}
	e.setStatus(t, v.ID, "paused")
	entered := make(chan struct{})
	e.p.StopSupervisor = func(ctx context.Context, w workspace.Workspace) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	hold := filepath.Join(e.cli.dir, "docker-hold-pause")
	os.WriteFile(hold, nil, 0o600)
	t.Cleanup(func() { os.Remove(hold) })
	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	<-entered
	if b.Serving(v.ID) {
		t.Fatal("control: the stop unpaused the container with its access open")
	}

	if late := e.shutdown(300 * time.Millisecond); len(late) != 1 {
		t.Fatalf("control: the wait left %v running; want the stop, held in its re-pause", late)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(e.cli.dir, "in-pause")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stop never reached its re-pause")
		}
	}
	b.CloseAll() // Serve's, once the wait has given up
	os.Remove(hold)
	e.p.idle()

	if _, err := os.Lstat(b.SocketPath(v.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a socket exists after the broker closed: %v", err)
	}
	if b.Serving(v.ID) {
		t.Error("the workspace is served after the broker closed")
	}
	for _, st := range e.containers(t, v.ID) {
		if st != "paused" {
			t.Errorf("the container is %s; the re-pause should have paused it", st)
		}
	}
	if d := deref(e.view(t, v.ID).StateDetail); !strings.Contains(d, RepausedClosedSentence) || !strings.Contains(d, RestoreAccessHint) {
		t.Errorf("the stop's detail does not say access stayed closed: %q", d)
	}
}

// TestBootLeavesAPausedContainerAlone: boot's follow-ups give a running
// workspace whose container is paused neither a broker socket nor a session
// server — the operator's pause, or one a stop cut off by shutdown put back,
// stands until a stop and a start or a rebuild. The control is the same
// workspace unpaused, which gets both.
func TestBootLeavesAPausedContainerAlone(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	for _, paused := range []bool{true, false} {
		status := "running"
		if paused {
			status = "paused"
		}
		e.setStatus(t, v.ID, status)
		b := &stubBroker{}
		var started []string
		p := &Provisioner{Workspaces: e.p.Workspaces, Events: e.log, Broker: b,
			Cloner: e.p.Cloner, Containers: e.p.Containers, Logf: t.Logf,
			StartSupervisor: func(_ context.Context, w workspace.Workspace) error {
				started = append(started, w.ID)
				return nil
			}}
		runIn(t, p)
		if err := p.ReopenSockets(ctx); err != nil {
			t.Fatal(err)
		}
		if err := p.ResumeSupervisors(ctx); err != nil {
			t.Fatal(err)
		}
		if got := b.isOpen(v.ID); got == paused {
			t.Errorf("paused=%v: socket open %v", paused, got)
		}
		if got := len(started) == 1; got == paused {
			t.Errorf("paused=%v: session servers started %v", paused, started)
		}
	}
}
