package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/krelinga/drydock/internal/events"
)

func jobOf(t *testing.T, e events.Event) JobData {
	t.Helper()
	if e.Kind != KindJob {
		t.Fatalf("%s is not a job end", e.Kind)
	}
	var d JobData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A move made with Ending carries the job's end in its own commit — the two
// events written and published together, the end marked written so EndJob
// adds nothing — while a refused move carries nothing and leaves the end to
// EndJob. A context with no job, or a job for another workspace, adds
// nothing to a move.
func TestEndingCarriesTheJobsEndInTheMovesCommit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	other := f.create(t, 2)
	sub := f.events.Subscribe()
	defer f.events.Cancel(sub)

	end := NewJobEnd(ctx, w.ID, "create")
	jctx := WithJob(ctx, end)

	// Without Ending, a move in the job's context is not its last act.
	before, _ := f.events.Latest(ctx)
	if _, err := f.store.Move(jctx, w.ID, Cloning, ""); err != nil {
		t.Fatal(err)
	}
	// Ending for this job, on another workspace's move: not its end.
	if _, err := f.store.Move(Ending(jctx, true), other.ID, Cloning, ""); err != nil {
		t.Fatal(err)
	}
	// Ending without a job in the context: nothing to carry.
	if _, err := f.store.Move(Ending(ctx, true), other.ID, Building, ""); err != nil {
		t.Fatal(err)
	}
	if evs, _ := f.events.Since(ctx, before); len(evs) != 3 || end.Written() {
		t.Fatalf("moves that are not the job's end wrote %d events (written %v)", len(evs), end.Written())
	}

	// A refused move: nothing, and nothing written.
	before, _ = f.events.Latest(ctx)
	if _, err := f.store.Move(Ending(jctx, true), w.ID, Running, ""); !errors.As(err, &ErrIllegalMove{}) {
		t.Fatalf("Cloning → Running = %v", err)
	}
	if evs, _ := f.events.Since(ctx, before); len(evs) != 0 || end.Written() {
		t.Fatalf("a refused move wrote %+v (written %v)", evs, end.Written())
	}

	// The last act: the move and the end, in that order, one commit.
	if _, err := f.store.Move(Ending(jctx, true), w.ID, Failed, "The clone step failed."); err != nil {
		t.Fatal(err)
	}
	evs, _ := f.events.Since(ctx, before)
	if len(evs) != 2 || evs[0].Kind != KindState || evs[1].ID != evs[0].ID+1 {
		t.Fatalf("the last move wrote %+v", evs)
	}
	if d := jobOf(t, evs[1]); d != (JobData{Kind: "create", Outcome: JobFailed}) || evs[1].WorkspaceID != w.ID {
		t.Errorf("end %+v for %s", d, evs[1].WorkspaceID)
	}
	if !end.Written() {
		t.Error("the end is not marked written")
	}
	// Published together, in commit order, after the three earlier moves.
	var got []string
	for range 5 {
		got = append(got, (<-sub.C).Kind)
	}
	if got[3] != KindState || got[4] != KindJob {
		t.Errorf("published %v", got)
	}

	// EndJob after a carried end writes nothing more.
	if err := f.store.EndJob(ctx, end, errors.New("failed")); err != nil {
		t.Fatal(err)
	}
	if after, _ := f.events.Since(ctx, evs[1].ID); len(after) != 0 {
		t.Errorf("EndJob wrote a second end: %+v", after)
	}
}

// EndJob writes the end of a job no commit carried, once, with the outcome
// the job's result and context say: ok, failed, or — a failure under a
// cancelled job context — cancelled.
func TestEndJobOutcomes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, c := range []struct {
		job  context.Context
		err  error
		want string
	}{
		{ctx, nil, JobOK},
		{ctx, errors.New("x"), JobFailed},
		{cancelled, errors.New("x"), JobCancelled},
		{cancelled, nil, JobOK}, // it finished before it saw the cancellation
	} {
		end := NewJobEnd(c.job, w.ID, "stop")
		before, _ := f.events.Latest(ctx)
		if err := f.store.EndJob(ctx, end, c.err); err != nil {
			t.Fatal(err)
		}
		if err := f.store.EndJob(ctx, end, c.err); err != nil {
			t.Fatal(err)
		}
		evs, _ := f.events.Since(ctx, before)
		if len(evs) != 1 || jobOf(t, evs[0]).Outcome != c.want {
			t.Errorf("err %v, cancelled %v: %+v, want one %s", c.err, c.job.Err() != nil, evs, c.want)
		}
	}
}
