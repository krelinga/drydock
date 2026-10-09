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
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
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

var (
	imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	pinnedPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)
	containerID    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Volumes makes the shared credential volume: container.Manager's
// EnsureClaudeVolume, which creates it labelled, refuses a foreign or
// non-local one (Spike 00: the refresh lock needs mkdir to be atomic), gives
// an empty one to Drydock's uid — the uid this login runs as — and refuses
// one another uid has written to (container.ErrVolumeOwner). §6 step 4 runs
// the same call, so a login and a create can never disagree about whose the
// volume is.
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
	Run         subproc.Runner
	PTY         subproc.PTYRunner
	Volumes     Volumes
	Image       Images
	Volume      string
	LabelPrefix string
	// UID and GID are Drydock's own, which the workspaces' remote user has.
	UID, GID int
	// Entrypoint is "claude" unless a test says otherwise.
	Entrypoint string
	// Extra is `docker run` options placed before the image. A test's seam
	// — fakeclaude bind-mounted in — and empty in production.
	Extra []string
	// RemoveSettle bounds Remove's wait for a killed CLI's container; zero
	// is DefaultRemoveSettle.
	RemoveSettle time.Duration
	// Clock is what RemoveSettle and its relisting are measured on; nil is
	// the real clock.
	Clock sys.Clock
}

// Launch implements Launcher.
func (d DockerLauncher) Launch(ctx context.Context, id string, cols, rows int) (*Proc, error) {
	if !ValidID(id) {
		return nil, &LaunchError{Problem: ProblemStart, Detail: "invalid login id"}
	}
	if err := d.check(); err != nil {
		return nil, &LaunchError{Problem: ProblemStart, Detail: err.Error()}
	}
	if _, err := d.Volumes.EnsureClaudeVolume(ctx, d.Volume); err != nil {
		var oe *container.VolumeOwnerError
		if errors.As(err, &oe) {
			return nil, &LaunchError{Problem: ProblemVolumeOwner, Detail: err.Error(),
				Message: fmt.Sprintf("The login could not start: the shared Claude volume holds files that belong to %s, and Drydock runs as uid %d. Every workspace runs as Drydock's uid and must be able to read the login.", oe.OwnerName(), d.UID)}
		}
		return nil, &LaunchError{Problem: ProblemVolume, Detail: err.Error()}
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

// DefaultRemoveSettle is how long Remove keeps looking for the container of
// a killed `docker run` that is not listed yet. A `docker run` killed during
// its create leaves the create to finish without it, and the container is
// listed only then: on an idle Docker 29.8.2, 9 kills in 80 left a
// container that appeared 29–95 ms after the client was gone. Whatever lands
// later still goes, at the next login's sweep.
const DefaultRemoveSettle = 3 * time.Second

// Remove implements Launcher: every container carrying this login's label,
// by full id. When the CLI was killed and nothing is listed yet, it keeps
// looking until RemoveSettle has passed, since the CLI's create may still be
// landing; once something is listed it is removed, and that is the one
// container the CLI's one create could make.
func (d DockerLauncher) Remove(ctx context.Context, id string, killed bool) error {
	if !ValidID(id) {
		return fmt.Errorf("%q is not a login id", id)
	}
	settle := d.RemoveSettle
	if settle <= 0 {
		settle = DefaultRemoveSettle
	}
	clock := d.Clock
	if clock == nil {
		clock = sys.RealClock{}
	}
	settled, stopSettle := sys.NewTimer(clock, settle)
	defer stopSettle()
	over := false
	for {
		ids, err := d.list(ctx, d.label(id))
		if err != nil {
			return err
		}
		if len(ids) > 0 || !killed || over {
			return d.rm(ctx, ids)
		}
		poll, stopPoll := sys.NewTimer(clock, removePoll)
		select {
		case <-ctx.Done():
			stopPoll()
			return ctx.Err()
		case <-settled:
			// One last look, then give up: what lands later goes at the
			// next login's sweep.
			over = true
		case <-poll:
		}
		stopPoll()
	}
}

// removePoll is Remove's interval while it waits for a container to list.
const removePoll = 50 * time.Millisecond

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
