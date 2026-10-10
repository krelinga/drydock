package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// View is a workspace as the API serves it (GET /api/workspaces and
// GET /api/workspaces/{id}): the row, its repository's name, and the latest
// workspace.step event for each step it has reached.
//
// Every field is read from the row or the event log, and nothing here is
// inferred: steps are exactly what the steps wrote, so the UI can render the
// pipeline from this on a cold load and then keep it current with the same
// workspace.step events off the stream (frontend §2.1 — one writer).
type View struct {
	ID           string `json:"id"`
	RepositoryID int64  `json:"repository_id"`
	FullName     string `json:"full_name"`
	Branch       string `json:"branch"`
	State        State  `json:"state"`
	// StateDetail and ContainerID are null, not "", when unset: an empty
	// string would be a value the UI could mistake for one.
	StateDetail *string              `json:"state_detail"`
	ContainerID *string              `json:"container_id"`
	CreatedAt   time.Time            `json:"created_at"`
	Steps       map[Step]StepOutcome `json:"steps"`
	// LastAction is the latest workspace.action event — a stop's or a
	// delete's newest sub-step — or null when there has been none. It is
	// what lets a card loaded from this list alone say which sub-step a
	// failed stop stopped at, beside the state_detail that says it in prose
	// (frontend §4.5 #15): the client reads structure, never the sentence.
	LastAction *ActionOutcome `json:"last_action"`
	// EnvironmentID is the env_… the workspace's remote-control server
	// advertised (§8): one per workspace, kept across restarts, and the
	// card's only link. Null until a server has announced one.
	EnvironmentID *string `json:"environment_id"`
	// Supervisor is the latest supervisor.state event — the session
	// server's process state, why, and how many times it has been
	// restarted — or null when no server was ever started. Session is the
	// latest session.status event: the environment link, the capacity
	// fraction from `Capacity: N/M`, and how many sessions were seen. Both
	// are what the stream said, read back, so a card loaded cold and a card
	// kept live from events are the same card (frontend §2.1).
	Supervisor *SupervisorView `json:"supervisor"`
	Session    *SessionView    `json:"session"`
	// Approval is the host-access request a stopped workspace is waiting on
	// (approval.go), or null: the hash to approve, and what is added,
	// changed and removed relative to the subset last approved for the
	// repository. The workspace.state event that stopped it carries the
	// same object, and every other workspace.state event carries none.
	Approval *ApprovalView `json:"approval"`
	// Resources is the sampler's latest measurement of the workspace
	// (resources.go), filled in by the route from memory, never read from
	// the database; null when this server measures nothing yet.
	Resources *Resources `json:"resources"`
	// VSCode is the card's *Open in VS Code* link (internal/vscode), filled
	// in by the route from Docker as the view is served, never stored; null
	// when this server fills none.
	VSCode *VSCodeLink `json:"vscode"`
}

// VSCodeLink is a view's *Open in VS Code* state. Configured is whether the
// server was given the SSH host VS Code reaches it at (`serve
// --vscode-ssh-host`); URL is the vscode:// link to the workspace's running
// container, or null when there is none to open.
type VSCodeLink struct {
	Configured bool    `json:"configured"`
	URL        *string `json:"url"`
}

// Kinds the session supervisor (internal/supervisor, design §8) writes.
const (
	// KindSupervisor: the remote-control process's state moved, or its
	// reason did. data: SupervisorData.
	KindSupervisor = "supervisor.state"
	// KindSession: what the server's output said changed — the
	// environment id, the capacity fraction, a session newly seen.
	// data: SessionData.
	KindSession = "session.status"
)

// SupervisorData is a supervisor.state event's data.
type SupervisorData struct {
	State string `json:"state"`
	From  string `json:"from,omitempty"`
	// Reason is the cause as a code the UI turns into one sentence —
	// never the server's own message, which is prose (frontend §4.5 #9).
	Reason string `json:"reason,omitempty"`
	// Detail is Drydock's sentence for the reason.
	Detail       string `json:"detail,omitempty"`
	RestartCount int    `json:"restart_count"`
}

// SupervisorView is SupervisorData with the event's time.
type SupervisorView struct {
	SupervisorData
	At time.Time `json:"at"`
}

// SessionData is a session.status event's data.
type SessionData struct {
	EnvironmentID string `json:"environment_id,omitempty"`
	// URL is https://claude.ai/code?environment=<id>, built by Drydock from
	// the id — never a scraped URL.
	URL string `json:"url,omitempty"`
	// CapacityUsed and CapacityTotal are the last `Capacity: N/M` line;
	// null before the server printed one. The pre-created session counts
	// toward Used.
	CapacityUsed  *int `json:"capacity_used"`
	CapacityTotal *int `json:"capacity_total"`
	// Sessions is how many distinct session ids the supervisor has seen
	// announced (rc_session rows): a cache, never the card's count.
	Sessions int `json:"sessions"`
}

// SessionView is SessionData with the event's time.
type SessionView struct {
	SessionData
	At time.Time `json:"at"`
}

// ActionOutcome is one workspace.action event, as the view reports it.
type ActionOutcome struct {
	Action string    `json:"action"` // stop | delete
	Step   string    `json:"step"`
	Status string    `json:"status"` // started | done | failed
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// StepOutcome is the latest workspace.step event for one step.
type StepOutcome struct {
	Status string    `json:"status"` // started | done | failed | needs_approval
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

const viewColumns = `w.id, w.repository_id, coalesce(r.full_name, ''), w.branch, w.state,
	w.state_detail, w.container_id, coalesce(w.created_at, ''), w.environment_id, w.pending_approval`

// Views reads every workspace with a row, deleting ones included, newest
// first (ids are ULIDs, so id order is creation order).
func (s *Store) Views(ctx context.Context) ([]View, error) {
	return s.views(ctx, `SELECT `+viewColumns+` FROM workspace w LEFT JOIN repository r ON r.id = w.repository_id
		ORDER BY w.id DESC`)
}

// View reads one workspace, or ErrNotFound.
func (s *Store) View(ctx context.Context, id string) (View, error) {
	vs, err := s.views(ctx, `SELECT `+viewColumns+` FROM workspace w LEFT JOIN repository r ON r.id = w.repository_id
		WHERE w.id = ?`, id)
	if err != nil {
		return View{}, err
	}
	if len(vs) == 0 {
		return View{}, ErrNotFound
	}
	return vs[0], nil
}

func (s *Store) views(ctx context.Context, q string, args ...any) ([]View, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []View{}
	index := map[string]int{}
	for rows.Next() {
		var v View
		var detail, container, env, pending sql.NullString
		var created string
		if err := rows.Scan(&v.ID, &v.RepositoryID, &v.FullName, &v.Branch, &v.State,
			&detail, &container, &created, &env, &pending); err != nil {
			rows.Close()
			return nil, err
		}
		if detail.Valid && detail.String != "" {
			v.StateDetail = &detail.String
		}
		if container.Valid && container.String != "" {
			v.ContainerID = &container.String
		}
		if env.Valid && env.String != "" {
			v.EnvironmentID = &env.String
		}
		if pending.Valid && pending.String != "" {
			var p PendingApproval
			if err := json.Unmarshal([]byte(pending.String), &p); err != nil {
				rows.Close()
				return nil, fmt.Errorf("workspace %s pending_approval: %w", v.ID, err)
			}
			a := p.view()
			v.Approval = &a
		}
		if created != "" {
			if v.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
				rows.Close()
				return nil, fmt.Errorf("workspace %s created_at: %w", v.ID, err)
			}
		}
		v.Steps = map[Step]StepOutcome{}
		index[v.ID] = len(out)
		out = append(out, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	return out, s.fillSteps(ctx, out, index, args...)
}

// latestEvents is fillSteps' query: with no argument, over every workspace
// that has a row; with one, over that workspace alone.
func latestEvents(one ...any) (string, []any) {
	scope := `workspace_id IN (SELECT id FROM workspace)`
	args := []any{KindStep, KindAction, KindSupervisor, KindSession}
	if len(one) == 1 {
		scope = `workspace_id = ?`
		args = append(args, one[0])
	}
	return `SELECT workspace_id, kind, data, at FROM event WHERE id IN (
		SELECT max(id) FROM event
		WHERE kind IN (?, ?, ?, ?) AND ` + scope + `
		GROUP BY workspace_id, kind,
		  CASE WHEN kind = '` + KindStep + `' AND json_valid(data) THEN json_extract(data, '$.step') END
	) ORDER BY id`, args
}

// fillSteps reads, for the workspaces being listed and no others, the newest
// event of each kind the view reports: per step for workspace.step, and one
// each for workspace.action, supervisor.state and session.status.
//
// Bounded twice over (#39's review). Only live workspaces' events are read —
// a deleted workspace's history is kept on purpose (§4) and must not make
// every list request slower — and SQL keeps only the newest row per group,
// so the work is the number of steps, not the length of the history. The
// grouping key is the step's name, read with json_extract guarded by
// json_valid; the *data* is still parsed here, in Go, the way the reducer
// parses it, rather than through a second dialect.
//
// An event whose data cannot be read is skipped and logged, never an error:
// one malformed row must not take the whole list down with a 500. Its field
// is then reported as nothing known — the same as a workspace with no such
// event — and the next event of its kind replaces it.
func (s *Store) fillSteps(ctx context.Context, out []View, index map[string]int, args ...any) error {
	q, qargs := latestEvents(args...)
	rows, err := s.DB.QueryContext(ctx, q, qargs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ws, kind, at string
		var data sql.NullString
		if err := rows.Scan(&ws, &kind, &data, &at); err != nil {
			return err
		}
		i, ok := index[ws]
		if !ok || !data.Valid {
			continue
		}
		skip := func(err error) {
			s.logf("workspace %s: skipping a %s event the view cannot read: %v", ws, kind, err)
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			skip(err)
			continue
		}
		switch kind {
		case KindSupervisor:
			var d SupervisorData
			if err := json.Unmarshal([]byte(data.String), &d); err != nil || d.State == "" {
				skip(errors.Join(errors.New("no state"), err))
				continue
			}
			out[i].Supervisor = &SupervisorView{SupervisorData: d, At: t}
			continue
		case KindSession:
			var d SessionData
			if err := json.Unmarshal([]byte(data.String), &d); err != nil {
				skip(err)
				continue
			}
			out[i].Session = &SessionView{SessionData: d, At: t}
			continue
		}
		var d struct {
			Action string `json:"action"`
			Step   Step   `json:"step"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(data.String), &d); err != nil || d.Step == "" ||
			(kind == KindAction && d.Action == "") {
			skip(errors.Join(errors.New("no step or action"), err))
			continue
		}
		if kind == KindAction {
			out[i].LastAction = &ActionOutcome{Action: d.Action, Step: string(d.Step), Status: d.Status, Detail: d.Detail, At: t}
			continue
		}
		out[i].Steps[d.Step] = StepOutcome{Status: d.Status, Detail: d.Detail, At: t}
	}
	return rows.Err()
}
