//go:build linux

package supervisor

import (
	"context"
	"errors"
	"slices"

	"github.com/krelinga/drydock/internal/container"
)

// One owner per server process (design §8, *One signaller*). Every signal a
// workspace's session server is sent — the SIGTERM, the SIGKILL after
// StopTimeout, the questions in between — is sent by one goroutine: the sup's
// own (sup.own), which is its supervision loop for a sup Start made, and a
// loop born stopped for one Stop made. Nothing else calls terminate. A Stop
// is a request (stopReq) put on the owner's queue, and the caller waits for
// the owner's reply; the gate-hang stop and the stop of a server left running
// before a launch are the loop acting on its own, inline. So two stoppers
// never signal one server at once: a stop asked while the loop is stopping a
// leftover is answered after it, and one asked while another is under way is
// answered by the same outcome — the server is gone, and nothing is sent
// again.
//
// Between sups the rule is a handoff: a sup replaced (retire) takes no new
// request, and its successor sends nothing until the replaced one is quiet —
// it has answered every request it took and is not signalling — so a stop
// the old owner was making can never land on the server its successor has
// just launched.

// stopReq is one Stop: the caller's context, under which the owner stops the
// server, and the reply, which carries what the stop came to.
type stopReq struct {
	ctx   context.Context
	reply chan error // buffered: an owner never waits on a caller
}

// submit queues req for the owner, and reports false when this sup takes no
// more requests — its owner has ended, or it was replaced — so the caller
// must find the workspace's current owner instead. Asking is stopping: from
// here no server is launched, as before the owner has even seen the request.
// m.mu held, so a submit and a replacement are ordered.
func (s *sup) submit(req *stopReq) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.intake || s.retired {
		return false
	}
	s.pending = append(s.pending, req)
	s.stopping = true
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

// withdraw takes back a request its owner has not yet taken, and reports
// whether it did: a caller whose context ended while queued has sent nothing,
// and need not wait behind whatever its owner is doing.
func (s *sup) withdraw(req *stopReq) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.Index(s.pending, req)
	if i < 0 {
		return false
	}
	s.pending = slices.Delete(s.pending, i, i+1)
	s.quietIfDoneLocked()
	return true
}

// next takes the oldest queued request, or nil. A taken request is owed a
// reply (settle), and the sup is not quiet until it has one.
func (s *sup) next() *stopReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	req := s.pending[0]
	s.pending = s.pending[1:]
	s.busy++
	return req
}

// nextOrEnd is next for an owner that is ending: with nothing queued it
// takes no more requests, atomically with finding the queue empty, so no
// request is ever queued on an owner that will not answer it.
func (s *sup) nextOrEnd() *stopReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		s.intake = false
		s.ownerDone = true
		s.quietIfDoneLocked()
		return nil
	}
	req := s.pending[0]
	s.pending = s.pending[1:]
	s.busy++
	return req
}

// beginOwn is the loop's own stop — of a leftover server before a launch, or
// of one hung at a gate — and reports false, sending nothing, once the sup
// was replaced: its successor owns the server now.
func (s *sup) beginOwn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return false
	}
	s.busy++
	return true
}

func (s *sup) endOwn() {
	s.mu.Lock()
	s.busy--
	s.quietIfDoneLocked()
	s.mu.Unlock()
}

// retire is a replacement's: the sup takes no new request, and its loop no
// stop of its own. Its successor waits for quiet. m.mu held.
func (s *sup) retire() {
	s.mu.Lock()
	s.retired = true
	s.quietIfDoneLocked()
	s.mu.Unlock()
}

// quietIfDoneLocked closes quiet once the sup will never signal again:
// replaced or ended, with nothing queued and nothing taken unanswered.
func (s *sup) quietIfDoneLocked() {
	if s.quietClosed || !(s.retired || s.ownerDone) || len(s.pending) > 0 || s.busy > 0 {
		return
	}
	s.quietClosed = true
	close(s.quiet)
}

// own is the sup's goroutine for its whole life: the one place its server
// is signalled from. It waits for the sup it replaced to be quiet, runs the
// supervision loop (a sup Start made), and then answers every request still
// queued before it ends — a sup Stop made is born stopped and does only
// that.
func (s *sup) own(ctx context.Context, prev *sup) {
	if prev != nil {
		// Not cut short by shutdown: what prev is still doing is a stop a
		// caller asked for, bounded by its own timeouts, and sending before
		// it ends is exactly two signallers at once.
		<-prev.quiet
	}
	if s.supervise {
		s.loop(ctx)
	}
	s.drain()
}

// drain answers what is still queued once the loop has ended, or for a sup
// born stopped: through the pid file, as for a server Drydock holds no
// terminal for — unless this owner has already stopped its server and
// launched nothing since, when it is gone and nothing is sent.
func (s *sup) drain() {
	for req := s.nextOrEnd(); req != nil; req = s.nextOrEnd() {
		s.settle(req, s.stopFor(req, nil, nil))
	}
}

// settle records what a stop came to and answers its caller.
//
// A stop that worked records exited, when the sup has a row to say it on (a
// sup born for a server no supervisor was holding has none, and writes
// nothing). One that failed is recorded, never only returned: degraded, with
// stop_failed when Docker could not be asked and survived_kill when the
// server outlived SIGKILL, each with the sentence naming the one action that
// can fix it — written even when it repeats the last (announce), because
// every press of Restart session server waits for a supervisor.state
// (frontend §4.2). A sup with no row yet makes one first, as for a server an
// earlier Drydock left. A stop cut off by its caller (a delete, shutdown)
// records nothing: that is not a stop that failed, and what cancelled it
// says what happens next. Recorded before the reply, so the event is there
// when the caller returns, and under the caller's values with its
// cancellation removed, since the record is owed once the outcome is known.
func (s *sup) settle(req *stopReq, err error) {
	book := context.WithoutCancel(req.ctx)
	if err != nil {
		s.m.logf("drydock: workspace %s: stopping the session server: %v", s.ws, err)
	}
	switch {
	case err == nil:
		if s.hasRow() {
			s.record(func() { s.set(book, Exited, ReasonStopped, stoppedSentence, 0) })
		}
	case req.ctx.Err() != nil:
	default:
		ok, rerr := s.adoptRow(book)
		if rerr != nil {
			err = errors.Join(err, rerr)
		} else if ok {
			r := ReasonStopFailed
			if errors.Is(err, container.ErrSessionSurvivedKill) {
				r = ReasonSurvivedKill
			}
			s.record(func() { s.announce(book, Degraded, r, stopFailedDetail(r, err)) })
		}
	}
	req.reply <- err
	s.mu.Lock()
	s.busy--
	s.quietIfDoneLocked()
	s.mu.Unlock()
}

// record writes a stop's outcome unless the sup has been replaced (or
// forgotten): its successor's state — the starting a Start wrote as it
// replaced it — is the card's now, and an exited landing after it would
// show a stopped server under a running loop. Decided under m.mu, which a
// replacement holds from its retire to that first write, as a park is.
func (s *sup) record(write func()) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.mu.Lock()
	retired := s.retired
	s.mu.Unlock()
	if !retired {
		write()
	}
}

// stoppedSentence is exited's sentence after a stop.
const stoppedSentence = "The session server was stopped."

func (s *sup) hasRow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row != ""
}

// adoptRow gives a sup born for a server no supervisor was holding the
// workspace's row and log, the first time it has something to record: made
// if need be, with the state the row holds — none for a row made just now,
// whose placeholder no server ever had, so the first event is from nothing.
// It reports false, recording nothing, once Drydock is shutting down.
func (s *sup) adoptRow(ctx context.Context) (bool, error) {
	if s.hasRow() {
		return true, nil
	}
	m := s.m
	m.mu.Lock()
	if m.stoppingLocked() {
		m.mu.Unlock()
		return false, nil
	}
	ring := m.ringLocked(s.ws)
	m.mu.Unlock()
	row, restarts, st, err := m.rowFor(ctx, s.ws)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.row, s.restarts, s.state, s.log = row, restarts, st, ring
	s.mu.Unlock()
	return true, nil
}

// rowFor is the workspace's supervisor row, made if need be, with its
// restart count and the state it holds — none for a row made just now.
func (m *Manager) rowFor(ctx context.Context, workspaceID string) (string, int, State, error) {
	// Unread, it is taken for a row made just now: the first event is from
	// nothing rather than from a state no server had.
	var existed bool
	m.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM supervisor WHERE workspace_id = ?)`, workspaceID).Scan(&existed)
	row, restarts, err := m.ensureRow(ctx, workspaceID)
	if err != nil {
		return "", 0, "", err
	}
	var st State
	if existed {
		st, _, _ = m.storedState(ctx, row)
	}
	return row, restarts, st, nil
}
