package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

// SupervisorRestarter is what POST …/supervisor needs from internal/provision.
type SupervisorRestarter interface {
	RestartSupervisor(ctx context.Context, id string) error
}

// LogLine is one line of a session server's log (internal/supervisor.Line).
type LogLine struct {
	N    int64     `json:"n"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// SupervisorLogs reads a workspace's session server log: its last n lines,
// whether older lines are gone, and whether Drydock holds a log for it at all.
type SupervisorLogs func(workspaceID string, n int) (lines []LogLine, truncated, held bool)

// SupervisorRoutes serves the two session-server routes of design §5.
type SupervisorRoutes struct {
	Provisioner SupervisorRestarter
	Workspaces  WorkspaceReader
	Logs        SupervisorLogs
}

// Handlers returns the map Build consumes. A route whose backing is nil is
// left out, so Build mounts its 501 behind the gate.
func (sr SupervisorRoutes) Handlers() map[string]http.HandlerFunc {
	h := map[string]http.HandlerFunc{}
	if sr.Provisioner != nil {
		h["workspaces.supervisor"] = sr.restart
	}
	if sr.Logs != nil {
		h["workspaces.logs"] = sr.logs
	}
	return h
}

// restart is POST /api/workspaces/{id}/supervisor: start or restart the
// session server. 202, and the outcome arrives as supervisor.state events.
func (sr SupervisorRoutes) restart(w http.ResponseWriter, r *http.Request) {
	err := sr.Provisioner.RestartSupervisor(r.Context(), r.PathValue("id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, struct{}{})
	case errors.Is(err, workspace.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such workspace.", "")
	case errors.Is(err, workspace.ErrInProgress):
		WriteError(w, http.StatusConflict, CodeInProgress,
			"The session server can be restarted only on a running workspace with nothing else in progress.", "")
	case errors.Is(err, provision.ErrNoSupervisor):
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured, "No session supervisor is configured.", "")
	case errors.Is(err, provision.ErrShuttingDown):
		WriteError(w, http.StatusServiceUnavailable, CodeInternal, "Drydock is shutting down.", "")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not restart the session server.", "")
	}
}

// LogTail is GET /api/workspaces/{id}/logs: the session server's ring buffer
// (§8), redacted when it was written, held in memory and never persisted.
// Held is false when Drydock has no log for the workspace — no server was
// started since Drydock did — which is not the same as an empty one.
type LogTail struct {
	Lines     []LogLine `json:"lines"`
	Truncated bool      `json:"truncated"`
	Held      bool      `json:"held"`
}

const (
	defaultLogTail = 200
	maxLogTail     = 5000
)

func (sr SupervisorRoutes) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := sr.Workspaces.View(r.Context(), id); err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such workspace.", "")
			return
		}
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the workspace.", "")
		return
	}
	n := defaultLogTail
	if q := r.URL.Query().Get("tail"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil || v < 1 || v > maxLogTail {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "tail must be a number from 1 to 5000.", "")
			return
		}
		n = v
	}
	lines, truncated, held := sr.Logs(id, n)
	if lines == nil {
		lines = []LogLine{}
	}
	writeJSON(w, http.StatusOK, LogTail{Lines: lines, Truncated: truncated, Held: held})
}
