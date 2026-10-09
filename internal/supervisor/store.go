//go:build linux

package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// ensureRow returns the workspace's supervisor row, creating it the first
// time (§4: one live row per workspace). The row outlives restarts — its
// restart_count is cumulative — and goes with the workspace (workspace.Remove).
func (m *Manager) ensureRow(ctx context.Context, workspaceID string) (string, int, error) {
	var id string
	var restarts int
	err := m.DB.QueryRowContext(ctx, `SELECT id, restart_count FROM supervisor WHERE workspace_id = ?
		ORDER BY started_at DESC LIMIT 1`, workspaceID).Scan(&id, &restarts)
	if err == nil {
		return id, restarts, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", 0, err
	}
	random := m.Env.Random
	if random == nil {
		random = sys.CryptoRandom{}
	}
	now := m.clock().Now().UTC()
	if id, err = workspace.NewID(now, random); err != nil {
		return "", 0, err
	}
	_, err = m.DB.ExecContext(ctx, `INSERT INTO supervisor (id, workspace_id, state, capacity, restart_count, started_at)
		VALUES (?, ?, ?, ?, 0, ?)`, id, workspaceID, string(Starting), m.policy().Capacity, now.Format(time.RFC3339Nano))
	return id, 0, err
}

func (m *Manager) storedState(ctx context.Context, row string) (State, Reason, string) {
	var st string
	var last sql.NullString
	if err := m.DB.QueryRowContext(ctx, `SELECT state, last_error FROM supervisor WHERE id = ?`, row).Scan(&st, &last); err != nil {
		return "", "", ""
	}
	return State(st), "", last.String
}

// set records a state, its reason and its sentence on the row and as a
// supervisor.state event — only when one of them changed, so a retry loop
// does not write the same event every few seconds.
func (s *sup) set(ctx context.Context, st State, r Reason, detail string, pid int) {
	s.write(ctx, st, r, detail, pid, false)
}

// announce is set without the check for a change: for the answer to a
// request, which is owed one even when it repeats the last (a restart whose
// stop fails twice the same way).
func (s *sup) announce(ctx context.Context, st State, r Reason, detail string) {
	s.write(ctx, st, r, detail, 0, true)
}

// write records a state on the row and as its supervisor.state event in one
// events.Commit, and only then in memory.
//
// One commit, because a row change and its event are one fact (the events
// package's rule): written as two steps, two writers — the loop and a Stop,
// say — could commit A then B and publish B then A, leaving the stream on a
// state the row no longer holds. Inside Commit the order of commits is the
// order of events for every writer. The event's from is read from the row in
// that same transaction, so it is the state the previous commit left,
// whichever sup — the loop's, one born stopped, or one that only records —
// wrote it.
//
// Memory after the commit, never before it, and under wmu, which every
// writer of this sup holds from the check for a change to the memory update:
// so memory moves in commit order too, and a write that fails leaves memory
// where the row is. The next write of the same state is then not taken for a
// repeat and tries again: a launch's starting that could not be recorded is
// recorded by the loop's own (#108's review), where before memory already
// said starting and the loop's was dropped as a repeat. The one event
// written without its row is announce's when the row cannot be written: an
// answer to a press is owed either way.
func (s *sup) write(ctx context.Context, st State, r Reason, detail string, pid int, always bool) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	if !always && s.state == st && s.reason == r && s.detail == detail {
		s.mu.Unlock()
		if r == ReasonLaunching && pid > 0 {
			// The launch's own starting, which the start recorded before
			// the loop ran (launchLocked): no new state to announce, but
			// the process it now has.
			s.recordLaunch(ctx, pid)
		}
		return
	}
	// A sup that knows no state writes its first event from nothing: a row
	// made just now holds ensureRow's placeholder, which no server ever had.
	known := s.state != ""
	restarts := s.restarts
	s.mu.Unlock()

	m := s.m
	now := m.clock().Now().UTC().Format(time.RFC3339Nano)
	var pidv any
	if pid > 0 {
		pidv = pid
	}
	var lastErr any
	if st == Degraded || st == WaitingRegistration || st == AwaitingLogin || r == ReasonBackoff {
		lastErr = detail
	}
	q := `UPDATE supervisor SET state = ?, restart_count = ?, capacity = ?, last_error = coalesce(?, last_error)`
	args := []any{string(st), restarts, m.policy().Capacity, lastErr}
	if r == ReasonLaunching {
		q += `, pid = ?, started_at = ?`
		args = append(args, pidv, now)
	}
	level := events.Info
	switch st {
	case Degraded:
		level = events.Error
	case WaitingRegistration, AwaitingLogin:
		level = events.Warn
	}
	if r == ReasonBackoff {
		level = events.Warn
	}
	msg := detail
	if msg == "" {
		msg = "The session server is " + string(st) + "."
	}
	err := m.commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		var from string
		if known {
			// No row (answer's, when it could not read one) is from nothing.
			err := tx.QueryRowContext(ctx, `SELECT state FROM supervisor WHERE id = ?`, s.row).Scan(&from)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, q+` WHERE id = ?`, append(args, s.row)...); err != nil {
			return nil, err
		}
		e, err := events.NewEvent(s.ws, level, workspace.KindSupervisor, msg, workspace.SupervisorData{
			State: string(st), From: from, Reason: string(r), Detail: detail, RestartCount: restarts,
		})
		return []events.Event{e}, err
	})
	if err != nil {
		m.logf("drydock: workspace %s: recording the session server's state: %v", s.ws, err)
		if always && ctx.Err() == nil {
			// An answer is owed to a press even when its row cannot be
			// written (answer's, whose row may be unreadable): the event
			// alone, from what memory knows, and memory left where it was,
			// since the row did not move.
			from := ""
			if known {
				from = string(s.current())
			}
			err = m.commit(ctx, func(*sql.Tx) ([]events.Event, error) {
				e, err := events.NewEvent(s.ws, level, workspace.KindSupervisor, msg, workspace.SupervisorData{
					State: string(st), From: from, Reason: string(r), Detail: detail, RestartCount: restarts,
				})
				return []events.Event{e}, err
			})
			if err != nil {
				m.logf("drydock: workspace %s: session server event: %v", s.ws, err)
			} else {
				s.mark(msg)
			}
		}
		return
	}
	s.mu.Lock()
	s.state, s.reason, s.detail = st, r, detail
	s.mu.Unlock()
	s.mark(msg)
}

// mark puts a recorded state's sentence in the workspace's log. A sup born
// stopped for a server no supervisor held has no log until it has something
// to record (adoptRow), which is when it is given one, under mu.
func (s *sup) mark(msg string) {
	s.mu.Lock()
	log := s.log
	s.mu.Unlock()
	if log != nil {
		log.Mark(s.m.clock().Now(), msg)
	}
}

// commit is events.Commit, or a bare transaction for a Manager with no
// event log, whose events then go nowhere.
func (m *Manager) commit(ctx context.Context, fn func(tx *sql.Tx) ([]events.Event, error)) error {
	if m.Events != nil {
		_, err := m.Events.Commit(ctx, fn)
		return err
	}
	tx, err := m.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// recordLaunch records a launched server's pid and start on the row.
func (s *sup) recordLaunch(ctx context.Context, pid int) {
	now := s.m.clock().Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.m.DB.ExecContext(ctx, `UPDATE supervisor SET pid = ?, started_at = ? WHERE id = ?`, pid, now, s.row); err != nil {
		s.m.logf("drydock: workspace %s: recording the session server's process: %v", s.ws, err)
	}
}

// countRestart writes no event of its own (the next supervisor.state carries
// the count), but takes wmu, so a state write that read the count before it
// cannot commit the older count after it.
func (s *sup) countRestart(ctx context.Context) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	s.restarts++
	n := s.restarts
	s.mu.Unlock()
	if _, err := s.m.DB.ExecContext(ctx, `UPDATE supervisor SET restart_count = ? WHERE id = ?`, n, s.row); err != nil {
		s.m.logf("drydock: workspace %s: recording a restart: %v", s.ws, err)
	}
}

func (s *sup) heartbeat(ctx context.Context) {
	now := s.m.clock().Now()
	if !s.lastBeat.IsZero() && now.Sub(s.lastBeat) < s.m.policy().HeartbeatEvery {
		return
	}
	s.lastBeat = now
	s.m.DB.ExecContext(ctx, `UPDATE supervisor SET last_heartbeat_at = ? WHERE id = ?`,
		now.UTC().Format(time.RFC3339Nano), s.row)
}

// EnvironmentURL is the card's link for an environment id (§8). Built from
// the id, never scraped: the id is what the discovery classifier vouches for.
func EnvironmentURL(env string) string {
	return "https://claude.ai/code?environment=" + env
}

// recordDiscovery records what the server newly announced — its environment
// id, sessions first seen, a capacity change — and the session.status event
// saying so, in one events.Commit: the rows and the event are one fact.
//
// The environment id goes on the workspace (§4: the card's only link), and
// the sentence says whether the stored id changed. Each session is upserted
// as an rc_session row — a cache of what the server announced; the Claude
// app is right when they disagree (§8). The first session a supervisor ever
// sees is the primary: the pre-created one in the workspace folder. The
// event carries how many sessions the supervisor has seen. It reports
// whether it committed: what it did not is recorded by a later window, or a
// heartbeat tick.
func (s *sup) recordDiscovery(ctx context.Context, d *discovered, env string, sessionIDs []string, capChanged bool) bool {
	m := s.m
	now := m.clock().Now().UTC().Format(time.RFC3339Nano)
	err := m.commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		var what []string
		if env != "" {
			res, err := tx.ExecContext(ctx, `UPDATE workspace SET environment_id = ?
				WHERE id = ? AND (environment_id IS NULL OR environment_id != ?)`, env, s.ws, env)
			if err != nil {
				return nil, fmt.Errorf("recording the environment id: %w", err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				what = append(what, "The session server's environment is "+env+".")
			} else {
				what = append(what, "The session server reconnected to environment "+env+".")
			}
		}
		for _, id := range sessionIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO rc_session (id, supervisor_id, is_primary, first_seen_at, last_seen_at)
				VALUES (?, ?, NOT EXISTS (SELECT 1 FROM rc_session WHERE supervisor_id = ? AND is_primary = 1), ?, ?)
				ON CONFLICT (id) DO UPDATE SET last_seen_at = excluded.last_seen_at`, id, s.row, s.row, now, now); err != nil {
				return nil, fmt.Errorf("recording a session: %w", err)
			}
			what = append(what, "Session "+id+" is being served.")
		}
		if capChanged {
			what = append(what, fmt.Sprintf("Capacity %d/%d.", d.capUsed, d.capTotal))
		}
		var sessions int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rc_session WHERE supervisor_id = ?`, s.row).Scan(&sessions); err != nil {
			return nil, fmt.Errorf("counting sessions: %w", err)
		}
		data := workspace.SessionData{EnvironmentID: d.env, Sessions: sessions}
		if d.env != "" {
			data.URL = EnvironmentURL(d.env)
		}
		if d.capTotal > 0 {
			used, total := d.capUsed, d.capTotal
			data.CapacityUsed, data.CapacityTotal = &used, &total
		}
		e, err := events.NewEvent(s.ws, events.Info, workspace.KindSession, strings.Join(what, " "), data)
		return []events.Event{e}, err
	})
	if err != nil {
		// Said once a run: it is tried again on every chunk and heartbeat
		// tick, and a chatty server against a failing database would
		// otherwise fill the journal with the same line.
		if !d.logged {
			d.logged = true
			m.logf("drydock: workspace %s: recording what the session server announced (tried again as it prints, and on each heartbeat; said once): %v", s.ws, err)
		}
		return false
	}
	return true
}
