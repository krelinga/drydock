// Package events is Drydock's append-only event log and its live fan-out:
// the `event` table (design §4) and what feeds GET /api/events.
//
// Two jobs, one lock. Every state change in the system is written here, so a
// failure at step six of a clone names step six (§6), and the same row is what
// the frontend's reducer applies (frontend §4.1) — it is the only thing that
// ever changes what a browser shows. Writing and publishing happen under one
// mutex, so subscribers see events in id order. Without that, two concurrent
// appends could publish 6 before 5, and a client that dropped its connection
// between the two would resume after 6 and never see 5.
//
// Redact by default (§13.5) applies to Message and Data like every other sink.
// Callers compose them from Drydock's own facts — a state name, a step, an id —
// and never from a credential, a login code, a secret value, or raw subprocess
// output. The canary sweep reads this table's raw bytes to hold that line.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// Level is an event's severity, for the activity view's styling.
type Level string

const (
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

// Event is one row of the log.
type Event struct {
	ID int64 `json:"id"`
	// WorkspaceID is empty for events about the whole system (auth.*).
	WorkspaceID string `json:"workspace_id,omitempty"`
	Level       Level  `json:"level"`
	// Kind is dotted, entity first: workspace.state, session.seen, auth.identity.
	Kind string `json:"kind"`
	// Message is for a human, and nothing may parse it.
	Message string `json:"message"`
	// Data is what the reducer applies: a JSON object, or absent.
	Data json.RawMessage `json:"data,omitempty"`
	At   time.Time       `json:"at"`
}

// DefaultReplayWindow is how many events a reconnecting client may be behind
// and still be caught up by replay rather than told to resync. A thousand is
// many hours of a busy fleet, and the cost of exceeding it is one refetch.
const DefaultReplayWindow = 1000

// subBuffer is how far a subscriber may lag before it is cut off. Cutting it
// off is safe — the client reconnects with Last-Event-ID and replays the gap —
// whereas blocking would let one stalled phone hold the lock every writer
// needs.
const subBuffer = 256

// Log is the event table plus its live subscribers.
type Log struct {
	DB    *sql.DB
	Clock sys.Clock
	// ReplayWindow overrides DefaultReplayWindow when positive.
	ReplayWindow int

	mu     sync.Mutex
	subs   map[*Sub]struct{}
	closed bool
}

// New returns a Log over db.
func New(db *sql.DB, clock sys.Clock) *Log {
	return &Log{DB: db, Clock: clock}
}

func (l *Log) window() int {
	if l.ReplayWindow > 0 {
		return l.ReplayWindow
	}
	return DefaultReplayWindow
}

// Append writes e, assigns its ID and time, and delivers it to every
// subscriber. A nil or empty Data is stored as NULL; anything else must be a
// JSON object, so the reducer never has to handle a bare string or array.
func (l *Log) Append(ctx context.Context, e Event) (Event, error) {
	if e.Kind == "" {
		return Event{}, errors.New("events: an event needs a kind")
	}
	switch e.Level {
	case Info, Warn, Error:
	case "":
		e.Level = Info
	default:
		return Event{}, fmt.Errorf("events: unknown level %q", e.Level)
	}
	var data any // NULL unless there is an object to store
	if len(e.Data) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e.Data, &obj); err != nil || obj == nil {
			return Event{}, fmt.Errorf("events: %s data must be a JSON object", e.Kind)
		}
		data = string(e.Data)
	}
	var ws any
	if e.WorkspaceID != "" {
		ws = e.WorkspaceID
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	e.At = l.Clock.Now().UTC()
	res, err := l.DB.ExecContext(ctx,
		`INSERT INTO event (workspace_id, level, kind, message, data, at) VALUES (?, ?, ?, ?, ?, ?)`,
		ws, string(e.Level), e.Kind, e.Message, data, e.At.Format(time.RFC3339Nano))
	if err != nil {
		return Event{}, fmt.Errorf("events: append %s: %w", e.Kind, err)
	}
	if e.ID, err = res.LastInsertId(); err != nil {
		return Event{}, err
	}
	for s := range l.subs {
		select {
		case s.ch <- e:
		default:
			l.drop(s) // too far behind; it will reconnect and replay
		}
	}
	return e, nil
}

// Emit is Append for callers that build Data from a Go value.
func (l *Log) Emit(ctx context.Context, workspaceID string, level Level, kind, message string, data any) (Event, error) {
	e := Event{WorkspaceID: workspaceID, Level: level, Kind: kind, Message: message}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return Event{}, fmt.Errorf("events: %s data: %w", kind, err)
		}
		e.Data = b
	}
	return l.Append(ctx, e)
}

// Sub is one live subscriber. Events arrive on C in id order; C is closed when
// the subscriber falls too far behind, is cancelled, or the Log closes — or
// at once, for a Sub made after the Log closed. C is closed exactly once
// whichever of those happen, in whatever order: every close goes through end.
type Sub struct {
	ch   chan Event
	C    <-chan Event
	once sync.Once
}

// end closes C, once. It is the only place that does.
func (s *Sub) end() { s.once.Do(func() { close(s.ch) }) }

// Subscribe registers a subscriber. Call it before reading the backlog with
// Since, so nothing written in between is missed; the overlap is the caller's
// to skip, by id. After Close it returns a Sub that is already over, and
// Cancel on it is as safe as on any other: a goroutine Serve started can
// subscribe after shutdown has closed the log.
func (l *Log) Subscribe() *Sub {
	s := &Sub{ch: make(chan Event, subBuffer)}
	s.C = s.ch
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		s.end()
		return s
	}
	if l.subs == nil {
		l.subs = map[*Sub]struct{}{}
	}
	l.subs[s] = struct{}{}
	return s
}

// Cancel unregisters s. Safe to call more than once, and in any order with
// Close.
func (l *Log) Cancel(s *Sub) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drop(s)
}

func (l *Log) drop(s *Sub) { // l.mu held
	delete(l.subs, s)
	s.end()
}

// Close ends every subscription, which ends every open stream: the server
// calls it before shutting down, since a streaming handler never goes idle on
// its own.
func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for s := range l.subs {
		l.drop(s)
	}
}

// Latest returns the highest event ID, or 0 for an empty log.
func (l *Log) Latest(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := l.DB.QueryRowContext(ctx, `SELECT max(id) FROM event`).Scan(&id)
	return id.Int64, err
}

// ErrResync means a client asked to resume from a point replay cannot reach:
// further behind than the window, or an id this log never issued (a restored
// or replaced database). The client must refetch rather than replay.
var ErrResync = errors.New("events: too far behind to replay; resync")

// Since returns every event after id, oldest first — or ErrResync if there are
// more of them than the replay window, or if id is ahead of the log.
func (l *Log) Since(ctx context.Context, id int64) ([]Event, error) {
	latest, err := l.Latest(ctx)
	if err != nil {
		return nil, err
	}
	if id > latest {
		return nil, ErrResync
	}
	rows, err := l.DB.QueryContext(ctx,
		`SELECT id, coalesce(workspace_id,''), coalesce(level,''), coalesce(kind,''),
		        coalesce(message,''), data, at
		 FROM event WHERE id > ? ORDER BY id LIMIT ?`, id, l.window()+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > l.window() {
		return nil, ErrResync
	}
	return out, nil
}

// ForWorkspace returns a workspace's most recent events, newest first, for
// the detail view's activity list.
func (l *Log) ForWorkspace(ctx context.Context, workspaceID string, limit int) ([]Event, error) {
	rows, err := l.DB.QueryContext(ctx,
		`SELECT id, coalesce(workspace_id,''), coalesce(level,''), coalesce(kind,''),
		        coalesce(message,''), data, at
		 FROM event WHERE workspace_id = ? ORDER BY id DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scan(rows *sql.Rows) (Event, error) {
	var e Event
	var level, at string
	var data sql.NullString
	if err := rows.Scan(&e.ID, &e.WorkspaceID, &level, &e.Kind, &e.Message, &data, &at); err != nil {
		return Event{}, err
	}
	e.Level = Level(level)
	if data.Valid {
		e.Data = json.RawMessage(data.String)
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Event{}, fmt.Errorf("events: event %d time: %w", e.ID, err)
	}
	e.At = t
	return e, nil
}
