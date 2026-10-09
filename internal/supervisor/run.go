//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// terminal, and decides what an exit means.
type sup struct {
	m      *Manager
	ws     string
	row    string
	log    *Ring
	cancel context.CancelFunc
	done   chan struct{} // closed when the loop returns
	// detached is the manager's group's context ending — shutdown — on
	// which a run closes its terminal even when its context was already
	// cancelled by a stop. Nil for one with no loop.
	detached <-chan struct{}

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
	stopping bool
	// stopOver is closed when the latest stop's terminate has returned,
	// worked or not: until then shutdown leaves the terminal to the stop,
	// whose wait for the server may be on it.
	stopOver chan struct{}
	proc     subproc.Process
	procDone chan struct{}
	lastBeat time.Time
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
		// Defer to the fleet's identity before spending anything (§7.3,
		// frontend §6.6): a server that cannot run is not started, and
		// waiting for a sign-in costs no restart.
		if st, known := m.identity(ctx); known && signedOut(st) {
			s.set(ctx, AwaitingLogin, ReasonSignedOut, signedOutSentence(st), 0)
			return
		}
		// A server an earlier process (or an earlier run) left behind still
		// holds the folder, and every start would be refused as already
		// served until it exits. Stop it, SIGTERM first, so its sessions
		// reconnect to the new one (Spike 02).
		if _, err := m.terminate(ctx, s.ws, nil, nil); err != nil && ctx.Err() == nil {
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
			s.set(ctx, AwaitingLogin, out.reason, out.detail, 0)
			return
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

func (s *sup) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// sleep waits d on the injected clock, or until the loop is cancelled.
func (s *sup) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.m.clock().After(d):
		return !s.isStopping()
	}
}

// discovered is what one run's output has announced so far, and how much of
// it is recorded. The announced half (env, capUsed/capTotal) decides
// serving; the recorded half moves only when recordDiscovery commits, so a
// write that fails is tried again on the next window rather than lost for
// the server's lifetime.
type discovered struct {
	env               string
	capUsed, capTotal int

	envRecorded bool
	sessions    map[string]bool // recorded
	pending     []string        // announced, not yet recorded, in order
	capRecUsed  int
	capRecTotal int
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
		m.logf("drydock: workspace %s: session server spec: %v", s.ws, err)
		return outcome{kind: outCrash, detail: "could not be started: Drydock could not read the workspace's configuration"}
	}
	spec.Capacity = p.Capacity
	spec.PidFile = m.PidFile

	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return outcome{kind: outStopped}
	}
	// The process is not tied to the loop's context: cancelling that is
	// Drydock shutting down, which must leave the server running, and a stop
	// ends the process by signalling it in the container first.
	procCtx, procCancel := context.WithCancel(context.Background())
	proc, master, err := m.Runtime.Start(procCtx, spec, p.Cols, p.Rows)
	if err != nil {
		s.mu.Unlock()
		procCancel()
		m.logf("drydock: workspace %s: starting the session server: %v", s.ws, err)
		return outcome{kind: outCrash, detail: "could not be started"}
	}
	procDone := make(chan struct{})
	s.proc, s.procDone = proc, procDone
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.proc, s.procDone = nil, nil
		s.mu.Unlock()
		procCancel()
		close(procDone)
	}()
	// A retry while the previous server's registration lapses stays a wait
	// on the card: it is the same wait, asked again, not a new start.
	if s.current() != WaitingRegistration {
		s.set(ctx, Starting, ReasonLaunching, "Starting the session server.", proc.Pid())
	}

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

	d := &discovered{sessions: map[string]bool{}}
	var window, tail []byte
	gate := m.clock().After(p.GateTimeout)
	cancelled := ctx.Done()
	// leave is shutdown: close the terminal and leave the server serving.
	// Only Drydock's own end is ended — the local `devcontainer exec`,
	// which does not reach the server (measured); nothing is signalled in
	// the container.
	leave := func() outcome {
		master.Close()
		for range chunks {
		}
		waited := make(chan struct{})
		go func() { proc.Wait(); close(waited) }()
		proc.Signal(subproc.SignalTerm)
		select {
		case <-waited:
		case <-m.clock().After(p.KillWait):
			proc.Signal(subproc.SignalKill)
			<-waited
		}
		return outcome{kind: outDetached}
	}
	detach := s.detached
	var stopDecided chan struct{}
	var hang Reason
	serving := false
	for open := true; open; {
		select {
		case b, ok := <-chunks:
			if !ok {
				open = false
				break
			}
			s.log.Write(m.clock().Now(), b)
			tail = keepTail(append(tail, b...), refusalTail)
			window = keepTail(append(window, b...), discoveryWindow)
			s.heartbeat(ctx)
			if s.discover(ctx, d, window) && !serving {
				serving = true
				gate = nil
				s.set(ctx, Serving, ReasonServing, "", 0)
			}
		case <-gate:
			gate = nil
			// No environment within the deadline. A hang has no message
			// (two of the three gates wait at a prompt rather than fail), so
			// the timeout is the verdict; the prompt's text only names the
			// key. Stop it — SIGTERM first — and keep reading until it ends.
			hang = hangReason(visible(tail))
			// The prompt has no line end; put it in the log now, so the log
			// shows what the server is waiting at.
			s.log.Flush(m.clock().Now())
			// In the group, not under this run's context: a stop of the
			// supervisor cancels that and does its own terminate, and this
			// one must carry on until the server ends, which ends the read.
			// Shutdown cuts it off like any other of the group's (no
			// SIGKILL under a cancelled context), and waits for it; once the
			// group is stopping it is not started, and the cancel below
			// leaves the server where it is.
			m.g.Go("gate stop "+s.ws, func(gctx context.Context) {
				m.terminate(gctx, s.ws, proc, procDone)
			})
		case <-cancelled:
			cancelled = nil
			if !s.isStopping() {
				return leave()
			}
			// A stop cancelled it: the stop ends the server, and this run
			// reads on until it does. If the stop failed with the terminal
			// kept (a paused container), only shutdown ends the read.
		case <-detach:
			// A run that is not stopping leaves on the cancel, which shutdown
			// sends under the same lock; this case is for one a stop
			// cancelled already. A stop under way may be waiting for the
			// server on this very terminal (`devcontainer exec` exits when
			// the server does), and closing it now would read to that stop
			// as the server ending: leave once it has decided — at once for
			// one that already failed. It is bounded by its own timeouts.
			detach = nil
			s.mu.Lock()
			stopDecided = s.stopOver // set with stopping; nil for a run not stopping
			s.mu.Unlock()
		case <-stopDecided:
			return leave()
		}
	}
	proc.Wait()
	master.Close()
	s.log.Flush(m.clock().Now())

	if s.isStopping() {
		return outcome{kind: outStopped}
	}
	text := visible(tail)
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

// stop ends the supervisor: the server is signalled in its container,
// SIGTERM then SIGKILL on timeout, and the loop is waited for.
func (s *sup) stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	over := make(chan struct{})
	s.stopOver = over
	proc, procDone := s.proc, s.procDone
	s.mu.Unlock()
	_, err := s.m.terminate(ctx, s.ws, proc, procDone)
	close(over)
	if s.cancel != nil { // nil for one with no loop (detachedLocked)
		s.cancel()
	}
	// After a failed stop the loop may be reading a terminal whose server
	// did not end — in a paused container, say, where Drydock's end is kept. It
	// ends when the terminal closes, and the stop does not wait on it for
	// good. It writes nothing meanwhile, even once a Start has replaced it:
	// stopping is set, so it sets no state, and the context just cancelled
	// is the one every write it would make (the heartbeat, rc_session, the
	// session events) runs under, so none lands.
	var bound <-chan time.Time
	if err != nil {
		bound = s.m.clock().After(s.m.policy().KillWait)
	}
	select {
	case <-s.done:
	case <-bound:
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
	return err
}

// terminate stops the workspace's server: SIGTERM inside the container, then
// a wait of Policy.StopTimeout, and only then SIGKILL. With a local process
// (proc, done), the wait is for it to end — `devcontainer exec` exits when
// the server does; without one — a server an earlier Drydock left — it polls
// the container. It reports whether a server was found.
func (m *Manager) terminate(ctx context.Context, ws string, proc subproc.Process, done chan struct{}) (bool, error) {
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
