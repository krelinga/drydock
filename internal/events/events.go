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
//
// # Rules and details
//
// A subscriber that lags 256 events is cut off rather than allowed to block
// writers, and recovers by replay. A subscription's channel is closed in
// exactly one place, behind one sync.Once, so Cancel, Close and a lag cut-off
// are safe in any order and any number of times — and Subscribe after Close
// returns one already closed, since a goroutine Serve started can subscribe
// after shutdown closed the log (a second close was a release-run panic).
//
// data is a JSON object for the reducer; message is prose nothing may parse.
//
// Commit(ctx, fn) runs fn's transaction and appends the events it returns in
// that same transaction, under the same lock, publishing only after the
// commit: a row change and its event are one fact, so commit order is id order
// is publish order for every writer.
//
// The row rule: an event that describes a row change goes through Commit, in
// the transaction that changes the row. Emit and Append are the exception,
// for events with no row (token.refused, the login's phases, a step, a
// container fact). TestRowRule lists every bare call with the reason it has
// no row, and fails on a new one.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// Live, when set, marks a frame Broadcast sent: never a row, with no id,
	// named Live on the stream, and Data its whole payload. Never marshalled.
	Live string `json:"-"`
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
	p, err := prepare(e)
	if err != nil {
		return Event{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, err = l.insert(ctx, l.DB, p); err != nil {
		return Event{}, err
	}
	l.publish(p.Event)
	return p.Event, nil
}

// Commit runs fn in one transaction, appends the events fn returns in that
// same transaction, commits, and only then publishes them — all under the
// log's lock, the one Append takes.
//
// It is for a change whose event must not be told apart from the change
// itself: a workspace's state and its workspace.state event. Committing the
// row and then appending the event as two steps let two movers interleave —
// A commits, B reads A's state, commits and appends, then A appends — so the
// stream ended on A's state while the row held B's. Under Commit the order
// of commits is the order of ids is the order of publication, for every
// writer, because every writer of the event table holds the same lock while
// it commits. It also closes the crash window between the two: the row and
// its event are written together or not at all.
//
// fn returning an error rolls everything back; returning no events commits
// and publishes nothing. fn must not call Append, Emit or Commit (the lock is
// held), and should be short: every other event waits on it.
func (l *Log) Commit(ctx context.Context, fn func(tx *sql.Tx) ([]Event, error)) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	es, err := fn(tx)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(es))
	for _, e := range es {
		p, err := prepare(e)
		if err != nil {
			return nil, err
		}
		if p, err = l.insert(ctx, tx, p); err != nil {
			return nil, err
		}
		out = append(out, p.Event)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, e := range out {
		l.publish(e)
	}
	return out, nil
}

// prepared is an event checked for its kind, level and data, with the data
// and workspace as they are stored.
type prepared struct {
	Event
	data any // NULL unless there is an object to store
	ws   any
}

func prepare(e Event) (prepared, error) {
	if e.Kind == "" {
		return prepared{}, errors.New("events: an event needs a kind")
	}
	switch e.Level {
	case Info, Warn, Error:
	case "":
		e.Level = Info
	default:
		return prepared{}, fmt.Errorf("events: unknown level %q", e.Level)
	}
	p := prepared{Event: e}
	if len(e.Data) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e.Data, &obj); err != nil || obj == nil {
			return prepared{}, fmt.Errorf("events: %s data must be a JSON object", e.Kind)
		}
		p.data = string(e.Data)
	}
	if e.WorkspaceID != "" {
		p.ws = e.WorkspaceID
	}
	return p, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (l *Log) insert(ctx context.Context, db execer, p prepared) (prepared, error) { // l.mu held
	p.At = l.Clock.Now().UTC()
	res, err := db.ExecContext(ctx,
		`INSERT INTO event (workspace_id, level, kind, message, data, at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.ws, string(p.Level), p.Kind, p.Message, p.data, p.At.Format(time.RFC3339Nano))
	if err != nil {
		return p, fmt.Errorf("events: append %s: %w", p.Kind, err)
	}
	if p.ID, err = res.LastInsertId(); err != nil {
		return p, err
	}
	return p, nil
}

func (l *Log) publish(e Event) { // l.mu held
	for s := range l.subs {
		select {
		case s.ch <- e:
		default:
			l.drop(s) // too far behind; it will reconnect and replay
		}
	}
}

// Broadcast sends v to every live subscriber as a frame named name, and
// writes nothing: no row, no id, no replay. It is for a measurement (design
// §6, *Resources*) — a value the next one replaces, which a client that missed
// it loses nothing by missing, and which persisted every half-minute would
// push the events that matter out of the replay window. So a subscriber whose
// buffer is full skips it rather than being cut off for it.
//
// name must be a bare word; the stream writes it as the frame's `event:`.
func (l *Log) Broadcast(name string, v any) error {
	if name == "" || strings.ContainsAny(name, " \r\n:") {
		return fmt.Errorf("events: %q is not a frame name", name)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("events: %s: %w", name, err)
	}
	e := Event{Live: name, Data: b}
	l.mu.Lock()
	defer l.mu.Unlock()
	for s := range l.subs {
		select {
		case s.ch <- e:
		default: // behind: this one is skipped, and the next replaces it
		}
	}
	return nil
}

// NewEvent builds an Event whose Data is v marshalled: Commit's callers use
// it as Emit's do implicitly.
func NewEvent(workspaceID string, level Level, kind, message string, v any) (Event, error) {
	e := Event{WorkspaceID: workspaceID, Level: level, Kind: kind, Message: message}
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return Event{}, fmt.Errorf("events: %s data: %w", kind, err)
		}
		e.Data = b
	}
	return e, nil
}

// Emit is Append for callers that build Data from a Go value. Both are for
// events with no row; one that describes a row change goes through Commit
// (the row rule, in the package comment).
func (l *Log) Emit(ctx context.Context, workspaceID string, level Level, kind, message string, data any) (Event, error) {
	e, err := NewEvent(workspaceID, level, kind, message, data)
	if err != nil {
		return Event{}, err
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
