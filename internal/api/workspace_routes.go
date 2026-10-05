package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

// Provisioner is what the workspace routes need from internal/provision.
type Provisioner interface {
	Create(ctx context.Context, repositoryID int64, branch string) (workspace.Workspace, error)
	Start(ctx context.Context, id string) error
}

// WorkspaceReader is what they need from the workspace store and the log.
type WorkspaceReader interface {
	Views(ctx context.Context) ([]workspace.View, error)
	View(ctx context.Context, id string) (workspace.View, error)
}

// EventReader is a workspace's recent events.
type EventReader interface {
	ForWorkspace(ctx context.Context, workspaceID string, limit int) ([]events.Event, error)
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
}

// Handlers returns the map Build consumes, keyed by route Name.
func (wr WorkspaceRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"workspaces.list":   wr.list,
		"workspaces.read":   wr.read,
		"workspaces.create": wr.create,
		"workspaces.start":  wr.start,
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
	writeJSON(w, http.StatusOK, struct {
		Workspaces []workspace.View `json:"workspaces"`
	}{vs})
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
		writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		ID string `json:"id"`
	}{ws.ID})
}

func (wr WorkspaceRoutes) start(w http.ResponseWriter, r *http.Request) {
	if err := wr.Provisioner.Start(r.Context(), r.PathValue("id")); err != nil {
		writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// writeProvisionError maps create's and start's refusals to the envelope.
// Each code is one the UI can turn into a sentence and, for at_capacity, an
// action (frontend §4.5): which workspace to stop.
func writeProvisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provision.ErrNotConfigured):
		WriteError(w, http.StatusServiceUnavailable, CodeAppNotConfigured,
			"No GitHub App is configured, so Drydock cannot clone anything.",
			"Start drydock serve with --github-app-id and --github-app-key.")
	case errors.Is(err, provision.ErrUnknownRepository):
		WriteError(w, http.StatusNotFound, CodeNotFound,
			"That repository is not in the GitHub App's installation.", "")
	case errors.Is(err, workspace.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such workspace.", "")
	case errors.Is(err, provision.ErrBadBranch):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "That is not a branch name Drydock can clone.", "")
	case errors.Is(err, workspace.ErrInProgress):
		WriteError(w, http.StatusConflict, CodeInProgress,
			"This repository already has a workspace, or this workspace is already running or starting.", "")
	case errors.Is(err, workspace.ErrAtCap):
		WriteError(w, http.StatusConflict, CodeAtCapacity,
			"Drydock is at its concurrent-container cap. Stop a workspace to make room.", "")
	case errors.Is(err, provision.ErrShuttingDown):
		WriteError(w, http.StatusServiceUnavailable, CodeInternal, "Drydock is shutting down.", "")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not start the workspace.", "")
	}
}
