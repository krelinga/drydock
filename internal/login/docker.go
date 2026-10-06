//go:build linux

package login

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/subproc"
)

// LabelLogin is the label a login container carries, valued with its login
// id: `<prefix>.login=<id>`. Never `<prefix>.workspace`, so reconciliation
// never lists one; and not the identity watch's `<prefix>.identity` either,
// because the boot sweep removes everything carrying this label while the
// watch's first check runs beside it.
const LabelLogin = "login"

// MountPoint is where the shared volume appears in the login container: the
// path every workspace mounts it at (container.ClaudeConfigMountPoint, the
// Feature's CLAUDE_CONFIG_DIR), so anything Claude Code records about where
// its config lives is what the workspaces will find.
const MountPoint = container.ClaudeConfigMountPoint

// prepMount is where the ownership helper sees the volume.
const prepMount = "/claude"

// prepScript is the one shell line the ownership helper runs, as root with
// only CHOWN and FOWNER. A volume Drydock's uid already owns is left alone;
// a fresh one — root's and empty, as `docker volume create` leaves it, with
// no image directory to copy an owner from — is given to Drydock's uid, 0700,
// as a workspace's first mount would have done. Anything else is somebody
// else's volume: it prints the owner and exits 4, and nothing is changed.
// Constant: the uid and gid arrive as validated integers in the environment.
const prepScript = `d=` + prepMount + `; o=$(stat -c %u "$d") || exit 5; ` +
	`if [ "$o" = "$DRYDOCK_UID" ]; then exit 0; fi; ` +
	`if [ "$o" = 0 ] && [ -z "$(ls -A "$d")" ]; then chown "$DRYDOCK_UID:$DRYDOCK_GID" "$d" && chmod 0700 "$d" && exit 0; exit 5; fi; ` +
	`echo "$o"; exit 4`

const exitForeignOwner = 4

var (
	imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	pinnedPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)
	containerID    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Volumes makes the shared credential volume: container.Manager's
// EnsureClaudeVolume, which creates it labelled and refuses a foreign or
// non-local one (Spike 00: the refresh lock needs mkdir to be atomic).
type Volumes interface {
	EnsureClaudeVolume(ctx context.Context, name string) (bool, error)
}

// Images is internal/claudeimage's Builder.
type Images interface {
	Ensure(ctx context.Context) (string, error)
}

// DockerLauncher runs the login in a short-lived container (§7.2: "a tiny
// drydock-auth container that mounts only the credential volume").
//
// Why a container and not the host: the host need not have Claude Code, the
// volume's files are the daemon's to reach (internal/identity says why), and a
// container gets exactly the environment it is given — none of §2.1's
// variables can leak in from Drydock's own. Why this container: the Claude
// image the watch already builds (pinned base, exact version), the volume
// mounted read-write for this one job at the workspaces' own path, no other
// mount, no capabilities, a read-only root with a tmpfs HOME, and Drydock's
// own uid, which is the uid the dev container CLI gives every workspace's
// remote user — so the credential Claude Code writes, 0600, is theirs to
// read. Measured on 2.1.289: its one os.userInfo() call is inside a try, and
// it runs as a uid with no passwd entry.
//
// The PTY is Drydock's (internal/pty), handed to `docker run -it` as its
// terminal: the CLI makes the container's terminal from it, sizes it to it,
// and relays the stream both ways.
type DockerLauncher struct {
	// Run runs the short docker commands; PTY starts the one on a terminal
	// (subproc.Exec in production, the same PTY start the supervisor
	// uses). Nil PTY is subproc.Exec{}.
	Run     subproc.Runner
	PTY     subproc.PTYRunner
	Volumes Volumes
	Image   Images
	// PrepImage runs the ownership helper: the cleanup helper's busybox,
	// pinned by digest.
	PrepImage   string
	Volume      string
	LabelPrefix string
	// UID and GID are Drydock's own, which the workspaces' remote user has.
	UID, GID int
	// Entrypoint is "claude" unless a test says otherwise.
	Entrypoint string
	// Extra is `docker run` options placed before the image. A test's seam
	// — fakeclaude bind-mounted in — and empty in production.
	Extra []string
}

// Launch implements Launcher.
func (d DockerLauncher) Launch(ctx context.Context, id string, cols, rows int) (*Proc, error) {
	if !ValidID(id) {
		return nil, &LaunchError{Problem: ProblemStart, Detail: "invalid login id"}
	}
	if _, err := d.Volumes.EnsureClaudeVolume(ctx, d.Volume); err != nil {
		return nil, &LaunchError{Problem: ProblemVolume, Detail: err.Error()}
	}
	if err := d.prepare(ctx, id); err != nil {
		return nil, err
	}
	img, err := d.Image.Ensure(ctx)
	if err != nil {
		return nil, &LaunchError{Problem: ProblemImage, Detail: err.Error()}
	}
	args, err := d.RunArgs(img, id)
	if err != nil {
		return nil, &LaunchError{Problem: ProblemStart, Detail: err.Error()}
	}
	if ctx.Err() != nil {
		return nil, &LaunchError{Problem: ProblemStart, Detail: ctx.Err().Error()}
	}
	r := d.PTY
	if r == nil {
		r = subproc.Exec{}
	}
	// Env nil: docker inherits Drydock's own, as every other docker call
	// does; the container gets only the --env RunArgs gives it.
	p, err := StartProc(r, subproc.Cmd{Name: "docker", Args: args}, cols, rows)
	if err != nil {
		return nil, &LaunchError{Problem: ProblemDocker, Detail: err.Error()}
	}
	return p, nil
}

// prepare gives a fresh volume to Drydock's uid, and refuses one another uid
// owns, before Claude Code writes a file into it that the workspaces could
// not read.
func (d DockerLauncher) prepare(ctx context.Context, id string) error {
	args, err := d.PrepArgs(id)
	if err != nil {
		return &LaunchError{Problem: ProblemVolume, Detail: err.Error()}
	}
	var out bytes.Buffer
	res := d.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: args, Stdout: &capped{buf: &out, max: 64}})
	if res.Err != nil {
		return &LaunchError{Problem: ProblemDocker, Detail: res.Err.Error()}
	}
	switch res.ExitCode {
	case 0:
		return nil
	case exitForeignOwner:
		owner := strings.TrimSpace(out.String())
		if _, err := strconv.Atoi(owner); err != nil {
			owner = "another uid"
		} else {
			owner = "uid " + owner
		}
		return &LaunchError{Problem: ProblemVolumeOwner, Detail: "volume owned by " + owner,
			Message: fmt.Sprintf("The login could not start: the shared Claude volume belongs to %s, and Drydock runs as uid %d. Every workspace runs as Drydock's uid and must be able to read the login.", owner, d.UID)}
	}
	return &LaunchError{Problem: ProblemVolume, Detail: fmt.Sprintf("the ownership helper exited %d", res.ExitCode)}
}

func (d DockerLauncher) check() error {
	if !config.ValidVolumeName(d.Volume) {
		return fmt.Errorf("%q is not a volume name", d.Volume)
	}
	if d.LabelPrefix == "" {
		return errors.New("no label prefix")
	}
	// Root would write a credential no workspace's user could read, and the
	// Feature refuses a root remote user.
	if d.UID <= 0 || d.GID < 0 {
		return fmt.Errorf("uid %d gid %d: Drydock must not run the login as root", d.UID, d.GID)
	}
	return nil
}

// PrepArgs is the ownership helper's argv, exported so it can be asserted.
func (d DockerLauncher) PrepArgs(id string) ([]string, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	if !pinnedPattern.MatchString(d.PrepImage) {
		return nil, fmt.Errorf("%q is not pinned by digest", d.PrepImage)
	}
	return []string{"run", "--rm",
		"--label", d.label(id),
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER",
		"--security-opt", "no-new-privileges",
		"--user", "0:0",
		"--mount", "type=volume,source=" + d.Volume + ",target=" + prepMount,
		"--env", "DRYDOCK_UID=" + strconv.Itoa(d.UID),
		"--env", "DRYDOCK_GID=" + strconv.Itoa(d.GID),
		"--entrypoint", "sh",
		d.PrepImage, "-c", prepScript,
	}, nil
}

// RunArgs is the login container's argv, exported so it can be asserted on
// directly: it mounts the login every workspace runs on, read-write. The code
// is never in it — it is typed into the terminal.
func (d DockerLauncher) RunArgs(image, id string) ([]string, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	if !ValidID(id) {
		return nil, fmt.Errorf("%q is not a login id", id)
	}
	if !imageIDPattern.MatchString(image) && !pinnedPattern.MatchString(image) {
		return nil, fmt.Errorf("%q is neither an image id nor a reference pinned by digest", image)
	}
	entry := d.Entrypoint
	if entry == "" {
		entry = "claude"
	}
	args := []string{"run", "--rm", "--interactive", "--tty",
		"--label", d.label(id),
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", strconv.Itoa(d.UID) + ":" + strconv.Itoa(d.GID),
		"--mount", "type=volume,source=" + d.Volume + ",target=" + MountPoint,
		// The whole environment, given: none of §2.1's variables can
		// reach it, and the auto-updater is off as in the Feature.
		"--env", "CLAUDE_CONFIG_DIR=" + MountPoint,
		"--env", "HOME=/tmp",
		"--env", "DISABLE_AUTOUPDATER=1",
	}
	args = append(args, d.Extra...)
	return append(args, "--entrypoint", entry, image, "auth", "login", "--claudeai"), nil
}

func (d DockerLauncher) label(id string) string { return d.LabelPrefix + "." + LabelLogin + "=" + id }

// Remove implements Launcher: every container carrying this login's label,
// the ownership helper's included, by full id.
func (d DockerLauncher) Remove(ctx context.Context, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%q is not a login id", id)
	}
	ids, err := d.list(ctx, d.label(id))
	if err != nil {
		return err
	}
	return d.rm(ctx, ids)
}

// Sweep implements Launcher: every login container but keep's.
func (d DockerLauncher) Sweep(ctx context.Context, keep string) (int, error) {
	all, err := d.list(ctx, d.LabelPrefix+"."+LabelLogin)
	if err != nil {
		return 0, err
	}
	spare := map[string]bool{}
	if keep != "" && ValidID(keep) {
		kept, err := d.list(ctx, d.label(keep))
		if err != nil {
			return 0, err
		}
		for _, k := range kept {
			spare[k] = true
		}
	}
	var gone []string
	for _, c := range all {
		if !spare[c] {
			gone = append(gone, c)
		}
	}
	return len(gone), d.rm(ctx, gone)
}

func (d DockerLauncher) list(ctx context.Context, filter string) ([]string, error) {
	if d.LabelPrefix == "" {
		return nil, errors.New("login: no label prefix")
	}
	var out bytes.Buffer
	res := d.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args:   []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=" + filter},
		Stdout: &capped{buf: &out, max: 1 << 20}})
	if res.Err != nil {
		return nil, fmt.Errorf("docker ps: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker ps exited %d", res.ExitCode)
	}
	ids := strings.Fields(out.String())
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return nil, fmt.Errorf("docker ps: %q is not a container id", id)
		}
	}
	return ids, nil
}

func (d DockerLauncher) rm(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	res := d.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"rm", "--force", "--"}, ids...)})
	if res.Err != nil {
		return fmt.Errorf("docker rm: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker rm exited %d", res.ExitCode)
	}
	return nil
}

// capped keeps the first max bytes.
type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}
