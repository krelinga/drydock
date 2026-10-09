package provision

import (
	"context"
	"errors"

	"github.com/krelinga/drydock/internal/workspace"
)

// ErrNoSupervisor: no session supervisor is configured.
var ErrNoSupervisor = errors.New("provision: no session supervisor is configured")

// RestartSupervisor is POST /api/workspaces/{id}/supervisor: start the
// workspace's session server, or restart it (SIGTERM first), as a job under
// the same one-per-workspace ownership as a stop or a rebuild — so a stop or
// a rebuild asked for meanwhile is in_progress rather than racing it, and a
// delete cancels it. Only a running workspace with nothing in flight has a
// session server to restart; anything else is workspace.ErrInProgress.
// Every session the server was serving is ended (§5); Spike 02 says they
// reconnect, because a SIGTERMed server keeps its environment.
func (p *Provisioner) RestartSupervisor(ctx context.Context, id string) error {
	if p.SupervisorRestart == nil {
		return ErrNoSupervisor
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrShuttingDown
	}
	w, err := p.Workspaces.Get(ctx, id)
	if err != nil {
		return err
	}
	if p.active[id] != nil || w.State != workspace.Running {
		return workspace.ErrInProgress
	}
	p.launch(id, JobSupervisor, func(ctx context.Context) error {
		if err := p.SupervisorRestart(ctx, id); err != nil {
			p.logf("drydock: workspace %s: restarting the session server: %v", id, err)
			return err
		}
		return nil
	})
	return nil
}

// stillRunning re-reads the workspace (p.mu held) and reports whether it is
// still running, refreshing w. A failed read is not running: boot's
// follow-ups act only on what they can confirm.
func (p *Provisioner) stillRunning(ctx context.Context, w *workspace.Workspace) bool {
	cur, err := p.Workspaces.Get(ctx, w.ID)
	if err != nil || cur.State != workspace.Running {
		return false
	}
	*w = cur
	return true
}

// ReopenSockets opens the broker socket of every running workspace with no
// job in flight, after a restart: after reconciliation, so the set is the one
// Docker confirmed. A stopped workspace has no container to mount it into,
// and stop closed it (§9.1: access follows Drydock's state); start opens it
// again at step 5. A workspace mid-provision is this process's own run, whose
// step 5 opens it; a deleting one is having its socket removed. Each is
// decided and opened under the lock every job starts under, from the row read
// again there — boot runs beside serving, and a stop that finished after the
// list was read has closed the socket this would otherwise open again.
func (p *Provisioner) ReopenSockets(ctx context.Context) error {
	if p.Broker == nil {
		return nil
	}
	all, err := p.Workspaces.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range all {
		if w.State != workspace.Running {
			continue
		}
		p.mu.Lock()
		if p.active[w.ID] == nil && !p.closed && p.stillRunning(ctx, &w) {
			if err := p.Broker.Open(ctx, w.ID); err != nil {
				errs = append(errs, err)
			}
		}
		p.mu.Unlock()
	}
	return errors.Join(errs...)
}

// ResumeSupervisors is boot adoption's half of §6's first reconciliation row
// ("Adopt. Restart the supervisor"): every running workspace with no job in
// flight gets its session server started — which first stops the one an
// earlier Drydock left serving in the container, SIGTERM first, so its
// sessions reconnect to the new one. Run after reconciliation, so the set is
// the one Docker confirmed; nothing stopped is started.
func (p *Provisioner) ResumeSupervisors(ctx context.Context) error {
	if p.StartSupervisor == nil {
		return nil
	}
	all, err := p.Workspaces.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range all {
		if w.State != workspace.Running {
			continue
		}
		// A container an earlier Drydock made with the broker socket
		// mounted as a file has had no broker since this restart, and a
		// server started in it would fail its secrets prelude on every
		// launch. Say so, once, with the fix — never a restart loop — and
		// leave whatever is running there alone. A failed look starts it as
		// before: the check is a courtesy, not a gate.
		legacy := false
		if p.ParkSupervisor != nil && p.Broker != nil {
			var lerr error
			if legacy, lerr = p.Containers.LegacyBrokerMount(ctx, w.ID); lerr != nil {
				p.logf("drydock: workspace %s: inspecting its container's mounts: %v", w.ID, lerr)
			}
		}
		p.mu.Lock()
		busy := p.active[w.ID] != nil || p.closed
		if !busy {
			// Read again under the lock: the list is from before, and a
			// stop asked since may have finished, leaving nothing in flight
			// and a stopped row.
			busy = !p.stillRunning(ctx, &w)
		}
		var serr error
		switch {
		case busy:
		case legacy:
			p.logf("drydock: workspace %s: %v; not starting its session server", w.ID, errLegacyMount)
			serr = p.ParkSupervisor(ctx, w, LegacyMountSentence)
		default:
			serr = p.StartSupervisor(ctx, w)
		}
		p.mu.Unlock()
		if serr != nil {
			errs = append(errs, serr)
		}
	}
	return errors.Join(errs...)
}
