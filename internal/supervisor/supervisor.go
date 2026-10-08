//go:build linux

// Package supervisor is design §8: one `claude remote-control` server per
// running workspace, on a terminal Drydock owns, supervised — and its output
// tailed for the environment id, the sessions and the capacity it announces.
//
// The rules it keeps, each of which fails silently when broken:
//
//   - **Exit status is not a discriminator.** Every startup refusal exits 1
//     (Spike 02), so an exit is classified by its message
//     (classify.ClassifyRefusal), and the four refusals lead to four
//     different places. One of them — `already served by a terminal` — is a
//     wait: its own state, retried on a flat interval, never charged to the
//     restart budget, and matched on that text, never on `409`.
//   - **Two config gates hang rather than fail.** A missing remoteDialogSeen
//     or trust record leaves the server at a prompt, which has no error to
//     match; the verdict is a timeout, and the prompt's text only names the
//     missing key afterwards. Nothing is ever typed at a gate.
//   - **SIGTERM first, SIGKILL only on timeout**, delivered inside the
//     container (container.SignalSession): a SIGKILLed server with no live
//     session holds the folder for minutes, and a signal to the local
//     `devcontainer exec` does not reach the server at all (measured).
//   - **A signed-out fleet is not a crash.** When the stored identity says
//     there is no login on the volume — blanked or absent (§7.3) — no server
//     is started and no restart is spent; the supervisor waits in
//     awaiting_login and starts again when the identity changes. Expired is
//     not one of them: it dates the access token, which the server renews
//     from the refresh token beside it as it starts (Spike 00), so it is
//     the one thing that can clear it.
//   - **Sessions are observed, not owned.** Ids come only from OSC 8
//     hyperlink targets (classify.ClassifyDiscovery), never from visible
//     text the model might have printed.
//   - **Redact by default.** The log is a bounded ring of redacted lines,
//     in memory; raw terminal bytes are held only transiently, to classify,
//     and are never written anywhere.
//   - **Nothing stops a workspace automatically.** The supervisor stops its
//     own server only when told to (stop, rebuild, delete) or when a hung
//     gate has been diagnosed; it never stops a container.
package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// State is supervisor.state; the database's CHECK constraint holds the set.
type State string

const (
	Starting            State = "starting"
	WaitingRegistration State = "waiting_registration"
	AwaitingLogin       State = "awaiting_login"
	Serving             State = "serving"
	Degraded            State = "degraded"
	Exited              State = "exited"
)

// Reason is why the supervisor is in its state, as a code the UI gives one
// sentence each (frontend §4.5 #9: the matched signature, not the message).
type Reason string

const (
	ReasonLaunching        Reason = "launching"         // starting: the process was started
	ReasonBackoff          Reason = "backoff"           // starting: restarting after an exit, after a pause
	ReasonWaitRegistration Reason = "wait_registration" // the previous server's folder registration
	ReasonNoOrganization   Reason = "no_organization"   // refusal: account record missing
	ReasonSignedOut        Reason = "signed_out"        // the stored identity is blanked or absent (never expired: see signedOut)
	ReasonNotTrusted       Reason = "not_trusted"       // refusal: the trust record is missing
	ReasonBadCommandLine   Reason = "bad_command_line"  // refusal: Drydock's argv is wrong
	ReasonHangRemoteDialog Reason = "hang_remote_dialog"
	ReasonHangTrust        Reason = "hang_trust"
	ReasonBudgetSpent      Reason = "budget_spent" // too many exits in the window
	// ReasonStaleBrokerMount: the container has the broker socket mounted
	// as a file, as a Drydock before the directory mount made it, so it has
	// had no broker since the restart and every command's secrets prelude
	// exits 69. Not started; only a rebuild fixes it (Park).
	ReasonStaleBrokerMount Reason = "stale_broker_mount"
	// ReasonStopFailed: a stop (a restart's first half) could not reach the
	// server — docker ps or docker exec failed — so it may still be
	// running. Asking again is the fix once Docker answers.
	ReasonStopFailed Reason = "stop_failed"
	// ReasonSurvivedKill: the server was still there after SIGTERM and then
	// SIGKILL were delivered. Asking again cannot help; replacing the
	// container (Rebuild) ends it.
	ReasonSurvivedKill Reason = "survived_kill"
	// ReasonStartFailed: a restart stopped the old server and could not
	// start the new one (its row could not be read or written). Asking
	// again is the fix; the card reads it as any other degraded.
	ReasonStartFailed Reason = "start_failed"
	ReasonServing     Reason = "connected"
	ReasonStopped     Reason = "stopped"
)

// Policy is the supervisor's timing, all of it configuration rather than
// constants a test cannot move.
type Policy struct {
	// Capacity is --capacity. §8: four, because the default is 32; the
	// pre-created session counts toward it.
	Capacity int
	// Backoff doubles from Backoff to BackoffMax between restarts after a
	// crash; Budget restarts are allowed within BudgetWindow, and the next
	// crash parks the supervisor in degraded (§8: 2 s → 60 s, 6 in 10 min).
	Backoff, BackoffMax time.Duration
	Budget              int
	BudgetWindow        time.Duration
	// RegistrationRetry is the flat interval between attempts while the
	// previous server's registration has not lapsed. Not a guess at the
	// lapse — that was measured at 60–200 s and is not a constant (Spike
	// 02) — just how often to ask again.
	RegistrationRetry time.Duration
	// GateTimeout is how long a new server may go without announcing an
	// environment before it is treated as hung (§8, Spike 02: two gates
	// hang instead of failing).
	GateTimeout time.Duration
	// StopTimeout is how long a SIGTERMed server has to exit before
	// SIGKILL; KillWait how long after SIGKILL before giving up on it.
	StopTimeout, KillWait time.Duration
	// StopPoll is how often a server Drydock has no terminal for — one a
	// previous process left — is asked whether it has exited yet.
	StopPoll time.Duration
	// Cols and Rows are the terminal's size.
	Cols, Rows int
	// LogBytes bounds each workspace's log ring (§8: 1 MB).
	LogBytes int
	// HeartbeatEvery bounds how often supervisor.last_heartbeat_at is
	// written while output arrives.
	HeartbeatEvery time.Duration
}

// DefaultPolicy is §8's numbers.
func DefaultPolicy() Policy {
	return Policy{
		Capacity: 4, Backoff: 2 * time.Second, BackoffMax: 60 * time.Second,
		Budget: 6, BudgetWindow: 10 * time.Minute, RegistrationRetry: 15 * time.Second,
		GateTimeout: 90 * time.Second, StopTimeout: 15 * time.Second, KillWait: 5 * time.Second,
		StopPoll: 500 * time.Millisecond, Cols: 200, Rows: 50, LogBytes: 1 << 20,
		HeartbeatEvery: 30 * time.Second,
	}
}

// Runtime is what the supervisor needs from containers: start a server on a
// terminal, and signal one inside its container.
type Runtime interface {
	Start(ctx context.Context, spec container.SessionSpec, cols, rows int) (subproc.Process, *os.File, error)
	Signal(ctx context.Context, workspaceID string, sig container.SessionSignal, pidFile string) (bool, error)
}

// ContainerRuntime is the production Runtime: `devcontainer exec` on a PTY,
// and `docker exec` for signals.
type ContainerRuntime struct {
	Containers container.Manager
	PTY        subproc.PTYRunner
}

func (r ContainerRuntime) Start(ctx context.Context, spec container.SessionSpec, cols, rows int) (subproc.Process, *os.File, error) {
	return r.Containers.StartSession(ctx, r.PTY, spec, cols, rows)
}

func (r ContainerRuntime) Signal(ctx context.Context, workspaceID string, sig container.SessionSignal, pidFile string) (bool, error) {
	return r.Containers.SignalSession(ctx, workspaceID, sig, pidFile)
}

// ErrClosed: Drydock is shutting down and starts nothing.
var ErrClosed = errors.New("supervisor: shutting down")

// Manager supervises every workspace's session server.
type Manager struct {
	DB      *sql.DB
	Events  *events.Log
	Env     sys.Env
	Runtime Runtime
	// Spec says how to exec into a workspace's container: its folder, its
	// override config, its remote env (internal/provision.SessionSpec).
	// Capacity and the pid file are filled in here.
	Spec func(ctx context.Context, workspaceID string) (container.SessionSpec, error)
	// Redact returns literal values to mask in a workspace's log — its
	// granted secrets' values. Nil masks the credential patterns only. The
	// workspace's log ring asks it on every write, flush and mark (read
	// when asked, so it may be set after the Manager is made), and keeps
	// masking every value it has ever returned.
	Redact func(ctx context.Context, workspaceID string) []string
	// Identity reports the stored Claude identity verdict (§7.3) and
	// whether there is one: the server wires identity.Watch.Read. Nil reads
	// claude_identity directly, the same row. Unknown
	// never blocks a start: only a verdict that there is no login on the
	// volume — blanked or absent — does. Expired does not: see signedOut.
	Identity func(ctx context.Context) (state string, known bool)
	// PidFile overrides container.RemoteControlPidFile, for a test whose
	// "container" is the host.
	PidFile string
	Policy  Policy
	Logf    func(format string, args ...any)

	// afterStop, when set (tests only), runs in Restart between its stop
	// and its start: the window a delete or a broken row can land in.
	afterStop func()

	mu     sync.Mutex
	sups   map[string]*sup
	logs   map[string]*Ring
	closed bool
	base   context.Context
	cancel context.CancelFunc
}

// redactValues is a workspace's log ring's source of values to mask.
func (m *Manager) redactValues(workspaceID string) func() []string {
	return func() []string {
		if m.Redact == nil {
			return nil
		}
		// Not the loop's context: the ring outlives a run, and a flush
		// after a cancelled run must still be masked.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.Redact(ctx, workspaceID)
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) policy() Policy {
	p, d := m.Policy, DefaultPolicy()
	if p.Capacity <= 0 {
		p.Capacity = d.Capacity
	}
	if p.Backoff <= 0 {
		p.Backoff = d.Backoff
	}
	if p.BackoffMax <= 0 {
		p.BackoffMax = d.BackoffMax
	}
	if p.Budget <= 0 {
		p.Budget = d.Budget
	}
	if p.BudgetWindow <= 0 {
		p.BudgetWindow = d.BudgetWindow
	}
	if p.RegistrationRetry <= 0 {
		p.RegistrationRetry = d.RegistrationRetry
	}
	if p.GateTimeout <= 0 {
		p.GateTimeout = d.GateTimeout
	}
	if p.StopTimeout <= 0 {
		p.StopTimeout = d.StopTimeout
	}
	if p.KillWait <= 0 {
		p.KillWait = d.KillWait
	}
	if p.StopPoll <= 0 {
		p.StopPoll = d.StopPoll
	}
	if p.Cols <= 0 || p.Rows <= 0 {
		p.Cols, p.Rows = d.Cols, d.Rows
	}
	if p.LogBytes <= 0 {
		p.LogBytes = d.LogBytes
	}
	if p.HeartbeatEvery <= 0 {
		p.HeartbeatEvery = d.HeartbeatEvery
	}
	return p
}

func (m *Manager) clock() sys.Clock {
	if m.Env.Clock == nil {
		return sys.RealClock{}
	}
	return m.Env.Clock
}

// Start makes sure the workspace's session server is running: §6 step 8,
// boot adoption, and the start half of POST …/supervisor. A supervisor
// already running — serving, starting, waiting on a registration — is left
// alone; one parked (degraded, awaiting login, exited) starts again with a
// fresh restart budget.
func (m *Manager) Start(ctx context.Context, workspaceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	// One kept after a failed stop is not left alone: it is stopping, and
	// ends when its server does.
	if s := m.sups[workspaceID]; s != nil && s.running() && !s.isStopping() {
		return nil
	}
	return m.launchLocked(ctx, workspaceID)
}

// Restart stops the workspace's server (SIGTERM first) and starts it again:
// POST …/supervisor on a running one. Every session it was serving is ended
// (§5); Spike 02 says they reconnect, since a SIGTERMed server keeps its
// environment. A stop that fails is not followed by a start — a second server
// beside one that would not stop is refused as already served for as long as
// the first lives — and Stop has recorded it (degraded, stop_failed or
// survived_kill), so the press that asked is answered either way.
func (m *Manager) Restart(ctx context.Context, workspaceID string) error {
	if err := m.Stop(ctx, workspaceID); err != nil {
		return err
	}
	if m.afterStop != nil {
		m.afterStop()
	}
	// Cancelled after a good stop — a delete or shutdown cut in: start
	// nothing, since what cancelled it asked for the opposite, and write
	// nothing, since it answers the press (the delete's own events end it;
	// after a shutdown, the next boot's adoption writes starting).
	if err := ctx.Err(); err != nil {
		return err
	}
	err := m.Start(ctx, workspaceID)
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrClosed) {
		// The stop's `exited` does not end the press; this does.
		m.logf("drydock: workspace %s: starting the session server after a restart's stop: %v", workspaceID, err)
		m.answer(context.WithoutCancel(ctx), workspaceID, Degraded, ReasonStartFailed, startFailedSentence)
	}
	return err
}

// answer announces a state for a workspace whose supervisor may not be in
// memory, and whose row may not be readable: the event is what answers the
// press, so it is written even when the row is not.
func (m *Manager) answer(ctx context.Context, workspaceID string, st State, r Reason, detail string) {
	m.mu.Lock()
	s := m.sups[workspaceID]
	if s == nil {
		if m.logs == nil {
			m.logs = map[string]*Ring{}
		}
		ring := m.logs[workspaceID]
		if ring == nil {
			ring = NewRing(m.policy().LogBytes, m.redactValues(workspaceID))
			m.logs[workspaceID] = ring
		}
		var row string
		m.DB.QueryRowContext(ctx, `SELECT id FROM supervisor WHERE workspace_id = ?
			ORDER BY started_at DESC LIMIT 1`, workspaceID).Scan(&row)
		s = &sup{m: m, ws: workspaceID, row: row, log: ring, done: make(chan struct{})}
		s.state, _, _ = m.storedState(ctx, row)
		close(s.done)
	}
	m.mu.Unlock()
	s.announce(ctx, st, r, detail)
}

// Park records the workspace's session server as degraded for a reason
// Drydock found before starting one, without starting one and without
// touching a server already running in the container: boot adoption's answer
// to a container that cannot work until it is rebuilt
// (ReasonStaleBrokerMount). The card reads the reason as a container fault
// and offers Rebuild, whose stop half stops any server an earlier process
// left (Stop, through its pid file). Nothing is held in memory for it, so
// nothing restarts it but an explicit Start.
func (m *Manager) Park(ctx context.Context, workspaceID string, r Reason, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if s := m.sups[workspaceID]; s != nil && s.running() {
		return nil
	}
	s, err := m.detachedLocked(ctx, workspaceID)
	if err != nil {
		return err
	}
	s.set(ctx, Degraded, r, detail, 0)
	return nil
}

// detachedLocked is a supervisor with no loop, to record a state on: its row
// (made if need be), its log, and the state the row holds. m.mu held.
func (m *Manager) detachedLocked(ctx context.Context, workspaceID string) (*sup, error) {
	if m.logs == nil {
		m.logs = map[string]*Ring{}
	}
	// A row made just now holds ensureRow's placeholder state, which no
	// server ever had: the first event then comes from nothing ("").
	var existed bool
	m.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM supervisor WHERE workspace_id = ?)`, workspaceID).Scan(&existed)
	row, restarts, err := m.ensureRow(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	ring := m.logs[workspaceID]
	if ring == nil {
		ring = NewRing(m.policy().LogBytes, m.redactValues(workspaceID))
		m.logs[workspaceID] = ring
	}
	s := &sup{m: m, ws: workspaceID, row: row, restarts: restarts, log: ring, done: make(chan struct{})}
	if existed {
		s.state, _, _ = m.storedState(ctx, row)
	}
	close(s.done)
	return s, nil
}

func (m *Manager) launchLocked(ctx context.Context, workspaceID string) error { // m.mu held
	if m.base == nil {
		m.base, m.cancel = context.WithCancel(context.Background())
	}
	if m.sups == nil {
		m.sups = map[string]*sup{}
	}
	if m.logs == nil { // Park or a failed stop may have made it first
		m.logs = map[string]*Ring{}
	}
	row, restarts, err := m.ensureRow(ctx, workspaceID)
	if err != nil {
		return err
	}
	ring := m.logs[workspaceID]
	if ring == nil {
		ring = NewRing(m.policy().LogBytes, m.redactValues(workspaceID))
		m.logs[workspaceID] = ring
	}
	prev := m.sups[workspaceID]
	s := &sup{m: m, ws: workspaceID, row: row, restarts: restarts, log: ring, done: make(chan struct{})}
	if prev != nil {
		s.state, s.reason, s.detail = prev.state, prev.reason, prev.detail
	} else {
		s.state, s.reason, s.detail = m.storedState(ctx, row)
	}
	loopCtx, cancel := context.WithCancel(m.base)
	s.cancel = cancel
	m.sups[workspaceID] = s
	go s.loop(loopCtx)
	return nil
}

// Stop stops the workspace's server with SIGTERM, escalating to SIGKILL only
// after Policy.StopTimeout, and waits for it: the provisioner's
// StopSupervisor seam, run before a stop, a rebuild or a delete touches the
// container (Spike 02: a server killed with its container blocks the next
// start for minutes). A server this process has no terminal for — one an
// earlier Drydock left running — is stopped the same way, through its pid
// file. Nothing running is not an error.
//
// A stop that fails is recorded, never only returned: degraded, with
// stop_failed when Docker could not be asked and survived_kill when the
// server outlived SIGKILL, each with the sentence naming the one action that
// can fix it — written even when it repeats the last, because every press of
// Restart session server waits for a supervisor.state (frontend §4.2). The
// supervisor stays registered, so the next stop or restart reaches the same
// server, through the terminal Drydock may still hold. A stop cut off by its
// caller (a delete, shutdown) records nothing: that is not a stop that
// failed, and what cancelled it says what happens next.
func (m *Manager) Stop(ctx context.Context, workspaceID string) error {
	m.mu.Lock()
	s := m.sups[workspaceID]
	delete(m.sups, workspaceID)
	m.mu.Unlock()
	var err error
	if s != nil {
		err = s.stop(ctx)
	} else {
		_, err = m.terminate(ctx, workspaceID, nil, nil)
	}
	book := context.WithoutCancel(ctx)
	if err == nil {
		if s != nil {
			s.set(book, Exited, ReasonStopped, "The session server was stopped.", 0)
		}
		return nil
	}
	m.logf("drydock: workspace %s: stopping the session server: %v", workspaceID, err)
	m.mu.Lock()
	if s == nil && ctx.Err() == nil && !m.closed {
		ns, nerr := m.detachedLocked(book, workspaceID)
		if nerr != nil {
			m.mu.Unlock()
			return errors.Join(err, nerr)
		}
		s = ns
	}
	if s != nil && m.sups[workspaceID] == nil {
		if m.sups == nil {
			m.sups = map[string]*sup{}
		}
		m.sups[workspaceID] = s
	}
	m.mu.Unlock()
	if s != nil && ctx.Err() == nil {
		r := ReasonStopFailed
		if errors.Is(err, container.ErrSessionSurvivedKill) {
			r = ReasonSurvivedKill
		}
		s.announce(book, Degraded, r, stopFailedSentence(r))
	}
	return err
}

// stopFailedSentence is the card's sentence for a stop that failed, naming
// what can fix it. It is written by every stop — a restart's, and the first
// sub-step of a workspace stop, rebuild or delete — so it names no one of
// them. Never the error: docker's stderr can carry anything.
func stopFailedSentence(r Reason) string {
	if r == ReasonSurvivedKill {
		return "The session server was still running after SIGKILL, so Drydock could not stop it, and it may still hold the workspace's environment. Only its container going ends it: Rebuild the workspace to replace the container; the clone is kept."
	}
	return "Drydock could not stop the session server: Docker did not answer when asked to signal it or whether it had exited, so it may still be running. Ask again once Docker answers."
}

// startFailedSentence: a restart stopped the old server and could not start
// the new one (its row could not be read or written).
const startFailedSentence = "Drydock stopped the session server but could not start a new one. Restart the session server to try again."

// Forget drops what the manager holds for a deleted workspace: its log.
func (m *Manager) Forget(workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.logs, workspaceID)
	if s := m.sups[workspaceID]; s != nil {
		if s.cancel != nil { // nil for one with no loop (detachedLocked)
			s.cancel()
		}
		delete(m.sups, workspaceID)
	}
}

// Logs returns the last n lines of the workspace's server log, oldest first,
// and whether older lines are gone. ok is false when Drydock holds no log for
// it — none was started since Drydock did.
func (m *Manager) Logs(workspaceID string, n int) (lines []Line, truncated, ok bool) {
	m.mu.Lock()
	r := m.logs[workspaceID]
	m.mu.Unlock()
	if r == nil {
		return nil, false, false
	}
	lines, truncated = r.Tail(n)
	return lines, truncated, true
}

// Detach is Drydock's shutdown: every terminal is closed and every loop
// ended, but no server is signalled. A Drydock restart must not end the
// sessions it was supervising — Spike 02: a plain restart reconnects the same
// environment and sessions — so the servers keep serving while Drydock is
// down, and boot adoption replaces each with one Drydock has a terminal for.
func (m *Manager) Detach(wait time.Duration) {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	var all []*sup
	for _, s := range m.sups {
		all = append(all, s)
	}
	m.mu.Unlock()
	deadline := m.clock().After(wait)
	for _, s := range all {
		select {
		case <-s.done:
		case <-deadline:
			return
		}
	}
}

// Watch follows the event log until ctx ends, and starts every supervisor
// parked in awaiting_login when the stored identity changes (auth.identity,
// written by the expiry watch, §7.3) — the operator signed in, and the
// servers that were waiting for exactly that may run. It starts only
// supervisors of running workspaces that were waiting on a login; nothing
// stopped is started (§6).
func (m *Manager) Watch(ctx context.Context) {
	if m.Events == nil {
		return
	}
	sub := m.Events.Subscribe()
	defer m.Events.Cancel(sub)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			if ev.Kind == identity.KindIdentity {
				m.resumeWaiting(ctx)
			}
		}
	}
}

func (m *Manager) resumeWaiting(ctx context.Context) {
	if st, known := m.identity(ctx); known && signedOut(st) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sups {
		if s.running() || s.current() != AwaitingLogin {
			continue
		}
		var state string
		if err := m.DB.QueryRowContext(ctx, `SELECT state FROM workspace WHERE id = ?`, id).Scan(&state); err != nil ||
			state != string(workspace.Running) {
			continue
		}
		if err := m.launchLocked(ctx, id); err != nil {
			m.logf("drydock: workspace %s: restarting the session server after a sign-in: %v", id, err)
		}
	}
}

// identity reads the stored verdict.
func (m *Manager) identity(ctx context.Context) (string, bool) {
	if m.Identity != nil {
		return m.Identity(ctx)
	}
	if m.DB == nil {
		return "", false
	}
	var st string
	if err := m.DB.QueryRowContext(ctx, `SELECT state FROM claude_identity WHERE id = 1`).Scan(&st); err != nil {
		return "", false
	}
	return st, true
}

// signedOut: a verdict under which no server can run until someone signs in.
//
// Blanked and absent only. Expired means the credential file's expiresAt has
// passed, and that dates the *access* token: the refresh token beside it is
// live (a dead one is blanked — Claude Code tombstones the file on a refresh
// the server rejects), and Claude Code renews an expired access token from
// it on its own, under the volume's refresh lock (Spike 00, whose harness
// seeds exactly that). The watch's own read is read-only and offline, so it
// never refreshes; with every server parked on expired, nothing would, and
// the fleet would wait for a full sign-in it does not need. So a server is
// started under expired, and the one start renews the login for all of them.
// If the refresh token is in fact dead, that start blanks the file, the next
// check says blanked, and the refusal classifier and the restart budget
// judge the start in between, as for any other exit.
func signedOut(state string) bool {
	switch identity.State(state) {
	case identity.Blanked, identity.Absent:
		return true
	}
	return false
}

func signedOutSentence(state string) string {
	switch state {
	case "blanked":
		return "Claude was signed out on the shared volume, so no session server can run. Sign in again."
	case "absent":
		return "No one has signed in to Claude yet, so no session server can run."
	}
	return "No session server can run until someone signs in to Claude."
}

// Backoff is the pause before restart number n (1-based) after a crash:
// Backoff doubled n-1 times, capped at BackoffMax.
func (p Policy) BackoffFor(n int) time.Duration {
	d := p.Backoff
	for i := 1; i < n && d < p.BackoffMax; i++ {
		d *= 2
	}
	if d > p.BackoffMax {
		d = p.BackoffMax
	}
	return d
}

func durationText(d time.Duration) string {
	switch {
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d >= time.Second:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", d.Seconds()), "0"), ".") + " s"
	}
	return d.String()
}
