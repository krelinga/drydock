package workspace

import (
	"context"
	"sync"

	"github.com/krelinga/drydock/internal/events"
)

// KindJob ends a workspace job: one per job internal/provision runs (a
// create's or start's run, a stop, a rebuild, a delete, a session server
// restart, an approval's continuation), written whichever way it ended.
// data: {kind, outcome} — JobData. It changes no entity: it is what tells a
// client that the request it pressed is over (frontend §4.2), so no error
// path has to remember to say so with an event that merely implies it.
const KindJob = "workspace.job"

// Job outcomes: the job did what it was asked, failed on its own, or was cut
// off — by Drydock's shutdown, or by a delete that preempted it.
const (
	JobOK        = "ok"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// JobData is a workspace.job event's data.
type JobData struct {
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
}

// JobEnd is the one workspace.job event a job owes. The provisioner makes
// one per job and hands it down in the job's context (WithJob); where the
// job's last act is a commit this package makes — a move, an annotation, the
// row's removal — that commit carries the event (Ending), so the move and the
// end are one fact, published together. Otherwise the provisioner writes it
// in a commit of its own once the job has returned (Store.EndJob), after
// every event the job wrote. Either way it is written once.
type JobEnd struct {
	id, kind string
	// job is the job's own context, never one with its cancellation
	// removed: a failure while it is cancelled is a cancellation.
	job context.Context

	mu      sync.Mutex
	written bool
}

// NewJobEnd is the end owed for a job of kind on workspace id, running under
// job (the context its cancellation reaches).
func NewJobEnd(job context.Context, id, kind string) *JobEnd {
	return &JobEnd{id: id, kind: kind, job: job}
}

// Kind is the job's kind.
func (j *JobEnd) Kind() string { return j.kind }

// ID is the job's workspace.
func (j *JobEnd) ID() string { return j.id }

// Written reports whether the event has been committed.
func (j *JobEnd) Written() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.written
}

// outcome is how a job that failed (or not) ended: a failure under a
// cancelled job context is the cancellation's.
func (j *JobEnd) outcome(failed bool) string {
	switch {
	case !failed:
		return JobOK
	case j.job.Err() != nil:
		return JobCancelled
	}
	return JobFailed
}

func (j *JobEnd) event(failed bool) (events.Event, error) {
	o := j.outcome(failed)
	level := events.Info
	if o != JobOK {
		level = events.Warn
	}
	return events.NewEvent(j.id, level, KindJob, jobMessage(j.kind, o), JobData{Kind: j.kind, Outcome: o})
}

func jobMessage(kind, outcome string) string {
	what := map[string]string{
		"create": "The create", "start": "The start", "rebuild": "The rebuild", "approve": "The approved run",
		"stop": "The stop", "delete": "The delete", "supervisor": "The session server restart",
	}[kind]
	if what == "" {
		what = "The job"
	}
	switch outcome {
	case JobOK:
		return what + " finished."
	case JobCancelled:
		return what + " was cut off."
	}
	return what + " failed."
}

type jobKey struct{}

// WithJob carries end in ctx, for Ending to find. A nil end hides the job
// from Ending: for work whose last move is not the job's last event.
func WithJob(ctx context.Context, end *JobEnd) context.Context {
	return context.WithValue(ctx, jobKey{}, end)
}

type endingKey struct{}

type ending struct {
	end    *JobEnd
	failed bool
}

// Ending marks ctx for the job's last act: the next Move, Annotate, Remove
// or approval stop made with it carries the job's workspace.job event in its
// own commit — outcome ok, or failed (cancelled, if the job's context is) —
// and a commit that is refused carries nothing, leaving the end to
// Store.EndJob. Without a JobEnd in ctx it changes nothing.
func Ending(ctx context.Context, failed bool) context.Context {
	end, _ := ctx.Value(jobKey{}).(*JobEnd)
	if end == nil {
		return ctx
	}
	return context.WithValue(ctx, endingKey{}, ending{end, failed})
}

// trailer is what a commit for workspace id made under ctx appends: the
// job's end when ctx is marked Ending for it and it is not yet written, and
// the function to call once the commit has succeeded.
func trailer(ctx context.Context, id string) ([]events.Event, func(), error) {
	e, ok := ctx.Value(endingKey{}).(ending)
	if !ok || e.end.id != id {
		return nil, func() {}, nil
	}
	e.end.mu.Lock()
	written := e.end.written
	e.end.mu.Unlock()
	if written {
		return nil, func() {}, nil
	}
	ev, err := e.end.event(e.failed)
	if err != nil {
		return nil, nil, err
	}
	return []events.Event{ev}, func() {
		e.end.mu.Lock()
		e.end.written = true
		e.end.mu.Unlock()
	}, nil
}

// EndJob writes the job's workspace.job event in a commit of its own, unless
// its last act already carried it: the provisioner calls it once the job
// has returned, so the event follows every event the job wrote. err is the
// job's result; one under a cancelled job context is a cancellation.
//
// Only the job's own goroutine ends it, so the check and the write need no
// lock held across them (and holding end's across the event log's would
// invert trailer's order).
func (s *Store) EndJob(ctx context.Context, end *JobEnd, err error) error {
	if end.Written() {
		return nil
	}
	ev, eerr := end.event(err != nil)
	if eerr != nil {
		return eerr
	}
	if _, eerr = s.Events.Append(ctx, ev); eerr != nil {
		return eerr
	}
	end.mu.Lock()
	end.written = true
	end.mu.Unlock()
	return nil
}

// withEnd is the tail of a commit's function for workspace id: es, plus the
// job's end when ctx is marked Ending for it, with *done set to what marks
// it written once the commit succeeds.
func withEnd(ctx context.Context, id string, done *func(), es []events.Event, err error) ([]events.Event, error) {
	if err != nil {
		return nil, err
	}
	more, ok, err := trailer(ctx, id)
	if err != nil {
		return nil, err
	}
	*done = ok
	return append(es, more...), nil
}

// ended marks the job's end written, after a commit that carried it.
func ended(done func(), err error) {
	if err == nil && done != nil {
		done()
	}
}
