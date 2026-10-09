package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The session server's argv: the workspace's label, the override config when
// there is one, the remote env on the exec itself, and the launch script as
// one constant argument with the pid file and capacity as positional
// parameters — never interpolated into the script.
func TestSessionArgs(t *testing.T) {
	m := Manager{LabelPrefix: "dd"}
	args, err := m.SessionArgs(SessionSpec{WorkspaceID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Folder: "/srv/ws/x/repo",
		OverrideConfig: "/srv/ws/x/.drydock/devcontainer.json", Capacity: 4,
		RemoteEnv: map[string]string{"CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX": "repo; rm -rf /"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--workspace-folder", "/srv/ws/x/repo", "--id-label", "dd.workspace=01JAAAAAAAAAAAAAAAAAAAAAAA",
		"--override-config", "/srv/ws/x/.drydock/devcontainer.json",
		"--remote-env", "CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX=repo; rm -rf /",
		"--", "sh", "-c", RemoteControlLaunch, "sh", RemoteControlPidFile, "4"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv\n got %q\nwant %q", args, want)
	}
	if !strings.Contains(RemoteControlLaunch, `eval "$(drydock-secrets export || echo exit 69)"`) {
		t.Error("the launch does not fetch the secrets through the fail-closed prelude")
	}
	if !strings.Contains(RemoteControlLaunch, "exec claude remote-control --spawn worktree --capacity \"$2\" --verbose") {
		t.Error("the launch is not §8's invocation")
	}
	for _, bad := range []SessionSpec{
		{WorkspaceID: "nope", Folder: "/a", Capacity: 4},
		{WorkspaceID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Folder: "rel", Capacity: 4},
		{WorkspaceID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Folder: "/a", Capacity: 0},
		{WorkspaceID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Folder: "/a", Capacity: 4, OverrideConfig: "rel.json"},
		{WorkspaceID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Folder: "/a", Capacity: 4, RemoteEnv: map[string]string{"A=B": "x"}},
	} {
		if _, err := m.SessionArgs(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// The signal script's exit 4 — the server is there and refused the signal —
// is decided by the same test as the signal itself: the pid must still be a
// remote-control process after the kill failed. A pid that exited and was
// reused in that instant is "no server" (exit 3), never a server refusing a
// signal, which a stop reports as one that survived SIGKILL. The process
// table cannot be raced on demand, so `grep` on the script's PATH stands in
// for it: the first look says remote-control and the second answers as the
// case says, and the kill fails because the signal is not one kill knows
// (the branch is the same whatever made it fail, and this way it does not
// depend on who the test runs as). The control is a second look that still
// says remote-control: exit 4.
func TestTheSignalScriptRechecksBeforeSayingRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		second int // the second look's exit: 0 still remote-control
		want   int
	}{
		{"control: still a remote-control", 0, 4},
		{"the pid is something else now", 1, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "rc.pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			count := filepath.Join(dir, "looks")
			grep := fmt.Sprintf("#!/bin/sh\nn=$(cat %q 2>/dev/null || echo 0); n=$((n+1)); echo $n > %q\n"+
				"[ $n -eq 1 ] && exit 0\nexit %d\n", count, count, c.second)
			if err := os.WriteFile(filepath.Join(dir, "grep"), []byte(grep), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", remoteControlSignal, "sh", pidFile, "NOSUCHSIGNAL")
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != c.want {
				t.Errorf("exit %d, want %d: %s", code, c.want, out)
			}
			b, _ := os.ReadFile(count)
			if strings.TrimSpace(string(b)) != "2" {
				t.Errorf("the script looked %q times, want twice", strings.TrimSpace(string(b)))
			}
		})
	}
}

// A paused container carrying the workspace's label is not "no server": its
// processes are frozen, and Docker refuses to exec into it (measured, Docker
// 29.8.2: `docker ps --filter status=running` does not list it, and `docker
// exec` exits 1, "is paused, unpause the container before exec"). So
// SignalSession says ErrSessionContainerPaused and execs nothing into it.
// The control is the same container running, where the script's exit 3 is
// no server and no error.
func TestSignalSessionSaysAPausedContainerIsPaused(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			status := "running"
			if paused {
				status = "paused"
			}
			body := `case "$*" in
*status=` + status + `*) echo ` + idA + `;;
ps*) ;;
exec*) [ ` + status + ` = paused ] && { echo "Error response from daemon: Container ` + idA + ` is paused, unpause the container before exec" >&2; exit 1; }; exit 3;;
esac`
			run, dir := fakes(t, map[string]string{"docker": body})
			m := Manager{Run: run, LabelPrefix: "dd"}
			found, err := m.SignalSession(context.Background(), wsID, SessionAlive, "")
			execs := 0
			for _, a := range argv(t, dir, "docker") {
				if a == "exec" {
					execs++
				}
			}
			if !paused {
				if found || err != nil || execs != 1 {
					t.Errorf("control: found %v, err %v, %d execs; want no server, no error, one exec", found, err, execs)
				}
				return
			}
			if !errors.Is(err, ErrSessionContainerPaused) {
				t.Errorf("err %v, want ErrSessionContainerPaused", err)
			}
			if execs != 0 {
				t.Errorf("%d execs into a paused container", execs)
			}
		})
	}
}
