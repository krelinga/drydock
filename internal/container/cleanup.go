package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/krelinga/drydock/internal/subproc"
)

// The cleanup helper: how a delete removes what the drydock user cannot.
//
// A process running as root inside a workspace's container — sudo, a build
// tool, a postCreateCommand — leaves root-owned files in the bind-mounted
// clone, and the drydock user on the host cannot unlink them. Drydock is in
// the docker group, which is root by another name (§13.4), so the remedy
// costs no new privilege: a short-lived container, as root, removes what is
// left. It sees exactly one host path, the workspace's own directory,
// bind-mounted; it has no network, a read-only root filesystem, no
// capabilities but the two that let root remove another user's entries, and
// an image pinned by digest.

// LabelCleanup is the label a cleanup helper carries, valued with the
// workspace id. Deliberately not LabelWorkspace: reconciliation lists
// containers by <prefix>.workspace, so a helper is never mistaken for a
// workspace's container — never adopted, and never given a row.
const LabelCleanup = "cleanup"

// CleanupMount is where the workspace's directory appears in the helper.
const CleanupMount = "/w"

var (
	// ErrCleanupNotRun: RemoveContents never started a helper — its argv
	// was refused, or docker could not list or clear a stray one first. The
	// delete's sentence must not then claim a helper was tried.
	ErrCleanupNotRun = errors.New("container: the cleanup helper was not run")
	// ErrCleanupImage: the configured cleanup image is not pinned by
	// digest, so the helper is refused (and ErrCleanupNotRun with it).
	ErrCleanupImage = errors.New("container: the cleanup image is not pinned by digest")
)

// cleanupImagePattern is a reference pinned by digest: name[:tag]@sha256:<64 hex>.
var cleanupImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)

// ValidCleanupImage reports whether ref is an image reference pinned by
// digest, which is the only kind the helper will run.
func ValidCleanupImage(ref string) bool { return cleanupImagePattern.MatchString(ref) }

// CleanupArgs builds the helper's `docker run` argv. Exported so the argv —
// a root container with a host mount — can be asserted on directly.
//
// dir must be the workspace's own directory: absolute, clean, its last
// element the workspace id, and free of the characters --mount would read
// as more options. Checking that it is the right directory under the right
// root is the caller's job (provision.removeWorkspaceDir), done before this
// is called; this refuses only what could never be one.
func (m Manager) CleanupArgs(workspaceID, dir string) ([]string, error) {
	if !workspaceIDPattern.MatchString(workspaceID) {
		return nil, fmt.Errorf("container: %q is not a workspace id", workspaceID)
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Base(dir) != workspaceID ||
		strings.ContainsAny(dir, ",=\n\"") {
		return nil, fmt.Errorf("container: %q is not a workspace directory the helper may mount", dir)
	}
	if !ValidCleanupImage(m.CleanupImage) {
		return nil, fmt.Errorf("%w: %q", ErrCleanupImage, m.CleanupImage)
	}
	return []string{"run", "--rm",
		"--label", m.key(LabelCleanup) + "=" + workspaceID,
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
		"--security-opt", "no-new-privileges",
		"--user", "0:0",
		"--mount", "type=bind,source=" + dir + ",target=" + CleanupMount,
		"--entrypoint", "find",
		m.CleanupImage,
		// Everything inside the mount, depth first, symlinks unlinked and
		// never followed; the mount point itself stays, for the host to
		// remove once it is empty.
		CleanupMount, "-mindepth", "1", "-delete",
	}, nil
}

// RemoveContents empties the workspace's directory through the cleanup
// helper. A helper an earlier attempt left behind — Drydock killed while it
// ran, and the daemon never got to --rm — is removed first, found by its
// label, so a retried delete never runs two.
//
// Everything before the helper's own `docker run` that fails is
// ErrCleanupNotRun, so the caller can tell "no helper ran" from "a helper
// ran and failed".
func (m Manager) RemoveContents(ctx context.Context, workspaceID, dir string) error {
	args, err := m.CleanupArgs(workspaceID, dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupNotRun, err)
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args: []string{"ps", "--all", "--quiet", "--no-trunc",
			"--filter", "label=" + m.key(LabelCleanup) + "=" + workspaceID},
		Stdout: limit(&out, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker ps", res, &stderr); err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupNotRun, err)
	}
	if stray := strings.Fields(out.String()); len(stray) > 0 {
		if err := m.Remove(ctx, stray); err != nil {
			return fmt.Errorf("%w: removing a stray cleanup helper: %w", ErrCleanupNotRun, err)
		}
	}
	stderr.Reset()
	res = m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: args,
		Stdout: limit(&bytes.Buffer{}, 64<<10), Stderr: limit(&stderr, 64<<10)})
	return failed("docker run (cleanup)", res, &stderr)
}

// Helper is a cleanup helper container, found by its label.
type Helper struct {
	ContainerID string
	// WorkspaceID is the label's value: the workspace whose delete ran it.
	WorkspaceID string
}

// ListHelpers finds every container, running or not, carrying this prefix's
// cleanup label, whatever its value: the boot sweep's listing (design §6). As
// List does, `docker ps` for the ids, filtered on the daemon's side, then
// `docker inspect` for the labels — never a table parse.
//
// A container that also carries this prefix's workspace label is never
// returned. Drydock's helpers never carry it (CleanupArgs), so one that does
// was not made by a delete, and the one thing a sweep must never remove is a
// workspace's container: it is left to reconciliation, which owns that label.
// A container listed by the cleanup label whose inspect lacks it is an error,
// as in List: the contract moved, and acting on it would be guessing.
func (m Manager) ListHelpers(ctx context.Context) ([]Helper, error) {
	var ids, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args:   []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=" + m.key(LabelCleanup)},
		Stdout: limit(&ids, 1<<20), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker ps", res, &stderr); err != nil {
		return nil, err
	}
	list := strings.Fields(ids.String())
	if len(list) == 0 {
		return nil, nil
	}
	for _, id := range list {
		if !containerID.MatchString(id) {
			return nil, fmt.Errorf("docker ps: %q is not a container id", id)
		}
	}
	var out bytes.Buffer
	stderr.Reset()
	res = m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"inspect", "--type", "container"}, list...),
		Stdout: &out, Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker inspect", res, &stderr); err != nil {
		return nil, err
	}
	var all []inspect
	if err := json.Unmarshal(out.Bytes(), &all); err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var found []Helper
	for _, c := range all {
		ws, ok := c.Config.Labels[m.key(LabelCleanup)]
		if !ok || !containerID.MatchString(c.ID) {
			return nil, fmt.Errorf("docker inspect: container %q lacks the %s label it was listed by", c.ID, m.key(LabelCleanup))
		}
		if _, isWorkspace := c.Config.Labels[m.key(LabelWorkspace)]; isWorkspace {
			continue
		}
		found = append(found, Helper{ContainerID: c.ID, WorkspaceID: ws})
	}
	return found, nil
}
