package provision

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/workspace"
)

// Step 8 hands the running workspace to the supervisor, once, after the
// probe; a supervisor that refuses fails the step and leaves the workspace
// running, since the container is fine (§6).
func TestStepEightHandsOffToTheSupervisor(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e := newEnv(t)
		e.wire(t)
		var got []string
		e.p.StartSupervisor = func(_ context.Context, w workspace.Workspace) error {
			got = append(got, string(w.State))
			if fail {
				return errors.New("supervisor: shutting down")
			}
			return nil
		}
		v := e.create(t, alpha, "")
		if len(got) != 1 || got[0] != string(workspace.Running) {
			t.Fatalf("fail=%v: StartSupervisor saw %v; want one call on a running workspace", fail, got)
		}
		if v.State != workspace.Running {
			t.Errorf("fail=%v: state %s; step 8 never fails the workspace", fail, v.State)
		}
		st := v.Steps[workspace.StepSessionServer]
		if fail && (st.Status != "failed" || !strings.Contains(st.Detail, "session supervisor")) {
			t.Errorf("a refused hand-off reads %+v", st)
		}
		if !fail && (st.Status != "done" || !strings.Contains(st.Detail, "Handed to the session supervisor")) {
			t.Errorf("control: %+v", st)
		}
	}
}

// SessionSpec decides the override as step 3 does, passes the remote env on
// the exec, and names sessions after the repository.
func TestSessionSpec(t *testing.T) {
	e := newEnv(t)
	e.wire(t)
	v := e.create(t, alpha, "")
	s, err := e.p.SessionSpec(context.Background(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.OverrideConfig != "" {
		t.Errorf("a repository with its own config got override %q", s.OverrideConfig)
	}
	if s.Folder != filepath.Join(e.root, v.ID, "repo") || s.WorkspaceID != v.ID {
		t.Errorf("spec %+v", s)
	}
	want := map[string]string{"DRYDOCK_WORKSPACE": v.ID, "DRYDOCK_REPO": "krelinga/alpha",
		"CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX": "alpha"}
	for k, val := range want {
		if s.RemoteEnv[k] != val {
			t.Errorf("remote env %s = %q, want %q", k, s.RemoteEnv[k], val)
		}
	}
}
