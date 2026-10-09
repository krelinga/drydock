package provision

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/workspace"
)

// resumeSeam stands in for the supervisor's half of a sign-in: the set of
// workspaces waiting on one, and the resume each job runs — recording, as it
// runs, whether the provisioner holds the workspace with a supervisor job.
type resumeSeam struct {
	p       *Provisioner
	waiting []string

	mu    sync.Mutex
	calls []string // workspace ids resumed, in order
	inJob []bool   // whether each ran as the workspace's supervisor job
}

func (s *resumeSeam) wire(p *Provisioner) {
	s.p = p
	p.SupervisorsAwaitingLogin = func() []string { return append([]string(nil), s.waiting...) }
	p.SupervisorResume = func(ctx context.Context, id string) error {
		p.mu.Lock()
		j := p.active[id]
		p.mu.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, id)
		s.inJob = append(s.inJob, j != nil && j.kind == JobSupervisor)
		return nil
	}
}

func (s *resumeSeam) got() ([]string, []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...), append([]bool(nil), s.inJob...)
}

// TestASignInResumesThroughJobs (R5, one door per workspace): a sign-in's
// resume reaches each waiting workspace as a supervisor job of its own —
// admitted like any other, holding the workspace while it runs, and ending
// with its workspace.job event — and only a running workspace gets one. The
// stopped workspace in the same set is the control: no job, no resume, no
// event.
func TestASignInResumesThroughJobs(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	a := e.running(t, alpha)
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
	e.wire(t)
	b := e.running(t, plain)
	if err := e.p.Stop(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	seam := &resumeSeam{waiting: []string{a.ID, b.ID}}
	seam.wire(e.p)

	mark := e.latest(t)
	if err := e.p.ResumeAwaitingLogin(ctx); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	calls, inJob := seam.got()
	if !slices.Equal(calls, []string{a.ID}) || !slices.Equal(inJob, []bool{true}) {
		t.Fatalf("resumes %v (in a job: %v), want the running workspace's alone, in its job", calls, inJob)
	}
	e.endsOnce(t, a.ID, mark, JobSupervisor, workspace.JobOK)
	if ends := e.jobEnds(t, b.ID, mark); len(ends) != 0 {
		t.Errorf("control: the stopped workspace's job ends %+v, want none", ends)
	}
}

// TestASignInFindingTheWorkspaceBusyResumesWhenTheJobEnds: a sign-in that
// lands while another job holds the workspace — here a session server
// restart, whose own start may have read the identity just before the
// sign-in was stored — does not run beside that job, and is not dropped
// either: the resume is owed, and launched as the busy job releases the
// workspace, after its end. A stop that holds the workspace instead leaves
// it stopped, and the owed resume, deciding afresh, starts nothing (the
// control: owing is not starting).
func TestASignInFindingTheWorkspaceBusyResumesWhenTheJobEnds(t *testing.T) {
	ctx := context.Background()
	t.Run("restart", func(t *testing.T) {
		e := lifecycleEnv(t)
		v := e.running(t, alpha)
		seam := &resumeSeam{waiting: []string{v.ID}}
		seam.wire(e.p)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		free := func() { once.Do(func() { close(release) }) }
		defer free() // a failure below must not leave the job held
		e.p.SupervisorRestart = func(ctx context.Context, id string) error {
			close(entered)
			<-release
			return nil
		}
		mark := e.latest(t)
		if err := e.p.RestartSupervisor(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		<-entered
		if err := e.p.ResumeAwaitingLogin(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if calls, _ := seam.got(); len(calls) != 0 {
			t.Fatalf("a resume ran beside the restart: %v", calls)
		}
		free()
		e.p.idle()
		calls, inJob := seam.got()
		if !slices.Equal(calls, []string{v.ID}) || !slices.Equal(inJob, []bool{true}) {
			t.Fatalf("after the restart: resumes %v (in a job: %v), want one, in its own job", calls, inJob)
		}
		ends := e.jobEnds(t, v.ID, mark)
		if len(ends) != 2 || ends[0].Kind != JobSupervisor || ends[1].Kind != JobSupervisor ||
			ends[0].Outcome != workspace.JobOK || ends[1].Outcome != workspace.JobOK {
			t.Fatalf("job ends %+v, want the restart's then the resume's", ends)
		}
	})
	t.Run("stop", func(t *testing.T) {
		e := lifecycleEnv(t)
		v := e.running(t, alpha)
		seam := &resumeSeam{waiting: []string{v.ID}}
		seam.wire(e.p)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		free := func() { once.Do(func() { close(release) }) }
		defer free() // a failure below must not leave the job held
		e.p.StopSupervisor = func(ctx context.Context, w workspace.Workspace) error {
			close(entered)
			<-release
			return nil
		}
		mark := e.latest(t)
		if err := e.p.Stop(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		<-entered
		if err := e.p.ResumeAwaitingLogin(ctx); err != nil {
			t.Fatal(err)
		}
		free()
		e.p.idle()
		if calls, _ := seam.got(); len(calls) != 0 {
			t.Fatalf("a stopped workspace was resumed: %v", calls)
		}
		if got := e.view(t, v.ID); got.State != workspace.Stopped {
			t.Fatalf("state %s, want stopped", got.State)
		}
		if ends := e.jobEnds(t, v.ID, mark); len(ends) != 1 || ends[0].Kind != JobStop {
			t.Fatalf("job ends %+v, want the stop's alone", ends)
		}
	})
}

// TestNoResumeOnceTheGroupStops: after shutdown a sign-in starts nothing —
// ResumeAwaitingLogin is refused ErrShuttingDown with no job and no event —
// and a resume owed to a workspace that was busy when the stop came is
// dropped as the busy job ends, cancelled. The control is the same sign-in
// before the stop (TestASignInResumesThroughJobs), and here the owed resume
// itself: it was taken (nothing ran beside the busy job) and is only lost to
// the stop.
func TestNoResumeOnceTheGroupStops(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	seam := &resumeSeam{waiting: []string{v.ID}}
	seam.wire(e.p)
	entered := make(chan struct{})
	e.p.SupervisorRestart = func(ctx context.Context, id string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	if err := e.p.RestartSupervisor(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := e.p.ResumeAwaitingLogin(ctx); err != nil {
		t.Fatalf("control: a sign-in while busy = %v", err)
	}
	e.p.mu.Lock()
	owed := e.p.resumeOwed[v.ID]
	e.p.mu.Unlock()
	if !owed {
		t.Fatal("control: the busy workspace is owed no resume")
	}

	e.g.Stop()
	if late := e.shutdown(5 * time.Second); late != nil {
		t.Fatalf("still running after the stop: %v", late)
	}
	mark := e.latest(t)
	if err := e.p.ResumeAwaitingLogin(ctx); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("a sign-in after the stop = %v, want ErrShuttingDown", err)
	}
	if late := e.shutdown(5 * time.Second); late != nil {
		t.Fatalf("still running: %v", late)
	}
	if calls, _ := seam.got(); len(calls) != 0 {
		t.Errorf("resumes after the stop: %v", calls)
	}
	if latest := e.latest(t); latest != mark {
		t.Errorf("a refused sign-in wrote %d events", latest-mark)
	}
}

// A delete forgets a resume a sign-in left owed: the workspace is going, and
// nothing is started for it as the delete — or the job it cancelled — ends.
func TestADeleteForgetsAnOwedResume(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	seam := &resumeSeam{waiting: []string{v.ID}}
	seam.wire(e.p)
	entered := make(chan struct{})
	e.p.SupervisorRestart = func(ctx context.Context, id string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	if err := e.p.RestartSupervisor(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := e.p.ResumeAwaitingLogin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	if calls, _ := seam.got(); len(calls) != 0 {
		t.Errorf("resumes for a deleted workspace: %v", calls)
	}
	e.p.mu.Lock()
	owed := len(e.p.resumeOwed)
	e.p.mu.Unlock()
	if owed != 0 {
		t.Errorf("%d resumes still owed after the delete", owed)
	}
}
