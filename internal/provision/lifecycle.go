package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/krelinga/drydock/internal/broker"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/ephemeral"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// Stop and delete, design §5 and §6 (Phase 6). Each runs as a job beside the
// provisioning runs — one job per workspace at a time — and each sub-step
// writes a workspace.action event as it starts and as it ends, so a stop or a
// delete that sticks names where it stuck.

// KindAction is the event a stop or delete sub-step writes.
// data: {action: "stop"|"delete", step, status: started|done|failed, detail?}
const KindAction = workspace.KindAction

// Sub-steps, in order.
const (
	ActStop   = "stop"
	ActDelete = "delete"

	SubSessionServer = "session_server" // Phase 5's supervisor (StopSupervisor)
	SubContainer     = "container"      // stop: docker stop
	SubContainers    = "containers"     // delete: docker rm, every one with the label
	SubBrokerSocket  = "broker_socket"  // close the socket: GitHub access ends here
	SubFiles         = "files"          // delete: /srv/drydock/ws/<id>/
)

var (
	// ErrConfirmMismatch: a delete whose ?confirm= is not the repository's
	// full name, exactly. Typing the name is the delete's only friction
	// (§15.3), so a near miss is refused rather than corrected.
	ErrConfirmMismatch = errors.New("provision: confirm does not match the repository's full name")
	// ErrUnsafePath: the directory a delete would remove is not exactly
	// <WorkspaceRoot>/<workspace id>, a real directory there.
	ErrUnsafePath = errors.New("provision: refusing to remove a path that is not the workspace's own directory")
)

// Stop is POST /api/workspaces/{id}/stop. Only a running workspace with no
// job in flight stops; anything else is workspace.ErrInProgress.
//
// A stop during a build is refused rather than cancelling the build. Stopped
// means a container that came up and passed its probe, with a complete
// clone, so that start can resume from step 3 and reattach; a cancelled
// build is none of those, and is failed — which is what the provisioning
// timeout already makes of a build that never ends. The escape from a build
// the operator has given up on is delete, which does cancel it.
//
// The job: the session server (Phase 5), then `docker stop` of every
// container carrying the workspace's label, then the broker socket, then
// running → stopped. A failed sub-step leaves the workspace running —
// which, if its container is still up, is what it is.
func (p *Provisioner) Stop(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.admit(JobStop + " " + id)
	if err != nil {
		return err
	}
	defer a.abandon()
	w, err := p.Workspaces.Get(ctx, id)
	if err != nil {
		return err
	}
	if p.active[id] != nil || w.State != workspace.Running {
		return workspace.ErrInProgress
	}
	// A stop asked for again after one failed: the failure's annotation goes
	// as this one starts, on the stream, so nothing reading the list alone
	// still says the stop failed while it is running again (frontend §4.5
	// #15). Under p.mu, so the clear lands before the job's first event.
	if _, err := p.Workspaces.ClearDetail(ctx, id, workspace.Running); err != nil {
		return err
	}
	a.launch(id, JobStop, func(ctx context.Context) error {
		err := p.stopJob(ctx, w)
		if err != nil {
			p.logf("drydock: workspace %s: stop: %v", id, err)
		}
		return err
	})
	return nil
}

func (p *Provisioner) stopJob(ctx context.Context, w workspace.Workspace) error {
	book := context.WithoutCancel(ctx)
	un := &unpaused{}
	steps := []subStep{
		{SubSessionServer, func(ctx context.Context) error { return p.stopSupervisor(ctx, w, un) }},
		{SubContainer, func(ctx context.Context) error {
			ids, err := p.Containers.Find(ctx, w.ID)
			if err != nil {
				return workspace.Public("Drydock could not list the workspace's containers.", err)
			}
			if len(ids) == 0 {
				return workspace.Note("No container was found; it was already gone.")
			}
			if err := p.Containers.Stop(ctx, ids); err != nil {
				return workspace.Public("docker could not stop the workspace's container.", err)
			}
			return nil
		}},
		// A stopped workspace has no container to mount its socket into, and
		// access follows Drydock's state rather than Docker's: a container
		// someone restarts by hand gets no GitHub access until Drydock
		// starts the workspace, whose step 5 opens the socket again.
		{SubBrokerSocket, func(context.Context) error { return p.closeSocket(w.ID) }},
	}
	if err := p.subSteps(ctx, ActStop, w.ID, steps); err != nil {
		// The workspace stays running, and the row says why (frontend §4.5
		// #15): in state_detail, where every reader of the row finds it —
		// the list a reloaded home page reads included — rather than only
		// in the action events a detail body carries. The sentence names
		// the sub-step in Drydock's words; the next move replaces it (a
		// stop that works, a rebuild, a delete), and a stop asked for again
		// clears it as it starts. A stop a delete cut off is not annotated:
		// the workspace is deleting, and Annotate refuses it, which is the
		// answer wanted.
		var pub workspace.PublicError
		detail := StopFailedDetail("")
		if errors.As(err, &pub) {
			detail = StopFailedDetail(pub.Public())
		}
		// A container this stop unpaused and did not stop goes back to the
		// pause the operator made, with its access as it was.
		if said := p.repause(ctx, w, un, true); said != "" {
			detail += " " + said
		}
		var ill workspace.ErrIllegalMove
		// The job's last act, so it carries the job's end (failed, or
		// cancelled); refused, it carries nothing and launch writes it.
		if aerr := p.Workspaces.Annotate(workspace.Ending(book, true), w.ID, workspace.Running, detail); aerr != nil && !errors.As(aerr, &ill) {
			err = errors.Join(err, aerr)
		}
		return err
	}
	_, err := p.Workspaces.Move(workspace.Ending(book, false), w.ID, workspace.Stopped, "")
	return err
}

// StopFailedDetail is the state_detail a failed stop leaves on its running
// workspace: the sub-step's own sentence, framed. Exported so a test can name
// the exact sentence the UI shows live and after a reload.
func StopFailedDetail(public string) string {
	if public == "" {
		return "The stop did not finish; stop again to retry."
	}
	return "The stop did not finish: " + public + " Stop again to retry."
}

// Delete is DELETE /api/workspaces/{id}?confirm=<full_name>. The workspace
// moves to deleting before anything is removed — persisted first, which is
// what lets reconciliation resume a delete a restart interrupted (§6) — and
// the rest runs as a job: the session server (Phase 5), every container
// carrying the label (a failed up's leftover included), the broker socket,
// the workspace's directory (the clone and .drydock/), and then the row.
//
// A job in flight is cancelled first and waited for: a provisioning run owns
// the workspace until it ends, and must not open a socket or bring up a
// container after the delete has looked. A workspace already deleting is a
// no-op when its delete is in flight, and resumes the delete when not — a
// delete that stopped part-way is retried by asking again.
//
// Errors: workspace.ErrNotFound, ErrConfirmMismatch.
func (p *Provisioner) Delete(ctx context.Context, id, confirm string) error {
	v, err := p.Workspaces.View(ctx, id)
	if err != nil {
		return err
	}
	if v.FullName == "" || confirm != v.FullName {
		return ErrConfirmMismatch
	}
	_, err = p.startDelete(ctx, id)
	return err
}

// ResumeDelete finishes a delete a restart interrupted, and waits for it:
// reconciliation's resume_delete (§6). The confirm was given when the delete
// began; the persisted deleting state is the record of it.
func (p *Provisioner) ResumeDelete(ctx context.Context, id string) error {
	j, err := p.startDelete(ctx, id)
	if err != nil {
		return err
	}
	select {
	case <-j.done:
		return j.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Provisioner) startDelete(ctx context.Context, id string) (*job, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Admitted before the move to deleting, so a delete refused for
	// shutdown has written nothing; one admitted runs, even if the group
	// stops before it launches, and its first sub-step then says so.
	a, err := p.admit(JobDelete + " " + id)
	if err != nil {
		return nil, err
	}
	defer a.abandon()
	prev := p.active[id]
	if prev != nil && prev.kind == JobDelete {
		return prev, nil
	}
	if _, err := p.Workspaces.Move(ctx, id, workspace.Deleting, ""); err != nil {
		var ill workspace.ErrIllegalMove
		if !errors.As(err, &ill) || ill.From != workspace.Deleting {
			return nil, err
		}
		// Already deleting, with nothing in flight here: resume it — asked
		// again, or at boot. A delete that stuck says so in state_detail
		// ("…Delete again to retry."); that stops being true as the resume
		// starts, so it goes now, on the stream, before the first sub-step
		// (frontend §4.5 #16). A page that reads only the list then shows a
		// delete in progress rather than offering Delete again. If this one
		// sticks too, deleteJob writes a new annotation.
		if _, err := p.Workspaces.ClearDetail(ctx, id, workspace.Deleting); err != nil {
			return nil, err
		}
	}
	if prev != nil {
		prev.cancel(errDeleting)
	}
	// A resume a sign-in left owed is for a workspace that is going.
	delete(p.resumeOwed, id)
	return a.launch(id, JobDelete, func(ctx context.Context) error {
		if prev != nil {
			<-prev.done
		}
		err := p.deleteJob(ctx, id)
		if err != nil {
			p.logf("drydock: workspace %s: delete: %v", id, err)
		}
		return err
	}), nil
}

func (p *Provisioner) deleteJob(ctx context.Context, id string) error {
	book := context.WithoutCancel(ctx)
	w, err := p.Workspaces.Get(book, id)
	if err != nil {
		return err
	}
	un := &unpaused{}
	steps := []subStep{
		{SubSessionServer, func(ctx context.Context) error { return p.stopSupervisor(ctx, w, un) }},
		{SubContainers, func(ctx context.Context) error {
			ids, err := p.Containers.Find(ctx, id)
			if err != nil {
				return workspace.Public("Drydock could not list the workspace's containers.", err)
			}
			var said string
			switch len(ids) {
			case 0:
				said = "No container to remove."
			default:
				if err := p.Containers.Remove(ctx, ids); err != nil {
					return workspace.Public("docker could not remove the workspace's containers.", err)
				}
				// Docker is the truth: gone means not listed any more.
				left, err := p.Containers.Find(ctx, id)
				if err != nil {
					return workspace.Public("Drydock could not list the workspace's containers.", err)
				}
				if len(left) > 0 {
					return workspace.Public("A container carrying the workspace's label is still there.", container.ErrStillThere)
				}
				said = "Removed its container."
				if len(ids) > 1 {
					said = fmt.Sprintf("Removed %d containers.", len(ids))
				}
			}
			return workspace.Note(said + " " + p.removeBuiltImages(ctx, w))
		}},
		// The socket and its directory: the containers are gone, so no mount
		// names the directory any more.
		{SubBrokerSocket, func(context.Context) error { return p.removeSocket(id) }},
		{SubFiles, func(ctx context.Context) error {
			helped, err := removeWorkspaceDir(ctx, p.Workspaces.Root, w, p.Containers.RemoveContents)
			switch {
			case errors.Is(err, ErrUnsafePath):
				return workspace.Public("Drydock refused to remove the workspace's directory: it is not the workspace's own directory under the workspace root.", err)
			// The helper was asked for but never ran: say so, rather than
			// claim it was tried. A misconfigured image is the operator's to
			// fix, so it gets its own sentence naming the flag.
			case errors.Is(err, container.ErrCleanupImage):
				return workspace.Public("Drydock could not remove the workspace's directory: some files belong to another user, and the helper container that removes those cannot run, because --cleanup-image is not pinned by digest.", err)
			case errors.Is(err, container.ErrCleanupNotRun):
				return workspace.Public("Drydock could not remove the workspace's directory: some files belong to another user, and Drydock could not start the helper container that removes those.", err)
			case err != nil && helped:
				return workspace.Public("Drydock could not remove the workspace's directory, even with a helper container for the files it does not own.", err)
			case err != nil:
				return workspace.Public("Drydock could not remove the workspace's directory; files inside may belong to another user.", err)
			case helped:
				return workspace.Note("Some files belonged to another user (root, inside the container), so a short-lived helper container removed them.")
			}
			return nil
		}},
	}
	if err := p.subSteps(ctx, ActDelete, id, steps); err != nil {
		var pub workspace.PublicError
		detail := "The delete stopped part-way; delete again to retry."
		if errors.As(err, &pub) {
			detail = "The delete stopped part-way: " + pub.Public() + " Delete again to retry."
		}
		// Halted or cancelled before its containers went: what it unpaused
		// is paused again, its access left closed (the workspace is deleting).
		if said := p.repause(ctx, w, un, false); said != "" {
			detail += " " + said
		}
		if aerr := p.Workspaces.Annotate(workspace.Ending(book, true), id, workspace.Deleting, detail); aerr != nil {
			err = errors.Join(err, aerr)
		}
		return err
	}
	// The row's removal is the delete's last event, and carries its end.
	if err := p.Workspaces.Remove(workspace.Ending(book, false), id); err != nil {
		return err
	}
	p.dropBuildLog(id)
	if p.ForgetSupervisor != nil {
		p.ForgetSupervisor(id)
	}
	return nil
}

// removeBuiltImages removes the images `up` built for the workspace once its
// containers are gone, and returns the sentence the containers sub-step adds.
// It never fails the delete: an image left behind costs disk, not
// correctness, and the one reason Docker refuses — another container made
// from it — is a reason to keep it. The raw error goes to the journal only.
func (p *Provisioner) removeBuiltImages(ctx context.Context, w workspace.Workspace) string {
	removed, err := p.Containers.RemoveBuiltImages(ctx, w.HostPath)
	switch {
	case err != nil:
		p.logf("drydock: workspace %s: removing its built images: %v", w.ID, err)
		return "Its built images could not all be removed; they stay on the daemon."
	case len(removed) == 0:
		return "No built image to remove."
	case len(removed) == 1:
		return "Removed its built image."
	}
	return fmt.Sprintf("Removed its %d built images.", len(removed))
}

// KindHelpersSwept is the system event the boot sweep writes when it removed
// anything. data: {count}
const KindHelpersSwept = "container.helpers_swept"

// SweepHelpers is boot's sweep of helper containers (internal/ephemeral):
// every kind there is — a delete's cleanup helper, the docker guard's log
// probe, the identity watch's reads, a login container, the volume's owner
// helper — under this instance's prefix, that an earlier process left behind:
// Drydock killed while one ran, a killed docker client whose create landed
// after it, the daemon never reaching --rm. Each helper already removes its
// own label however it ends; this is for what a process that did not end
// left. Boot runs it after reconciliation has resumed and finished every
// interrupted delete.
//
// It never touches a workspace's container: ephemeral.SweepAll lists by the
// helper labels alone and keeps anything carrying the workspace label too,
// and another prefix's helpers are never listed. It spares a helper a holder
// in this process is running (a login, an identity read), and — since the
// docker guard's probe runs in a process of its own — a cleanup helper or log
// probe whose workspace has a job in flight here: a delete or start the
// operator asked for since boot may be running it right now. p.mu is held
// throughout, so no job can start between that check and the removal; the
// routes wait for the listings and a remove, once, at boot.
func (p *Provisioner) SweepHelpers(ctx context.Context) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping() {
		return 0, ErrShuttingDown
	}
	gone, err := p.Containers.SweepHelpers(ctx, func(f ephemeral.Found) bool {
		switch f.Kind {
		case ephemeral.Cleanup, ephemeral.LogProbe:
			return p.active[f.Value] != nil
		}
		return false
	})
	if err != nil {
		return 0, err
	}
	if len(gone) == 0 {
		return 0, nil
	}
	msg := "Removed a helper container an earlier Drydock left behind."
	if len(gone) > 1 {
		msg = fmt.Sprintf("Removed %d helper containers an earlier Drydock left behind.", len(gone))
	}
	_, err = p.Events.Emit(ctx, "", events.Info, KindHelpersSwept, msg, map[string]any{"count": len(gone)})
	return len(gone), err
}

// SweepGuardPolicies removes the docker guard's policy (and refusal) an up
// left behind when Drydock was killed during it, so a guard directory holds a
// policy only while an up runs (design §6, "The docker guard"). Boot calls it
// after reconciliation; a workspace with a job in flight here is skipped,
// under the lock every job starts under, since its up may be running now.
func (p *Provisioner) SweepGuardPolicies(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping() {
		return ErrShuttingDown
	}
	entries, err := os.ReadDir(p.Workspaces.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || p.active[e.Name()] != nil {
			continue
		}
		errs = append(errs, container.SweepPolicy(filepath.Join(p.Workspaces.Root, e.Name(), "repo")))
	}
	return errors.Join(errs...)
}

func (p *Provisioner) stopSupervisor(ctx context.Context, w workspace.Workspace, un *unpaused) error {
	if p.StopSupervisor == nil {
		return workspace.Note("Nothing to do yet: the Claude Code session server arrives with Claude support.")
	}
	err := p.unpauseAndStopSupervisor(ctx, w, un)
	if err != nil {
		if errors.Is(err, container.ErrSessionSurvivedKill) {
			// SIGTERM first exists so the server deregisters (Spike 02); one
			// that outlived SIGKILL will not, and asking again cannot end
			// it. The next sub-step — docker stop, or docker rm --force —
			// ends every process in the container, so carry on: a stop or
			// delete that stopped here would fail the same way for good,
			// and a stuck delete has no other way out.
			p.logf("drydock: workspace %s: the session server outlived SIGKILL; the container step ends it: %v", w.ID, err)
			return workspace.Note("The session server was still running after SIGKILL, so it ends with the container, in the next step.")
		}
		if errors.Is(err, container.ErrSessionContainerPaused) {
			// Still paused: Docker would not unpause it, or it was paused
			// again. Frozen, not gone, and Docker will not exec into it to
			// signal the server; but docker stop and docker rm --force do
			// end a paused container (measured), so the next sub-step ends
			// it, as for a server that outlived SIGKILL. Stopping here would
			// leave a delete stuck until someone unpaused the container by
			// hand. The cost is the one the unpause exists to avoid: killed
			// with its container, the server never deregisters (Spike 02).
			p.logf("drydock: workspace %s: the container is paused and could not be unpaused, so the session server could not be signalled; the container step ends it: %v", w.ID, err)
			return workspace.Note(PausedNote)
		}
		return workspace.Public("Drydock could not stop the session server.", err)
	}
	if len(un.ids) > 0 {
		return workspace.Note(UnpausedNote)
	}
	return workspace.Note("Stopped the session server, SIGTERM first, so its environment is kept for the next start.")
}

// UnpausedNote is the session_server sub-step's note when the container was
// paused and Drydock unpaused it to stop the server.
const UnpausedNote = "The workspace's container was paused, so Drydock closed its GitHub access and unpaused it to stop the session server, SIGTERM first, so its environment is kept for the next start."

// PausedNote is the session_server sub-step's note when the container is
// paused and stays paused: the server is ended with the container — docker
// stop thaws a paused container to signal PID 1 alone, and PID 1's exit
// SIGKILLs the rest (measured, test/container) — so it does not release its
// environment, and the next start waits for the registration to lapse
// (Spike 02: one to three minutes, shown as waiting_registration, a wait and
// not a failure).
const PausedNote = "The workspace's container is paused and Drydock could not unpause it, so the session server could not be signalled; it ends with the container, in the next step. " +
	"Ended that way it does not release its environment, so the next start may wait a few minutes for it."

// unpaused is what one stop, rebuild or delete unpaused: the containers to
// pause again if the action ends without ending them.
type unpaused struct {
	ids []string
}

// Kinds of the events an action's unpause and re-pause write, which join the
// workspace's feed (a rebuild's unpause has no sub-step to note it on).
// data: {action, count} and {action, count, failed}.
const (
	KindUnpaused = "container.unpaused"
	KindRepaused = "container.repaused"
)

// unpauseAndStopSupervisor is how a workspace stop, rebuild or delete stops
// the session server: the workspace's container is unpaused first, and then
// the server is stopped as always, SIGTERM first. Each of those ends the
// container anyway, but a server ended with its container — docker stop
// signals PID 1 alone, even of a paused container, which it thaws to do so,
// and docker rm --force kills — is SIGKILLed without deregistering, which
// holds the folder against the next start for minutes (Spike 02); unpaused,
// it gets its SIGTERM.
//
// **Access goes before the unpause.** A pause can be the operator's brake on
// an agent, and the unpause resumes every process in the container, not just
// the server, for as long as the stop takes. So the workspace's broker socket
// is closed first (§9.1: access follows Drydock's state, and this workspace
// is on its way out): what runs in that window gets no *new* token or
// secret — a fresh git or gh call fails, a tool command's secrets prelude
// exits 69. It withholds only what is fetched after the close: a process
// frozen after its fetch (a git push past its credential helper, a command
// whose prelude ran) keeps what it holds. The server needs neither to
// deregister. If the
// socket cannot be closed, nothing is unpaused, and the server ends with its
// container as for one that stays paused. A stop and a delete close the
// socket later anyway; a rebuild reopens it at step 5.
//
// A pause that lands after the unpause (the stop then fails with
// container.ErrSessionContainerPaused) is unpaused once more and the stop
// asked again. What was unpaused is recorded in un, so an action that then
// ends without ending the container pauses it again (repause). A session
// server restart alone never comes here: it must not change the container's
// state.
func (p *Provisioner) unpauseAndStopSupervisor(ctx context.Context, w workspace.Workspace, un *unpaused) error {
	unpause := func() bool {
		ids, err := p.Containers.Paused(ctx, w.ID)
		if err != nil || len(ids) == 0 {
			if err != nil {
				p.logf("drydock: workspace %s: listing paused containers before stopping the session server: %v", w.ID, err)
			}
			return false
		}
		if p.Broker != nil {
			if err := p.Broker.Close(w.ID); err != nil {
				p.logf("drydock: workspace %s: closing GitHub access before unpausing the container, so it stays paused: %v", w.ID, err)
				return false
			}
		}
		// Recorded before the unpause, so one that half-worked is paused
		// again all the same (repause pauses only what is running).
		un.ids = append(un.ids, ids...)
		if err := p.Containers.Unpause(ctx, ids); err != nil {
			p.logf("drydock: workspace %s: unpausing the container before stopping the session server: %v", w.ID, err)
			return false
		}
		p.emit(context.WithoutCancel(ctx), w.ID, events.Info, KindUnpaused,
			"The workspace's container was paused; Drydock closed its GitHub access and unpaused it to stop the session server cleanly.",
			map[string]any{"count": len(ids)})
		return true
	}
	unpause()
	err := p.StopSupervisor(ctx, w)
	if errors.Is(err, container.ErrSessionContainerPaused) && ctx.Err() == nil && unpause() {
		err = p.StopSupervisor(ctx, w)
	}
	return err
}

// RepauseTimeout bounds a re-pause, which runs under sys.Cleanup of the
// action's context: that may be the very thing that was cancelled.
//
// It must finish inside shutdown's wait for the jobs: a stop cut off by
// shutdown after unpausing first winds its cut-off subprocess down
// (subproc.DefaultWaitDelay), then re-pauses, whose last docker command the
// bound may cut off in turn (another DefaultWaitDelay). A re-pause killed
// with the process leaves an operator-paused container running, and the
// next boot would hand it a socket and a session server. So the server
// asserts, at compile time, that this and two wind-downs fit in its
// workShutdownWait.
const RepauseTimeout = 20 * time.Second

// repause pauses again what an action unpaused and then did not end — a stop
// or delete that halted or was cancelled before its container step, a
// rebuild that failed before its step 3 stopped the container — so the
// operator's pause is not silently undone. A container the action ended is
// not running and is left alone, so it is safe to call after any outcome.
// reopen (a stop's, whose workspace stays running) reopens the broker socket
// once everything is paused again, restoring the state the action found; a
// container that could not be paused again keeps its access closed. It
// returns a sentence for the action's failure detail, or "" when there was
// nothing to pause again.
//
// parent is the action's context: its values are kept and its cancellation
// is not (sys.Cleanup), since a re-pause is owed after a cancel; the bound is
// on the injected clock.
func (p *Provisioner) repause(parent context.Context, w workspace.Workspace, un *unpaused, reopen bool) string {
	if un == nil || len(un.ids) == 0 {
		return ""
	}
	ctx, cancel := sys.Cleanup(parent, p.clock(), RepauseTimeout)
	defer cancel()
	n, err := p.Containers.Repause(ctx, w.ID, un.ids)
	if n == 0 && err == nil {
		return ""
	}
	// Reopened only for a workspace still running: one a delete has taken
	// over is deleting, and has no access. A running one left without
	// access says how to get it back (a delete's is on its way out, and a
	// failed rebuild's start reopens it at step 5).
	running := false
	if reopen {
		cur, gerr := p.Workspaces.Get(ctx, w.ID)
		running = gerr == nil && cur.State == workspace.Running
	}
	hint := func(s string) string {
		if running {
			return s + " " + RestoreAccessHint
		}
		return s
	}
	if err != nil {
		p.logf("drydock: workspace %s: pausing the container again: %v", w.ID, err)
		said := hint(RepauseFailedSentence)
		p.emit(ctx, w.ID, events.Warn, KindRepaused, said, map[string]any{"count": n, "failed": true})
		return said
	}
	said := RepausedClosedSentence
	switch {
	case p.Broker == nil:
		said = RepausedSentence // there was no access to close
	case running:
		if oerr := p.Broker.Open(ctx, w.ID); oerr != nil {
			p.logf("drydock: workspace %s: reopening GitHub access after pausing the container again: %v", w.ID, oerr)
			said = hint(RepausedClosedSentence)
		} else {
			said = RepausedSentence
		}
	}
	p.emit(ctx, w.ID, events.Info, KindRepaused, said, map[string]any{"count": n, "failed": false})
	return said
}

// The sentences repause adds to a failed action's detail.
const (
	RepausedSentence       = "Drydock had unpaused the workspace's container to stop its session server; it is paused again, as it was."
	RepausedClosedSentence = "Drydock had unpaused the workspace's container to stop its session server; it is paused again, with its GitHub access closed."
	RepauseFailedSentence  = "Drydock had unpaused the workspace's container to stop its session server and could not pause it again: it is running, with its GitHub access closed."
	// RestoreAccessHint follows either closed-access sentence on a stop,
	// whose workspace stays running: a start's or a rebuild's step 5 is
	// what opens the socket again.
	RestoreAccessHint = "To restore its GitHub access, stop the workspace and start it again, or rebuild it."
)

func (p *Provisioner) emit(ctx context.Context, id string, level events.Level, kind, msg string, data map[string]any) {
	if p.Events == nil {
		return
	}
	if _, err := p.Events.Emit(ctx, id, level, kind, msg, data); err != nil {
		p.logf("drydock: workspace %s: writing %s: %v", id, kind, err)
	}
}

func (p *Provisioner) closeSocket(id string) error {
	if p.Broker == nil {
		return workspace.Note("No GitHub App is configured, so there is no socket.")
	}
	if err := p.Broker.Close(id); err != nil {
		return workspace.Public("Drydock could not close the workspace's GitHub access socket.", err)
	}
	return nil
}

// removeSocket is a delete's: the socket and the workspace's directory that
// held it (broker.Remove).
func (p *Provisioner) removeSocket(id string) error {
	if p.Broker == nil {
		return workspace.Note("No GitHub App is configured, so there is no socket.")
	}
	switch err := p.Broker.Remove(id); {
	case errors.Is(err, broker.ErrLeftover):
		// The socket is gone, which is what ends access; what is left is
		// files root in the container made, on a tmpfs. Not worth a stuck
		// delete that only a host root could unstick.
		p.logf("drydock: workspace %s: %v", id, err)
		return workspace.Note("Removed the socket. Something the container left beside it could not be removed; it goes at the next reboot.")
	case err != nil:
		return workspace.Public("Drydock could not remove the workspace's GitHub access socket.", err)
	}
	return nil
}

type subStep struct {
	name string
	f    func(ctx context.Context) error
}

// subSteps runs a stop's or a delete's sub-steps in order, writing a
// workspace.action event as each starts and ends — Drydock's sentence only,
// never a subprocess's stderr, as for the provisioning steps (§6). A
// sub-step is not started once the job's context has ended.
func (p *Provisioner) subSteps(ctx context.Context, action, id string, steps []subStep) error {
	book := context.WithoutCancel(ctx)
	emit := func(step, status, detail string) error {
		level, msg := events.Info, fmt.Sprintf("%s: %s %s.", capitalize(action), step, status)
		data := map[string]any{"action": action, "step": step, "status": status}
		if status == "failed" {
			level = events.Error
		}
		if detail != "" {
			data["detail"] = detail
			msg = fmt.Sprintf("%s: %s %s. %s", capitalize(action), step, status, detail)
		}
		_, err := p.Events.Emit(book, id, level, KindAction, msg, data)
		return err
	}
	for _, st := range steps {
		if err := emit(st.name, "started", ""); err != nil {
			return err
		}
		var err error
		if ctx.Err() != nil {
			err = workspace.Public(stoppedBy(ctx), context.Cause(ctx))
		} else {
			err = st.f(ctx)
		}
		if note, ok := workspace.IsNote(err); err == nil || ok {
			if err := emit(st.name, "done", note); err != nil {
				return err
			}
			if p.afterStep != nil {
				if err := p.afterStep(action, st.name); err != nil {
					return err
				}
			}
			continue
		}
		if ctx.Err() != nil {
			// Cut off, not failed on its own: say which, whatever the
			// interrupted subprocess printed.
			err = workspace.Public(stoppedBy(ctx), err)
		}
		detail := "The step failed."
		var pub workspace.PublicError
		if errors.As(err, &pub) {
			detail = pub.Public()
		}
		if eerr := emit(st.name, "failed", detail); eerr != nil {
			return errors.Join(err, eerr)
		}
		return fmt.Errorf("%s %s: %w", action, st.name, err)
	}
	return nil
}

// stoppedBy is the sentence for a sub-step its job's context cut off.
func stoppedBy(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errDeleting) {
		return "The workspace is being deleted, so Drydock stopped this step."
	}
	return "Drydock shut down while this step was running."
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// removeWorkspaceDir removes <root>/<id> — the clone and .drydock/ — and
// nothing else. It is the one place Drydock deletes a clone, and a clone can
// hold unpushed work, so it refuses anything it cannot prove is that
// directory (verifyWorkspaceDir) before anything runs.
//
// The host removal comes first. Inside the directory, os.RemoveAll unlinks a
// symlink rather than following it, so a link in the clone pointing out of
// the tree removes the link and leaves its target. What the host removal
// leaves is what the drydock user may not remove: files a root process in
// the container wrote into the clone. Those go through helper — the cleanup
// container, which sees <root>/<id> alone, bind-mounted, never a parent
// (container.CleanupArgs) — after the directory is proved again, and then the
// now-empty directory, which is Drydock's own, goes from the host. helped
// reports whether the helper ran.
//
// A directory already gone is success: a resumed delete may have removed it
// before the restart.
func removeWorkspaceDir(ctx context.Context, root string, w workspace.Workspace,
	helper func(ctx context.Context, workspaceID, dir string) error) (helped bool, err error) {
	dir, err := verifyWorkspaceDir(root, w)
	if err != nil || dir == "" {
		return false, err
	}
	if err := hostRemoveAll(dir); err == nil {
		return false, nil
	}
	// A read-only directory in the tree (a tool's cache, say) stops
	// RemoveAll. Make Drydock's own directories writable and try once more.
	// WalkDir does not follow symlinks, and a symlink's entry is not a
	// directory, so nothing outside the tree is touched.
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o700)
		}
		return nil
	})
	hostErr := hostRemoveAll(dir)
	if hostErr == nil || helper == nil {
		return false, hostErr
	}
	// Prove it again: the helper runs as root, so the directory it is given
	// is checked immediately before, not only at the top.
	real, err := verifyWorkspaceDir(root, w)
	if err != nil || real == "" {
		return false, err
	}
	if err := helper(ctx, w.ID, real); err != nil {
		return true, errors.Join(hostErr, err)
	}
	return true, hostRemoveAll(dir)
}

// hostRemoveAll is os.RemoveAll; a test replaces it to stand in for files
// the drydock user does not own, which a unit test cannot make without root.
var hostRemoveAll = os.RemoveAll

// verifyWorkspaceDir proves <root>/<id> is the workspace's own directory and
// returns it resolved, or "" with no error when it is already gone. It
// refuses:
//
//   - an id that is not a ULID, so it cannot be "", "..", or a path;
//   - a root that is not absolute and clean, or is "/";
//   - a row whose host_path is not <root>/<id>/repo — a root changed since
//     the workspace was created means the clone is somewhere else, and
//     removing <new root>/<id> would remove the wrong thing or nothing;
//   - a <root>/<id> that is not a real directory, is a symlink, or does not
//     resolve to <resolved root>/<id>.
func verifyWorkspaceDir(root string, w workspace.Workspace) (string, error) {
	if !ulidPattern.MatchString(w.ID) {
		return "", fmt.Errorf("%w: %q is not a workspace id", ErrUnsafePath, w.ID)
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return "", fmt.Errorf("%w: the workspace root %q is not a clean absolute path", ErrUnsafePath, root)
	}
	dir := filepath.Join(root, w.ID)
	if w.HostPath != filepath.Join(dir, "repo") {
		return "", fmt.Errorf("%w: the clone is recorded at %q, not under %q", ErrUnsafePath, w.HostPath, dir)
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory (mode %s)", ErrUnsafePath, dir, fi.Mode())
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if realDir != filepath.Join(realRoot, w.ID) {
		return "", fmt.Errorf("%w: %s resolves to %s", ErrUnsafePath, dir, realDir)
	}
	return realDir, nil
}
