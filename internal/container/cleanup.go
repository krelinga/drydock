package container

import (
	"bytes"
	"context"
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
		return nil, fmt.Errorf("container: cleanup image %q is not pinned by digest", m.CleanupImage)
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
func (m Manager) RemoveContents(ctx context.Context, workspaceID, dir string) error {
	args, err := m.CleanupArgs(workspaceID, dir)
	if err != nil {
		return err
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args: []string{"ps", "--all", "--quiet", "--no-trunc",
			"--filter", "label=" + m.key(LabelCleanup) + "=" + workspaceID},
		Stdout: limit(&out, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker ps", res, &stderr); err != nil {
		return err
	}
	if stray := strings.Fields(out.String()); len(stray) > 0 {
		if err := m.Remove(ctx, stray); err != nil {
			return fmt.Errorf("container: removing a stray cleanup helper: %w", err)
		}
	}
	stderr.Reset()
	res = m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: args,
		Stdout: limit(&bytes.Buffer{}, 64<<10), Stderr: limit(&stderr, 64<<10)})
	return failed("docker run (cleanup)", res, &stderr)
}
