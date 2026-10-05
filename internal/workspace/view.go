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
	Status string    `json:"status"` // started | done | failed
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

const viewColumns = `w.id, w.repository_id, coalesce(r.full_name, ''), w.branch, w.state,
	w.state_detail, w.container_id, coalesce(w.created_at, '')`

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
		var detail, container sql.NullString
		var created string
		if err := rows.Scan(&v.ID, &v.RepositoryID, &v.FullName, &v.Branch, &v.State,
			&detail, &container, &created); err != nil {
			rows.Close()
			return nil, err
		}
		if detail.Valid && detail.String != "" {
			v.StateDetail = &detail.String
		}
		if container.Valid && container.String != "" {
			v.ContainerID = &container.String
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

// fillSteps reads the step and action events oldest first and keeps the last
// per step, and the last action, which are the latest. Read in Go rather than
// with a GROUP BY over json_extract: data is the reducer's contract, and the
// API parses it the same way the reducer does rather than through a second
// dialect.
func (s *Store) fillSteps(ctx context.Context, out []View, index map[string]int, args ...any) error {
	q := `SELECT workspace_id, kind, data, at FROM event WHERE kind IN (?, ?) AND workspace_id IS NOT NULL ORDER BY id`
	qargs := []any{KindStep, KindAction}
	if len(args) == 1 { // one workspace
		q = `SELECT workspace_id, kind, data, at FROM event WHERE kind IN (?, ?) AND workspace_id = ? ORDER BY id`
		qargs = append(qargs, args[0])
	}
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
		var d struct {
			Action string `json:"action"`
			Step   Step   `json:"step"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(data.String), &d); err != nil || d.Step == "" ||
			(kind == KindAction && d.Action == "") {
			return errors.Join(fmt.Errorf("workspace %s: a %s event has unreadable data", ws, kind), err)
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return fmt.Errorf("workspace %s: %s event time: %w", ws, kind, err)
		}
		if kind == KindAction {
			out[i].LastAction = &ActionOutcome{Action: d.Action, Step: string(d.Step), Status: d.Status, Detail: d.Detail, At: t}
			continue
		}
		out[i].Steps[d.Step] = StepOutcome{Status: d.Status, Detail: d.Detail, At: t}
	}
	return rows.Err()
}
