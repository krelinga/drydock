package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/krelinga/drydock/internal/ephemeral"
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
// workspace id: ephemeral.Cleanup's. Deliberately not LabelWorkspace:
// reconciliation lists containers by <prefix>.workspace, so a helper is never
// mistaken for a workspace's container — never adopted, and never given a
// row (internal/ephemeral refuses an argv that names it).
const LabelCleanup = string(ephemeral.Cleanup)

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
	label, err := ephemeral.Label(m.LabelPrefix, ephemeral.Cleanup, workspaceID)
	if err != nil {
		return nil, err
	}
	return []string{"run", "--rm",
		"--label", label,
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
// helper, run as an ephemeral helper (internal/ephemeral): what an earlier
// attempt left behind — Drydock killed while it ran, and the daemon never got
// to --rm — is removed by its label first, so a retried delete never runs
// two, and whatever carries the label is removed again on every way the run
// ends.
//
// Everything before the helper's own `docker run` that fails is
// ErrCleanupNotRun, so the caller can tell "no helper ran" from "a helper
// ran and failed".
func (m Manager) RemoveContents(ctx context.Context, workspaceID, dir string) error {
	args, err := m.CleanupArgs(workspaceID, dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupNotRun, err)
	}
	var stderr bytes.Buffer
	res, err := m.helper(ephemeral.Cleanup, workspaceID).Run(ctx, subproc.Cmd{Name: "docker", Args: args,
		Stdout: limit(&bytes.Buffer{}, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupNotRun, err)
	}
	return failed("docker run (cleanup)", res, &stderr)
}

// helper is an ephemeral helper of kind k under this manager's prefix,
// runner and clock.
func (m Manager) helper(k ephemeral.Kind, value string) ephemeral.Helper {
	return ephemeral.Helper{Docker: m.Run, Prefix: m.LabelPrefix, Kind: k, Value: value, Clock: m.Clock, Logf: m.Logf}
}

// SweepHelpers is boot's sweep of every helper kind (ephemeral.SweepAll):
// every container carrying one of this prefix's helper labels, except one
// that also carries the workspace label (reconciliation's), one a holder in
// this process is running, and what skip spares. It returns what it removed.
func (m Manager) SweepHelpers(ctx context.Context, skip func(ephemeral.Found) bool) ([]ephemeral.Found, error) {
	return ephemeral.SweepAll(ctx, m.Run, m.LabelPrefix, skip)
}
