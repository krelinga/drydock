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
//
// # Rules and details
//
// The server runs through devcontainer exec on a PTY Drydock owns
// (container.SessionArgs: sh -c of the constant RemoteControlLaunch, which
// records the pid in the container, runs the secrets prelude and execs the
// server). Every stop is container.SignalSession: docker exec -u 0 of a
// constant script that signals the recorded pid only if it is a
// remote-control, SIGTERM, SIGKILL after StopTimeout. Every start first stops
// any server the pid file names, which is how boot adoption replaces the
// server an earlier Drydock left serving; shutdown — the manager's
// life.Group stopping (RunIn) — closes terminals and signals nobody — every
// run's terminal, promptly, including a run still reading after a failed
// stop (a paused container's), so no supervisor holds shutdown to its bound;
// a stop under way keeps its terminal until it has decided, since closing it
// would read to the stop as the server ending. Every goroutine is the
// group's, so shutdown waits for all of them, and none starts after it. A
// start records starting (or awaiting_login) before it returns, so a parked
// state is never left on the card while the loop gets going.
//
// **One signaller per server** (own.go). Every signal a workspace's server is
// sent comes from one goroutine, the sup's owner: its supervision loop, or,
// when no loop holds the server (one an earlier Drydock left, or one beside a
// loop that parked), a sup born stopped that answers what it is asked and
// ends. Stop sends the owner a request and waits for its reply; the owner
// stops the server under Stop's context, records the outcome, and replies.
// The loop's own stops — of a leftover server before each launch, and of a
// server hung at a gate — run inline in the loop. So a stop beside a
// restart, a second stop, or the loop's own stop is answered after the one
// under way, and by its outcome when that worked: nothing is sent twice.
// Across sups the rule is a handoff: a replaced sup takes no new request and
// records nothing more, and its successor sends nothing until it is quiet
// (answered everything it took, signalling nothing). A run being stopped
// records nothing from the moment its owner takes the stop: what it reads is
// recorded under a context that ends then, so a server still talking on a
// kept terminal after a failed stop reaches no row, even once a Start has
// replaced the sup. A stop that worked is answered once the terminal is
// read to its end, so the log holds the server's last words before
// "stopped".
//
// Retries: the registration wait is waiting_registration on a flat retry and
// never charged to the budget (2 s→60 s, 6 in 10 min, then degraded); the
// organization refusal is awaiting_login; trust and --spawn are degraded, not
// retried. No environment within GateTimeout is a hang: stopped, then named by
// its prompt, never answered. A stored identity of blanked or absent starts
// nothing and spends nothing. A sign-in resumes the supervisors waiting on one
// through internal/provision — a supervisor job per workspace (Resume), started
// by the identity watch's OnChange, never by an event subscription — and a
// loop about to park when it lands goes round again instead (sup.park).
// **expired starts
// the server** — parking every server on it left nothing to refresh (a dead
// refresh token is blanked, not expired).
//
// Discovery is classify.ClassifyDiscovery over a 16 KB raw window (transient):
// environment id to workspace.environment_id, OSC 8 session ids to rc_session,
// `Capacity: N/M` to session.status.
//
// The log is a 1 MB Ring of redacted visible lines (token shapes and the
// workspace's granted secret values), served by GET …/logs, never persisted or
// mirrored to event. **The ring owns its redaction**: NewRing takes the
// values' source (Manager.Redact), asked on every Write, Flush and Mark, and
// add — the only way in — masks, so no call site passes a list and none can
// pass nil (two Flush calls once did, and a final unterminated line carried a
// secret's value out unmasked). It masks every value it was ever given,
// longest first, so a rotated-out or revoked value, or a moment of
// undeliverable snapshot, unmasks nothing. The server's source caches each
// workspace's repository id and reads values from the broker's snapshot, so no
// query per read and never a stale set.
//
// Every reason is a code (Reason), the UI's key. Park records degraded with a
// reason Drydock found before starting (stale_broker_mount), starts nothing,
// holds nothing in memory, and leaves alone a server already running there;
// only an explicit Start (a rebuild's step 8) starts it.
//
// **A stop that fails is recorded, never only returned**: degraded with
// stop_failed (Docker could not be asked — docker ps/exec failed, or whether
// SIGKILL worked could not be asked; the card offers *Restart session server*
// again) or survived_kill (SIGKILL sent, or refused by the kernel — the signal
// script's exit 4, container.ErrSessionSignalRefused, which it says only if
// the pid is still a remote-control after the refusal — and the server still
// there; the card offers Rebuild), each a constant sentence naming no one
// caller, never the error — and written even when it repeats the last
// (announce, not set), because every *Restart session server* press waits for
// a supervisor.state. **Drydock's terminal closing is not the server ending**:
// killing a docker exec client leaves its process running (measured), so a
// held-terminal stop that reaches SIGKILL asks the container through the pid
// file before calling it done. **A paused container is not a stopped
// server**: its processes are frozen and Docker will not exec into it, so
// the signal is container.ErrSessionContainerPaused — stop_failed with its
// own sentence (unpause and ask again), nothing sent, Drydock's terminal
// kept — and so is one paused between the listing and the exec, or while the
// stop waits for the server to exit (SignalSession lists again after a
// refused exec; the pause is noticed at the next question to the container —
// at once when the exit is polled through the pid file, at SIGKILL when the
// wait is on Drydock's terminal). Every start's own stop of a leftover
// server records survived_kill, or that paused stop_failed, and starts
// nothing, rather than a second server over the pid file or a launch into a
// paused container. A restart never unpauses: it must not change the
// container's state. A workspace stop, rebuild or delete does
// (internal/provision unpauses before calling Stop, so the server has its
// SIGTERM and deregisters); one whose server outlived SIGKILL, or whose
// container stays paused (both sentinels through the StopSupervisor seam),
// carries on to its container step, which ends it. The supervisor stays registered
// after a failed stop, so a retry reaches the same server; Start replaces one
// still stopping. A stop or restart its caller cancelled records and starts
// nothing; a start that fails after a good stop writes start_failed.
//
// Tested against fakeclaude behind fake devcontainer/docker binaries that run
// the real launch line and signal script on the host, and in test/container
// through the real CLI and Docker.
package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
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
	// ReasonContainerPaused: boot found the workspace's container paused, so
	// Drydock started no server and opened no broker socket for it. A stop
	// and a start, or a rebuild, restores both (Park; provision's
	// ContainerPausedSentence).
	ReasonContainerPaused Reason = "container_paused"
	// ReasonStopFailed: a stop (a restart's first half) could not reach the
	// server — docker ps or docker exec failed, or its container is
	// paused — so it may still be running. Asking again is the fix once
	// Docker answers (or the container is unpaused).
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

	mu   sync.Mutex
	sups map[string]*sup
	logs map[string]*Ring
	// g is the group every sup's owner runs in (RunIn): each loop, and each
	// sup born stopped. Its context ending is shutdown: every run's terminal
	// closes on it, and nothing starts after it.
	g *life.Group
}

// RunIn gives the manager the group its goroutines run in: a child of
// Serve's work, which shutdown stops and waits for. Before RunIn, and once
// the group is stopping, Start and Park are ErrClosed.
//
// The group stopping is Drydock's shutdown, and it is a detach: every
// terminal is closed and every loop ended, but no server is signalled. A
// Drydock restart must not end the sessions it was supervising — Spike 02:
// a plain restart reconnects the same environment and sessions — so the
// servers keep serving while Drydock is down, and boot adoption replaces
// each with one Drydock has a terminal for. Every run's terminal is closed,
// whatever it is doing: one whose stop failed and which is still reading —
// a paused container's frozen server, whose terminal the stop kept — is
// ended by the loop's context, which is the group's: a stop ends only what
// the run records, never the loop's context. A stop under way keeps its
// terminal until it has decided: it runs in the loop, which sees the
// shutdown only once the stop has returned. What is still running at the
// group's deadline is named by its Wait.
func (m *Manager) RunIn(g *life.Group) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.g != nil {
		return errors.New("supervisor: already started")
	}
	m.g = g
	return nil
}

// stoppingLocked reports (m.mu held) whether nothing may start: before
// RunIn, or once the group is stopping.
func (m *Manager) stoppingLocked() bool { return m.g == nil || m.g.Ctx().Err() != nil }

// ringLocked is the workspace's log ring, made the first time (m.mu held).
// Only once RunIn has given the manager its group: every caller has checked
// stoppingLocked, or follows a Start that did.
func (m *Manager) ringLocked(workspaceID string) *Ring {
	if m.logs == nil {
		m.logs = map[string]*Ring{}
	}
	ring := m.logs[workspaceID]
	if ring == nil {
		ring = NewRing(m.policy().LogBytes, m.redactValues(m.g, workspaceID))
		m.logs[workspaceID] = ring
	}
	return ring
}

// redactValues is a workspace's log ring's source of values to mask. Not
// under the loop's context: the ring outlives a run, and a flush after a
// cancelled run — or after shutdown has cancelled the group — must still be
// masked, so the read is a cleanup of the group's, bounded on its own.
func (m *Manager) redactValues(g *life.Group, workspaceID string) func() []string {
	parent := g.Ctx() // never nil: a ring is made only once RunIn has
	return func() []string {
		if m.Redact == nil {
			return nil
		}
		ctx, cancel := sys.Cleanup(parent, m.clock(), 5*time.Second)
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
	if m.stoppingLocked() {
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
//
// The start has recorded the new server's first state (starting, or
// awaiting login) when it returns (launchLocked): the restart is a
// provisioner job whose end is what ends the press (workspace.job), and
// ending it on the stop's `exited` would show the card a stopped server for
// as long as the new loop takes to say anything.
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
		ring := m.ringLocked(workspaceID)
		var row string
		var restarts int
		// The row's restart count too: a write sets restart_count from
		// memory, and a fresh sup's zero would reset the cumulative count.
		m.DB.QueryRowContext(ctx, `SELECT id, restart_count FROM supervisor WHERE workspace_id = ?
			ORDER BY started_at DESC LIMIT 1`, workspaceID).Scan(&row, &restarts)
		s = m.recordOnly(workspaceID, row, restarts, ring)
		s.state, _, _ = m.storedState(ctx, row)
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
	if m.stoppingLocked() {
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

// detachedLocked is a supervisor with no loop and no owner, only to record a
// state on (recordOnly): its row (made if need be), its log, and the state
// the row holds — none for a row made just now, whose placeholder no server
// ever had, so the first event comes from nothing. m.mu held.
func (m *Manager) detachedLocked(ctx context.Context, workspaceID string) (*sup, error) {
	row, restarts, st, err := m.rowFor(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	s := m.recordOnly(workspaceID, row, restarts, m.ringLocked(workspaceID))
	s.state = st
	return s, nil
}

// launchLocked starts the workspace's supervisor loop in the group (m.mu
// held), replacing one still stopping, and records its first state before
// returning: starting, or awaiting_login when the stored identity already
// says no server can run. So whatever the row said before — a parked
// container_paused or stale_broker_mount, a stop's exited, a stop_failed —
// is replaced as the start is asked for, not when the loop first gets as far
// as writing, which is after an identity read and a docker exec.
//
// The loop's goroutine is taken from the group first, so a start the group
// refuses (shutdown) writes nothing; it runs the loop only once the first
// state is written, so the loop's own writes come after it.
func (m *Manager) launchLocked(ctx context.Context, workspaceID string) error {
	if m.stoppingLocked() {
		return ErrClosed
	}
	body := make(chan func(), 1)
	if err := m.g.TryGo("session server "+workspaceID, func(context.Context) {
		if run := <-body; run != nil {
			run()
		}
	}); err != nil {
		return ErrClosed
	}
	var run func() // nil, and the goroutine ends, unless the launch is complete
	defer func() { body <- run }()
	if m.sups == nil {
		m.sups = map[string]*sup{}
	}
	row, restarts, err := m.ensureRow(ctx, workspaceID)
	if err != nil {
		return err
	}
	s := m.newSup(workspaceID, row, restarts, m.ringLocked(workspaceID))
	s.supervise = true
	// The sup it replaces is retired — it takes no new stop, and stops
	// nothing of its own accord — and the new loop sends nothing until it
	// is quiet (own): a stop it is still making cannot land on the server
	// the new loop launches.
	prev := m.sups[workspaceID]
	if prev != nil {
		prev.retire()
		prev.mu.Lock()
		s.state, s.reason, s.detail = prev.state, prev.reason, prev.detail
		prev.mu.Unlock()
	}
	if s.state == "" {
		s.state, s.reason, s.detail = m.storedState(ctx, row)
	}
	loopCtx, cancel := context.WithCancel(m.g.Ctx())
	s.cancel = cancel
	m.sups[workspaceID] = s
	st, r, detail := Starting, ReasonLaunching, launchingSentence
	if id, known := m.identity(ctx); known && signedOut(id) {
		st, r, detail = AwaitingLogin, ReasonSignedOut, signedOutSentence(id)
	}
	s.set(ctx, st, r, detail, 0)
	run = func() { s.own(loopCtx, prev) }
	return nil
}

// launchingSentence is starting's sentence as a server is launched.
const launchingSentence = "Starting the session server."

// Stop stops the workspace's server with SIGTERM, escalating to SIGKILL only
// after Policy.StopTimeout, and waits for it: the provisioner's
// StopSupervisor seam, run before a stop, a rebuild or a delete touches the
// container (Spike 02: a server killed with its container blocks the next
// start for minutes). A server this process has no terminal for — one an
// earlier Drydock left running — is stopped the same way, through its pid
// file. Nothing running is not an error.
//
// Stop signals nothing itself. It asks the workspace's owner — the sup's
// own goroutine (own.go): its supervision loop, or, when no loop is running,
// a sup born stopped for the purpose — and waits for the reply; the owner
// stops the server under Stop's context and records the outcome (settle)
// before it replies. So a Stop beside another Stop, a Restart, or the loop's
// own stop of a leftover server is answered after that one, never sent
// beside it. A stop that fails is recorded, never only returned (settle),
// and the supervisor stays registered, so the next stop or restart reaches
// the same server, through the terminal Drydock may still hold. A stop cut
// off by its caller (a delete, shutdown) records nothing: one still queued
// is taken back and sends no signal, and one under way ends as its context
// does, with no SIGKILL. Taken back is not undone, though: asking made the
// supervisor stopping, so its loop launches nothing more and its run records
// nothing more, as a cancelled stop always left it.
func (m *Manager) Stop(ctx context.Context, workspaceID string) error {
	req := &stopReq{ctx: ctx, reply: make(chan error, 1)}
	m.mu.Lock()
	s := m.sups[workspaceID]
	if s == nil || !s.submit(req) {
		ns, err := m.bornStoppedLocked(workspaceID, s, req)
		if err != nil {
			m.mu.Unlock()
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return err
		}
		s = ns
	}
	m.mu.Unlock()
	var err error
	select {
	case err = <-req.reply:
	case <-ctx.Done():
		if s.withdraw(req) {
			err = ctx.Err()
			break
		}
		// Taken: under this context, the stop ends as promptly as it does.
		err = <-req.reply
	}
	// A stop that worked drops the supervisor, as does one by a sup that
	// recorded nothing (born for a server no supervisor held) — but only
	// once that sup is quiet: its owner may still be stopping the server for
	// another request, and unregistered, the next Stop would start a second
	// owner waiting for no one, and two SIGTERMs. Kept, the next Stop queues
	// on it, or takes it as the sup it waits for. One that failed keeps it,
	// for the retry.
	if err == nil || (!s.hasRow() && s.isQuiet()) {
		m.mu.Lock()
		if m.sups[workspaceID] == s {
			delete(m.sups, workspaceID)
		}
		m.mu.Unlock()
	}
	return err
}

// bornStoppedLocked registers a sup born stopped for the workspace, with req
// queued, and starts its owner (m.mu held): a server no running loop holds —
// one an earlier Drydock left, or one beside a loop that has parked or
// ended — is stopped by it, through the pid file. It takes over from prev,
// the sup registered before (its row, its log, the state it knows), and
// waits for prev to be quiet before sending anything. With no prev it has no
// row until it has a failure to record (adoptRow). Once Drydock is shutting
// down nothing is started, and that is ErrClosed.
func (m *Manager) bornStoppedLocked(workspaceID string, prev *sup, req *stopReq) (*sup, error) {
	if m.stoppingLocked() {
		return nil, ErrClosed
	}
	var s *sup
	if prev != nil {
		prev.retire()
		prev.mu.Lock()
		s = m.newSup(workspaceID, prev.row, prev.restarts, prev.log)
		s.state, s.reason, s.detail = prev.state, prev.reason, prev.detail
		prev.mu.Unlock()
	} else {
		s = m.newSup(workspaceID, "", 0, nil)
	}
	close(s.done)
	s.submit(req) // before its owner runs, which ends once its queue is empty
	if err := m.g.TryGo("session server "+workspaceID, func(context.Context) { s.own(nil, prev) }); err != nil {
		return nil, ErrClosed
	}
	if m.sups == nil {
		m.sups = map[string]*sup{}
	}
	m.sups[workspaceID] = s
	return s, nil
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

// pausedSentence is stop_failed's sentence when the reason Docker could not
// be asked is that the workspace's container is paused: its processes are
// frozen, not gone, so the server may still be there, and asking again works
// once the container is unpaused.
const pausedSentence = "Drydock could not stop the session server: the workspace's container is paused, so its processes are frozen rather than gone and Docker will not signal them. Unpause the container and ask again, or stop the workspace."

// stopFailedDetail is the sentence for a stop that failed with err.
func stopFailedDetail(r Reason, err error) string {
	if r == ReasonStopFailed && errors.Is(err, container.ErrSessionContainerPaused) {
		return pausedSentence
	}
	return stopFailedSentence(r)
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
		if s.cancel != nil { // nil for one with no loop (born stopped)
			s.cancel()
		}
		// Retired without a handoff: a sup born later for this workspace
		// has no prev and waits for nothing. Forget follows a delete whose
		// own stop was answered, so nothing is left to signal.
		s.retire()
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

// AwaitingLogin lists the workspaces whose supervisor is waiting on a sign-in
// (awaiting_login: signed out, or the organization refusal), sorted: the set
// internal/provision's ResumeAwaitingLogin gives a job each when the stored
// identity becomes a live login. Read from memory, so a workspace stopped or
// deleted since is not in it (a stop drops its supervisor first).
func (m *Manager) AwaitingLogin() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, s := range m.sups {
		if s.current() == AwaitingLogin && !s.isStopping() {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// SignedIn tells every supervisor loop that is running and has not parked —
// whatever its state: starting a server, serving, in backoff — that a live
// login was just stored. It starts nothing, so it is no job: the loop takes
// the news at its next park (sup.park) and goes round again under the new
// login instead of parking. That is the case of a sign-in during a run the
// server then refuses as no_organization — the refusal whose own sentence
// asks for a sign-in — which is not awaiting_login when the sign-in lands, so
// AwaitingLogin does not list it and Resume would leave it alone. Called
// before internal/provision's ResumeAwaitingLogin, under the same m.mu every
// park takes: a loop that parked first is listed and resumed by a job, and
// one that parks after is told here.
func (m *Manager) SignedIn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sups {
		if s.running() && !s.parked {
			s.loginSeen = true
		}
	}
}

// Resume starts the workspace's session server again if its supervisor is
// waiting on a sign-in and the stored identity is now a live login: the body
// of the supervisor job internal/provision launches for it (ResumeAwaitingLogin),
// so it runs only while the provisioner holds the workspace — nothing else
// starts a server on a sign-in, and no event subscription does (design §8).
// Anything else — no supervisor, one serving or degraded, a signed-out
// identity — is left alone, and is not an error.
//
// A loop still running when the sign-in lands — launched a moment before,
// its identity read before the new verdict was stored, about to park; or
// any running loop at all, through SignedIn — is told rather than replaced: it takes the news at its park (sup.park) and
// goes round again, so the sign-in is never lost between its read and its
// park. One that has already parked is replaced, as a parked one is by
// Start.
func (m *Manager) Resume(ctx context.Context, workspaceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stoppingLocked() {
		return ErrClosed
	}
	if st, known := m.identity(ctx); known && signedOut(st) {
		return nil
	}
	s := m.sups[workspaceID]
	// One being stopped stays registered while its stop runs, still saying
	// awaiting_login until the stop records: it is not resumed.
	if s == nil || s.current() != AwaitingLogin || s.isStopping() {
		return nil
	}
	if s.running() && !s.parked {
		s.loginSeen = true
		return nil
	}
	return m.launchLocked(ctx, workspaceID)
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
