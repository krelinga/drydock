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
	OnAdopt func(ctx context.Context, w workspace.Workspace) error
	// Delete finishes a delete. Nil until deletion exists (Phase 6); the
	// workspace then stays in deleting, which is safe: it is persisted
	// precisely so a later run can resume it.
	Delete func(ctx context.Context, w workspace.Workspace, containerID string) error
	// Busy reports a workspace this process has provisioned, or is
	// provisioning. Its actions are skipped: reconciliation is about what
	// happened before this process started, and it runs beside serving, so
	// a create in the first seconds after boot is pending or building
	// because it is being provisioned, not because a restart interrupted it
	// — and marking it failed would race the run that owns it, or undo one
	// that finished between the listing and the action. Nil means none.
	Busy func(workspaceID string) bool
}

// Run lists containers, plans, and applies every action, continuing past a
// failed one so one bad row cannot stop the rest from being reconciled. It
// returns the plan and every error joined.
func (r *Reconciler) Run(ctx context.Context) ([]Action, error) {
	found, err := r.Containers.List(ctx)
	if err != nil {
		// Without the list, every running row would look absent and be
		// marked stopped. Refuse to guess: change nothing.
		return nil, fmt.Errorf("reconcile: cannot list containers, so nothing was changed: %w", err)
	}
	rows, err := r.Workspaces.List(ctx)
	if err != nil {
		return nil, err
	}
	plan := Plan(rows, found)
	var errs []error
	for _, a := range plan {
		// Asked per action, after the rows were read: a run that started
		// since is in the plan only if its row was, and is busy by now.
		if r.Busy != nil && a.WorkspaceID != "" && r.Busy(a.WorkspaceID) {
			continue
		}
		if err := r.apply(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", a.Kind, a.WorkspaceID, err))
		}
	}
	return plan, errors.Join(errs...)
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
