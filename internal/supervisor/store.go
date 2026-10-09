//go:build linux

package supervisor

import (
	"context"
	"database/sql"
	"errors"
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

func (s *sup) write(ctx context.Context, st State, r Reason, detail string, pid int, always bool) {
	s.mu.Lock()
	if !always && s.state == st && s.reason == r && s.detail == detail {
		s.mu.Unlock()
		return
	}
	from := s.state
	s.state, s.reason, s.detail = st, r, detail
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
	if _, err := m.DB.ExecContext(ctx, q+` WHERE id = ?`, append(args, s.row)...); err != nil {
		m.logf("drydock: workspace %s: recording the session server's state: %v", s.ws, err)
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
	if m.Events != nil {
		if _, err := m.Events.Emit(ctx, s.ws, level, workspace.KindSupervisor, msg, workspace.SupervisorData{
			State: string(st), From: string(from), Reason: string(r), Detail: detail, RestartCount: restarts,
		}); err != nil {
			m.logf("drydock: workspace %s: session server event: %v", s.ws, err)
		}
	}
	s.log.Mark(m.clock().Now(), msg)
}

func (s *sup) countRestart(ctx context.Context) {
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

// recordEnvironment stores the environment id on the workspace (§4: the
// card's only link). It reports whether the stored id changed.
func (s *sup) recordEnvironment(ctx context.Context, env string) bool {
	res, err := s.m.DB.ExecContext(ctx, `UPDATE workspace SET environment_id = ?
		WHERE id = ? AND (environment_id IS NULL OR environment_id != ?)`, env, s.ws, env)
	if err != nil {
		s.m.logf("drydock: workspace %s: recording the environment id: %v", s.ws, err)
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// recordSession upserts an rc_session row — a cache of what the server
// announced; the Claude app is right when they disagree (§8). The first
// session a supervisor ever sees is the primary: the pre-created one in the
// workspace folder. It returns how many sessions the supervisor has seen.
func (s *sup) recordSession(ctx context.Context, id string) int {
	now := s.m.clock().Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.m.DB.ExecContext(ctx, `INSERT INTO rc_session (id, supervisor_id, is_primary, first_seen_at, last_seen_at)
		VALUES (?, ?, NOT EXISTS (SELECT 1 FROM rc_session WHERE supervisor_id = ? AND is_primary = 1), ?, ?)
		ON CONFLICT (id) DO UPDATE SET last_seen_at = excluded.last_seen_at`, id, s.row, s.row, now, now); err != nil {
		s.m.logf("drydock: workspace %s: recording a session: %v", s.ws, err)
	}
	return s.sessionCount(ctx)
}

func (s *sup) sessionCount(ctx context.Context) int {
	var n int
	s.m.DB.QueryRowContext(ctx, `SELECT count(*) FROM rc_session WHERE supervisor_id = ?`, s.row).Scan(&n)
	return n
}

// EnvironmentURL is the card's link for an environment id (§8). Built from
// the id, never scraped: the id is what the discovery classifier vouches for.
func EnvironmentURL(env string) string {
	return "https://claude.ai/code?environment=" + env
}

func (s *sup) emitSession(ctx context.Context, d *discovered, sessions int, what string) {
	if s.m.Events == nil {
		return
	}
	data := workspace.SessionData{EnvironmentID: d.env, Sessions: sessions}
	if d.env != "" {
		data.URL = EnvironmentURL(d.env)
	}
	if d.capTotal > 0 {
		used, total := d.capUsed, d.capTotal
		data.CapacityUsed, data.CapacityTotal = &used, &total
	}
	if _, err := s.m.Events.Emit(ctx, s.ws, events.Info, workspace.KindSession, what, data); err != nil {
		s.m.logf("drydock: workspace %s: session event: %v", s.ws, err)
	}
}
