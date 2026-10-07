package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// Workspace is one row of the workspace table (§4): one clone, and the unit
// the UI operates on.
type Workspace struct {
	ID           string    `json:"id"`
	RepositoryID int64     `json:"repository_id"`
	HostPath     string    `json:"host_path"`
	Branch       string    `json:"branch"`
	State        State     `json:"state"`
	StateDetail  string    `json:"state_detail,omitempty"`
	ContainerID  string    `json:"container_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Store reads and moves workspace rows, writing an event for every move.
type Store struct {
	DB     *sql.DB
	Events *events.Log
	Env    sys.Env
	// Root is the workspace root: a workspace's clone is Root/<id>/repo.
	Root string
	// Cap is the concurrent-container cap (config.ContainerCap).
	Cap int
	// GrantsDropped is called after a Remove commits that deleted secret
	// grants — its repository had left the installation, and this workspace
	// was what held it — so the secrets store rebuilds the snapshot the
	// broker serves from (secrets.Store.Invalidate).
	GrantsDropped func()
}

var (
	// ErrInProgress refuses a second workspace for a repository that already
	// has one — in any state (frontend §4.5 #3): a double-tap on a phone is a
	// real input, and two clones of one repo is a wasted build and a
	// confusing list. Any state, not only the occupying ones, because a
	// stopped or failed workspace still holds the clone, and its way back is
	// start, not a second clone; a deleting one is removing its clone, and
	// a create waits for that to finish rather than racing it. One
	// repository, one workspace, until delete.
	ErrInProgress = errors.New("workspace: this repository already has a workspace")
	// ErrAtCap refuses a create past the concurrent-container cap (§6 step 1).
	ErrAtCap = errors.New("workspace: the concurrent-container cap is reached")
	// ErrNotFound is a workspace id with no row.
	ErrNotFound = errors.New("workspace: no such workspace")
)

// Kinds of event this package writes. The frontend's reducer switches on
// these; data carries the fields it applies.
const (
	KindState = "workspace.state" // data: {state, from, detail?}
	KindStep  = "workspace.step"  // data: {step, status, detail?}
	KindGone  = "workspace.gone"  // data: {} — the row is removed
	// KindAction is a stop's or a delete's sub-step, written by
	// internal/provision. data: {action: stop|delete, step, status, detail?}
	KindAction = "workspace.action"
)

// Create is step 1, allocate: a new row in pending, with its directory path
// minted but not yet made (the caller makes it, so a failure there is the
// allocate step failing, with its own event).
func (s *Store) Create(ctx context.Context, repositoryID int64, branch string) (Workspace, error) {
	if branch == "" {
		return Workspace{}, errors.New("workspace: a branch is required")
	}
	now := s.Env.Clock.Now().UTC()
	id, err := NewID(now, s.Env.Random)
	if err != nil {
		return Workspace{}, err
	}
	w := Workspace{
		ID: id, RepositoryID: repositoryID, Branch: branch, State: Pending,
		HostPath: filepath.Join(s.Root, id, "repo"), CreatedAt: now,
	}

	// One transaction for the two checks and the insert, so two creates
	// racing past the checks cannot both land: the store opens every
	// transaction IMMEDIATE, so the write lock is held before the reads.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, err
	}
	defer tx.Rollback()
	var same, total int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM workspace WHERE repository_id = ?`, // any state: see ErrInProgress
		repositoryID).Scan(&same); err != nil {
		return Workspace{}, err
	}
	if same > 0 {
		return Workspace{}, ErrInProgress
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM workspace WHERE state IN (`+occupyingSQL+`)`).Scan(&total); err != nil {
		return Workspace{}, err
	}
	if s.Cap > 0 && total >= s.Cap {
		return Workspace{}, ErrAtCap
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		w.ID, w.RepositoryID, w.HostPath, w.Branch, string(w.State), ts(now)); err != nil {
		return Workspace{}, fmt.Errorf("workspace: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, err
	}
	_, err = s.Events.Emit(ctx, w.ID, events.Info, KindState, "Workspace created.",
		map[string]any{"state": Pending, "repository_id": repositoryID, "branch": branch})
	return w, err
}

// Occupied counts the workspaces holding or building a container: what the
// concurrent-container cap is measured against (Occupying).
func (s *Store) Occupied(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM workspace WHERE state IN (`+occupyingSQL+`)`).Scan(&n)
	return n, err
}

// Move takes a workspace from its current state to `to`, refusing an illegal
// move, and writes a workspace.state event. The update is conditional on the
// state it read, so two movers racing cannot both win: the loser gets an
// ErrIllegalMove naming the state it lost to, or retries.
func (s *Store) Move(ctx context.Context, id string, to State, detail string) (Workspace, error) {
	for attempt := 0; attempt < 3; attempt++ {
		w, err := s.Get(ctx, id)
		if err != nil {
			return Workspace{}, err
		}
		if !CanMove(w.State, to) {
			return Workspace{}, ErrIllegalMove{From: w.State, To: to}
		}
		res, err := s.DB.ExecContext(ctx,
			`UPDATE workspace SET state = ?, state_detail = ? WHERE id = ? AND state = ?`,
			string(to), nullable(detail), id, string(w.State))
		if err != nil {
			return Workspace{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // moved under us; re-read and re-check
		}
		level := events.Info
		if to == Failed {
			level = events.Error
		}
		data := map[string]any{"state": to, "from": w.State}
		// Reaching running is when the container id matters to a reader,
		// and no other event carries it; the reducer applies it from here
		// rather than refetching the workspace.
		if to == Running && w.ContainerID != "" {
			data["container_id"] = w.ContainerID
		}
		if detail != "" {
			data["detail"] = detail
		}
		if _, err := s.Events.Emit(ctx, id, level, KindState, message(to, detail), data); err != nil {
			return Workspace{}, err
		}
		w.State, w.StateDetail = to, detail
		return w, nil
	}
	return Workspace{}, fmt.Errorf("workspace %s: state kept changing during a move to %s", id, to)
}

// SetContainer records the container id, a cache reconciliation rebuilds
// from labels (§6). Empty clears it.
func (s *Store) SetContainer(ctx context.Context, id, containerID string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE workspace SET container_id = ? WHERE id = ?`, nullable(containerID), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Annotate sets the detail of a workspace without moving it, and writes a
// workspace.state event carrying the same state and the new detail — so the
// card says why a workspace is where it is when nothing moved it. Used for a
// delete that stopped part-way — the workspace stays deleting, which is
// resumable, and the detail names the sub-step that failed — and for a stop
// that failed, which leaves the workspace running (frontend §4.5 #15). Any
// later move replaces it (Move writes its own detail), and ClearDetail
// removes it as a retry starts.
func (s *Store) Annotate(ctx context.Context, id string, want State, detail string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE workspace SET state_detail = ? WHERE id = ? AND state = ?`,
		nullable(detail), id, string(want))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		w, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		return ErrIllegalMove{From: w.State, To: want}
	}
	data := map[string]any{"state": want, "from": want}
	if detail != "" {
		data["detail"] = detail
	}
	_, err = s.Events.Emit(ctx, id, events.Warn, KindState, message(want, detail), data)
	return err
}

// ClearDetail removes the detail Annotate set, as the operation it explains
// starts again: a resumed delete, or a stop asked for again after one failed
// (frontend §4.5 #15, #16). It writes a workspace.state event carrying the
// same state and no detail, so a client following the stream drops the
// sentence as the retry begins, and one that reads only GET /api/workspaces
// stops offering the retry for a job that is already running. A workspace
// with no detail, or not in want, is left alone and nothing is written;
// cleared reports whether there was a detail to clear.
func (s *Store) ClearDetail(ctx context.Context, id string, want State) (cleared bool, err error) {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE workspace SET state_detail = NULL WHERE id = ? AND state = ? AND state_detail IS NOT NULL`,
		id, string(want))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := s.Events.Emit(ctx, id, events.Info, KindState, message(want, ""),
		map[string]any{"state": want, "from": want}); err != nil {
		return false, err
	}
	return true, nil
}

// Remove deletes the row of a workspace whose delete has finished. Only a
// workspace in Deleting can be removed: the persisted state is what lets an
// interrupted delete be resumed at boot rather than forgotten.
//
// What it leaves is deliberate. The event log, token_grant and
// secret_access carry the workspace id with no foreign key and are kept:
// "which workspaces ever held this secret?" (§10.4) is asked after the fact,
// and a deleted workspace is exactly one whose history that question needs.
// The supervisor row (§4) references the workspace, so it goes in the same
// transaction — it describes a process, and the process is gone. So does its
// repository's row, with that repository's secret grants, when the
// installation has dropped the repository: this workspace was the one thing
// keeping them (§4, §12), and a repository re-added later is granted nothing.
func (s *Store) Remove(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The supervisor rows first, and only for a workspace in deleting:
	// foreign keys are checked per statement.
	if _, err := tx.ExecContext(ctx, `DELETE FROM supervisor WHERE workspace_id = ?
		AND EXISTS (SELECT 1 FROM workspace WHERE id = ? AND state = ?)`, id, id, string(Deleting)); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM workspace WHERE id = ? AND state = ?`, id, string(Deleting))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		tx.Rollback()
		w, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		return ErrIllegalMove{From: w.State, To: "removed"}
	}
	dropped, err := store.DropReleasedRepositories(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if dropped > 0 && s.GrantsDropped != nil {
		s.GrantsDropped()
	}
	_, err = s.Events.Emit(ctx, id, events.Info, KindGone, "Workspace deleted.", map[string]any{})
	return err
}

// Get reads one workspace.
func (s *Store) Get(ctx context.Context, id string) (Workspace, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+columns+` FROM workspace WHERE id = ?`, id)
	w, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	return w, err
}

// List reads every workspace, oldest first.
func (s *Store) List(ctx context.Context) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+columns+` FROM workspace ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

const columns = `id, repository_id, host_path, branch, state, coalesce(state_detail,''),
	coalesce(container_id,''), coalesce(created_at,'')`

type scanner interface{ Scan(...any) error }

func scan(r scanner) (Workspace, error) {
	var w Workspace
	var state, created string
	if err := r.Scan(&w.ID, &w.RepositoryID, &w.HostPath, &w.Branch, &state, &w.StateDetail,
		&w.ContainerID, &created); err != nil {
		return Workspace{}, err
	}
	w.State = State(state)
	if created != "" {
		t, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return Workspace{}, fmt.Errorf("workspace %s created_at: %w", w.ID, err)
		}
		w.CreatedAt = t
	}
	return w, nil
}

func message(to State, detail string) string {
	m := map[State]string{
		Pending: "Waiting to start.", Cloning: "Cloning.", Building: "Building the container.",
		Running: "Running.", Stopped: "Stopped.", Failed: "Failed.", Deleting: "Deleting.",
	}[to]
	if detail != "" {
		m += " " + detail
	}
	return m
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Adopt recreates the row for a container found with no row — an orphan from
// a lost database (§6). It bypasses Create's duplicate and cap checks on
// purpose: the container already exists, and refusing to record it would
// not make it go away, only make it invisible. The repository row is a cache
// (§4), so a stub is written if the repository is not known yet, for the
// next catalog refresh to fill in.
func (s *Store) Adopt(ctx context.Context, w Workspace, fullName string) error {
	if w.State != Running && w.State != Stopped {
		return fmt.Errorf("workspace: an orphan is adopted as running or stopped, not %s", w.State)
	}
	if w.HostPath == "" {
		w.HostPath = filepath.Join(s.Root, w.ID, "repo")
	}
	now := s.Env.Clock.Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (?, 0, ?, ?)
		 ON CONFLICT(id) DO NOTHING`, w.RepositoryID, fullName, w.Branch); err != nil {
		return fmt.Errorf("workspace: stub repository: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state, state_detail, container_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.RepositoryID, w.HostPath, w.Branch, string(w.State), nullable(w.StateDetail),
		nullable(w.ContainerID), ts(now)); err != nil {
		return fmt.Errorf("workspace: adopt %s: %w", w.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	data := map[string]any{"state": w.State, "adopted": true, "repository_id": w.RepositoryID, "branch": w.Branch}
	if w.StateDetail != "" {
		data["detail"] = w.StateDetail
	}
	_, err = s.Events.Emit(ctx, w.ID, events.Warn, KindState, message(w.State, w.StateDetail), data)
	return err
}
