package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// jobEnd is one workspace.job event, with where it sits in the workspace's
// own events.
type jobEnd struct {
	ID      int64
	Kind    string
	Outcome string
}

// jobRecorder records the jobEnded seam: per job, whether its last commit
// carried its end.
type jobRecorder struct {
	mu  sync.Mutex
	got []string // "kind:carried" or "kind:own"
}

func (r *jobRecorder) record(kind string, carried bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	how := "own"
	if carried {
		how = "carried"
	}
	r.got = append(r.got, kind+":"+how)
}

func (r *jobRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) == 0 {
		return ""
	}
	return r.got[len(r.got)-1]
}

func (r *jobRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// allEvents is the workspace's events, oldest first.
func (e *env) allEvents(t *testing.T, id string) []events.Event {
	t.Helper()
	evs, err := e.log.ForWorkspace(context.Background(), id, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs
}

// jobEnds is the workspace's workspace.job events after event id since, in order.
func (e *env) jobEnds(t *testing.T, id string, since int64) []jobEnd {
	t.Helper()
	var out []jobEnd
	for _, ev := range e.allEvents(t, id) {
		if ev.Kind != workspace.KindJob || ev.ID <= since {
			continue
		}
		var d workspace.JobData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, jobEnd{ev.ID, d.Kind, d.Outcome})
	}
	return out
}

// latest is the newest event id in the log.
func (e *env) latest(t *testing.T) int64 {
	t.Helper()
	id, err := e.log.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// endsOnce asserts that exactly one workspace.job event followed since, of
// kind and outcome, and that it is the workspace's newest event: written
// after every event the job wrote. It returns the event before it.
func (e *env) endsOnce(t *testing.T, id string, since int64, kind, outcome string) events.Event {
	t.Helper()
	ends := e.jobEnds(t, id, since)
	if len(ends) != 1 || ends[0].Kind != kind || ends[0].Outcome != outcome {
		t.Fatalf("job ends after %d: %+v, want one %s:%s", since, ends, kind, outcome)
	}
	all := e.allEvents(t, id)
	last := all[len(all)-1]
	if last.ID != ends[0].ID {
		t.Fatalf("the %s job's end (%d) is not its workspace's last event: %s %s (%d)",
			kind, ends[0].ID, last.Kind, last.Data, last.ID)
	}
	return all[len(all)-2]
}

// TestEveryJobEndsWithOneEvent: each kind of job the provisioner runs ends
// with exactly one workspace.job event, of its kind and outcome, written after
// every other event the job wrote; and where the job's last act is a move, an
// annotation or the row's removal, that commit carried it (the move and the
// end are one fact). The failed and stuck variants sit beside the successes
// they are the negatives of.
func TestEveryJobEndsWithOneEvent(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	rec := &jobRecorder{}
	e.p.jobEnded = rec.record

	// create: the run's last event is step 8's, not a move.
	mark := e.latest(t)
	v := e.running(t, alpha)
	prev := e.endsOnce(t, v.ID, mark, JobCreate, workspace.JobOK)
	if prev.Kind != workspace.KindStep || !strings.Contains(string(prev.Data), `"session_server"`) {
		t.Errorf("create: the event before the end is %s %s, want step 8's", prev.Kind, prev.Data)
	}
	if got := rec.last(); got != "create:own" {
		t.Errorf("create: %s", got)
	}

	// stop, failed: the annotation carries the end.
	os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-stop"), nil, 0o600)
	mark = e.latest(t)
	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	prev = e.endsOnce(t, v.ID, mark, JobStop, workspace.JobFailed)
	if prev.Kind != workspace.KindState || !strings.Contains(string(prev.Data), "The stop did not finish") {
		t.Errorf("failed stop: the event before the end is %s %s, want the annotation", prev.Kind, prev.Data)
	}
	if got := rec.last(); got != "stop:carried" {
		t.Errorf("failed stop: %s", got)
	}

	// stop: the move to stopped carries it.
	os.Remove(filepath.Join(e.cli.dir, "docker-fail-stop"))
	mark = e.latest(t)
	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	prev = e.endsOnce(t, v.ID, mark, JobStop, workspace.JobOK)
	if prev.Kind != workspace.KindState || !strings.Contains(string(prev.Data), `"state":"stopped"`) {
		t.Errorf("stop: the event before the end is %s %s, want the move to stopped", prev.Kind, prev.Data)
	}
	if got := rec.last(); got != "stop:carried" {
		t.Errorf("stop: %s", got)
	}

	// start.
	mark = e.latest(t)
	if err := e.p.Start(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	e.endsOnce(t, v.ID, mark, JobStart, workspace.JobOK)

	// rebuild, failed at up: the move to failed carries it.
	good := e.cli.up
	e.cli.up = upFailing("the build broke")
	e.wire(t)
	mark = e.latest(t)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	prev = e.endsOnce(t, v.ID, mark, JobRebuild, workspace.JobFailed)
	if prev.Kind != workspace.KindState || !strings.Contains(string(prev.Data), `"state":"failed"`) {
		t.Errorf("failed rebuild: the event before the end is %s %s, want the move to failed", prev.Kind, prev.Data)
	}
	if got := rec.last(); got != "rebuild:carried" {
		t.Errorf("failed rebuild: %s", got)
	}

	// rebuild.
	e.cli.up = good
	e.wire(t)
	mark = e.latest(t)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	e.endsOnce(t, v.ID, mark, JobRebuild, workspace.JobOK)

	// supervisor restart, ok and failed: its events are the supervisor's
	// own, so the end is a commit of its own after them.
	var restartErr error
	e.p.SupervisorRestart = func(ctx context.Context, id string) error {
		_, err := e.log.Emit(ctx, id, events.Error, workspace.KindSupervisor, "The session server could not be stopped.",
			workspace.SupervisorData{State: "degraded", Reason: "stop_failed"})
		if err != nil {
			return err
		}
		return restartErr
	}
	for _, failed := range []bool{false, true} {
		restartErr = nil
		outcome := workspace.JobOK
		if failed {
			// #86: the restart whose stop failed.
			restartErr = errors.New("stopping the session server: docker exec failed")
			outcome = workspace.JobFailed
		}
		mark = e.latest(t)
		if err := e.p.RestartSupervisor(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		prev = e.endsOnce(t, v.ID, mark, JobSupervisor, outcome)
		if prev.Kind != workspace.KindSupervisor {
			t.Errorf("supervisor (failed %v): the event before the end is %s", failed, prev.Kind)
		}
		if got := rec.last(); got != "supervisor:own" {
			t.Errorf("supervisor (failed %v): %s", failed, got)
		}
	}

	// delete, stuck: the annotation carries it; resumed: the removal does,
	// after workspace.gone.
	os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-rm"), nil, 0o600)
	mark = e.latest(t)
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	prev = e.endsOnce(t, v.ID, mark, JobDelete, workspace.JobFailed)
	if prev.Kind != workspace.KindState || !strings.Contains(string(prev.Data), "The delete stopped part-way") {
		t.Errorf("stuck delete: the event before the end is %s %s", prev.Kind, prev.Data)
	}
	if got := rec.last(); got != "delete:carried" {
		t.Errorf("stuck delete: %s", got)
	}
	os.Remove(filepath.Join(e.cli.dir, "docker-fail-rm"))
	mark = e.latest(t)
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	prev = e.endsOnce(t, v.ID, mark, JobDelete, workspace.JobOK)
	if prev.Kind != workspace.KindGone {
		t.Errorf("delete: the event before the end is %s, want workspace.gone", prev.Kind)
	}
	if got := rec.last(); got != "delete:carried" {
		t.Errorf("delete: %s", got)
	}

	// Every job above, and nothing else, ended.
	want := []string{"create:own", "stop:carried", "stop:carried", "start:own", "rebuild:carried", "rebuild:own",
		"supervisor:own", "supervisor:own", "delete:carried", "delete:carried"}
	if got := rec.all(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("job ends\n got %v\nwant %v", got, want)
	}
	if n := len(e.jobEnds(t, v.ID, 0)); n != len(want) {
		t.Errorf("%d workspace.job events for %d jobs", n, len(want))
	}
}

// TestAJobStoppedForApprovalAndItsApprovalEachEnd: a start that stops for a
// host-access approval has done what it could — ok, carried by the stop for
// approval — and the approval's run is a job of its own.
func TestAJobStoppedForApprovalAndItsApprovalEachEnd(t *testing.T) {
	ctx := context.Background()
	e, set := approvalEnv(t)
	rec := &jobRecorder{}
	e.p.jobEnded = rec.record
	v := e.running(t, alpha)
	set(withMerged(t, fixture(t, "read-configuration-merged-ok.json"), map[string]any{"privileged": true}))
	mark := e.latest(t)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	prev := e.endsOnce(t, v.ID, mark, JobRebuild, workspace.JobOK)
	if prev.Kind != workspace.KindState || !strings.Contains(string(prev.Data), `"approval"`) {
		t.Errorf("the event before the end is %s %s, want the stop for approval", prev.Kind, prev.Data)
	}
	if got := rec.last(); got != "rebuild:carried" {
		t.Errorf("stopped for approval: %s", got)
	}
	_, a := e.approval(t, v.ID)
	mark = e.latest(t)
	if err := e.p.ApproveConfig(ctx, v.ID, a.Hash, "session-x"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	e.endsOnce(t, v.ID, mark, JobApprove, workspace.JobOK)
}

// TestAJobCutOffByADeleteEndsCancelled: a run and a session server restart,
// each cut off by a delete, end cancelled — before the delete's first event,
// since the delete waits for them — and the delete then ends ok. The control
// is the delete's own end.
func TestAJobCutOffByADeleteEndsCancelled(t *testing.T) {
	ctx := context.Background()

	t.Run("run", func(t *testing.T) {
		e := lifecycleEnv(t)
		e.cli.up = registeringUp(e.cli.dir) + ` >/dev/null; exec sleep 30`
		e.wire(t)
		mark := e.latest(t)
		w, err := e.p.Create(ctx, alpha, "")
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for len(e.containers(t, w.ID)) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("up never started")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := e.p.Delete(ctx, w.ID, "krelinga/alpha"); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		ends := e.jobEnds(t, w.ID, mark)
		if len(ends) != 2 || ends[0].Kind != JobCreate || ends[0].Outcome != workspace.JobCancelled ||
			ends[1].Kind != JobDelete || ends[1].Outcome != workspace.JobOK {
			t.Fatalf("job ends %+v, want create:cancelled then delete:ok", ends)
		}
		// The run's end follows its last event (up failed, saying why) and
		// precedes the delete's first sub-step.
		var upFailed, firstAction int64
		for _, ev := range e.allEvents(t, w.ID) {
			if ev.Kind == workspace.KindStep && strings.Contains(string(ev.Data), `"status":"failed"`) {
				upFailed = ev.ID
			}
			if ev.Kind == KindAction && firstAction == 0 {
				firstAction = ev.ID
			}
		}
		if !(upFailed < ends[0].ID && ends[0].ID < firstAction) {
			t.Errorf("order: step failed %d, run's end %d, delete's first action %d", upFailed, ends[0].ID, firstAction)
		}
	})

	t.Run("supervisor", func(t *testing.T) {
		e := lifecycleEnv(t)
		v := e.running(t, alpha)
		started := make(chan struct{})
		e.p.SupervisorRestart = func(ctx context.Context, id string) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		mark := e.latest(t)
		if err := e.p.RestartSupervisor(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		<-started
		if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		ends := e.jobEnds(t, v.ID, mark)
		if len(ends) != 2 || ends[0].Kind != JobSupervisor || ends[0].Outcome != workspace.JobCancelled ||
			ends[1].Kind != JobDelete || ends[1].Outcome != workspace.JobOK {
			t.Fatalf("job ends %+v, want supervisor:cancelled then delete:ok", ends)
		}
	})
}

// TestAJobCutOffByShutdownEndsCancelled: shutdown cancels a run, whose move
// to failed — saying Drydock shut down — carries its end, cancelled. The
// control is the same run left alone, which ends ok (TestEveryJobEndsWithOneEvent).
func TestAJobCutOffByShutdownEndsCancelled(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	rec := &jobRecorder{}
	e.p.jobEnded = rec.record
	e.cli.up = "exec sleep 30"
	e.wire(t)
	mark := e.latest(t)
	w, err := e.p.Create(ctx, alpha, "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(e.cli.callsTo(t, "up")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("up never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.p.Shutdown(15 * time.Second)
	prev := e.endsOnce(t, w.ID, mark, JobCreate, workspace.JobCancelled)
	if prev.Kind != workspace.KindState || !strings.Contains(string(prev.Data), "Drydock shut down") {
		t.Errorf("the event before the end is %s %s, want the move to failed", prev.Kind, prev.Data)
	}
	if got := rec.all(); len(got) != 1 || got[0] != "create:carried" {
		t.Errorf("job ends %v, want the move to have carried it", got)
	}
}
