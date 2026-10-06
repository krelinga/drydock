//go:build linux

// Package login is the login handshake (design §7.2): Drydock signs the shared
// credential volume into Claude Code by running `claude auth login --claudeai`
// on a PTY it owns, scraping the authorize URL, typing the one-time code the
// operator pastes back, and recognising the verdict.
//
// Four rules shape every line here.
//
//   - **The verdict is the classifier's, never a guess.** Each phase change
//     is classify.ClassifyLogin's reading of the stream: the URL and the
//     prompt make awaiting_code, `Invalid code` makes invalid_code, `Login
//     successful` makes succeeded. A deadline is the only verdict that is
//     not a byte in the stream, and it is a clock's (classify.LoginTimedOut).
//   - **A wrong code is a loop, not an exit** (Spike 01, frontend §6.2): the
//     process stays at the prompt with the same URL valid, so invalid_code
//     accepts another code and the deadline keeps running.
//   - **Redact by default** (§13.5). The code arrives over HTTP, is checked
//     for shape, typed into the PTY as the trimmed code and one carriage
//     return, and the buffers that held it are zeroed. No event, view, error
//     or log line is built from it — the errors are classify's constants.
//     The PTY buffer lives in this package's memory only, is never logged or
//     stored, and is zeroed when the login ends, because the prompt not
//     echoing is undocumented behaviour of a pinned version.
//   - **One login at a time, and nothing left behind.** A second start is
//     refused while one runs; every exit path — success, a wrong code left to
//     time out, cancel, the container dying, shutdown — kills the local
//     process and removes the container by label before the login is
//     announced as over, and boot sweeps whatever an earlier process left.
//
// The process itself is a Launcher's: in production a short-lived container
// (DockerLauncher), in the component tier fakeclaude on a PTY directly.
package login

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// Phase is where a login has got to (frontend §6.2).
type Phase string

const (
	Starting     Phase = "starting"
	AwaitingCode Phase = "awaiting_code"
	Submitting   Phase = "submitting"
	InvalidCode  Phase = "invalid_code"
	Succeeded    Phase = "succeeded"
	TimedOut     Phase = "timed_out"
	Failed       Phase = "failed"
	Cancelled    Phase = "cancelled"
)

// Ended reports whether the phase is terminal.
func (p Phase) Ended() bool {
	return p == Succeeded || p == TimedOut || p == Failed || p == Cancelled
}

// accepting reports whether a code may be submitted.
func (p Phase) accepting() bool { return p == AwaitingCode || p == InvalidCode }

// Problem names why a login failed, for the sentence the operator reads.
type Problem string

const (
	ProblemDocker      Problem = "docker"       // Docker could not be run, or refused
	ProblemVolume      Problem = "volume"       // the shared volume could not be made or checked
	ProblemVolumeOwner Problem = "volume_owner" // the volume belongs to a uid that is not Drydock's
	ProblemImage       Problem = "image"        // the Claude image could not be built
	ProblemStart       Problem = "start"        // the login process did not start
	ProblemNoURL       Problem = "no_url"       // no authorize URL within the start timeout
	ProblemURL         Problem = "url"          // a URL Drydock cannot use: a parser change
	ProblemExited      Problem = "exited"       // the process ended before a verdict
	ProblemOutput      Problem = "output"       // far more output than a login prints
	ProblemShutdown    Problem = "shutdown"     // Drydock shut down mid-login
)

// KindLogin is the one event kind: every phase change, carrying the whole
// view, as auth.identity carries the whole identity. data: {login: View}.
const KindLogin = "auth.login"

// Defaults, each configuration on Manager.
const (
	// DefaultDeadline is §7.2's five minutes, counted from the moment the
	// URL is shown — the part the operator can see and act within.
	DefaultDeadline = 5 * time.Minute
	// DefaultStartTimeout bounds everything before the URL: the volume, the
	// image (its first build needs the network and can take minutes), the
	// container, and the scrape.
	DefaultStartTimeout = 10 * time.Minute
	// DefaultSettle is how long a successful login is given to exit by
	// itself — Claude Code writes the credential before it says so, and a
	// clean exit is the cleaner end — before its container is removed.
	DefaultSettle = 15 * time.Second
	// DefaultKeep is how long an ended login stays readable from GET
	// /api/auth/claude, so a page reloaded just after says how it ended.
	DefaultKeep = 10 * time.Minute
	// DefaultCols × DefaultRows is the PTY. The URL arrives unbroken at
	// every width measured (Spike 01, re-measured on 2.1.289), and the
	// classifier matches per line; wide costs nothing and is what the
	// spike's harness used.
	DefaultCols = 1000
	DefaultRows = 50
)

// maxOutput bounds the PTY buffer. A login prints about a kilobyte.
const maxOutput = 1 << 20

// View is the login as GET /api/auth/claude and auth.login carry it. There is
// no field a code could go in.
type View struct {
	ID    string `json:"login_id"`
	Phase Phase  `json:"phase"`
	// URL is the authorize URL, from awaiting_code on.
	URL *string `json:"url"`
	// Deadline is when an unfinished login times out, from awaiting_code on.
	Deadline  *time.Time `json:"deadline"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	// Attempts is how many codes have been typed.
	Attempts int      `json:"attempts"`
	Problem  *Problem `json:"problem"`
	// Message is Drydock's sentence for this phase. Shown, never parsed.
	Message string `json:"message"`
}

// Errors the routes turn into refusals.
var (
	ErrInProgress  = errors.New("login: a login is already in progress")
	ErrNotFound    = errors.New("login: no such login")
	ErrEnded       = errors.New("login: the login has ended")
	ErrNotAwaiting = errors.New("login: the login is not waiting for a code")
	ErrShutdown    = errors.New("login: Drydock is shutting down")
)

// CodeError is a code refused for its shape. It wraps one of classify's
// constant errors, so its text never holds the code or any part of it.
type CodeError struct{ Err error }

func (e *CodeError) Error() string { return e.Err.Error() }
func (e *CodeError) Unwrap() error { return e.Err }

// LaunchError is a launch that failed, with the problem it is. Detail is for
// the service log and carries no subprocess output; Message, when set,
// replaces the problem's standard sentence.
type LaunchError struct {
	Problem Problem
	Detail  string
	Message string
}

func (e *LaunchError) Error() string { return "login: " + string(e.Problem) + ": " + e.Detail }

// Launcher starts `claude auth login` on a PTY and removes what it started.
type Launcher interface {
	// Launch readies what the process needs and starts it on a fresh PTY
	// cols × rows. id names the login, for labels. Cancelling ctx abandons
	// a launch that has not yet started the process.
	Launch(ctx context.Context, id string, cols, rows int) (*Proc, error)
	// Remove removes everything Launch made for id. Idempotent; the local
	// process has already been killed.
	Remove(ctx context.Context, id string) error
	// Sweep removes whatever an earlier process left, except keep's.
	Sweep(ctx context.Context, keep string) (int, error)
}

// IdentityRecorder is the expiry watch: after a successful login it checks
// the volume at once, recording when the login happened, so the fleet state
// updates from the one place that writes it (§7.3).
type IdentityRecorder interface {
	LoggedIn(ctx context.Context, at time.Time) error
}

// Proc is a process on a PTY Drydock owns: started through
// subproc.PTYRunner, the one way Drydock starts anything on a terminal
// (internal/pty underneath, the program resolved by name, no shell).
type Proc struct {
	// Master is the PTY: Drydock reads the stream here and types the code.
	Master *os.File
	proc   subproc.Process
	exited chan struct{}
}

// StartProc starts c on a fresh PTY cols × rows through r, and reaps it in
// the background. The process is not tied to a context — subproc would
// SIGTERM it when one ended, and a login's end is the session's to decide —
// so it lives until it exits or kill is called.
func StartProc(r subproc.PTYRunner, c subproc.Cmd, cols, rows int) (*Proc, error) {
	proc, m, err := r.StartPTY(context.Background(), c, cols, rows)
	if err != nil {
		return nil, err
	}
	p := &Proc{Master: m, proc: proc, exited: make(chan struct{})}
	go func() {
		proc.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Pid is the local process's id.
func (p *Proc) Pid() int { return p.proc.Pid() }

// Exited is closed when the local process has exited.
func (p *Proc) Exited() <-chan struct{} { return p.exited }

// kill kills the local process and waits a little for it to go. For the
// docker launcher that is the CLI: the container itself is Remove's — a
// killed CLI leaves its container running (measured).
func (p *Proc) kill(wait time.Duration) {
	select {
	case <-p.exited:
		return
	default:
	}
	p.proc.Signal(subproc.SignalKill)
	select {
	case <-p.exited:
	case <-time.After(wait):
	}
}

// Manager runs at most one login at a time.
type Manager struct {
	Launcher Launcher
	Events   *events.Log
	Clock    sys.Clock
	// Identity is told about a successful login. Nil skips it.
	Identity IdentityRecorder
	// Random makes login ids. Nil is crypto/rand.
	Random io.Reader

	Deadline     time.Duration
	StartTimeout time.Duration
	Settle       time.Duration
	Keep         time.Duration
	Cols, Rows   int
	// Logf is the service log: Drydock's sentences and details with no
	// stream bytes in them.
	Logf func(string, ...any)

	once   sync.Once
	base   context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup
	sweep  sync.RWMutex // a boot sweep holds it, so it never races a launch
	mu     sync.Mutex
	cur    *session
	last   *View
	closed bool
}

type session struct {
	m         *Manager
	view      View // guarded by m.mu
	submit    chan submission
	cancelReq chan struct{}
	cancelOne sync.Once
	done      chan struct{}
}

type submission struct {
	code  []byte
	reply chan error
}

func (m *Manager) init() {
	m.once.Do(func() { m.base, m.stop = context.WithCancel(context.Background()) })
}

func durOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (m *Manager) logf(f string, a ...any) {
	if m.Logf != nil {
		m.Logf(f, a...)
	}
}

func (m *Manager) newID() (string, error) {
	r := m.Random
	if r == nil {
		r = rand.Reader
	}
	var b [12]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidID reports whether s could be a login id: 24 lowercase hex.
func ValidID(s string) bool {
	if len(s) != 24 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Begin starts a login and returns its first view, which is already
// announced. A login in progress is ErrInProgress, with its view.
func (m *Manager) Begin(ctx context.Context) (View, error) {
	m.init()
	id, err := m.newID()
	if err != nil {
		return View{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return View{}, ErrShutdown
	}
	if m.cur != nil {
		v := m.cur.view
		m.mu.Unlock()
		return v, ErrInProgress
	}
	s := &session{m: m, submit: make(chan submission), cancelReq: make(chan struct{}), done: make(chan struct{}),
		view: View{ID: id, Phase: Starting, StartedAt: m.Clock.Now().UTC(), Message: message(Starting, "")}}
	m.cur, m.last = s, nil
	v := s.view
	sctx, cancel := context.WithCancel(m.base)
	m.wg.Add(1)
	m.mu.Unlock()
	// The receipt is written before the 202, so a client that reads the
	// stream from its request's position always sees it.
	m.emit(ctx, v)
	go func() {
		defer cancel()
		s.run(sctx)
	}()
	return v, nil
}

// Submit types a code into the login's PTY. The shape is checked first, so a
// truncated copy is refused here, at once (Spike 01). code is the caller's;
// Submit reads it and never keeps it.
func (m *Manager) Submit(ctx context.Context, id string, code []byte) error {
	// classify's errors are constants: no part of the code is in them.
	if err := classify.ValidateCodeShape(string(code)); err != nil {
		return &CodeError{Err: err}
	}
	s, err := m.find(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	accepting := s.view.Phase.accepting()
	m.mu.Unlock()
	if !accepting {
		return ErrNotAwaiting
	}
	reply := make(chan error, 1)
	select {
	case s.submit <- submission{code: code, reply: reply}:
	case <-s.done:
		return ErrEnded
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-s.done:
		return ErrEnded
	}
}

// Cancel ends a login. Its end — cancelled, after the container is gone — is
// announced as auth.login.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	s, err := m.find(id)
	if err != nil {
		return err
	}
	s.cancelOne.Do(func() { close(s.cancelReq) })
	return nil
}

func (m *Manager) find(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && m.cur.view.ID == id {
		return m.cur, nil
	}
	if m.last != nil && m.last.ID == id {
		return nil, ErrEnded
	}
	return nil, ErrNotFound
}

// Current is the login in progress, or one that ended within Keep; nil
// otherwise.
func (m *Manager) Current() *View {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil {
		v := m.cur.view
		return &v
	}
	if m.last != nil && m.last.EndedAt != nil && m.Clock.Now().Sub(*m.last.EndedAt) < durOr(m.Keep, DefaultKeep) {
		v := *m.last
		return &v
	}
	return nil
}

// Sweep removes what an earlier process left behind — a login container a
// crash or a kill orphaned — sparing the login in progress, if any. Boot runs
// it; it holds off any launch until it is done.
func (m *Manager) Sweep(ctx context.Context) (int, error) {
	m.sweep.Lock()
	defer m.sweep.Unlock()
	keep := ""
	m.mu.Lock()
	if m.cur != nil {
		keep = m.cur.view.ID
	}
	m.mu.Unlock()
	return m.Launcher.Sweep(ctx, keep)
}

// Shutdown ends a login in progress — failed, saying Drydock shut down — and
// waits up to wait for its cleanup.
func (m *Manager) Shutdown(wait time.Duration) {
	m.init()
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.stop()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

func (m *Manager) emit(ctx context.Context, v View) {
	if m.Events == nil {
		return
	}
	level := events.Info
	switch v.Phase {
	case Failed, TimedOut:
		level = events.Warn
	}
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	m.Events.Emit(ctx, "", level, KindLogin, v.Message, map[string]any{"login": v})
}

// message is the sentence for a phase. Never built from the stream or the code.
func message(p Phase, problem Problem) string {
	switch p {
	case Starting:
		return "Starting a login container for Claude. One sign-in covers every workspace."
	case AwaitingCode:
		return "Open the link, sign in to Claude, and paste the code it shows you."
	case Submitting:
		return "Checking the code with Claude."
	case InvalidCode:
		return "Claude did not accept that code. Paste it again: the link is still valid."
	case Succeeded:
		return "Claude says the login succeeded. Checking the shared volume."
	case TimedOut:
		return "The login was not finished in time. Start over."
	case Cancelled:
		return "The login was cancelled."
	}
	switch problem {
	case ProblemDocker:
		return "The login could not start: Docker did not answer."
	case ProblemVolume:
		return "The login could not start: Drydock could not create or check the shared Claude volume."
	case ProblemImage:
		return "The login could not start: the image Claude Code runs in could not be built. Its first build needs the network."
	case ProblemStart:
		return "The login container did not start."
	case ProblemNoURL:
		return "Claude Code did not show a login link in time."
	case ProblemURL:
		return "Claude Code showed a login link Drydock cannot read. Drydock needs an update for this version of Claude Code."
	case ProblemExited:
		return "The login container stopped before the login finished."
	case ProblemOutput:
		return "The login printed far more than a login does, so Drydock stopped it."
	case ProblemShutdown:
		return "Drydock shut down during the login. Start over."
	}
	return "The login failed."
}

// set updates the view under the lock and returns a copy.
func (s *session) set(f func(v *View)) View {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	f(&s.view)
	return s.view
}

func (s *session) phase() Phase {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	return s.view.Phase
}

// outcome is how a session ends.
type outcome struct {
	phase   Phase
	problem Problem
	msg     string // overrides message() when set
}

func (s *session) run(ctx context.Context) {
	m := s.m
	defer m.wg.Done()
	defer close(s.done)

	startTimer := m.Clock.After(durOr(m.StartTimeout, DefaultStartTimeout))
	proc, out := s.launch(ctx, startTimer)
	var buf []byte
	if proc != nil {
		buf, out = s.drive(ctx, proc, startTimer)
	}
	s.finish(proc, buf, out)
}

// launch starts the process, abandoning it on cancel, timeout or shutdown.
func (s *session) launch(ctx context.Context, startTimer <-chan time.Time) (*Proc, outcome) {
	m := s.m
	lctx, abandon := context.WithCancel(ctx)
	defer abandon()
	type launched struct {
		p   *Proc
		err error
	}
	lc := make(chan launched, 1)
	go func() {
		m.sweep.RLock()
		defer m.sweep.RUnlock()
		p, err := m.Launcher.Launch(lctx, s.view.ID, durOr2(m.Cols, DefaultCols), durOr2(m.Rows, DefaultRows))
		lc <- launched{p, err}
	}()
	var stop outcome
	select {
	case l := <-lc:
		if l.err == nil {
			return l.p, outcome{}
		}
		var le *LaunchError
		if !errors.As(l.err, &le) {
			le = &LaunchError{Problem: ProblemStart, Detail: l.err.Error()}
		}
		m.logf("drydock: login %s: %s (%s)", s.view.ID, message(Failed, le.Problem), le.Detail)
		return nil, outcome{phase: Failed, problem: le.Problem, msg: le.Message}
	case <-s.cancelReq:
		stop = outcome{phase: Cancelled}
	case <-ctx.Done():
		stop = outcome{phase: Failed, problem: ProblemShutdown}
	case <-startTimer:
		stop = outcome{phase: Failed, problem: ProblemNoURL}
	}
	// Abandoned: wait for the launch to give up, and kill what it started.
	abandon()
	if l := <-lc; l.p != nil {
		l.p.kill(5 * time.Second)
		l.p.Master.Close()
	}
	return nil, stop
}

func durOr2(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// drive reads the stream and types codes until the login ends.
func (s *session) drive(ctx context.Context, p *Proc, startTimer <-chan time.Time) ([]byte, outcome) {
	m := s.m
	chunks := make(chan []byte, 16)
	go func() {
		defer close(chunks)
		b := make([]byte, 4096)
		for {
			n, err := p.Master.Read(b)
			if n > 0 {
				c := make([]byte, n)
				copy(c, b[:n])
				chunks <- c
			}
			if err != nil {
				wipe(b)
				return
			}
		}
	}()
	// Whatever is still in flight when this returns is drained and zeroed
	// in the background; the reader ends when finish closes the PTY.
	defer func() {
		go func() {
			for c := range chunks {
				wipe(c)
			}
		}()
	}()

	var (
		buf      []byte
		awaitLen int // the stream up to and including the first prompt
		mark     = -1
		deadline <-chan time.Time
	)
	announce := func(v View) { m.emit(ctx, v) }

	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				if s.phase() == Succeeded {
					return buf, outcome{phase: Succeeded}
				}
				return buf, outcome{phase: Failed, problem: ProblemExited}
			}
			buf = append(buf, c...)
			wipe(c)
			if len(buf) > maxOutput {
				return buf, outcome{phase: Failed, problem: ProblemOutput}
			}
			switch s.phase() {
			case Starting:
				l, err := classify.ClassifyLogin(buf)
				if err != nil {
					m.logf("drydock: login %s: the authorize URL did not validate: %v", s.view.ID, err)
					return buf, outcome{phase: Failed, problem: ProblemURL}
				}
				switch {
				case l.Phase == classify.LoginSuccess:
					// Already signed in by some other route; the
					// classifier says so, and the check that follows
					// says what is on the volume.
					return s.succeed(ctx, p, buf, chunks)
				case l.AuthorizeURL != "" && (l.Phase == classify.LoginAwaitingCode || l.Phase == classify.LoginInvalidCode):
					awaitLen = len(buf)
					d := durOr(m.Deadline, DefaultDeadline)
					deadline = m.Clock.After(d)
					startTimer = nil
					url := l.AuthorizeURL
					at := m.Clock.Now().UTC().Add(d)
					announce(s.set(func(v *View) {
						v.Phase, v.URL, v.Deadline, v.Message = AwaitingCode, &url, &at, message(AwaitingCode, "")
					}))
				}
			case Submitting:
				// The verdict for this submission is read from the stream
				// up to the prompt plus what arrived since the code was
				// typed, so an earlier `Invalid code` cannot answer for a
				// later code — the prompt is not re-printed (Spike 01).
				window := make([]byte, 0, awaitLen+len(buf)-mark)
				window = append(append(window, buf[:awaitLen]...), buf[mark:]...)
				l, err := classify.ClassifyLogin(window)
				wipe(window)
				if err != nil {
					m.logf("drydock: login %s: the stream after a code did not classify: %v", s.view.ID, err)
					return buf, outcome{phase: Failed, problem: ProblemURL}
				}
				switch l.Phase {
				case classify.LoginSuccess:
					return s.succeed(ctx, p, buf, chunks)
				case classify.LoginInvalidCode:
					mark = -1
					announce(s.set(func(v *View) { v.Phase, v.Message = InvalidCode, message(InvalidCode, "") }))
				}
			}

		case sub := <-s.submit:
			if !s.phase().accepting() {
				sub.reply <- ErrNotAwaiting
				continue
			}
			// The trimmed code and one carriage return: what a terminal
			// sends for Enter, and what fakeclaude judges correct. The
			// copy is zeroed once written.
			code := bytes.TrimSpace(sub.code)
			line := make([]byte, 0, len(code)+1)
			line = append(append(line, code...), '\r')
			mark = len(buf)
			_, err := p.Master.Write(line)
			wipe(line)
			if err != nil {
				sub.reply <- ErrEnded
				return buf, outcome{phase: Failed, problem: ProblemExited}
			}
			v := s.set(func(v *View) { v.Phase, v.Message = Submitting, message(Submitting, ""); v.Attempts++ })
			announce(v)
			sub.reply <- nil

		case <-s.cancelReq:
			return buf, outcome{phase: Cancelled}
		case <-ctx.Done():
			return buf, outcome{phase: Failed, problem: ProblemShutdown}
		case <-startTimer:
			return buf, outcome{phase: Failed, problem: ProblemNoURL}
		case <-deadline:
			return buf, outcome{phase: TimedOut}
		}
	}
}

// succeed announces the verdict at once — it is the one the operator is
// waiting for — then gives the process Settle to exit by itself.
func (s *session) succeed(ctx context.Context, p *Proc, buf []byte, chunks <-chan []byte) ([]byte, outcome) {
	m := s.m
	now := m.Clock.Now().UTC()
	m.emit(ctx, s.set(func(v *View) {
		v.Phase, v.Message, v.EndedAt, v.Deadline = Succeeded, message(Succeeded, ""), &now, nil
	}))
	settle := time.After(durOr(m.Settle, DefaultSettle))
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				return buf, outcome{phase: Succeeded}
			}
			wipe(c)
		case <-settle:
			return buf, outcome{phase: Succeeded}
		case <-ctx.Done():
			return buf, outcome{phase: Succeeded}
		}
	}
}

// finish kills the process, removes the container, zeroes the stream, and
// only then announces the end — so a login reported over has nothing left
// running, and a new one can start at once.
func (s *session) finish(p *Proc, buf []byte, out outcome) {
	m := s.m
	if p != nil {
		p.kill(5 * time.Second)
		p.Master.Close()
	}
	wipe(buf)
	rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	if err := m.Launcher.Remove(rctx, s.view.ID); err != nil {
		m.logf("drydock: login %s: removing the login container: %v", s.view.ID, err)
	}
	cancel()

	now := m.Clock.Now().UTC()
	v := s.set(func(v *View) {
		if out.phase != Succeeded {
			v.Phase = out.phase
			v.Message = message(out.phase, out.problem)
			if out.msg != "" {
				v.Message = out.msg
			}
			if out.problem != "" {
				pr := out.problem
				v.Problem = &pr
			}
			v.EndedAt = &now
			v.Deadline = nil
		}
	})
	m.mu.Lock()
	m.cur = nil
	last := v
	m.last = &last
	m.mu.Unlock()
	if out.phase != Succeeded {
		m.emit(context.Background(), v)
		return
	}
	if m.Identity != nil {
		ictx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := m.Identity.LoggedIn(ictx, *v.EndedAt); err != nil {
			m.logf("drydock: login %s: the check after the login failed: %v", v.ID, err)
		}
	}
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// String keeps a View out of a %v that might be printed somewhere unplanned;
// it carries nothing secret, but the habit is the point.
func (v View) String() string { return fmt.Sprintf("login %s %s", v.ID, v.Phase) }
