package container

import (
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
