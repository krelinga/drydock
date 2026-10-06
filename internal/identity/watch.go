// Package identity is the expiry watch (design §7.3): on an interval, at
// boot and on demand it reads the shared Claude login and stores the verdict
// — ok, expiring, expired, blanked or absent — in claude_identity, so the
// banner, the cards and GET /api/auth/claude all read one stored answer
// rather than each deriving their own (§4).
//
// The verdict is internal/classify's, exactly as built: the credential file
// decides blanked and absent before `claude auth status` is consulted, so a
// broken or reshaped second read can never hide the tombstone that takes
// every workspace down at once.
//
// A check that cannot read its inputs is not a verdict. It keeps the stored
// state, updates last_checked_at, and says what went wrong — an
// auth.identity_check_failed event and the check_error GET /api/auth/claude
// reports — and never writes ok on the strength of something it could not
// read.
//
// Redact by default (§13.5): the credential file's bytes exist in this
// package only between the read and the classifier. Nothing written anywhere
// — the row, an event, the service log, an HTTP body, an error — is built
// from them; the row and the events carry the verdict, the account email
// `auth status` reports, and the expiry, and the failure sentences are
// Drydock's own.
package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
)

// State is claude_identity.state.
type State string

const (
	OK       State = "ok"
	Expiring State = "expiring"
	Expired  State = "expired"
	Blanked  State = "blanked"
	Absent   State = "absent"
)

// Live reports whether a state is a login on the volume, as opposed to none.
func (s State) Live() bool { return s == OK || s == Expiring || s == Expired }

func stateOf(s classify.IdentityState) State {
	switch s {
	case classify.IdentityOK:
		return OK
	case classify.IdentityExpiring:
		return Expiring
	case classify.IdentityExpired:
		return Expired
	case classify.IdentityBlanked:
		return Blanked
	case classify.IdentityAbsent:
		return Absent
	}
	panic(fmt.Sprintf("identity: classifier state %d has no stored name", s))
}

// Event kinds.
const (
	// KindIdentity: the stored identity changed — its state, its account,
	// its expiry — or a failing check recovered. data: {identity: View}.
	KindIdentity = "auth.identity"
	// KindCheckFailed: a check could not read its inputs. The stored state
	// stands. data: {check_error: {at, problem, message}}.
	KindCheckFailed = "auth.identity_check_failed"
)

// DefaultInterval is §7.3's six hours.
const DefaultInterval = 6 * time.Hour

// CheckError is the last check's failure, in Drydock's words.
type CheckError struct {
	At      time.Time `json:"at"`
	Problem Problem   `json:"problem"`
	Message string    `json:"message"`
}

// View is the identity as GET /api/auth/claude and auth.identity carry it.
// State is nil until a check has ever succeeded: "not yet known" is not any
// of the five, and saying absent there would tell the operator nobody has
// signed in when Drydock simply has not looked.
type View struct {
	State         *State      `json:"state"`
	AccountEmail  *string     `json:"account_email"`
	ExpiresAt     *time.Time  `json:"expires_at"`
	LoggedInAt    *time.Time  `json:"logged_in_at"`
	LastCheckedAt *time.Time  `json:"last_checked_at"`
	Volume        string      `json:"volume"`
	CheckError    *CheckError `json:"check_error"`
}

// Watch reads the shared login and keeps claude_identity current.
type Watch struct {
	DB     *sql.DB
	Events *events.Log
	Clock  sys.Clock
	Source Source
	// Volume is recorded in the row (claude_identity.volume_name).
	Volume string
	// Window is the expiring threshold (configuration, §7.3).
	Window   time.Duration
	Interval time.Duration
	// Logf is the service log. It receives Drydock's sentence and a detail
	// that carries no input bytes.
	Logf func(string, ...any)

	mu      sync.Mutex
	running bool
	done    chan struct{}
	// failure is the last check's failure, nil once one succeeds. In memory,
	// like the catalog's: a restarted server checks at once.
	failure *CheckError
}

// Trigger starts a check in the background and returns at once.
func (w *Watch) Trigger() {
	go w.Check(context.Background())
}

// Run checks now and then every Interval until ctx ends.
func (w *Watch) Run(ctx context.Context) {
	every := w.Interval
	if every <= 0 {
		every = DefaultInterval
	}
	for {
		w.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.Clock.After(every):
		}
	}
}

// Check runs one check, joining one already running. It returns the stored
// view afterwards and the check's failure, if it failed.
func (w *Watch) Check(ctx context.Context) (View, error) {
	w.mu.Lock()
	if w.running {
		done := w.done
		w.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return View{}, ctx.Err()
		}
		return w.Read(ctx)
	}
	w.running, w.done = true, make(chan struct{})
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		close(w.done)
		w.mu.Unlock()
	}()

	id, err := w.read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return View{}, ctx.Err()
		}
		return w.fail(ctx, err)
	}
	return w.store(ctx, id)
}

// read is the two reads and the classifier, in the classifier's order.
func (w *Watch) read(ctx context.Context) (classify.Identity, error) {
	creds, err := w.Source.Credentials(ctx)
	if err != nil {
		return classify.Identity{}, err
	}
	var status []byte
	var statusErr error
	if creds != nil {
		// Read even when the file looks blanked: the classifier decides,
		// and it decides blanked without this — so a failure here is
		// passed on as nil bytes, which can only matter for a live file.
		status, statusErr = w.Source.AuthStatus(ctx)
		if statusErr != nil {
			status = nil
		}
	}
	id, err := classify.ClassifyIdentityWithin(status, creds, w.Clock.Now(), w.Window)
	if err != nil {
		if statusErr != nil {
			return classify.Identity{}, statusErr
		}
		return classify.Identity{}, classifyError(err)
	}
	return id, nil
}

// classifyError turns the classifier's refusal into a ReadError whose detail
// carries no input bytes. encoding/json's errors can quote what they choked
// on — a character of a syntax error, the literal of a mistyped number — so
// those are replaced by their shape; the classifier's own sentences name
// rules, not values.
func classifyError(err error) error {
	msg := err.Error()
	problem := ProblemUnknown
	switch {
	case strings.Contains(msg, "loggedIn:false"):
		problem = ProblemDisagree
	case strings.HasPrefix(msg, "classify identity: .credentials.json"):
		problem = ProblemCredentials
	case strings.HasPrefix(msg, "classify identity: auth status"):
		problem = ProblemAuthStatus
	}
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		msg = fmt.Sprintf("not valid JSON (at byte %d)", se.Offset)
	case errors.As(err, &te):
		msg = fmt.Sprintf("field %q has the wrong type", te.Field)
	case strings.Contains(msg, "unexpected end of JSON input"):
		msg = "not valid JSON (truncated)"
	}
	return &ReadError{Problem: problem, Detail: msg}
}

// sentence is what the operator reads for each problem. The stored state is
// kept every time, and each one says so.
func sentence(p Problem) string {
	const kept = " The last known state is kept."
	switch p {
	case ProblemDocker:
		return "Could not check the Claude login: Docker did not answer." + kept
	case ProblemImage:
		return "Could not check the Claude login: the image that reads it could not be built. Its first build needs the network." + kept
	case ProblemCredentials:
		return "Could not check the Claude login: the credential file on the shared volume could not be read or does not make sense." + kept
	case ProblemAuthStatus:
		return "Could not check the Claude login: claude auth status did not give a usable answer." + kept
	case ProblemDisagree:
		return "Could not check the Claude login: the credential file holds a login that Claude Code itself reports as signed out." + kept
	case ProblemForeign:
		return "Could not check the Claude login: a Docker volume with the shared volume's name exists, but this Drydock did not make it, so no workspace mounts it and it is not read." + kept
	}
	return "Could not check the Claude login." + kept
}

func (w *Watch) fail(ctx context.Context, err error) (View, error) {
	var re *ReadError
	if !errors.As(err, &re) {
		re = &ReadError{Problem: ProblemUnknown, Detail: "unexpected failure"}
	}
	now := w.Clock.Now().UTC()
	ce := &CheckError{At: now, Problem: re.Problem, Message: sentence(re.Problem)}
	if w.Logf != nil {
		w.Logf("drydock: identity: %s (%s)", ce.Message, re.Detail)
	}
	// §7.3: keep the state, update last_checked_at. No row yet means there
	// is no state to keep, and none is invented.
	if _, dbErr := w.DB.ExecContext(ctx, `UPDATE claude_identity SET last_checked_at = ? WHERE id = 1`, ts(now)); dbErr != nil {
		return View{}, dbErr
	}
	w.mu.Lock()
	w.failure = ce
	w.mu.Unlock()
	w.Events.Emit(ctx, "", events.Warn, KindCheckFailed, ce.Message, map[string]any{"check_error": ce})
	v, rerr := w.Read(ctx)
	if rerr != nil {
		return View{}, rerr
	}
	return v, re
}

type row struct {
	state      State
	email      sql.NullString
	expiresAt  sql.NullString
	loggedInAt sql.NullString
	checkedAt  sql.NullString
}

func (w *Watch) load(ctx context.Context) (*row, error) {
	var r row
	var state string
	err := w.DB.QueryRowContext(ctx,
		`SELECT state, account_email, expires_at, logged_in_at, last_checked_at FROM claude_identity WHERE id = 1`).
		Scan(&state, &r.email, &r.expiresAt, &r.loggedInAt, &r.checkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.state = State(state)
	return &r, nil
}

func (w *Watch) store(ctx context.Context, id classify.Identity) (View, error) {
	now := w.Clock.Now().UTC()
	state := stateOf(id.State)
	prev, err := w.load(ctx)
	if err != nil {
		return View{}, err
	}

	var email, expires, loggedIn sql.NullString
	if state.Live() {
		// Only beside a login: an email next to "Signed out" would name an
		// account that is not on the volume (the classifier's own rule).
		if id.AccountEmail != "" {
			email = sql.NullString{String: id.AccountEmail, Valid: true}
		}
		expires = sql.NullString{String: ts(id.ExpiresAt), Valid: true}
		// logged_in_at is when Drydock first saw this login: the first
		// live verdict after none. The handshake (§7.2), when it lands,
		// knows the moment exactly and can write it; until then this is
		// the watch's best honest answer, never a guess about the past.
		if prev != nil && prev.state.Live() && prev.loggedInAt.Valid {
			loggedIn = prev.loggedInAt
		} else {
			loggedIn = sql.NullString{String: ts(now), Valid: true}
		}
	}

	if _, err := w.DB.ExecContext(ctx, `
		INSERT INTO claude_identity (id, volume_name, account_email, logged_in_at, state, expires_at, last_checked_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  volume_name = excluded.volume_name, account_email = excluded.account_email,
		  logged_in_at = excluded.logged_in_at, state = excluded.state,
		  expires_at = excluded.expires_at, last_checked_at = excluded.last_checked_at`,
		w.Volume, email, loggedIn, string(state), expires, ts(now)); err != nil {
		return View{}, err
	}

	w.mu.Lock()
	recovered := w.failure != nil
	w.failure = nil
	w.mu.Unlock()

	v, err := w.Read(ctx)
	if err != nil {
		return View{}, err
	}
	changed := prev == nil || prev.state != state || prev.email != email || prev.expiresAt != expires
	if changed || recovered {
		w.Events.Emit(ctx, "", levelOf(state), KindIdentity, message(state, id), map[string]any{"identity": v})
	}
	return v, nil
}

func levelOf(s State) events.Level {
	switch s {
	case Blanked, Expired:
		return events.Error
	case Expiring:
		return events.Warn
	}
	return events.Info
}

// message is the event's prose: the same distinctions the UI keeps (§7.3,
// frontend §6.6). Shown, never parsed.
func message(s State, id classify.Identity) string {
	switch s {
	case Blanked:
		return "Claude is signed out on the shared volume: every workspace just lost access. Sign in again."
	case Absent:
		return "No one has signed in to Claude yet."
	case Expired:
		return "The Claude login has expired. Sign in again."
	case Expiring:
		return "The Claude login expires " + id.ExpiresAt.UTC().Format("2006-01-02 15:04 MST") + ". Sign in again before then."
	}
	return "Claude is signed in."
}

// Read returns the stored identity and the last check's failure.
func (w *Watch) Read(ctx context.Context) (View, error) {
	v := View{Volume: w.Volume}
	r, err := w.load(ctx)
	if err != nil {
		return View{}, err
	}
	if r != nil {
		s := r.state
		v.State = &s
		if r.email.Valid {
			e := r.email.String
			v.AccountEmail = &e
		}
		v.ExpiresAt = parseTS(r.expiresAt)
		v.LoggedInAt = parseTS(r.loggedInAt)
		v.LastCheckedAt = parseTS(r.checkedAt)
	}
	w.mu.Lock()
	if w.failure != nil {
		f := *w.failure
		v.CheckError = &f
	}
	w.mu.Unlock()
	return v, nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}
