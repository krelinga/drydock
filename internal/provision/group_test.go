package provision

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/workspace"
)

// TestNoJobStartsOnceTheGroupStops: once the provisioner's group is stopping
// — Serve's shutdown — every request for a job is refused ErrShuttingDown
// (the routes' 503 unavailable), and refused before anything is written for
// it: no row for a create, no move to building for a rebuild or a start, no
// move to deleting for a delete, no cleared annotation for a stop. The
// control is the same kind of request accepted, and its job run, just before
// the stop.
func TestNoJobStartsOnceTheGroupStops(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	restarted := 0
	e.p.SupervisorRestart = func(context.Context, string) error { restarted++; return nil }
	v := e.running(t, alpha)

	// Control: admitted and run while the group is live.
	if err := e.p.RestartSupervisor(ctx, v.ID); err != nil {
		t.Fatalf("control: restart = %v", err)
	}
	e.p.idle()
	if restarted != 1 {
		t.Fatalf("control: the restart's job ran %d times", restarted)
	}

	e.g.Stop()
	mark := e.latest(t)
	for _, c := range []struct {
		name string
		ask  func() error
	}{
		{"create", func() error { _, err := e.p.Create(ctx, plain, ""); return err }},
		{"start", func() error { return e.p.Start(ctx, v.ID) }},
		{"rebuild", func() error { return e.p.Rebuild(ctx, v.ID) }},
		{"stop", func() error { return e.p.Stop(ctx, v.ID) }},
		{"delete", func() error { return e.p.Delete(ctx, v.ID, "krelinga/alpha") }},
		{"restart", func() error { return e.p.RestartSupervisor(ctx, v.ID) }},
	} {
		if err := c.ask(); !errors.Is(err, ErrShuttingDown) {
			t.Errorf("%s after the stop = %v, want ErrShuttingDown", c.name, err)
		}
	}
	e.p.idle()
	if restarted != 1 {
		t.Errorf("a restart's job ran after the stop (%d runs)", restarted)
	}
	if got := e.view(t, v.ID); got.State != workspace.Running {
		t.Errorf("a refused request moved the workspace to %s", got.State)
	}
	if vs, _ := e.p.Workspaces.Views(ctx); len(vs) != 1 {
		t.Errorf("a refused create left a row: %d workspaces", len(vs))
	}
	if latest := e.latest(t); latest != mark {
		t.Errorf("refused requests wrote %d events", latest-mark)
	}
	if late := e.shutdown(5 * time.Second); late != nil {
		t.Errorf("still running after the stop: %v", late)
	}
}

// TestARunInFlightAtShutdownEndsBeforeTheWait: the group's Wait — what Serve
// waits on before the database closes — returns only once a run cut off by
// the stop has written its interrupted step, saying Drydock shut down, and
// its workspace.job end, cancelled, the workspace's last event. Step 8 is
// where it is cut off, so its move is no move (a step-8 failure leaves the
// workspace running) and the end is endJob's own write; the step is slow to
// return, and asks the database as it does, so a Wait that did not wait for
// the job would return before either. The control is the run at step 8, its
// end unwritten, before the stop.
func TestARunInFlightAtShutdownEndsBeforeTheWait(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	rec := &jobRecorder{}
	e.p.jobEnded = rec.record
	entered := make(chan struct{})
	var once sync.Once
	dbAtReturn := make(chan error, 1)
	e.p.StartSupervisor = func(ctx context.Context, w workspace.Workspace) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		time.Sleep(300 * time.Millisecond)
		dbAtReturn <- e.p.Workspaces.DB.PingContext(context.Background())
		return ctx.Err()
	}
	mark := e.latest(t)
	w, err := e.p.Create(ctx, alpha, "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the run never reached step 8")
	}
	if ends := e.jobEnds(t, w.ID, mark); len(ends) != 0 {
		t.Fatalf("control: the run in flight has ended already: %+v", ends)
	}

	if late := e.shutdown(time.Minute); late != nil {
		t.Fatalf("still running after the wait: %v", late)
	}
	select {
	case err := <-dbAtReturn:
		if err != nil {
			t.Errorf("the database was unusable as the cut-off step returned: %v", err)
		}
	default:
		t.Fatal("the wait returned before the cut-off step did")
	}
	v := e.view(t, w.ID)
	if st := v.Steps[workspace.StepSessionServer]; st.Status != "failed" || !strings.Contains(st.Detail, "Drydock shut down") {
		t.Errorf("step 8 after the shutdown: %+v", st)
	}
	if v.State != workspace.Running {
		t.Errorf("a step-8 failure moved the workspace to %s", v.State)
	}
	e.endsOnce(t, w.ID, mark, JobCreate, workspace.JobCancelled)
	if got := rec.all(); len(got) != 1 || got[0] != "create:own" {
		t.Errorf("job ends %v, want endJob's own write", got)
	}
}

// TestADeletePreemptsUnderTheGroup: a delete still cancels the run in
// flight, waits for it and then runs, with both jobs in the group: the run
// ends cancelled, the delete ok, the row is gone — and the group then has
// nothing left running, so neither job's place outlives it. The control is
// the run in flight, still building, before the delete.
func TestADeletePreemptsUnderTheGroup(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	e.cli.up = registeringUp(e.cli.dir) + ` >/dev/null; exec sleep 30`
	e.wire(t)
	mark := e.latest(t)
	w, err := e.p.Create(ctx, alpha, "")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); len(e.containers(t, w.ID)) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("up never started")
		}
	}
	if got := e.view(t, w.ID); got.State != workspace.Building {
		t.Fatalf("control: the run in flight is %s", got.State)
	}
	start := time.Now()
	if err := e.p.Delete(ctx, w.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("the delete took %s: it waited out up rather than cancelling it", took)
	}
	if _, err := e.p.Workspaces.Get(ctx, w.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("after the delete: %v", err)
	}
	ends := e.jobEnds(t, w.ID, mark)
	if len(ends) != 2 || ends[0].Kind != JobCreate || ends[0].Outcome != workspace.JobCancelled ||
		ends[1].Kind != JobDelete || ends[1].Outcome != workspace.JobOK {
		t.Fatalf("job ends %+v, want create:cancelled then delete:ok", ends)
	}
	if late := e.shutdown(5 * time.Second); late != nil {
		t.Errorf("the group still runs %v after both jobs ended", late)
	}
}
