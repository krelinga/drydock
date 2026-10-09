//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/pty"
	"github.com/krelinga/drydock/internal/subproc"
)

// sup is one workspace's supervisor: a loop that starts the server, reads its
// terminal, and decides what an exit means — and the one goroutine its server
// is ever signalled from (own.go).
type sup struct {
	m      *Manager
	ws     string
	row    string
	log    *Ring
	cancel context.CancelFunc // the loop's context; nil for a sup with no loop
	done   chan struct{}      // closed when the supervision loop returns; at once for a sup with none
	// supervise: the owner runs the supervision loop. False for a sup born
	// stopped (Stop's, for a server no running loop holds), whose owner only
	// answers what it is asked.
	supervise bool

	// loginSeen and parked are m.mu's, not mu's: they are how a sign-in
	// (Manager.Resume) and the loop's decision to park in awaiting_login
	// (park) are ordered. loginSeen: a sign-in landed while this loop was
	// running and had not parked, so its next park goes round again instead.
	// parked: the loop has parked and is only returning, so a sign-in
	// replaces it rather than telling it.
	loginSeen bool
	parked    bool

	// serverGone is the owner goroutine's alone: it stopped the server and
	// has launched none since, so a stop asked now has nothing to send.
	serverGone bool

	// wmu serialises this sup's state writes (write), from the check for a
	// change to the memory update after the commit. Taken before mu, and
	// held across the commit, which mu never is.
	wmu      sync.Mutex
	mu       sync.Mutex
	state    State
	reason   Reason
	detail   string
	restarts int
	crashes  []time.Time
	// stopping: a stop was asked (submit). Nothing is launched after it.
	stopping bool
	lastBeat time.Time

	// The owner's queue (own.go), under mu. intake: requests are taken,
	// until the owner has ended. retired: replaced, so it takes no new
	// request and its loop stops nothing of its own accord. busy counts the
	// requests taken and not yet answered, and the loop's own stop under
	// way. quiet is closed once the sup will never signal again; its
	// successor waits for it.
	pending     []*stopReq
	wake        chan struct{} // a request was queued; buffered 1
	intake      bool
	retired     bool
	ownerDone   bool
	busy        int
	quiet       chan struct{}
	quietClosed bool
}

// newSup is a sup whose owner is yet to run: it takes requests from now.
func (m *Manager) newSup(workspaceID, row string, restarts int, log *Ring) *sup {
	return &sup{m: m, ws: workspaceID, row: row, restarts: restarts, log: log,
		done: make(chan struct{}), wake: make(chan struct{}, 1), quiet: make(chan struct{}), intake: true}
}

// recordOnly is a sup that only records a state — Park's, answer's — and has
// no owner: it takes no request and never signals.
func (m *Manager) recordOnly(workspaceID, row string, restarts int, log *Ring) *sup {
	s := m.newSup(workspaceID, row, restarts, log)
	s.intake, s.ownerDone, s.quietClosed = false, true, true
	close(s.done)
	close(s.quiet)
	return s
}

func (s *sup) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *sup) current() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// outcome is how one run of the server ended.
type outcome struct {
	kind   outcomeKind
	reason Reason // for fatal, login, and the crash cause in detail
	detail string
}

type outcomeKind int

const (
	outStopped   outcomeKind = iota // Drydock stopped it
	outDetached                     // Drydock is shutting down; the server keeps running
	outWait                         // already served by a terminal: wait, flat, uncounted
	outLogin                        // the account cannot run Remote Control: awaiting_login
	outSignedOut                    // the stored identity went signed-out meanwhile
	outFatal                        // a config error or a bug: degraded, no retry
	outCrash                        // anything else: counted against the budget
)

func (s *sup) loop(ctx context.Context) {
	defer close(s.done)
	m, p := s.m, s.m.policy()
	for {
		if ctx.Err() != nil || s.isStopping() {
			return
		}
		// A sign-in before this point is in what this pass reads — the
		// identity below, the credential the server starts with — so only
		// one after it is news to the park.
		s.clearLoginSeen()
		// Defer to the fleet's identity before spending anything (§7.3,
		// frontend §6.6): a server that cannot run is not started, and
		// waiting for a sign-in costs no restart.
		if st, known := m.identity(ctx); known && signedOut(st) {
			if s.park(ctx, ReasonSignedOut, signedOutSentence(st)) {
				return
			}
			continue
		}
		// A server an earlier process (or an earlier run) left behind still
		// holds the folder, and every start would be refused as already
		// served until it exits. Stop it, SIGTERM first, so its sessions
		// reconnect to the new one (Spike 02). Here, in the loop: a Stop
		// asked meanwhile waits for it and is answered after it, with
		// nothing more to send if it worked.
		if !s.beginOwn() {
			return
		}
		_, err := m.terminate(ctx, s.ws, nil, nil)
		s.endOwn()
		if err == nil {
			s.serverGone = true
		}
		if ctx.Err() != nil {
			// Cut off by shutdown: launch nothing and record nothing. The
			// next boot's start stops that server first, as this one was.
			return
		}
		if err != nil {
			m.logf("drydock: workspace %s: stopping a session server left running: %v", s.ws, err)
			if errors.Is(err, container.ErrSessionSurvivedKill) {
				// It outlived SIGKILL and still holds the folder: a new
				// server beside it would be refused as already served,
				// shown as a wait that never clears, and would overwrite
				// the pid file that is the only way to reach it. Say so,
				// with the fix, and start nothing.
				s.set(ctx, Degraded, ReasonSurvivedKill, stopFailedSentence(ReasonSurvivedKill), 0)
				return
			}
			if errors.Is(err, container.ErrSessionContainerPaused) {
				// Frozen, not gone, and a `devcontainer exec` into a paused
				// container fails: a launch would only spend the restart
				// budget. Asking again once it is unpaused is the fix.
				s.set(ctx, Degraded, ReasonStopFailed, pausedSentence, 0)
				return
			}
		}
		out := s.runOnce(ctx)
		switch out.kind {
		case outStopped, outDetached:
			return
		case outWait:
			s.set(ctx, WaitingRegistration, ReasonWaitRegistration, fmt.Sprintf(
				"Waiting for the previous session server to release the folder; asking again every %s. This is a wait, not a failure.",
				durationText(p.RegistrationRetry)), 0)
			if !s.sleep(ctx, p.RegistrationRetry) {
				return
			}
		case outLogin:
			if s.park(ctx, out.reason, out.detail) {
				return
			}
		case outSignedOut:
			continue // the top of the loop says so
		case outFatal:
			s.set(ctx, Degraded, out.reason, out.detail, 0)
			return
		case outCrash:
			now := m.clock().Now()
			s.mu.Lock()
			kept := s.crashes[:0]
			for _, t := range s.crashes {
				if now.Sub(t) < p.BudgetWindow {
					kept = append(kept, t)
				}
			}
			s.crashes = append(kept, now)
			n := len(s.crashes)
			s.mu.Unlock()
			if n > p.Budget {
				s.set(ctx, Degraded, ReasonBudgetSpent, fmt.Sprintf(
					"The session server stopped %d times in %s, so Drydock stopped restarting it. The last time it %s. Restart it from the workspace once the cause is fixed.",
					n, durationText(p.BudgetWindow), out.detail), 0)
				return
			}
			s.countRestart(ctx)
			d := p.BackoffFor(n)
			s.set(ctx, Starting, ReasonBackoff, fmt.Sprintf(
				"The session server %s. Restarting it in %s (restart %d of %d allowed in %s).",
				out.detail, durationText(d), n, p.Budget, durationText(p.BudgetWindow)), 0)
			if !s.sleep(ctx, d) {
				return
			}
		}
	}
}

// park records awaiting_login and reports true — the loop is to return, and
// waits for a sign-in — unless a sign-in landed since this pass began
// (loginSeen), when it records nothing and reports false: go round again,
// under the new login. Decided and recorded under m.mu, which Manager.Resume
// holds too, so a sign-in is never lost between the loop's read and its park:
// a Resume before the park has set loginSeen, and one after it finds the loop
// parked and replaces it — after this write, so the replacement's starting
// follows this awaiting_login.
func (s *sup) park(ctx context.Context, r Reason, detail string) bool {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.loginSeen {
		s.loginSeen = false
		return false
	}
	s.set(ctx, AwaitingLogin, r, detail, 0)
	s.parked = true
	return true
}

// clearLoginSeen forgets a sign-in the pass about to begin will see anyway.
func (s *sup) clearLoginSeen() {
	s.m.mu.Lock()
	s.loginSeen = false
	s.m.mu.Unlock()
}

func (s *sup) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// sleep waits d on the injected clock, or until the loop is cancelled or
// asked to stop: a stop is answered as the loop ends (drain).
func (s *sup) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return false
	case <-s.m.clock().After(d):
		return !s.isStopping()
	}
}

// discovered is what one run's output has announced so far, and how much of
// it is recorded. The announced half (env, capUsed/capTotal) decides
// serving; the recorded half moves only when recordDiscovery commits, so a
// write that fails is tried again — on the next window, and on the next
// heartbeat tick for a server that has gone quiet — rather than lost for the
// server's lifetime.
type discovered struct {
	env               string
	capUsed, capTotal int

	envRecorded bool
	sessions    map[string]bool // recorded
	pending     []string        // announced, not yet recorded, in order
	capRecUsed  int
	capRecTotal int

	// logged: a failed write has been logged this run. A database that keeps
	// failing is said once, not on every chunk a chatty server prints.
	logged bool
}

const (
	// discoveryWindow is how much raw output is re-read on each chunk: more
	// than any one escape sequence or status block, so a sequence split by a
	// read is whole on the next pass. Transient, never stored.
	discoveryWindow = 16 << 10
	// refusalTail is how much of a run's output an exit is classified on.
	refusalTail = 32 << 10
)

func (s *sup) runOnce(ctx context.Context) outcome {
	m, p := s.m, s.m.policy()
	spec, err := m.Spec(ctx, s.ws)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{kind: outDetached}
		}
		m.logf("drydock: workspace %s: session server spec: %v", s.ws, err)
		return outcome{kind: outCrash, detail: "could not be started: Drydock could not read the workspace's configuration"}
	}
	spec.Capacity = p.Capacity
	spec.PidFile = m.PidFile

	// The last look before the exec: a stop asked meanwhile launches
	// nothing (it is answered as the loop ends), and neither does shutdown,
	// whose leave would abandon a server just started.
	if s.isStopping() {
		return outcome{kind: outStopped}
	}
	if ctx.Err() != nil {
		return outcome{kind: outDetached}
	}
	// The process is not tied to the loop's context: cancelling that is
	// Drydock shutting down, which must leave the server running, and a stop
	// ends the process by signalling it in the container first.
	procCtx, procCancel := context.WithCancel(context.Background())
	defer procCancel()
	proc, master, err := m.Runtime.Start(procCtx, spec, p.Cols, p.Rows)
	if err != nil {
		m.logf("drydock: workspace %s: starting the session server: %v", s.ws, err)
		return outcome{kind: outCrash, detail: "could not be started"}
	}
	s.serverGone = false
	// waited is Drydock's end of the server ending: `devcontainer exec`
	// exits when the server does. One waiter, since a process is waited for
	// once.
	waited := make(chan struct{})
	go func() { proc.Wait(); close(waited) }()
	// A retry while the previous server's registration lapses stays a wait
	// on the card: it is the same wait, asked again, not a new start.
	if s.current() != WaitingRegistration {
		s.set(ctx, Starting, ReasonLaunching, launchingSentence, proc.Pid())
	}

	// rctx is what the run's reading records under — the heartbeat, what
	// the server announces, serving — and it ends as a stop is taken: a run
	// being stopped records nothing more, even once a Start has replaced
	// its sup.
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	rd := s.read(rctx, master)

	// stopped are the stops that ended this run's server, answered once its
	// terminal is read to the end, so the log holds its last words first.
	var stopped []*stopReq
	defer func() {
		for _, req := range stopped {
			s.settle(req, nil)
		}
	}()
	gate := m.clock().After(p.GateTimeout)
	served := rd.served
	var hang Reason
	for open := true; open; {
		select {
		case <-rd.done:
			open = false
		case <-served:
			served, gate = nil, nil
		case <-gate:
			gate = nil
			if rd.serving() {
				break
			}
			// No environment within the deadline. A hang has no message
			// (two of the three gates wait at a prompt rather than fail), so
			// the timeout is the verdict; the prompt's text only names the
			// key. Stop it — SIGTERM first — and read on until it ends.
			hang = hangReason(visible(rd.tail()))
			// The prompt has no line end; put it in the log now, so the log
			// shows what the server is waiting at.
			s.log.Flush(m.clock().Now())
			// Here, in the loop, under its context: a Stop asked meanwhile
			// waits for this one and finds nothing more to send if it
			// worked, and shutdown cuts it off like any of the loop's own
			// (no SIGKILL under a cancelled context) — leaving the server at
			// its prompt for the next boot's start to stop first.
			if s.beginOwn() {
				_, err := m.terminate(ctx, s.ws, proc, waited)
				s.endOwn()
				if err == nil {
					s.serverGone = true
				}
			}
		case <-ctx.Done():
			// Shutdown, or the workspace forgotten: close the terminal and
			// leave the server serving. A stop under way here has already
			// decided — it ran in this goroutine — and a run still reading
			// after a stop that failed (a paused container's, its terminal
			// kept) is ended too, at once.
			return s.leave(master, proc, waited, rd)
		case <-s.wake:
			// A stop. What the run records ends here, whether it works or
			// not: a stop that fails keeps the terminal, and what the server
			// says on it after that is no longer this run's to record.
			rcancel()
			for req := s.next(); req != nil; req = s.next() {
				if err := s.stopFor(req, proc, waited); err != nil {
					s.settle(req, err)
					continue
				}
				stopped = append(stopped, req)
			}
		}
	}
	<-waited
	master.Close()
	s.log.Flush(m.clock().Now())

	if len(stopped) > 0 || s.isStopping() {
		return outcome{kind: outStopped}
	}
	text := visible(rd.tail())
	switch hang {
	case ReasonHangRemoteDialog:
		return outcome{kind: outFatal, reason: hang, detail: fmt.Sprintf(
			"The session server waited %s at Claude Code's “Enable Remote Control?” prompt, so Drydock stopped it: remoteDialogSeen is missing from the shared .claude.json, which the Feature writes when the container is created. Drydock never answers the prompt. Rebuild the workspace.",
			durationText(p.GateTimeout))}
	case ReasonHangTrust:
		return outcome{kind: outFatal, reason: hang, detail: fmt.Sprintf(
			"The session server waited %s at Claude Code's prompt to trust the workspace folder, so Drydock stopped it: hasTrustDialogAccepted is missing for that folder in the shared .claude.json, which the Feature writes when the container is created. Drydock never answers the prompt. Rebuild the workspace.",
			durationText(p.GateTimeout))}
	case "":
	default:
		return outcome{kind: outCrash, detail: fmt.Sprintf("announced no environment within %s", durationText(p.GateTimeout))}
	}
	tail := rd.tail()
	ref, _ := classify.ClassifyRefusal(tail)
	switch ref {
	case classify.RefusalWaitRegistration:
		return outcome{kind: outWait}
	case classify.RefusalNoOrganization:
		return outcome{kind: outLogin, reason: ReasonNoOrganization, detail: "Claude Code could not determine the account's organization, so it will not start Remote Control: the shared login is missing its account record. Sign in to Claude again."}
	case classify.RefusalWorkspaceNotTrusted:
		return outcome{kind: outFatal, reason: ReasonNotTrusted, detail: "Claude Code does not trust the workspace folder: hasTrustDialogAccepted is missing for it in the shared .claude.json, which the Feature writes when the container is created. Rebuild the workspace."}
	case classify.RefusalBadCommandLine:
		return outcome{kind: outFatal, reason: ReasonBadCommandLine, detail: "Claude Code refused the command line Drydock built for the session server. This is a Drydock bug; restarting will not help."}
	}
	if st, known := m.identity(ctx); known && signedOut(st) {
		return outcome{kind: outSignedOut}
	}
	if strings.Contains(text, "drydock: secrets unavailable") {
		return outcome{kind: outCrash, detail: "could not fetch the workspace's secrets before starting"}
	}
	return outcome{kind: outCrash, detail: "exited"}
}

// stopFor stops the server for one request, reporting what came of it and
// settling nothing: with the run's process and its end when the loop holds a
// terminal for it, through the pid file otherwise. A server this owner has
// already stopped, with nothing launched since, is gone, and nothing is
// sent; nor is anything under a context that has ended.
func (s *sup) stopFor(req *stopReq, proc subproc.Process, waited <-chan struct{}) error {
	if s.serverGone {
		return nil
	}
	if err := req.ctx.Err(); err != nil {
		return err
	}
	_, err := s.m.terminate(req.ctx, s.ws, proc, waited)
	if err == nil {
		s.serverGone = true
	}
	return err
}

// leave is shutdown's end of a run: close the terminal and leave the server
// serving. Only Drydock's own end is ended — the local `devcontainer exec`,
// which does not reach the server (measured); nothing is signalled in the
// container.
func (s *sup) leave(master *os.File, proc subproc.Process, waited <-chan struct{}, rd *reading) outcome {
	master.Close()
	<-rd.done
	proc.Signal(subproc.SignalTerm)
	select {
	case <-waited:
	case <-s.m.clock().After(s.m.policy().KillWait):
		proc.Signal(subproc.SignalKill)
		<-waited
	}
	return outcome{kind: outDetached}
}

// reading is one run's terminal being read, by goroutines of its own so that
// it is drained whatever the loop is doing — stopping the server included,
// whose last output must be read for it to exit.
type reading struct {
	done   chan struct{} // closed once the terminal is read to its end and all of it handled
	served chan struct{} // closed once the server is serving
	mu     sync.Mutex
	last   []byte // the run's last refusalTail bytes
}

func (r *reading) tail() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.last)
}

func (r *reading) serving() bool {
	select {
	case <-r.served:
		return true
	default:
		return false
	}
}

// read reads the run's terminal: every chunk into the log and the tail, and,
// while ctx lasts, the heartbeat and what the server announces (discover),
// recorded under ctx. A discovery write that failed is tried again on the
// next chunk and on the next heartbeat tick, so a server that has gone quiet
// still heals.
func (s *sup) read(ctx context.Context, master *os.File) *reading {
	m, p := s.m, s.m.policy()
	rd := &reading{done: make(chan struct{}), served: make(chan struct{})}
	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunks <- bytes.Clone(buf[:n])
			}
			if err != nil {
				if !pty.IsEOF(err) {
					m.logf("drydock: workspace %s: reading the session server's terminal: %v", s.ws, err)
				}
				return
			}
		}
	}()
	go func() {
		defer close(rd.done)
		d := &discovered{sessions: map[string]bool{}}
		var window []byte
		look := func() {
			if ctx.Err() != nil || len(window) == 0 {
				return
			}
			if s.discover(ctx, d, window) && !rd.serving() {
				close(rd.served)
				s.set(ctx, Serving, ReasonServing, "", 0)
			}
		}
		tick := m.clock().After(p.HeartbeatEvery)
		for {
			select {
			case b, ok := <-chunks:
				if !ok {
					return
				}
				s.log.Write(m.clock().Now(), b)
				rd.mu.Lock()
				rd.last = keepTail(append(rd.last, b...), refusalTail)
				rd.mu.Unlock()
				window = keepTail(append(window, b...), discoveryWindow)
				if ctx.Err() == nil {
					s.heartbeat(ctx)
				}
				look()
			case <-tick:
				tick = m.clock().After(p.HeartbeatEvery)
				look()
			}
		}
	}()
	return rd
}

// hangReason names which gate a server is waiting at, from what it printed.
// Only after the timeout has decided it is hung: the prompt is a question,
// not an error, and it is never answered.
func hangReason(text string) Reason {
	switch {
	case strings.Contains(text, "Enable Remote Control?"):
		return ReasonHangRemoteDialog
	case strings.Contains(text, "Trust ") && strings.Contains(text, "? [y/N]"):
		return ReasonHangTrust
	}
	return "no_environment"
}

func keepTail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append([]byte(nil), b[len(b)-n:]...)
}

// discover reads the window for what the server announced, records anything
// new, and reports whether the server is now serving: it has announced its
// environment and printed its status block (`Capacity: N/M` follows
// `Connected`).
func (s *sup) discover(ctx context.Context, d *discovered, window []byte) bool {
	got, _ := classify.ClassifyDiscovery(window)
	// What the process announced is latched whether or not the record below
	// commits: it decides serving, which the announcement does.
	if d.env == "" && got.EnvironmentID != "" {
		// The first id a process announces is its own (classify's rule);
		// it cannot change within one process, so it is latched.
		d.env = got.EnvironmentID
	}
	for _, id := range got.SessionIDs {
		if !d.sessions[id] && !slices.Contains(d.pending, id) {
			d.pending = append(d.pending, id)
		}
	}
	if got.CapacityTotal > 0 {
		d.capUsed, d.capTotal = got.CapacityUsed, got.CapacityTotal
	}
	// What is recorded moves only once the record commits: a failed write
	// leaves it all pending, for the next window to try again.
	var env string
	if d.env != "" && !d.envRecorded {
		env = d.env
	}
	capChanged := d.capTotal > 0 && (d.capUsed != d.capRecUsed || d.capTotal != d.capRecTotal)
	if env != "" || len(d.pending) > 0 || capChanged {
		if s.recordDiscovery(ctx, d, env, d.pending, capChanged) {
			d.envRecorded = d.env != ""
			for _, id := range d.pending {
				d.sessions[id] = true
			}
			d.pending = nil
			d.capRecUsed, d.capRecTotal = d.capUsed, d.capTotal
		}
	}
	return d.env != "" && d.capTotal > 0
}

// terminate stops the workspace's server: SIGTERM inside the container, then
// a wait of Policy.StopTimeout, and only then SIGKILL. With a local process
// (proc, done), the wait is for it to end — `devcontainer exec` exits when
// the server does; without one — a server an earlier Drydock left — it polls
// the container. It reports whether a server was found.
func (m *Manager) terminate(ctx context.Context, ws string, proc subproc.Process, done <-chan struct{}) (bool, error) {
	p := m.policy()
	found, err := m.Runtime.Signal(ctx, ws, container.SessionTerm, m.PidFile)
	if err != nil {
		m.logf("drydock: workspace %s: SIGTERM to the session server: %v", ws, err)
	}
	if errors.Is(err, container.ErrSessionContainerPaused) {
		// A paused container's processes are frozen, not gone, and Docker
		// will not exec into it to ask: nothing more can be sent or asked
		// until it is unpaused, and its silence is never "no server". Not
		// even Drydock's own end is killed — the server it reaches may be
		// the one that is there.
		return true, err
	}
	if done == nil && !found {
		return false, err
	}
	// lookErr is the last failure to ask whether the server is alive: a
	// stop that ends on one could not tell, which is Docker's failure, not a
	// server seen alive after SIGKILL.
	var lookErr error
	// ended waits for Drydock's own end of the server — `devcontainer
	// exec`, which exits when the server does.
	ended := func(d time.Duration) bool {
		select {
		case <-done:
			return true
		case <-m.clock().After(d):
			return false
		case <-ctx.Done():
			return false
		}
	}
	// exited asks the container, through the pid file, until the server is
	// gone or d passes.
	exited := func(d time.Duration) bool {
		deadline := m.clock().After(d)
		for {
			alive, aerr := m.Runtime.Signal(ctx, ws, container.SessionAlive, m.PidFile)
			if errors.Is(aerr, container.ErrSessionSignalRefused) {
				// It is there; the kernel would not let even signal 0 by.
				alive, aerr = true, nil
			}
			lookErr = aerr
			if aerr == nil && !alive {
				return true
			}
			if errors.Is(aerr, container.ErrSessionContainerPaused) {
				return false // paused meanwhile: nothing more can be asked
			}
			select {
			case <-deadline:
				return false
			case <-ctx.Done():
				return false
			case <-m.clock().After(p.StopPoll):
			}
		}
	}
	gone := func(d time.Duration) bool {
		if done != nil {
			return ended(d)
		}
		return exited(d)
	}
	if gone(p.StopTimeout) {
		return true, nil
	}
	// Paused while it was being waited for: as for SIGTERM finding it
	// paused, nothing more can be sent or asked.
	if errors.Is(lookErr, container.ErrSessionContainerPaused) {
		return true, errors.Join(err, lookErr)
	}
	if cerr := ctx.Err(); cerr != nil {
		// The wait was cut short, not timed out: no SIGKILL, which is only
		// for a server that outlived its grace period (and a docker exec on
		// a cancelled context could not deliver one anyway). Say what
		// happened rather than what would have.
		m.logf("drydock: workspace %s: the stop was cancelled while waiting for the session server to exit after SIGTERM; SIGKILL was not sent", ws)
		if proc != nil {
			// As before the cancel was noticed here: Drydock's own end of
			// the server, which does not reach the server itself.
			proc.Signal(subproc.SignalKill)
		}
		return true, errors.Join(err, cerr)
	}
	m.logf("drydock: workspace %s: the session server did not exit within %s of SIGTERM; sending SIGKILL", ws, p.StopTimeout)
	_, kerr := m.Runtime.Signal(ctx, ws, container.SessionKill, m.PidFile)
	if kerr != nil {
		err = errors.Join(err, kerr)
	}
	if errors.Is(kerr, container.ErrSessionContainerPaused) {
		return true, err
	}
	if gone(p.KillWait) {
		return true, nil
	}
	if proc != nil {
		// The server is beyond reach; at least end Drydock's side of it.
		// That proves nothing about the server: killing a `docker exec`
		// client leaves the process it started running in the container
		// (measured, Docker 29.8.2), and Drydock's terminal closes all the
		// same. So the container is asked, through the pid file, as for a
		// server Drydock holds no terminal for — never "Drydock's end is
		// gone, so the server is".
		proc.Signal(subproc.SignalKill)
		ended(p.KillWait)
		if exited(p.KillWait) {
			return true, nil
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return true, errors.Join(err, cerr)
	}
	if (kerr != nil && !errors.Is(kerr, container.ErrSessionSignalRefused)) || lookErr != nil {
		// SIGKILL could not be sent, or whether it worked could not be
		// asked: Docker failed, which says nothing about the server.
		return true, errors.Join(err, lookErr, errors.New("the session server could not be confirmed stopped after SIGKILL"))
	}
	// SIGKILL went out (or the container refused it), and the container
	// says the server is still there.
	return true, errors.Join(err, container.ErrSessionSurvivedKill)
}
