// Package reconcile is design §6's boot reconciliation: containers found by
// label against workspace rows, in one direction only. Docker is the truth
// and the database is the cache.
//
// Plan is a pure function from (rows, containers) to actions, so the table in
// §6 is a table test (testing §2: five rows as a pure function in unit, and
// only the two whose interest is Docker's behaviour again in the container
// tier). Run lists, plans, and applies.
//
// Two instincts govern every row. Adopt rather than kill: a container this
// instance did not expect is someone's work, so it is recorded, never removed.
// And never auto-start: a workspace found stopped stays stopped, and one that
// was mid-build when Drydock went down is marked failed rather than resumed,
// because the restart may have been the build's fault.
//
// Before the plan, Run closes any step an earlier process died inside: its
// newest event is started, and nothing will ever end it. It is failed with
// InterruptedStep, so no timeline shows a step running forever.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// Kind is what reconciliation does about one workspace or container.
type Kind string

const (
	// Adopt: the row says running and so does Docker. The supervisor is
	// restarted (§6: `--continue` resumes the session in the same directory).
	Adopt Kind = "adopt"
	// MarkStopped: the row says running and the container has exited or is
	// gone. Not restarted: the exit may have been a crash loop.
	MarkStopped Kind = "mark_stopped"
	// MarkInterrupted: the row was mid-provision (pending, cloning, building)
	// when Drydock stopped. Failed, with a detail saying so; retried only by
	// the operator.
	MarkInterrupted Kind = "mark_interrupted"
	// AdoptOrphan: a container with no row, whose labels say enough to
	// rebuild one.
	AdoptOrphan Kind = "adopt_orphan"
	// LeaveOrphan: a container with no row and labels too incomplete to
	// rebuild one from. Logged and left alone — never removed.
	LeaveOrphan Kind = "leave_orphan"
	// ResumeDelete: the row is deleting. Finish it (§6: this is why
	// deleting is persisted).
	ResumeDelete Kind = "resume_delete"
	// SyncContainer: the state is right, but the cached container id is
	// stale — a different container, or one that is gone.
	SyncContainer Kind = "sync_container"
	// ExtraContainer: a second container for a workspace that already has
	// one. Logged and left alone.
	ExtraContainer Kind = "extra_container"
)

// Action is one decision. ContainerID is the container it concerns, if any;
// for SyncContainer an empty ContainerID means "clear the cached id".
type Action struct {
	Kind        Kind
	WorkspaceID string
	ContainerID string
	From        workspace.State // the row's state, when there is a row
	Found       *container.Found
}

var ulid = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// Plan decides, for every row and every found container, what to do. The
// result is ordered by workspace id, then container id, so a log of it reads
// the same on every run.
func Plan(rows []workspace.Workspace, found []container.Found) []Action {
	byWS := map[string][]container.Found{}
	for _, f := range found {
		byWS[f.WorkspaceID] = append(byWS[f.WorkspaceID], f)
	}
	var out []Action

	for _, w := range rows {
		cs := byWS[w.ID]
		delete(byWS, w.ID)
		primary, extras := pick(cs)
		for i := range extras {
			out = append(out, Action{Kind: ExtraContainer, WorkspaceID: w.ID, ContainerID: extras[i].ContainerID, From: w.State, Found: &extras[i]})
		}
		a := Action{WorkspaceID: w.ID, From: w.State, Found: primary}
		if primary != nil {
			a.ContainerID = primary.ContainerID
		}

		switch w.State {
		case workspace.Deleting:
			a.Kind = ResumeDelete
		case workspace.Running:
			if primary != nil && primary.Running {
				a.Kind = Adopt
			} else {
				a.Kind = MarkStopped
			}
		case workspace.Pending, workspace.Cloning, workspace.Building:
			a.Kind = MarkInterrupted
		default: // stopped, failed: right as they are; only the cache may be stale
			if a.ContainerID == w.ContainerID {
				continue
			}
			a.Kind = SyncContainer
		}
		out = append(out, a)
	}

	for wsID, cs := range byWS {
		primary, extras := pick(cs)
		for i := range extras {
			out = append(out, Action{Kind: ExtraContainer, WorkspaceID: wsID, ContainerID: extras[i].ContainerID, Found: &extras[i]})
		}
		a := Action{WorkspaceID: wsID, ContainerID: primary.ContainerID, Found: primary, Kind: AdoptOrphan}
		if !ulid.MatchString(wsID) || primary.RepositoryID <= 0 || primary.Branch == "" || primary.Repo == "" {
			a.Kind = LeaveOrphan
		}
		out = append(out, a)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WorkspaceID != out[j].WorkspaceID {
			return out[i].WorkspaceID < out[j].WorkspaceID
		}
		return out[i].ContainerID < out[j].ContainerID
	})
	return out
}

// pick chooses a workspace's container when there are several: a running one
// over a stopped one, then the lowest id so the choice is stable.
func pick(cs []container.Found) (*container.Found, []container.Found) {
	if len(cs) == 0 {
		return nil, nil
	}
	sorted := append([]container.Found(nil), cs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Running != sorted[j].Running {
			return sorted[i].Running
		}
		return sorted[i].ContainerID < sorted[j].ContainerID
	})
	return &sorted[0], sorted[1:]
}

// Lister is the container manager's List.
type Lister interface {
	List(ctx context.Context) ([]container.Found, error)
}

// Reconciler applies a Plan.
type Reconciler struct {
	Workspaces *workspace.Store
	Events     *events.Log
	Containers Lister
	// OnAdopt restarts a running workspace's supervisor. Nil until the
	// supervisor exists (Phase 5); adoption still records the container.
	// It runs inside Exclusive, so it must not take the provisioner's lock.
	// Drydock leaves it nil: provision.ResumeSupervisors does this after
	// reconciliation.
	OnAdopt func(ctx context.Context, w workspace.Workspace) error
	// Delete finishes a delete. Nil until deletion exists (Phase 6); the
	// workspace then stays in deleting, which is safe: it is persisted
	// precisely so a later run can resume it.
	Delete func(ctx context.Context, w workspace.Workspace, containerID string) error
	// Exclusive runs act only if this process has started no job for the
	// workspace — provisioned it, or is provisioning, stopping or deleting
	// it — and reports whether it ran, deciding and acting under the lock
	// every job starts under (provision.Provisioner.Unowned). Reconciliation
	// is about what happened before this process started, and it runs
	// beside serving, so a create in the first seconds after boot is pending
	// or building because it is being provisioned, not because a restart
	// interrupted it; and a stop or start asked while reconciliation runs
	// must not have its workspace acted on from a plan made before it. A
	// check followed by the act left a window between them in which such a
	// job could start; one call holding the lock across both has none. Nil
	// means nothing is ever busy, and act runs directly.
	Exclusive func(workspaceID string, act func() error) (ran bool, err error)
}

// ErrNothingChanged is wrapped by Run's error when it stopped before acting:
// the containers or the rows could not be read, so no action was applied.
// Only this error may be reported as "nothing was changed".
var ErrNothingChanged = errors.New("reconcile: nothing was changed")

// Partial is Run's error when it applied the plan but some actions failed.
// Every other action was applied, so it must never be reported as "nothing
// was changed": a resumed delete that sticks made every boot say so while
// the same run adopted or stopped other rows.
type Partial struct {
	// Applied counts the actions that succeeded; Errs holds one error per
	// action that failed.
	Applied int
	Errs    []error
}

func (p *Partial) Error() string {
	return fmt.Sprintf("reconcile: %d of %d actions failed: %v",
		len(p.Errs), p.Applied+len(p.Errs), errors.Join(p.Errs...))
}

func (p *Partial) Unwrap() []error { return p.Errs }

// Run lists containers, plans, and applies every action, continuing past a
// failed one so one bad row cannot stop the rest from being reconciled. It
// returns the plan, and an error wrapping ErrNothingChanged when it could not
// read what it reconciles, or a *Partial when some actions failed.
func (r *Reconciler) Run(ctx context.Context) ([]Action, error) {
	found, err := r.Containers.List(ctx)
	if err != nil {
		// Without the list, every running row would look absent and be
		// marked stopped. Refuse to guess: change nothing.
		return nil, fmt.Errorf("cannot list containers: %w: %w", ErrNothingChanged, err)
	}
	rows, err := r.Workspaces.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read the workspaces: %w: %w", ErrNothingChanged, err)
	}
	plan := Plan(rows, found)
	var p Partial
	// Steps first, then the plan: a step the last process died inside is
	// closed before the plan moves its workspace, so a timeline reads as a
	// live failure does (the step's failed, then the move), and before
	// anything after reconciliation — ResumeSupervisors above all — writes
	// for the workspace, so this run's close is older than the next run's
	// events.
	for _, w := range rows {
		if err := r.closeDangling(ctx, w); err != nil {
			p.Errs = append(p.Errs, fmt.Errorf("closing steps of %s: %w", w.ID, err))
		}
	}
	for _, a := range plan {
		// Asked per action, after the rows were read: a job that started
		// since is in the plan only if its row was, and owns it by now.
		ran, err := r.exclusive(ctx, a)
		switch {
		case err != nil:
			p.Errs = append(p.Errs, fmt.Errorf("%s %s: %w", a.Kind, a.WorkspaceID, err))
		case ran:
			p.Applied++
		}
	}
	if len(p.Errs) > 0 {
		return plan, &p
	}
	return plan, nil
}

// InterruptedStep is the public sentence a step left started by an earlier
// process is closed with: the hard-kill counterpart of provision's "Drydock
// shut down while this step was running", which a graceful shutdown writes
// itself.
const InterruptedStep = "Drydock stopped while this step was running."

// closeDangling fails, under Exclusive, every step of w whose newest event is
// started (workspace.Store.FailDangling). Only an earlier process can have
// left one: every step that returns writes its end, and a cancelled one is
// failed by provision's guard. Not for a workspace this process has a job
// for — its started step is running — and not for a deleting one, whose
// resumed delete removes the row and its timeline with it. The workspace's
// state is the plan's to decide, and §6 decides it without the steps: a
// running row is adopted or marked stopped (a step-8 failure leaves a
// workspace running, §6 step 8), and one mid-provision is marked failed.
func (r *Reconciler) closeDangling(ctx context.Context, w workspace.Workspace) error {
	if w.State == workspace.Deleting {
		return nil
	}
	act := func() error {
		_, err := r.Workspaces.FailDangling(ctx, w.ID, InterruptedStep)
		return err
	}
	if r.Exclusive == nil {
		return act()
	}
	_, err := r.Exclusive(w.ID, act)
	return err
}

// exclusive applies a under Exclusive. A resumed delete is the exception: it
// is the provisioner's own job, which takes the lock itself, so it is only
// checked under the lock and then run outside it. That leaves no window that
// matters: a deleting row can only be deleted, and a delete asked while one
// runs joins it rather than starting a second.
func (r *Reconciler) exclusive(ctx context.Context, a Action) (bool, error) {
	if r.Exclusive == nil || a.WorkspaceID == "" {
		return true, r.apply(ctx, a)
	}
	if a.Kind == ResumeDelete {
		if ran, _ := r.Exclusive(a.WorkspaceID, func() error { return nil }); !ran {
			return false, nil
		}
		return true, r.apply(ctx, a)
	}
	return r.Exclusive(a.WorkspaceID, func() error { return r.apply(ctx, a) })
}

func (r *Reconciler) apply(ctx context.Context, a Action) error {
	ws := r.Workspaces
	switch a.Kind {
	case Adopt:
		if err := ws.SetContainer(ctx, a.WorkspaceID, a.ContainerID); err != nil {
			return err
		}
		if _, err := r.Events.Emit(ctx, a.WorkspaceID, events.Info, "workspace.adopted",
			"Found running after a restart.", map[string]any{"container_id": a.ContainerID}); err != nil {
			return err
		}
		if r.OnAdopt != nil {
			w, err := ws.Get(ctx, a.WorkspaceID)
			if err != nil {
				return err
			}
			return r.OnAdopt(ctx, w)
		}
		return nil

	case MarkStopped:
		detail := "Its container had exited when Drydock started."
		if a.Found == nil {
			detail = "Its container was gone when Drydock started."
		}
		if err := ws.SetContainer(ctx, a.WorkspaceID, a.ContainerID); err != nil {
			return err
		}
		_, err := ws.Move(ctx, a.WorkspaceID, workspace.Stopped, detail)
		return err

	case MarkInterrupted:
		if err := ws.SetContainer(ctx, a.WorkspaceID, a.ContainerID); err != nil {
			return err
		}
		_, err := ws.Move(ctx, a.WorkspaceID, workspace.Failed,
			fmt.Sprintf("Drydock restarted while this workspace was %s.", a.From))
		return err

	case SyncContainer:
		return ws.SetContainer(ctx, a.WorkspaceID, a.ContainerID)

	case AdoptOrphan:
		state := workspace.Stopped
		if a.Found.Running {
			state = workspace.Running
		}
		return ws.Adopt(ctx, workspace.Workspace{
			ID: a.WorkspaceID, RepositoryID: a.Found.RepositoryID, Branch: a.Found.Branch,
			State: state, ContainerID: a.ContainerID,
			StateDetail: "Found with no record; adopted from its labels.",
		}, a.Found.Repo)

	case LeaveOrphan, ExtraContainer:
		// System-wide (no workspace id) when the id is not one this
		// instance could have issued.
		wsID := a.WorkspaceID
		if !ulid.MatchString(wsID) {
			wsID = ""
		}
		_, err := r.Events.Emit(ctx, wsID, events.Warn, "container.unclaimed",
			"A container carries this instance's label but could not be matched to a workspace; it was left alone.",
			map[string]any{"container_id": a.ContainerID, "reason": string(a.Kind)})
		return err

	case ResumeDelete:
		if r.Delete == nil {
			return nil // stays deleting; resumed once deletion exists
		}
		w, err := ws.Get(ctx, a.WorkspaceID)
		if err != nil {
			return err
		}
		return r.Delete(ctx, w, a.ContainerID)
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}
