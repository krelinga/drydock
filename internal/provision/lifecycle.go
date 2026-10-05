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
const KindAction = "workspace.action"

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
		return err
	}
	_, err := p.Workspaces.Move(book, w.ID, workspace.Stopped, "")
	return err
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
		// Already deleting, with nothing in flight here: resume it.
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
		{SubFiles, func(context.Context) error {
			if err := removeWorkspaceDir(p.Workspaces.Root, w); err != nil {
				if errors.Is(err, ErrUnsafePath) {
					return workspace.Public("Drydock refused to remove the workspace's directory: it is not the workspace's own directory under the workspace root.", err)
				}
				return workspace.Public("Drydock could not remove the workspace's directory; files inside may belong to another user.", err)
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
	return p.Workspaces.Remove(book, id)
}

func (p *Provisioner) stopSupervisor(ctx context.Context, w workspace.Workspace) error {
	if p.StopSupervisor == nil {
		return workspace.Note("Nothing to do yet: the Claude Code session server arrives with Claude support.")
	}
	if err := p.StopSupervisor(ctx, w); err != nil {
		return workspace.Public("Drydock could not stop the session server.", err)
	}
	return nil
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
// directory:
//
//   - the id must be a ULID, so it cannot be "", "..", or a path;
//   - the root must be absolute and clean, and not "/";
//   - the row's host_path must be <root>/<id>/repo — a root changed since
//     the workspace was created means the clone is somewhere else, and
//     removing <new root>/<id> would remove the wrong thing or nothing;
//   - <root>/<id> must be a real directory, not a symlink, and resolve to
//     <resolved root>/<id>.
//
// Inside the directory, os.RemoveAll unlinks a symlink rather than following
// it, so a link in the clone pointing out of the tree removes the link and
// leaves its target. A directory already gone is success: a resumed delete
// may have removed it before the restart.
func removeWorkspaceDir(root string, w workspace.Workspace) error {
	if !ulidPattern.MatchString(w.ID) {
		return fmt.Errorf("%w: %q is not a workspace id", ErrUnsafePath, w.ID)
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return fmt.Errorf("%w: the workspace root %q is not a clean absolute path", ErrUnsafePath, root)
	}
	dir := filepath.Join(root, w.ID)
	if w.HostPath != filepath.Join(dir, "repo") {
		return fmt.Errorf("%w: the clone is recorded at %q, not under %q", ErrUnsafePath, w.HostPath, dir)
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory (mode %s)", ErrUnsafePath, dir, fi.Mode())
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if realDir != filepath.Join(realRoot, w.ID) {
		return fmt.Errorf("%w: %s resolves to %s", ErrUnsafePath, dir, realDir)
	}
	if err := os.RemoveAll(dir); err == nil {
		return nil
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
	return os.RemoveAll(dir)
}
