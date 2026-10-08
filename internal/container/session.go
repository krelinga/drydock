package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/krelinga/drydock/internal/subproc"
)

// The session server (design §8): one `claude remote-control` per workspace,
// started by `devcontainer exec` on a terminal Drydock owns, and stopped by a
// signal delivered *inside* the container.
//
// Two measurements on CLI 0.89.0 decide that shape.
//
//   - `devcontainer exec` gives the command a terminal in the container only
//     when its own stdin and stdout are one, and the inner terminal takes the
//     outer one's size; it runs as the remote user, in the workspace folder,
//     and exits with the command's own status. So Drydock's PTY reaches
//     Claude Code as a PTY, which is the case the corpus was recorded in.
//   - **Signalling the `devcontainer exec` process does not reach the
//     command.** SIGTERM to the CLI ends the CLI and leaves the command
//     running in the container, re-parented, for good — Docker does not
//     forward an exec client's signals. A stop that signalled the local
//     process would orphan a server that still holds the folder's
//     registration, and the next start would be refused as "already served
//     by a terminal" for as long as the orphan lived. The same orphan is what
//     a Drydock restart leaves: the server keeps serving, and only its
//     terminal is gone.
//
// So the server records its own pid in the container (RemoteControlPidFile),
// and a stop is `docker exec` of a constant script that signals exactly that
// pid, and only if it is still a `remote-control` process.

// RemoteControlPidFile is where, inside the container, the session server's
// launch script records the server's pid: the shell writes $$ and then execs
// claude, so the pid is claude's. Under /tmp because it must be writable by
// the remote user and must not outlive the container's own lifetime in a way
// that matters — the stop script checks the pid is a remote-control process
// before signalling it, so a stale file after a container restart is a
// "no server" rather than a signal to whatever reused the pid.
const RemoteControlPidFile = "/tmp/drydock-remote-control.pid"

// RemoteControlLaunch is the command the session server's terminal runs:
// `sh -c` of this constant, with the pid file and the capacity as positional
// parameters — never interpolated into the script, so nothing from workspace
// data is shell text.
//
// In order: record the pid; fetch the workspace's secrets into this shell's
// environment, failing the start (`exit 69`) if the broker cannot deliver
// them, so every session the server ever spawns inherits them (§8, §10.3);
// then exec Claude Code in place, keeping the pid. `|| echo exit 69` is the
// env file's own guard: a helper that cannot run prints nothing, and eval of
// nothing would succeed (§10.3).
//
// `sh`, not `bash -l`: the Feature supports Alpine images, which have no bash,
// and `devcontainer exec` already gives the remote user's probed environment,
// so a login shell would only add profile scripts printing into the stream
// the supervisor scrapes. There is no `cd`: exec starts in the workspace
// folder (measured), which is the folder whose trust record the Feature wrote.
const RemoteControlLaunch = `printf '%s\n' "$$" > "$1" && ` +
	`eval "$(drydock-secrets export || echo exit 69)" && ` +
	`exec claude remote-control --spawn worktree --capacity "$2" --verbose`

// remoteControlSignal is run by `docker exec -u 0 … sh -c` with the pid file
// and a signal name (TERM, KILL, or 0 to ask whether it is alive). Exit 3:
// there is no session server — no pid file, or its pid is not a
// remote-control process any more, or it exited before the signal reached
// it. Exit 4: the server is there and the kernel refused the signal (EPERM)
// — the container answered, so it is not Docker failing.
const remoteControlSignal = `f=$1; s=$2
[ -r "$f" ] || exit 3
p=$(cat "$f")
case $p in ''|*[!0-9]*) exit 3;; esac
tr '\000' '\n' < "/proc/$p/cmdline" 2>/dev/null | grep -qx remote-control || exit 3
kill -"$s" "$p" 2>/dev/null && exit 0
[ -d "/proc/$p" ] || exit 3
exit 4`

// ErrSessionSignalRefused: the session server is there, and the kernel in
// its container refused the signal. Docker answered; asking it again will
// not help.
var ErrSessionSignalRefused = errors.New("container: the session server refused the signal")

// ErrSessionSurvivedKill: SIGKILL was sent (or refused) and the session
// server was still there after it. Asking again cannot end it; removing or
// stopping its container does. internal/supervisor returns it from a stop,
// and a workspace stop or delete then carries on to its container step.
var ErrSessionSurvivedKill = errors.New("the session server did not exit after SIGKILL")

// SessionSpec is one workspace's session server.
type SessionSpec struct {
	WorkspaceID string
	// Folder is the clone on the host; OverrideConfig the same
	// --override-config `up` was given (exec reads the configuration too).
	Folder         string
	OverrideConfig string
	// RemoteEnv is passed on the exec itself: `up`'s --remote-env does not
	// reach a later exec (§6, measured). CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX
	// goes here.
	RemoteEnv map[string]string
	// Capacity is --capacity: how many sessions, the pre-created one
	// included, the server will hold (§8).
	Capacity int
	// PidFile overrides RemoteControlPidFile, for a test whose "container"
	// is the host.
	PidFile string
}

func (s SessionSpec) pidFile() string {
	if s.PidFile != "" {
		return s.PidFile
	}
	return RemoteControlPidFile
}

// SessionArgs builds the `devcontainer exec` argv for the session server.
// Exported so the argv — assembled from workspace data, a security surface —
// can be asserted on directly as well as through a fake binary.
func (m Manager) SessionArgs(s SessionSpec) ([]string, error) {
	if !workspaceIDPattern.MatchString(s.WorkspaceID) || !strings.HasPrefix(s.Folder, "/") {
		return nil, errors.New("container: a session server needs a workspace id and an absolute folder")
	}
	if s.Capacity < 1 || s.Capacity > 32 {
		return nil, fmt.Errorf("container: capacity %d is out of range", s.Capacity)
	}
	if !strings.HasPrefix(s.pidFile(), "/") || strings.ContainsAny(s.pidFile(), "\n\x00") {
		return nil, fmt.Errorf("container: pid file %q must be absolute", s.pidFile())
	}
	args := []string{"exec", "--workspace-folder", s.Folder, "--id-label", m.key(LabelWorkspace) + "=" + s.WorkspaceID}
	if s.OverrideConfig != "" {
		if !strings.HasPrefix(s.OverrideConfig, "/") {
			return nil, fmt.Errorf("container: override config %q must be absolute", s.OverrideConfig)
		}
		args = append(args, "--override-config", s.OverrideConfig)
	}
	env, err := remoteEnvArgs(s.RemoteEnv)
	if err != nil {
		return nil, err
	}
	args = append(append(args, env...), "--", "sh", "-c", RemoteControlLaunch, "sh", s.pidFile(), strconv.Itoa(s.Capacity))
	return args, nil
}

// StartSession starts the session server on a fresh terminal cols × rows and
// returns the process and the terminal's master. Its environment is
// Drydock's own (the CLI needs PATH, HOME and Docker's settings); nothing in
// it reaches the container but what --remote-env names.
func (m Manager) StartSession(ctx context.Context, r subproc.PTYRunner, s SessionSpec, cols, rows int) (subproc.Process, *os.File, error) {
	args, err := m.SessionArgs(s)
	if err != nil {
		return nil, nil, err
	}
	// Through the guard, which passes exec's docker commands straight to
	// docker (execve, so the terminal and signals are docker's own).
	dp, err := m.dockerPath(s.Folder)
	if err != nil {
		return nil, nil, err
	}
	return r.StartPTY(ctx, subproc.Cmd{Name: "devcontainer", Args: withDockerPath(args, dp)}, cols, rows)
}

// SessionSignal names the signals SignalSession sends. Alive sends none and
// only asks.
type SessionSignal string

const (
	SessionTerm  SessionSignal = "TERM"
	SessionKill  SessionSignal = "KILL"
	SessionAlive SessionSignal = "0"
)

// SignalSession delivers sig to the workspace's session server inside every
// running container carrying its label — found by label, never a cached id —
// and reports whether a server was there to receive it. No running container
// is no server: a stopped container's processes are gone with it.
func (m Manager) SignalSession(ctx context.Context, workspaceID string, sig SessionSignal, pidFile string) (bool, error) {
	switch sig {
	case SessionTerm, SessionKill, SessionAlive:
	default:
		return false, fmt.Errorf("container: signal %q is not one Drydock sends", sig)
	}
	if pidFile == "" {
		pidFile = RemoteControlPidFile
	}
	ids, err := m.findRunning(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	found := false
	var errs []error
	for _, id := range ids {
		var stderr bytes.Buffer
		res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
			Args:   []string{"exec", "-u", "0", "--", id, "sh", "-c", remoteControlSignal, "sh", pidFile, string(sig)},
			Stderr: limit(&stderr, 16<<10)})
		switch {
		case res.Err != nil:
			errs = append(errs, fmt.Errorf("docker exec: %w", res.Err))
		case res.ExitCode == 0:
			found = true
		case res.ExitCode == 3:
		case res.ExitCode == 4:
			found = true
			errs = append(errs, fmt.Errorf("%w: %s", ErrSessionSignalRefused, sig))
		default:
			errs = append(errs, fmt.Errorf("docker exec: signalling the session server exited %d: %s",
				res.ExitCode, strings.TrimSpace(stderr.String())))
		}
	}
	return found, errors.Join(errs...)
}

// findRunning is Find restricted to running containers.
func (m Manager) findRunning(ctx context.Context, workspaceID string) ([]string, error) {
	if !workspaceIDPattern.MatchString(workspaceID) {
		return nil, fmt.Errorf("container: %q is not a workspace id", workspaceID)
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args: []string{"ps", "--quiet", "--no-trunc", "--filter", "status=running",
			"--filter", "label=" + m.key(LabelWorkspace) + "=" + workspaceID},
		Stdout: limit(&out, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker ps", res, &stderr); err != nil {
		return nil, err
	}
	ids := strings.Fields(out.String())
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return nil, fmt.Errorf("docker ps: %q is not a container id", id)
		}
	}
	return ids, nil
}
