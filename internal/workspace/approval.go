package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/krelinga/drydock/internal/events"
)

// Host-access approvals (design §6, "What a configuration may ask of the
// host"). A run whose configuration reaches outside the container stops at
// step 3, before any `up`, unless the operator has approved exactly that
// host-access subset for the repository. The run ends with the workspace
// stopped — its containers were stopped before the configuration was read —
// and the request on the row (pending_approval), which a workspace.state
// event carries to the UI. Approving records the subset for the repository
// and continues the run; declining clears the request and leaves the
// workspace stopped. Neither is a failure.

// KindApproved is written when the operator approves a request: data
// {repository_id, hash}. It is what settles the approve button.
const KindApproved = "config.approved"

// PendingApproval is the request a run stopped at. Settings is the whole
// host-access subset being asked for (canonical JSON, a list of
// {field, source, value}), Hash its hash; Added, Changed and Removed are how
// it differs from the subset last approved for the repository, which is what
// the operator is shown. RemoveExisting is the stopped run's
// --remove-existing-container, which the continued run keeps.
type PendingApproval struct {
	Hash           string          `json:"hash"`
	Settings       json.RawMessage `json:"settings"`
	Added          json.RawMessage `json:"added"`
	Changed        json.RawMessage `json:"changed"`
	Removed        json.RawMessage `json:"removed"`
	RemoveExisting bool            `json:"remove_existing"`
}

// ApprovalView is the part of a request the API serves and the event
// carries: never the whole configuration, only what changed in its
// host-access subset.
type ApprovalView struct {
	Hash    string          `json:"hash"`
	Added   json.RawMessage `json:"added"`
	Changed json.RawMessage `json:"changed"`
	Removed json.RawMessage `json:"removed"`
}

func (p PendingApproval) view() ApprovalView {
	return ApprovalView{Hash: p.Hash, Added: p.Added, Changed: p.Changed, Removed: p.Removed}
}

// Approval is one approved subset.
type Approval struct {
	Hash       string
	Settings   json.RawMessage
	ApprovedAt time.Time
}

var (
	// ErrNeedsApproval is what a run that stopped for an approval returns
	// (wrapped in a *StepError): not a failure.
	ErrNeedsApproval = errors.New("workspace: the configuration's host access needs the operator's approval")
	// ErrNoApproval: the workspace is not stopped waiting for an approval.
	ErrNoApproval = errors.New("workspace: no host-access approval is pending for this workspace")
	// ErrApprovalStale: the hash the operator approved is not the one the
	// workspace is waiting on — the configuration changed after it was shown.
	ErrApprovalStale = errors.New("workspace: the approval is for a different host-access subset than the one pending")
)

// NeedsApproval is what step 3 returns when the subset is not the approved
// one. Provision records the step as needs_approval, not failed, and stops
// the workspace with the request on the row.
func NeedsApproval(sentence string, p PendingApproval) error { return needsApproval{sentence, p} }

type needsApproval struct {
	sentence string
	p        PendingApproval
}

func (n needsApproval) Error() string { return n.sentence }

// Approved is the subset currently approved for a repository: the newest
// approval not superseded. ok is false when there is none.
func (s *Store) Approved(ctx context.Context, repositoryID int64) (a Approval, ok bool, err error) {
	var at, settings string
	err = s.DB.QueryRowContext(ctx, `SELECT hash, settings, approved_at FROM config_approval
		WHERE repository_id = ? AND superseded_at IS NULL ORDER BY id DESC LIMIT 1`, repositoryID).
		Scan(&a.Hash, &settings, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, err
	}
	a.Settings = json.RawMessage(settings)
	a.ApprovedAt, _ = time.Parse(time.RFC3339Nano, at)
	return a, true, nil
}

// awaitApproval stops a building workspace for an approval: state stopped,
// the sentence as its detail, the request on the row, and one
// workspace.state event carrying the request's view — one events.Commit, as
// Move is, so the row and the stream cannot disagree about which came last.
func (s *Store) awaitApproval(ctx context.Context, id, detail string, p PendingApproval) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var done func()
	_, err = s.Events.Commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		res, err := tx.ExecContext(ctx, `UPDATE workspace SET state = ?, state_detail = ?, pending_approval = ?
			WHERE id = ? AND state = ?`, string(Stopped), nullable(detail), string(b), id, string(Building))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			w, err := get(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			return nil, ErrIllegalMove{From: w.State, To: Stopped}
		}
		es, err := one(events.NewEvent(id, events.Warn, KindState, message(Stopped, detail), map[string]any{
			"state": Stopped, "from": Building, "detail": detail, "approval": p.view(),
		}))
		return withEnd(ctx, id, &done, es, err)
	})
	ended(done, err)
	return err
}

// Pending is the request a workspace is waiting on, or ErrNoApproval.
func (s *Store) Pending(ctx context.Context, id string) (Workspace, PendingApproval, error) {
	w, err := s.Get(ctx, id)
	if err != nil {
		return Workspace{}, PendingApproval{}, err
	}
	var raw sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT pending_approval FROM workspace WHERE id = ?`, id).Scan(&raw); err != nil {
		return Workspace{}, PendingApproval{}, err
	}
	if w.State != Stopped || !raw.Valid || raw.String == "" {
		return w, PendingApproval{}, ErrNoApproval
	}
	var p PendingApproval
	if err := json.Unmarshal([]byte(raw.String), &p); err != nil {
		return w, PendingApproval{}, fmt.Errorf("workspace %s: pending_approval: %w", id, err)
	}
	return w, p, nil
}

// Approve records the operator's approval of the request a workspace is
// waiting on, if hash is that request's: the repository's current approval
// is superseded (kept, never deleted) and the new one inserted in one
// transaction, then a config.approved event is written. by is the approving
// session's id. The caller then continues the run; the move out of stopped
// clears the request. It returns the request approved.
func (s *Store) Approve(ctx context.Context, id, hash, by string) (PendingApproval, error) {
	w, p, err := s.Pending(ctx, id)
	if err != nil {
		return PendingApproval{}, err
	}
	if hash == "" || hash != p.Hash {
		return PendingApproval{}, ErrApprovalStale
	}
	now := ts(s.Env.Clock.Now())
	_, err = s.Events.Commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		// The request is re-checked inside the transaction, so two approvals
		// of one request, or an approval racing a move, cannot both record.
		var still sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT pending_approval FROM workspace WHERE id = ? AND state = ?`,
			id, string(Stopped)).Scan(&still); err != nil || !still.Valid {
			return nil, ErrNoApproval
		}
		var cur PendingApproval
		if json.Unmarshal([]byte(still.String), &cur) != nil || cur.Hash != hash {
			return nil, ErrApprovalStale
		}
		if _, err := tx.ExecContext(ctx, `UPDATE config_approval SET superseded_at = ?
			WHERE repository_id = ? AND superseded_at IS NULL`, now, w.RepositoryID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO config_approval
			(repository_id, hash, settings, workspace_id, approved_by, approved_at) VALUES (?, ?, ?, ?, ?, ?)`,
			w.RepositoryID, p.Hash, string(p.Settings), id, by, now); err != nil {
			return nil, err
		}
		return one(events.NewEvent(id, events.Warn, KindApproved,
			"Host access approved for this repository's configuration.",
			map[string]any{"repository_id": w.RepositoryID, "hash": p.Hash}))
	})
	if err != nil {
		return PendingApproval{}, err
	}
	return p, nil
}

// DeclineDetail is the stopped workspace's detail after a decline.
const DeclineDetail = "Host access was not approved, so the workspace stays stopped. Start asks again."

// Decline clears the request a workspace is waiting on and leaves it
// stopped, with a workspace.state event carrying no request — which is what
// settles the cancel button and takes the approval off the card.
func (s *Store) Decline(ctx context.Context, id string) error {
	_, err := s.Events.Commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		res, err := tx.ExecContext(ctx, `UPDATE workspace SET pending_approval = NULL, state_detail = ?
			WHERE id = ? AND state = ? AND pending_approval IS NOT NULL`, DeclineDetail, id, string(Stopped))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := get(ctx, tx, id); err != nil {
				return nil, err
			}
			return nil, ErrNoApproval
		}
		return one(events.NewEvent(id, events.Info, KindState, message(Stopped, DeclineDetail),
			map[string]any{"state": Stopped, "from": Stopped, "detail": DeclineDetail}))
	})
	return err
}
