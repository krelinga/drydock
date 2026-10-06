package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
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
	// A stop asked for again after one failed: the failure's annotation goes
	// as this one starts, on the stream, so nothing reading the list alone
	// still says the stop failed while it is running again (frontend §4.5
	// #15). Under p.mu, so the clear lands before the job's first event.
	if _, err := p.Workspaces.ClearDetail(ctx, id, workspace.Running); err != nil {
		return err
	}
	p.launch(id, "stop", func(ctx context.Context) error {
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
	steps := []subStep{
		{SubSessionServer, func(ctx context.Context) error { return p.stopSupervisor(ctx, w) }},
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
		var ill workspace.ErrIllegalMove
		if aerr := p.Workspaces.Annotate(book, w.ID, workspace.Running, detail); aerr != nil && !errors.As(aerr, &ill) {
			err = errors.Join(err, aerr)
		}
		return err
	}
	_, err := p.Workspaces.Move(book, w.ID, workspace.Stopped, "")
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
	if p.closed {
		return nil, ErrShuttingDown
	}
	prev := p.active[id]
	if prev != nil && prev.kind == "delete" {
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
	return p.launch(id, "delete", func(ctx context.Context) error {
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
	steps := []subStep{
		{SubSessionServer, func(ctx context.Context) error { return p.stopSupervisor(ctx, w) }},
		{SubContainers, func(ctx context.Context) error {
			ids, err := p.Containers.Find(ctx, id)
			if err != nil {
				return workspace.Public("Drydock could not list the workspace's containers.", err)
			}
			if len(ids) == 0 {
				return workspace.Note("No container to remove.")
			}
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
			if len(ids) == 1 {
				return workspace.Note("Removed its container.")
			}
			return workspace.Note(fmt.Sprintf("Removed %d containers.", len(ids)))
		}},
		{SubBrokerSocket, func(context.Context) error { return p.closeSocket(id) }},
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
		if aerr := p.Workspaces.Annotate(book, id, workspace.Deleting, detail); aerr != nil {
			err = errors.Join(err, aerr)
		}
		return err
	}
	if err := p.Workspaces.Remove(book, id); err != nil {
		return err
	}
	if p.ForgetSupervisor != nil {
		p.ForgetSupervisor(id)
	}
	return nil
}

// KindHelpersSwept is the system event the boot sweep writes when it removed
// anything. data: {count}
const KindHelpersSwept = "container.helpers_swept"

// SweepHelpers removes every cleanup helper a delete left behind — Drydock
// killed while one ran, and the daemon never reaching --rm — found by this
// instance's <prefix>.cleanup label (design §6). RemoveContents already
// clears a stray before running a new helper for the same workspace, but
// only for a workspace whose delete needs the helper again; a delete
// resumed at boot whose host removal then succeeds, or a workspace whose
// row is gone, would leave one for good. So boot sweeps, after
// reconciliation has resumed and finished every interrupted delete.
//
// It never touches a workspace's container: ListHelpers lists by the cleanup
// label alone and drops anything carrying the workspace label too. And it
// skips a helper whose workspace has a job in flight here — a delete the
// operator started since boot may be running that helper right now. p.mu is
// held throughout, so no job can start between that check and the removal;
// the routes wait for a docker listing and a remove, once, at boot.
func (p *Provisioner) SweepHelpers(ctx context.Context) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, ErrShuttingDown
	}
	found, err := p.Containers.ListHelpers(ctx)
	if err != nil {
		return 0, err
	}
	var ids []string
	for _, h := range found {
		if p.active[h.WorkspaceID] != nil {
			continue
		}
		ids = append(ids, h.ContainerID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := p.Containers.Remove(ctx, ids); err != nil {
		return 0, err
	}
	msg := "Removed a cleanup helper container an interrupted delete left behind."
	if len(ids) > 1 {
		msg = fmt.Sprintf("Removed %d cleanup helper containers interrupted deletes left behind.", len(ids))
	}
	_, err = p.Events.Emit(ctx, "", events.Info, KindHelpersSwept, msg, map[string]any{"count": len(ids)})
	return len(ids), err
}

func (p *Provisioner) stopSupervisor(ctx context.Context, w workspace.Workspace) error {
	if p.StopSupervisor == nil {
		return workspace.Note("Nothing to do yet: the Claude Code session server arrives with Claude support.")
	}
	if err := p.StopSupervisor(ctx, w); err != nil {
		return workspace.Public("Drydock could not stop the session server.", err)
	}
	return workspace.Note("Stopped the session server, SIGTERM first, so its environment is kept for the next start.")
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
