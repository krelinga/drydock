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
	p.launch(id, "supervisor", func(ctx context.Context) error {
		if err := p.SupervisorRestart(ctx, id); err != nil {
			p.logf("drydock: workspace %s: restarting the session server: %v", id, err)
			return err
		}
		return nil
	})
	return nil
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
		p.mu.Lock()
		busy := p.active[w.ID] != nil || p.closed
		var serr error
		if !busy {
			serr = p.StartSupervisor(ctx, w)
		}
		p.mu.Unlock()
		if serr != nil {
			errs = append(errs, serr)
		}
	}
	return errors.Join(errs...)
}
