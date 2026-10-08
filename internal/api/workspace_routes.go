package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

// Provisioner is what the workspace routes need from internal/provision.
type Provisioner interface {
	Create(ctx context.Context, repositoryID int64, branch string) (workspace.Workspace, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Rebuild(ctx context.Context, id string) error
	Delete(ctx context.Context, id, confirm string) error
	ApproveConfig(ctx context.Context, id, hash, by string) error
	DeclineConfig(ctx context.Context, id string) error
}

// WorkspaceReader is what they need from the workspace store and the log.
type WorkspaceReader interface {
	Views(ctx context.Context) ([]workspace.View, error)
	View(ctx context.Context, id string) (workspace.View, error)
	// CapacityOf counts the views that hold a container slot against the
	// cap (workspace.Store.CapacityOf).
	CapacityOf(vs []workspace.View) workspace.Capacity
}

// EventReader is a workspace's recent events.
type EventReader interface {
	ForWorkspace(ctx context.Context, workspaceID string, limit int) ([]events.Event, error)
}

// Resources is the sampler's latest measurements (internal/usage): read from
// memory and set on each view as it is served, never stored.
type Resources interface {
	Of(id string) *workspace.Resources
	HostDisk() *workspace.HostDisk
}

// detailEvents is how many events GET /api/workspaces/{id} carries.
const detailEvents = 50

// WorkspaceRoutes serves the workspace list, its detail, create and start
// (design §5). Create and start are the design's async shape: validate,
// write the row or the transition, answer 202, and let the client follow the
// run on /api/events. The 202's body is only what the client needs to
// address what it started; the frontend discards it rather than applying it
// (frontend §2.1), so it carries no state.
type WorkspaceRoutes struct {
	Provisioner Provisioner
	Workspaces  WorkspaceReader
	Events      EventReader
	// Resources, when set, fills each view's resources and the list's
	// disk. Nil serves both as null: nothing measured.
	Resources Resources
	// BuildLogs holds each workspace's latest failed `up` (provision.BuildLog).
	// Nil answers every build log as not held.
	BuildLogs BuildLogs
}

// BuildLogs is what the build-log route needs from internal/provision.
type BuildLogs interface {
	BuildLog(id string) (provision.BuildLog, bool)
}

// BuildLogBody is GET /api/workspaces/{id}/build-log. held is false when
// Drydock holds none — no failed build since it started, or a later build
// got past it — which is not an empty log.
type BuildLogBody struct {
	Lines []string   `json:"lines"`
	At    *time.Time `json:"at"`
	Held  bool       `json:"held"`
}

func (wr WorkspaceRoutes) buildLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := wr.Workspaces.View(r.Context(), id); errors.Is(err, workspace.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such workspace.", "")
		return
	} else if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the workspace.", "")
		return
	}
	body := BuildLogBody{Lines: []string{}}
	if wr.BuildLogs != nil {
		if b, ok := wr.BuildLogs.BuildLog(id); ok {
			body = BuildLogBody{Lines: append([]string{}, b.Lines...), At: &b.At, Held: true}
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// Handlers returns the map Build consumes, keyed by route Name.
func (wr WorkspaceRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"workspaces.list":      wr.list,
		"workspaces.read":      wr.read,
		"workspaces.create":    wr.create,
		"workspaces.start":     wr.start,
		"workspaces.stop":      wr.stop,
		"workspaces.rebuild":   wr.rebuild,
		"workspaces.delete":    wr.remove,
		"workspaces.approve":   wr.approve,
		"workspaces.decline":   wr.decline,
		"workspaces.build_log": wr.buildLog,
	}
}

// WorkspaceDetail is GET /api/workspaces/{id}: the view plus its most
// recent events, newest first, in exactly the stream's shape — so the client
// feeds both through one reducer.
type WorkspaceDetail struct {
	workspace.View
	Events []events.Event `json:"events"`
}

func (wr WorkspaceRoutes) list(w http.ResponseWriter, r *http.Request) {
	vs, err := wr.Workspaces.Views(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the workspaces.", "")
		return
	}
	list := WorkspaceList{Workspaces: vs, Capacity: wr.Workspaces.CapacityOf(vs)}
	if wr.Resources != nil {
		for i := range list.Workspaces {
			list.Workspaces[i].Resources = wr.Resources.Of(list.Workspaces[i].ID)
		}
		list.Disk = wr.Resources.HostDisk()
	}
	writeJSON(w, http.StatusOK, list)
}

// WorkspaceList is GET /api/workspaces: every workspace with a row, and the
// concurrent-container cap with how many of them count against it (frontend
// §4.5 #17). Occupied is counted from exactly these rows, by
// workspace.Occupying — the rule create, start and rebuild enforce — so the
// list and the number are one snapshot, and a client counting the list by the
// same rule gets the same number. That is what lets the UI keep it live from
// workspace.state events without a capacity event of its own.
type WorkspaceList struct {
	Workspaces []workspace.View   `json:"workspaces"`
	Capacity   workspace.Capacity `json:"capacity"`
	// Disk is the filesystem holding the workspaces against the limit the
	// pre-flight refuses at (design §12, *Disk full*), as last measured;
	// null before the first measurement. The stream's resources frames
	// carry the same object as it changes.
	Disk *workspace.HostDisk `json:"disk"`
}

func (wr WorkspaceRoutes) read(w http.ResponseWriter, r *http.Request) {
	v, err := wr.Workspaces.View(r.Context(), r.PathValue("id"))
	if errors.Is(err, workspace.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such workspace.", "")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the workspace.", "")
		return
	}
	evs, err := wr.Events.ForWorkspace(r.Context(), v.ID, detailEvents)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the workspace's events.", "")
		return
	}
	if evs == nil {
		evs = []events.Event{}
	}
	if wr.Resources != nil {
		v.Resources = wr.Resources.Of(v.ID)
	}
	writeJSON(w, http.StatusOK, WorkspaceDetail{View: v, Events: evs})
}

// createBody is POST /api/workspaces. Branch is optional: absent or empty is
// the repository's default branch.
type createBody struct {
	RepositoryID *int64 `json:"repository_id"`
	Branch       string `json:"branch"`
}

func (wr WorkspaceRoutes) create(w http.ResponseWriter, r *http.Request) {
	var body createBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	if err := dec.Decode(&body); err != nil || dec.More() || body.RepositoryID == nil || *body.RepositoryID <= 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"The request needs a repository_id, and optionally a branch.", "")
		return
	}
	ws, err := wr.Provisioner.Create(r.Context(), *body.RepositoryID, body.Branch)
	if err != nil {
		wr.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		ID string `json:"id"`
	}{ws.ID})
}

func (wr WorkspaceRoutes) start(w http.ResponseWriter, r *http.Request) {
	if err := wr.Provisioner.Start(r.Context(), r.PathValue("id")); err != nil {
		wr.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// stop, rebuild and remove are Phase 6's lifecycle (§5): each answers 202
// with an empty object once the job is started, and the outcome arrives on
// the stream — workspace.action events for a stop's and a delete's
// sub-steps, workspace.step for a rebuild's, workspace.state for every move,
// and workspace.gone when a delete has removed the row.
func (wr WorkspaceRoutes) stop(w http.ResponseWriter, r *http.Request) {
	if err := wr.Provisioner.Stop(r.Context(), r.PathValue("id")); err != nil {
		wr.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

func (wr WorkspaceRoutes) rebuild(w http.ResponseWriter, r *http.Request) {
	if err := wr.Provisioner.Rebuild(r.Context(), r.PathValue("id")); err != nil {
		wr.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// remove is DELETE /api/workspaces/{id}?confirm=<full_name>. The confirm is
// the repository's full name, typed: the one friction on the one action that
// destroys unpushed work (§15.3). It is compared exactly — no trimming, no
// case folding — because a near miss is the signal the friction exists for.
func (wr WorkspaceRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := wr.Provisioner.Delete(r.Context(), r.PathValue("id"), r.URL.Query().Get("confirm")); err != nil {
		wr.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// approve is POST /api/workspaces/{id}/config-approval {"hash": "sha256:…"}:
// the hash is the request's as the page showed it, so a configuration that
// changed in between is approval_stale rather than approved unseen. The
// approval is recorded as the session that made it.
func (wr WorkspaceRoutes) approve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hash string `json:"hash"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	if err := dec.Decode(&body); err != nil || dec.More() || body.Hash == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "The request needs the hash of the host access being approved.", "")
		return
	}
	by := "unknown"
	if s, ok := SessionFrom(r.Context()); ok {
		by = s.ID
	}
	if err := wr.Provisioner.ApproveConfig(r.Context(), r.PathValue("id"), body.Hash, by); err != nil {
		wr.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// decline is DELETE /api/workspaces/{id}/config-approval.
func (wr WorkspaceRoutes) decline(w http.ResponseWriter, r *http.Request) {
	if err := wr.Provisioner.DeclineConfig(r.Context(), r.PathValue("id")); err != nil {
		wr.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// writeProvisionError maps create's and start's refusals to the envelope.
// Each code is one the UI can turn into a sentence and, for at_capacity, an
// action (frontend §4.5): which workspace to stop.
func (wr WorkspaceRoutes) writeProvisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provision.ErrNotConfigured):
		WriteError(w, http.StatusServiceUnavailable, CodeAppNotConfigured,
			"No GitHub App is configured, so Drydock cannot clone, start or rebuild a workspace.",
			"Start drydock serve with --github-app-id and --github-app-key.")
	case errors.Is(err, provision.ErrUnknownRepository):
		WriteError(w, http.StatusNotFound, CodeNotFound,
			"That repository is not in the GitHub App's installation.", "")
	case errors.Is(err, workspace.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such workspace.", "")
	case errors.Is(err, provision.ErrConfirmMismatch):
		WriteError(w, http.StatusBadRequest, CodeConfirmMismatch,
			"To delete this workspace, confirm with the repository's full name, exactly.", "")
	case errors.Is(err, provision.ErrBadBranch):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "That is not a branch name Drydock can clone.", "")
	case errors.Is(err, workspace.ErrNoApproval):
		WriteError(w, http.StatusConflict, CodeApprovalNotPending,
			"This workspace is not waiting for a host-access approval.", "")
	case errors.Is(err, workspace.ErrApprovalStale):
		WriteError(w, http.StatusConflict, CodeApprovalStale,
			"The configuration changed after it was shown, so nothing was approved. Review the new request.", "")
	case errors.Is(err, workspace.ErrInProgress):
		WriteError(w, http.StatusConflict, CodeInProgress,
			"This repository already has a workspace, or this workspace is busy or not in a state that allows this.", "")
	case errors.Is(err, workspace.ErrAtCap):
		// The detail names the cap, so the refusal can say the number
		// (frontend §9); it is the configured value, true whenever this is.
		detail := ""
		if c := wr.Workspaces.CapacityOf(nil).Cap; c != nil {
			detail = fmt.Sprintf("The cap is %d.", *c)
		}
		WriteError(w, http.StatusConflict, CodeAtCapacity,
			"Drydock is at its concurrent-container cap. Stop a workspace to make room.", detail)
	case errors.Is(err, provision.ErrDiskFull):
		// The figures are the refusal's own, so the sentence and the
		// banner name the same numbers.
		detail := ""
		var full *provision.DiskFullError
		if errors.As(err, &full) {
			detail = fmt.Sprintf("The workspace disk is %d%% full; Drydock refuses at %d%%.", full.Percent(), full.LimitPercent)
		}
		WriteError(w, http.StatusInsufficientStorage, CodeDiskFull,
			"The disk that holds the workspaces is too full to clone or build another. Delete a workspace you no longer need.", detail)
	case errors.Is(err, provision.ErrShuttingDown):
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable, "Drydock is shutting down.", "")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not start the workspace.", "")
	}
}
