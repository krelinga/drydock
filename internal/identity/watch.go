// Package identity is the expiry watch (design §7.3): on an interval, at
// boot and on demand it reads the shared Claude login and stores the verdict
// — ok, expiring, expired, blanked or absent — in claude_identity, so the
// banner, the cards and GET /api/auth/claude all read one stored answer
// rather than each deriving their own (§4).
//
// Two dates come with a live login, and only one of them is the login's.
// expires_at is the access token's: about eight hours on a real login
// (measured 2026-10-08), renewed by every refresh, and it decides only the
// informational `expired`. login_expires_at is the refresh token's —
// Claude Code's own refreshTokenExpiresAt — and `expiring` is a countdown on
// that alone, by Claude Code's own rule. A login that is really over is
// neither: a refresh the server rejects blanks the file (Spike 00).
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
//
// Expired is live. It is dated by the credential file's expiresAt, which is
// the access token's expiry: the refresh token beside it is what keeps the
// login alive, and Claude Code renews an expired access token from it by
// itself (Spike 00). A login whose refresh token is dead is not expired but
// blanked — Claude Code tombstones the file when the server rejects a
// refresh. Expiring is live too: the login still works, and ends at
// login_expires_at unless someone signs in again.
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
	// KindChecked: a check someone asked for (Trigger, behind POST
	// /api/auth/claude/check) found nothing to announce — the same verdict,
	// no failure to clear. It is that request's settling event (frontend
	// §4.2): without it "Check now" stays in flight until a reload. A check
	// nobody asked for — the interval's, boot's, a handshake's — stays
	// silent when nothing changed, and the supervisors do not wake on this
	// kind. data: {identity: View}, last_checked_at moved.
	KindChecked = "auth.identity_checked"
)

// DefaultInterval is §7.3's six hours.
const DefaultInterval = 6 * time.Hour

// DefaultTimeout bounds each read a check makes — the credential file, and
// `auth status`. Either is a container that should be done in seconds; one
// that is not was made to hang (a workspace can replace the file with a FIFO,
// since every workspace mounts the volume read-write) or the daemon has
// stopped answering. Configuration: config.IdentityCheckTimeout.
const DefaultTimeout = 2 * time.Minute

// DefaultBuildTimeout bounds the Claude image's first build, which needs the
// network: separate from the reads', so a slow first build is not a hung
// read and a hung read is not given a build's minutes.
const DefaultBuildTimeout = 15 * time.Minute

// sweepTimeout bounds removing a cut-off read's helper. It runs even when the
// check's own context has ended — at shutdown above all — so it has its own.
const sweepTimeout = 30 * time.Second

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
//
// ExpiresAt is the access token's expiry, never the login's: a real one is
// hours away, and moves with every refresh. LoginExpiresAt is the login's —
// the refresh token's, as Claude Code records it — and nil when the
// credential file carries none.
type View struct {
	State          *State      `json:"state"`
	AccountEmail   *string     `json:"account_email"`
	ExpiresAt      *time.Time  `json:"expires_at"`
	LoginExpiresAt *time.Time  `json:"login_expires_at"`
	LoggedInAt     *time.Time  `json:"logged_in_at"`
	LastCheckedAt  *time.Time  `json:"last_checked_at"`
	Volume         string      `json:"volume"`
	CheckError     *CheckError `json:"check_error"`
}

// Watch reads the shared login and keeps claude_identity current.
type Watch struct {
	DB     *sql.DB
	Events *events.Log
	Clock  sys.Clock
	Source Source
	// Volume is recorded in the row (claude_identity.volume_name).
	Volume string
	// Window is the expiring threshold on the login's own expiry, never the
	// access token's (configuration, §7.3).
	Window   time.Duration
	Interval time.Duration
	// Timeout bounds each read (DefaultTimeout when zero); BuildTimeout the
	// Source's Prepare (DefaultBuildTimeout). Both run on Clock.
	Timeout      time.Duration
	BuildTimeout time.Duration
	// Logf is the service log. It receives Drydock's sentence and a detail
	// that carries no input bytes.
	Logf func(string, ...any)

	mu      sync.Mutex
	running bool
	done    chan struct{}
	// failure is the last check's failure, nil once one succeeds. In memory,
	// like the catalog's: a restarted server checks at once.
	failure *CheckError
	// login is when a handshake (§7.2) just signed the volume in, consumed
	// by the next check that finds a live login. Set by LoggedIn.
	login *time.Time
	// requested: a Trigger is owed a settling event. Set by Trigger, and
	// cleared by whichever event answers it: the check's own auth.identity or
	// auth.identity_check_failed, or — when the check had nothing to
	// announce, or the Trigger came after it announced — KindChecked as the
	// check ends. end clears it and running under one lock, so a Trigger is
	// either answered by the check it joined or starts its own: never
	// neither.
	requested bool
	// swept: the boot sweep of helpers an earlier process left has run. It
	// runs inside the first check, under running, so it can never remove a
	// helper of a check in flight.
	swept bool

	// base is what Trigger's checks run under; Shutdown cancels it and waits
	// for them, so none outlives the database it writes to.
	once     sync.Once
	base     context.Context
	stop     context.CancelFunc
	triggers sync.WaitGroup

	// observe is a test seam, nil in production: called with "joined" when a
	// Check joins the one running (before it waits), and with "ending" when
	// the running check has stored its result but not yet let go of running,
	// and with "parked" when Run has finished a check and registered its
	// interval timer — the only timer left on Clock then, since each read's
	// own timeout is stopped when the read returns, and with "announced"
	// right after a check writes the event that answers the Triggers so far
	// (auth.identity, auth.identity_check_failed, auth.identity_checked). A
	// test that blocks in it holds the interleaving open instead of hoping
	// the scheduler finds it.
	observe func(point string)
}

func (w *Watch) at(point string) {
	if w.observe != nil {
		w.observe(point)
	}
}

func (w *Watch) init() {
	w.once.Do(func() { w.base, w.stop = context.WithCancel(context.Background()) })
}

// Trigger starts a check in the background and returns at once — or, with
// one already running, joins it. It runs under the watch's own context,
// which Shutdown ends.
//
// Every Trigger is answered by an event written after it: the verdict when it
// changed or a failure cleared (auth.identity), a failure
// (auth.identity_check_failed), and otherwise auth.identity_checked — so the
// request that asked can settle whatever the check found. A joined Trigger is
// answered by the check it joined; one whose check was cut off by its own
// caller's context (the handshake's) gets a fresh check of its own.
//
// After Shutdown it starts nothing and returns ErrShutdown, so the route can
// refuse rather than accept a request nothing will answer.
func (w *Watch) Trigger() error {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.base.Err() != nil {
		return ErrShutdown
	}
	w.requested = true
	w.spawnLocked()
	return nil
}

// ErrShutdown: Trigger after Shutdown. Nothing will check, or answer.
var ErrShutdown = errors.New("identity: the watch is shut down")

// spawnLocked starts a check under the watch's own context. w.mu is held,
// and base is live: Shutdown cancels it under the same lock before it waits,
// so it never waits on a check added after.
func (w *Watch) spawnLocked() {
	w.triggers.Add(1)
	go func() {
		defer w.triggers.Done()
		w.Check(w.base)
	}()
}

// Shutdown ends the checks Trigger started and waits up to wait for them —
// each removes what it was running first — so none writes to a closed
// database. A wait that runs out is written to the service log. Run's checks end with Run's context; a caller's Check with its.
func (w *Watch) Shutdown(wait time.Duration) {
	w.init()
	w.mu.Lock()
	w.stop()
	w.mu.Unlock()
	done := make(chan struct{})
	go func() {
		w.triggers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		// Said, because what follows is the database closing under it.
		w.logf("drydock: identity: a triggered check did not stop within %s of shutdown", wait)
	}
}

// LoggedIn is the login handshake telling the watch it just signed the volume
// in, at at (§7.2). It waits out a check already running — that one read the
// volume before the login — then checks afresh, and the first check to find a
// live login records at as logged_in_at. The verdict is still the check's: a
// handshake that reported success over a volume with no login on it is
// stored as whatever the volume says.
func (w *Watch) LoggedIn(ctx context.Context, at time.Time) error {
	for {
		w.mu.Lock()
		if !w.running {
			at := at.UTC()
			w.login = &at
			w.mu.Unlock()
			break
		}
		done := w.done
		w.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := w.Check(ctx)
	return err
}

func (w *Watch) takeLogin() *time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	at := w.login
	w.login = nil
	return at
}

// Run checks now and then every Interval until ctx ends.
func (w *Watch) Run(ctx context.Context) {
	every := w.Interval
	if every <= 0 {
		every = DefaultInterval
	}
	for {
		w.Check(ctx)
		next := w.Clock.After(every)
		w.at("parked")
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
	}
}

// Check runs one check, joining one already running. It returns the stored
// view afterwards and the check's failure, if it failed.
//
// Joining is safe because a check always ends: each read is bounded by
// Timeout and the image's build by BuildTimeout, both on Clock, and a read cut
// off is followed by a sweep with its own bound. So a workspace that turns the
// credential file into something that never ends costs one failed check —
// reported, with the stored state kept — and never the checks after it.
func (w *Watch) Check(ctx context.Context) (View, error) {
	w.mu.Lock()
	if w.running {
		done := w.done
		w.mu.Unlock()
		w.at("joined")
		select {
		case <-done:
		case <-ctx.Done():
			return View{}, ctx.Err()
		}
		return w.Read(ctx)
	}
	w.running, w.done = true, make(chan struct{})
	boot := !w.swept
	w.swept = true
	w.mu.Unlock()
	defer w.end(ctx)

	if boot {
		// Helpers an earlier process left: killed mid-read, or a read it
		// cut off and could not clean up after.
		w.sweep(ctx, "left by an earlier process")
	}
	id, err := w.read(ctx)
	if err != nil {
		// Whatever the read failed on, a helper may be left: a cut-off
		// `docker run` client does not take its container with it.
		w.sweep(ctx, "after a failed read")
		if ctx.Err() != nil {
			return View{}, ctx.Err()
		}
		return w.fail(ctx, err)
	}
	v, err := w.store(ctx, id)
	if err != nil && ctx.Err() == nil {
		// A verdict the database would not take is a failed check, said
		// as one — never end's "nothing has changed".
		return w.fail(ctx, &ReadError{Problem: ProblemUnknown, Detail: "storing the verdict: " + err.Error()})
	}
	return v, err
}

// end finishes a check. It answers every Trigger the check's own
// announcement did not — one that came while it ran and found nothing to
// announce, or came after it announced — and only then lets the next check
// start. Finding no request and clearing running happen under one lock, which
// is what leaves no gap: a Trigger after that is not joined but starts a
// check of its own.
//
// A check whose context ended before it finished checked nothing, so it
// answers nothing: what a pending request gets then depends on whose
// context it was. The watch's own (Shutdown) — nothing; the stream it would
// answer on is closing, and Trigger refuses from then on. A caller's — the
// handshake's LoggedIn runs under its own five-minute bound, shorter than a
// first build — is not the watch ending, so the request stays owed and a
// fresh check under the watch's context is started for it, which answers.
func (w *Watch) end(ctx context.Context) {
	w.at("ending")
	// Answers are written under a context the caller's ending cannot cut:
	// a check that finished has finished, whoever was waiting on it.
	actx := context.WithoutCancel(ctx)
	for {
		w.mu.Lock()
		cut := ctx.Err() != nil
		if !w.requested || cut {
			w.running = false
			close(w.done)
			if w.requested && cut && w.base.Err() == nil {
				w.spawnLocked()
			}
			w.mu.Unlock()
			return
		}
		w.requested = false
		w.mu.Unlock()
		w.checked(actx)
	}
}

// answered clears requested: the caller is about to write the event that
// answers every Trigger so far.
func (w *Watch) answered() {
	w.mu.Lock()
	w.requested = false
	w.mu.Unlock()
}

// checked writes KindChecked: the stored view, as it stands. If the view
// cannot be read the answer is a failure, never silence.
func (w *Watch) checked(ctx context.Context) {
	v, err := w.Read(ctx)
	if err != nil {
		w.failed(ctx, &ReadError{Problem: ProblemUnknown, Detail: "reading the stored identity: " + err.Error()})
		return
	}
	w.Events.Emit(ctx, "", events.Info, KindChecked, "Checked the Claude login: nothing has changed.", map[string]any{"identity": v})
	w.at("announced")
}

// sweep removes the Source's helpers, if it leaves any, bounded on its own
// even when ctx has ended — a check cut off by shutdown still cleans up.
func (w *Watch) sweep(ctx context.Context, why string) {
	s, ok := w.Source.(Sweeper)
	if !ok {
		return
	}
	sctx, cancel := sys.WithTimeout(context.WithoutCancel(ctx), w.Clock, sweepTimeout)
	defer cancel()
	n, err := s.Sweep(sctx)
	switch {
	case err != nil:
		w.logf("drydock: identity: removing helper containers %s: %v", why, err)
	case n > 0:
		w.logf("drydock: identity: removed %d helper container(s) %s", n, why)
	}
}

func (w *Watch) logf(f string, a ...any) {
	if w.Logf != nil {
		w.Logf(f, a...)
	}
}

func durOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// bounded runs one step of a check under its own timeout on the injected
// clock. A step its timeout cut off is a ProblemTimeout, whatever error the
// cut-off produced; one that ended because ctx did is ctx's.
func bounded[T any](ctx context.Context, w *Watch, d time.Duration, what string, f func(context.Context) (T, error)) (T, error) {
	tctx, cancel := sys.WithTimeout(ctx, w.Clock, d)
	defer cancel()
	v, err := f(tctx)
	if err != nil && sys.TimedOut(tctx) && ctx.Err() == nil {
		var zero T
		return zero, &ReadError{Problem: ProblemTimeout, Detail: fmt.Sprintf("%s did not finish within %s", what, d)}
	}
	return v, err
}

// read is the two reads and the classifier, in the classifier's order.
func (w *Watch) read(ctx context.Context) (classify.Identity, error) {
	timeout := durOr(w.Timeout, DefaultTimeout)
	creds, err := bounded(ctx, w, timeout, "reading the credential file", w.Source.Credentials)
	if err != nil {
		return classify.Identity{}, err
	}
	var status []byte
	var statusErr error
	if creds != nil {
		// Read even when the file looks blanked: the classifier decides,
		// and it decides blanked without this — so a failure here is
		// passed on as nil bytes, which can only matter for a live file.
		if p, ok := w.Source.(Preparer); ok {
			_, statusErr = bounded(ctx, w, durOr(w.BuildTimeout, DefaultBuildTimeout), "building the Claude image",
				func(c context.Context) (struct{}, error) { return struct{}{}, p.Prepare(c) })
			var re *ReadError
			if errors.As(statusErr, &re) && re.Problem == ProblemTimeout {
				// A build that ran out of time is the image's problem,
				// not a hung read.
				re.Problem = ProblemImage
			}
		}
		if statusErr == nil {
			status, statusErr = bounded(ctx, w, timeout, "claude auth status", w.Source.AuthStatus)
		}
		if statusErr != nil {
			status = nil
		}
	}
	if ctx.Err() != nil {
		return classify.Identity{}, ctx.Err()
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
	case ProblemTimeout:
		return "Could not check the Claude login: reading the shared volume did not finish in the time allowed, so it was stopped. Docker may not be answering, or something in a workspace replaced the credential file with one that cannot be read to its end." + kept
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
	w.failed(ctx, re)
	v, rerr := w.Read(ctx)
	if rerr != nil {
		return View{}, rerr
	}
	return v, re
}

// failed records and announces a failed check: last_checked_at moved (§7.3:
// the state is kept), the failure held for GET, and
// auth.identity_check_failed, which answers every Trigger so far. A
// database that will not take last_checked_at is logged and the failure is
// still announced: the event is the answer someone may be waiting on.
func (w *Watch) failed(ctx context.Context, re *ReadError) {
	now := w.Clock.Now().UTC()
	ce := &CheckError{At: now, Problem: re.Problem, Message: sentence(re.Problem)}
	w.logf("drydock: identity: %s (%s)", ce.Message, re.Detail)
	// No row yet means there is no state to keep, and none is invented.
	if _, dbErr := w.DB.ExecContext(ctx, `UPDATE claude_identity SET last_checked_at = ? WHERE id = 1`, ts(now)); dbErr != nil {
		w.logf("drydock: identity: recording when the check ran: %v", dbErr)
	}
	w.mu.Lock()
	w.failure = ce
	w.requested = false // the event below answers every Trigger so far
	w.mu.Unlock()
	w.Events.Emit(ctx, "", events.Warn, KindCheckFailed, ce.Message, map[string]any{"check_error": ce})
	w.at("announced")
}

type row struct {
	state          State
	email          sql.NullString
	expiresAt      sql.NullString
	loginExpiresAt sql.NullString
	loggedInAt     sql.NullString
	checkedAt      sql.NullString
}

func (w *Watch) load(ctx context.Context) (*row, error) {
	var r row
	var state string
	err := w.DB.QueryRowContext(ctx,
		`SELECT state, account_email, expires_at, login_expires_at, logged_in_at, last_checked_at FROM claude_identity WHERE id = 1`).
		Scan(&state, &r.email, &r.expiresAt, &r.loginExpiresAt, &r.loggedInAt, &r.checkedAt)
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
	// A handshake's moment is consumed by this verdict whatever it is: one
	// that found no login must not date a later login made some other way.
	pending := w.takeLogin()
	prev, err := w.load(ctx)
	if err != nil {
		return View{}, err
	}

	var email, expires, loginExpires, loggedIn sql.NullString
	if state.Live() {
		// Only beside a login: an email next to "Signed out" would name an
		// account that is not on the volume (the classifier's own rule).
		if id.AccountEmail != "" {
			email = sql.NullString{String: id.AccountEmail, Valid: true}
		}
		expires = sql.NullString{String: ts(id.ExpiresAt), Valid: true}
		if !id.LoginExpiresAt.IsZero() {
			loginExpires = sql.NullString{String: ts(id.LoginExpiresAt), Valid: true}
		}
		// logged_in_at is when this login happened. The handshake (§7.2)
		// knows that exactly and hands it over through LoggedIn; a login
		// made some other way is dated by the first live verdict after
		// none — the watch's best honest answer, never a guess about the
		// past.
		if pending != nil {
			at := pending
			loggedIn = sql.NullString{String: ts(*at), Valid: true}
		} else if prev != nil && prev.state.Live() && prev.loggedInAt.Valid {
			loggedIn = prev.loggedInAt
		} else {
			loggedIn = sql.NullString{String: ts(now), Valid: true}
		}
	}

	if _, err := w.DB.ExecContext(ctx, `
		INSERT INTO claude_identity (id, volume_name, account_email, logged_in_at, state, expires_at, login_expires_at, last_checked_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  volume_name = excluded.volume_name, account_email = excluded.account_email,
		  logged_in_at = excluded.logged_in_at, state = excluded.state,
		  expires_at = excluded.expires_at, login_expires_at = excluded.login_expires_at,
		  last_checked_at = excluded.last_checked_at`,
		w.Volume, email, loggedIn, string(state), expires, loginExpires, ts(now)); err != nil {
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
	changed := prev == nil || prev.state != state || prev.email != email || prev.expiresAt != expires ||
		prev.loginExpiresAt != loginExpires || prev.loggedInAt != loggedIn
	if changed || recovered {
		// This answers every Trigger so far; one that comes after it is
		// answered by end.
		w.answered()
		w.Events.Emit(ctx, "", levelOf(state), KindIdentity, message(state, id), map[string]any{"identity": v})
		w.at("announced")
	}
	// Unchanged, nothing is said here: a check someone asked for is
	// answered by end, with KindChecked, and the interval's stays silent.
	return v, nil
}

func levelOf(s State) events.Level {
	switch s {
	case Blanked:
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
		// The access token's expiry, not the login's: the refresh token
		// beside it is live (a dead one is Blanked), and the next Claude
		// Code to use the volume renews it (Spike 00). Not a fault.
		return "Claude is signed in. The access token on the shared volume has lapsed; the next session server to start renews it."
	case Expiring:
		// The login's own end — the refresh token's — never the access
		// token's, which is always hours away (§7.3).
		return "The Claude login expires " + id.LoginExpiresAt.UTC().Format("2006-01-02 15:04 MST") + ". Sign in again before then."
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
		v.LoginExpiresAt = parseTS(r.loginExpiresAt)
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
