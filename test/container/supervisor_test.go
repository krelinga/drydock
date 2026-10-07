package container_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/supervisor"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestSessionServerInARealContainer runs the supervisor through the real
// `devcontainer exec` and `docker exec` against a real container, with
// fakeclaude as the container's `claude` — never a real remote-control
// against a real account. It asserts the measurement the design rests on and
// the behaviour built on it:
//
//   - the server gets a terminal in the container, and the environment and
//     session it announces are discovered;
//   - Drydock going away (Detach: its terminal closed, its `devcontainer
//     exec` signalled) leaves the server running in the container —
//     signalling the local CLI does not reach it;
//   - the next Drydock's start stops that server first, with SIGTERM (its
//     clean exit is fakeclaude's recorded shutdown, exit 0), and serves again
//     on the same environment;
//   - a stop is SIGTERM in the container, and leaves no server behind.
func TestSessionServerInARealContainer(t *testing.T) {
	needDevcontainer(t)
	ctx := context.Background()
	p := prefix(t)
	root := t.TempDir()
	ws, err := workspace.NewID(time.Now(), sys.CryptoRandom{})
	if err != nil {
		t.Fatal(err)
	}

	// The container's `claude`, its script and state, and a secrets helper
	// that delivers nothing, on a PATH the configuration's remoteEnv sets.
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	f := claudetest.Install(t, claudetest.Script{Version: claudetest.Version, StateDir: filepath.Join(root, "state"),
		RemoteControl: []claudetest.Step{{Mode: claudetest.RCServe}}})
	copyFile(t, f.Path, filepath.Join(bin, "claude"), 0o755)
	copyFile(t, f.Path+".json", filepath.Join(bin, "claude.json"), 0o644)
	os.WriteFile(filepath.Join(bin, "drydock-secrets"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.MkdirAll(filepath.Join(root, "state"), 0o777)
	os.Chmod(filepath.Join(root, "state"), 0o777)
	corpus := claudetest.CorpusRoot(t)

	folder := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(folder, ".devcontainer"), 0o755)
	cfg, _ := json.Marshal(map[string]any{
		"image": image,
		"mounts": []map[string]string{
			{"type": "bind", "source": root, "target": root},
			{"type": "bind", "source": corpus, "target": corpus},
		},
		"remoteEnv": map[string]string{"PATH": bin + ":/usr/local/bin:/usr/bin:/bin"},
	})
	os.WriteFile(filepath.Join(folder, ".devcontainer", "devcontainer.json"), cfg, 0o644)
	if out, err := exec.Command("devcontainer", "up", "--workspace-folder", folder,
		"--id-label", p+".workspace="+ws).CombinedOutput(); err != nil {
		t.Fatalf("devcontainer up: %v\n%s", err, tail(out))
	}

	db, err := store.Open(ctx, filepath.Join(root, "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.ExecContext(ctx, `INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (7, 1, 'krelinga/repo', 'main')`)
	db.ExecContext(ctx, `INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES (?, 7, ?, 'main', 'running')`, ws, folder)
	log := events.New(db.DB, sys.RealClock{})
	containers := manager(p)
	manager := func() *supervisor.Manager {
		return &supervisor.Manager{DB: db.DB, Events: log, Env: sys.Env{Clock: sys.RealClock{}, Random: sys.CryptoRandom{}},
			Runtime: supervisor.ContainerRuntime{Containers: containers, PTY: subproc.Exec{}},
			Spec: func(context.Context, string) (container.SessionSpec, error) {
				return container.SessionSpec{WorkspaceID: ws, Folder: folder,
					RemoteEnv: map[string]string{"CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX": "repo"}}, nil
			},
			Policy: supervisor.Policy{StopTimeout: 10 * time.Second, RegistrationRetry: time.Second, GateTimeout: 60 * time.Second},
			Logf:   t.Logf}
	}
	// last is the newest supervisor.state event and its id.
	last := func() (workspace.SupervisorData, int64) {
		evs, _ := log.ForWorkspace(ctx, ws, 200)
		for _, e := range evs {
			if e.Kind == workspace.KindSupervisor {
				var d workspace.SupervisorData
				json.Unmarshal(e.Data, &d)
				return d, e.ID
			}
		}
		return workspace.SupervisorData{}, 0
	}
	// waitServing waits for a serving written after the event id mark.
	waitServing := func(m *supervisor.Manager, mark int64) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for {
			d, id := last()
			if d.State == "serving" && id > mark {
				return
			}
			if time.Now().After(deadline) {
				lines, _, _ := m.Logs(ws, 0)
				t.Fatalf("not serving: %+v\n%v", d, lines)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	mark := func() int64 { id, _ := log.Latest(ctx); return id }
	alive := func() bool {
		ok, err := containers.SignalSession(ctx, ws, container.SessionAlive, "")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// fakeclaude runs as the container's root, so its report is root's 0600
	// file; open it up before reading it from the host.
	events := func() []claudetest.Event {
		t.Helper()
		ids, err := containers.Find(ctx, ws)
		if err != nil || len(ids) != 1 {
			t.Fatalf("find: %v %v", ids, err)
		}
		docker(t, "exec", ids[0], "chmod", "-R", "a+rwX", filepath.Join(root, "state"))
		return f.Events(t)
	}

	first := manager()
	second := manager()
	defer func() {
		first.Detach(10 * time.Second)
		second.Stop(context.Background(), ws)
		second.Detach(10 * time.Second)
	}()
	m0 := mark()
	if err := first.Start(ctx, ws); err != nil {
		t.Fatal(err)
	}
	waitServing(first, m0)
	var env string
	db.QueryRow(`SELECT coalesce(environment_id, '') FROM workspace WHERE id = ?`, ws).Scan(&env)
	if env != "env_01SWWUTySnsAEuAGMd6azA24" {
		t.Errorf("environment %q", env)
	}
	starts := claudetest.Kind(events(), claudetest.EventStart)
	if len(starts) != 1 || !starts[0].TTY {
		t.Fatalf("starts %+v: the server must have a terminal in the container", starts)
	}

	// Drydock goes away. The server does not.
	first.Detach(30 * time.Second)
	time.Sleep(2 * time.Second)
	if !alive() {
		t.Fatal("the server died with Drydock's terminal; the design says it keeps serving")
	}
	if n := len(claudetest.Kind(events(), claudetest.EventExit)); n != 0 {
		t.Fatalf("%d exits after Drydock went away", n)
	}

	// The next Drydock adopts it: SIGTERM to the stray, then its own.
	m1 := mark()
	if err := second.Start(ctx, ws); err != nil {
		t.Fatal(err)
	}
	waitServing(second, m1)
	evs := events()
	exits := claudetest.Kind(evs, claudetest.EventExit)
	if len(exits) != 1 || exits[0].Code != 0 || exits[0].Invocation != 1 {
		t.Errorf("the stray's exit %+v: want a clean SIGTERM exit of invocation 1", exits)
	}
	if n := len(claudetest.Kind(evs, claudetest.EventStart)); n != 2 {
		t.Errorf("%d starts, want 2", n)
	}

	// A stop is SIGTERM in the container, and leaves nothing running.
	if err := second.Stop(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if alive() {
		t.Error("a server is still running after Stop")
	}
	exits = claudetest.Kind(events(), claudetest.EventExit)
	if len(exits) != 2 || exits[1].Code != 0 {
		t.Errorf("exits %+v: the stop must be a clean SIGTERM exit", exits)
	}
	lines, _, _ := second.Logs(ws, 0)
	var text []string
	for _, l := range lines {
		text = append(text, l.Text)
	}
	if !strings.Contains(strings.Join(text, "\n"), "Environment preserved") {
		t.Errorf("the log has no clean shutdown:\n%s", strings.Join(text, "\n"))
	}
	for _, v := range claudetest.Kind(events(), claudetest.EventViolation) {
		t.Errorf("fakeclaude: %s", v.What)
	}
}

// noLogin stores the identity a test volume really has — no one signed in —
// so the supervisor defers at step 8 rather than run the Feature's real
// `claude remote-control`, which a test must never do (a real binary, the
// real service). It is also the container tier's check that a signed-out
// fleet starts no server: step 8 still hands off, and the supervisor waits.
func noLogin(t *testing.T, db *sql.DB, volume string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO claude_identity (id, volume_name, state) VALUES (1, ?, 'absent')
		ON CONFLICT (id) DO UPDATE SET state = 'absent'`, volume); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, mode); err != nil {
		t.Fatal(fmt.Errorf("copy %s: %w", from, err))
	}
}
