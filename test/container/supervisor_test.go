package container_test

import (
	"bytes"
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
	"github.com/krelinga/drydock/internal/life"
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
//   - Drydock going away (its supervisor group stopped: its terminal
//     closed, its `devcontainer exec` signalled) leaves the server running in the container —
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
	var upOut bytes.Buffer
	if res := (boundedRunner{Inner: subproc.Exec{}}).Run(ctx, subproc.Cmd{Name: "devcontainer",
		Args:   []string{"up", "--workspace-folder", folder, "--id-label", p + ".workspace=" + ws},
		Stdout: &upOut, Stderr: &upOut}); res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("devcontainer up: %v (exit %d)\n%s", res.Err, res.ExitCode, tail(upOut.Bytes()))
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
	// manager is one Drydock's supervisor, in a group of its own: stopping
	// and waiting for it (detach) is that Drydock going away.
	manager := func() (*supervisor.Manager, func(time.Duration)) {
		m := &supervisor.Manager{DB: db.DB, Events: log, Env: sys.Env{Clock: sys.RealClock{}, Random: sys.CryptoRandom{}},
			Runtime: supervisor.ContainerRuntime{Containers: containers, PTY: subproc.Exec{}},
			Spec: func(context.Context, string) (container.SessionSpec, error) {
				return container.SessionSpec{WorkspaceID: ws, Folder: folder,
					RemoteEnv: map[string]string{"CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX": "repo"}}, nil
			},
			Policy: supervisor.Policy{StopTimeout: 10 * time.Second, RegistrationRetry: time.Second, GateTimeout: 60 * time.Second},
			Logf:   t.Logf}
		g := life.NewGroup(context.Background())
		if err := m.RunIn(g); err != nil {
			t.Fatal(err)
		}
		return m, func(bound time.Duration) {
			if late := g.Wait(time.After(bound)); late != nil {
				t.Errorf("still running after shutdown: %v", late)
			}
		}
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

	first, detachFirst := manager()
	second, detachSecond := manager()
	defer func() {
		detachFirst(10 * time.Second)
		second.Stop(context.Background(), ws)
		detachSecond(10 * time.Second)
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
	detachFirst(30 * time.Second)
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

	// A restart from a serving card — the one path a Drydock holding the
	// server's terminal takes — leaves exactly one server, and it is the new
	// one. Killing a `docker exec` client does not end the process it
	// started, so Drydock's own end closing proves nothing; this counts the
	// servers in the container itself.
	servers := func() []string {
		t.Helper()
		ids, err := containers.Find(ctx, ws)
		if err != nil || len(ids) != 1 {
			t.Fatalf("find: %v %v", ids, err)
		}
		out, err := exec.Command("docker", "exec", "-u", "0", ids[0], "sh", "-c",
			`for p in /proc/[0-9]*; do tr '\000' '\n' < "$p/cmdline" 2>/dev/null | grep -qx remote-control && echo "${p#/proc/}"; done; true`).CombinedOutput()
		if err != nil {
			t.Fatalf("listing the servers: %v\n%s", err, out)
		}
		return strings.Fields(string(out))
	}
	pidNow := func() string {
		t.Helper()
		ids, _ := containers.Find(ctx, ws)
		out, err := exec.Command("docker", "exec", "-u", "0", ids[0], "cat", container.RemoteControlPidFile).Output()
		if err != nil {
			t.Fatalf("reading the pid file: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	before := servers()
	if len(before) != 1 || before[0] != pidNow() {
		t.Fatalf("before the restart: servers %v, pid file %s", before, pidNow())
	}
	m2 := mark()
	if err := second.Restart(ctx, ws); err != nil {
		t.Fatal(err)
	}
	waitServing(second, m2)
	after := servers()
	if len(after) != 1 || after[0] == before[0] || after[0] != pidNow() {
		t.Errorf("after the restart: servers %v (before %v), pid file %s: want exactly one, the new one", after, before, pidNow())
	}

	// A stop is SIGTERM in the container, and leaves nothing running.
	if err := second.Stop(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if alive() {
		t.Error("a server is still running after Stop")
	}
	exits = claudetest.Kind(events(), claudetest.EventExit)
	if len(exits) != 3 || exits[1].Code != 0 || exits[2].Code != 0 {
		t.Errorf("exits %+v: the restart and the stop must each be a clean SIGTERM exit", exits)
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
