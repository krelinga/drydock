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
//
// # Rules and details
//
// The watch runs at boot, every six hours and on POST /api/auth/claude/check,
// announcing changes as auth.identity. Every check runs on one worker, a
// life.Coalescer that Start runs under a life.Group Serve owns: one at a
// time, never under a caller's context, and a request is answered only by a
// check that begins after it — never by one already running, which may have
// read the volume before it — so requests made during a check share one more
// after it. Serve's shutdown stops the group, which ends a check running and
// starts no other, and waits for it before the database closes.
//
// **Every requested check is answered**: each request carries what its check
// owes it (an ask, the coalescer's payload), handed to exactly the check that
// answers it. A Trigger (the POST) is owed an event: the check's own
// auth.identity or auth.identity_check_failed, or, when it found nothing to
// announce, auth.identity_checked (the view, last_checked_at moved). Only the
// group's end (shutdown) leaves one unanswered, and after it Trigger returns
// life.ErrStopping, which the route answers 503 unavailable. A verdict the
// database will not take, or an answer whose view cannot be read, is
// auth.identity_check_failed. The seam's "announced" point follows each
// answering event. A check nobody is owed an answer by — boot's, the
// interval's, Check's, a handshake's — stays silent when nothing changed, and
// the supervisors, which wake on auth.identity, never see the new kind.
//
// The volume is read through short-lived containers — read-only mount,
// --network none, only DAC_READ_SEARCH — the file with the pinned busybox (so
// blanked and absent never wait on anything else) and `claude auth status
// --json` in the Claude image; a missing volume is absent without a container,
// which would create it, and a volume without <prefix>.claude-config
// (LabelVolume, which is container.LabelClaudeConfig, the label §6 step 4
// gives the one it makes) is a failed check, foreign_volume, and never read. A
// canary sweep covers the DB, events, the log, the HTTP body and every error.
//
// LoggedIn(at) is the login handshake's: a request carrying at (withLogin),
// so it waits out a running check and is answered by a fresh one, on the
// worker — the handshake's context bounds only its wait, and can cut no check
// off. The first live verdict records at as logged_in_at; the moment is
// consumed by the next stored verdict whatever it is, so a handshake over a
// volume still showing no login dates nothing later.
//
// **Every check ends** (a workspace can put a FIFO at the credential path, and
// the read once hung the watch for good): the reader's constant line refuses a
// symlink or anything not a regular file before opening it and reads with head
// -c one byte past the 64 KiB cap; each read is bounded by Timeout
// (--identity-check-timeout, two minutes) and the image's first build by
// BuildTimeout, both through sys.WithTimeout on the injected clock; a read cut
// off is a failed check, problem timeout, keeping the stored state; after any
// failed read, and in the first check after boot, helpers carrying
// <prefix>.identity are removed by label (Sweep) — a killed docker run client
// leaves its container running, measured. Checks run one at a time and sweep
// inside the one running, under sys.Cleanup's own bound, so a check that
// shutdown cut off still removes its helper.
//
// **expired is a live login** (§2.4): its event is info and the UI shows no
// fault for it. **expiring counts down to the login, never the access token**:
// a window on expiresAt called every login expiring. It is
// refreshTokenExpiresAt — which Claude Code 2.1.289 writes at every login, the
// server's figure or thirty days — within --identity-expiring-window and not
// yet past, by Claude Code's own oauth-expiry rule, stored as
// claude_identity.login_expires_at (migration 7 turned rows stored under the
// old meaning into ok). A file without that field has no countdown.
// fresh-login.json (access token 8 h, login 30 d) is the fixture that must
// stay ok.
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
	"github.com/krelinga/drydock/internal/life"
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

	// c runs every check, one at a time, on the goroutine Start gives it:
	// boot's, the interval's, and every one asked for. An ask says what
	// the check that answers it owes its asker.
	c life.Coalescer[View, ask]

	mu sync.Mutex
	// failure is the last check's failure, nil once one succeeds. In memory,
	// like the catalog's: a restarted server checks at once. Read by Read,
	// on any goroutine, hence mu.
	failure *CheckError

	// The worker's own: touched only by checks, which run one at a time on
	// the coalescer's goroutine, so they need no lock.
	//
	// login is when a handshake (§7.2) just signed the volume in, handed
	// over by the check that answered LoggedIn and consumed by the next
	// check that stores a verdict, whatever it is.
	login *time.Time
	// swept: the boot sweep of helpers an earlier process left has run. It
	// runs inside the first check, so it can never remove a helper of a
	// check in flight.
	swept bool

	// observe is a test seam, nil in production, called on the worker:
	// "announced" right after a check writes the event that answers it
	// (auth.identity, auth.identity_check_failed, auth.identity_checked),
	// and "answering" when a check that owes an answer and announced
	// nothing is about to read the view its auth.identity_checked carries.
	// A test that blocks in it holds the interleaving open instead of
	// hoping the scheduler finds it.
	observe func(point string)
}

// ask is what one request tells the check that answers it.
type ask struct {
	// answer: a Trigger (POST /api/auth/claude/check) is owed an event even
	// if the check finds nothing to announce.
	answer bool
	// login is LoggedIn's moment (withLogin).
	login *time.Time
}

// withLogin is LoggedIn's request: the check that answers it — one that
// begins after the call — hands at to the next stored verdict.
func withLogin(at time.Time) ask {
	at = at.UTC()
	return ask{login: &at}
}

func (w *Watch) at(point string) {
	if w.observe != nil {
		w.observe(point)
	}
}

// Start runs the watch's checks under g until g stops: one now, then every
// Interval, and one for each Trigger, Check or LoggedIn. g's Stop ends a
// check running — which still removes its helper, under its own bound — and
// starts no other; g's Wait waits for it, which is what keeps a check from
// outliving the database it writes to.
func (w *Watch) Start(g *life.Group) error { return w.start(g, true) }

// start is Start; boot false leaves out the check at once, for a test that
// counts what each of its own checks does.
func (w *Watch) start(g *life.Group, boot bool) error {
	w.c.Work = w.run
	w.c.Clock = w.Clock
	w.c.Interval = durOr(w.Interval, DefaultInterval)
	if err := w.c.Start(g, "check"); err != nil {
		return err
	}
	if boot {
		w.c.Trigger()
	}
	return nil
}

// Trigger asks for a check in the background and returns at once: one that
// begins after the call, never one already running, which may have read the
// volume before the press. Any number of Triggers during a check share one
// more after it.
//
// Every Trigger is answered by an event the check it asked for writes: the
// verdict when it changed or a failure cleared (auth.identity), a failure
// (auth.identity_check_failed), and otherwise auth.identity_checked — so the
// request that asked can settle whatever the check found.
//
// Once the watch's group is stopping it starts nothing and returns
// life.ErrStopping (life.ErrNotStarted before Start), so the route can
// refuse rather than accept a request nothing will answer.
func (w *Watch) Trigger() error {
	_, err := w.c.TriggerWith(ask{answer: true})
	return err
}

// LoggedIn is the login handshake telling the watch it just signed the volume
// in, at at (§7.2). It asks for a check that begins after the call — so one
// already running, which read the volume before the login, never takes the
// moment — and waits for it; the first check to store a live verdict records
// at as logged_in_at, and the next stored verdict, whatever it is, consumes
// it. The verdict is still the check's: a handshake that reported success
// over a volume with no login on it is stored as whatever the volume says.
//
// The check runs on the watch's own worker, under the watch's group, never
// under ctx: ctx bounds only how long the caller waits, and a caller that
// stops waiting cuts nothing off.
func (w *Watch) LoggedIn(ctx context.Context, at time.Time) error {
	t, err := w.c.TriggerWith(withLogin(at))
	if err != nil {
		return err
	}
	_, err = w.c.Await(ctx, t)
	return err
}

// Check asks for a check that begins after the call and waits for it,
// returning the stored view afterwards and the check's failure, if it
// failed. Like LoggedIn it runs on the worker, and ctx bounds only the wait.
// Nothing is owed: a Check that changes nothing is silent, as the
// interval's is.
//
// A check always ends: each read is bounded by Timeout and the image's build
// by BuildTimeout, both on Clock, and a read cut off is followed by a sweep
// with its own bound. So a workspace that turns the credential file into
// something that never ends costs one failed check — reported, with the
// stored state kept — and never the checks after it.
func (w *Watch) Check(ctx context.Context) (View, error) {
	return w.c.TriggerAndWait(ctx)
}

// run is one check, on the worker, answering asks: it records a
// handshake's moment for the verdict to take, checks, and — when a Trigger
// is owed an answer and the check announced nothing — answers with
// auth.identity_checked. A check the group's end cut off answers nothing:
// the stream it would answer on is closing, and Trigger refuses from then on.
func (w *Watch) run(ctx context.Context, asks []ask) (View, error) {
	owed := false
	for _, a := range asks {
		owed = owed || a.answer
		if a.login != nil {
			w.login = a.login
		}
	}
	v, announced, err := w.check(ctx)
	if owed && !announced && ctx.Err() == nil {
		w.at("answering")
		w.checked(ctx)
	}
	return v, err
}

// check reads, classifies and stores, and says whether it wrote an event
// that answers whoever asked.
func (w *Watch) check(ctx context.Context) (View, bool, error) {
	if !w.swept {
		w.swept = true
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
			return View{}, false, ctx.Err()
		}
		v, err := w.fail(ctx, err)
		return v, true, err
	}
	v, announced, err := w.store(ctx, id)
	if err != nil && ctx.Err() == nil {
		// A verdict the database would not take is a failed check, said
		// as one — never "nothing has changed".
		v, err := w.fail(ctx, &ReadError{Problem: ProblemUnknown, Detail: "storing the verdict: " + err.Error()})
		return v, true, err
	}
	return v, announced, err
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
	sctx, cancel := sys.Cleanup(ctx, w.Clock, sweepTimeout)
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
// auth.identity_check_failed, which answers the check's Triggers. A
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

// store writes a verdict and announces it if anything changed, saying
// whether it did.
func (w *Watch) store(ctx context.Context, id classify.Identity) (View, bool, error) {
	now := w.Clock.Now().UTC()
	state := stateOf(id.State)
	// A handshake's moment is consumed by this verdict whatever it is: one
	// that found no login must not date a later login made some other way.
	pending := w.login
	w.login = nil
	prev, err := w.load(ctx)
	if err != nil {
		return View{}, false, err
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
		return View{}, false, err
	}

	w.mu.Lock()
	recovered := w.failure != nil
	w.failure = nil
	w.mu.Unlock()

	v, err := w.Read(ctx)
	if err != nil {
		return View{}, false, err
	}
	changed := prev == nil || prev.state != state || prev.email != email || prev.expiresAt != expires ||
		prev.loginExpiresAt != loginExpires || prev.loggedInAt != loggedIn
	if !changed && !recovered {
		// Nothing is said here: a check someone asked for is answered by
		// run, with KindChecked, and the interval's stays silent.
		return v, false, nil
	}
	// This answers whoever asked for this check.
	w.Events.Emit(ctx, "", levelOf(state), KindIdentity, message(state, id), map[string]any{"identity": v})
	w.at("announced")
	return v, true, nil
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
