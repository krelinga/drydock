//go:build linux

package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const (
	wsID     = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	fakeCID  = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	envFixed = "env_01SWWUTySnsAEuAGMd6azA24" // the corpus's environment
	sessFix  = "session_01AZLp4a8noWuZ5eRHrecDgz"
)

// rig is one supervisor against fake `devcontainer` and `docker` binaries.
// The fakes are where the container would be: `devcontainer exec` runs the
// real launch script with sh on the host, against fakeclaude (or a stand-in
// `claude`), on the PTY the supervisor owns; `docker exec` runs the real
// signal script on the host. So the argv, the script, the pid file and the
// SIGTERM-first rule are all the production ones; only the container is not.
type rig struct {
	t      *testing.T
	m      *Manager
	db     *store.DB
	log    *events.Log
	dir    string
	bin    string // where `claude` and `drydock-secrets` are
	docker string // the fake docker's argv log
	devc   string // the fake devcontainer's argv log
	fake   *claudetest.Fake
}

type rigOpt func(*rig, *Policy)

func newRig(t *testing.T, opts ...rigOpt) *rig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, `INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (7, 1, 'krelinga/repo', 'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspace (id, repository_id, host_path, branch, state, created_at)
		VALUES (?, 7, ?, 'main', 'running', '2026-10-06T00:00:00Z')`, wsID, filepath.Join(dir, "repo")); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, db: db, dir: dir, bin: filepath.Join(dir, "bin"),
		docker: filepath.Join(dir, "docker.log"), devc: filepath.Join(dir, "devcontainer.log")}
	r.log = events.New(db.DB, sys.RealClock{})
	if err := os.MkdirAll(r.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	r.script("drydock-secrets", "exit 0\n")
	fakes := filepath.Join(dir, "fakes")
	os.MkdirAll(fakes, 0o755)
	writeExec(t, filepath.Join(fakes, "devcontainer"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
PATH=%q:/usr/bin:/bin
export PATH
exec "$@"
`, r.devc, r.bin))
	running := filepath.Join(dir, "container-running")
	os.WriteFile(running, nil, 0o600)
	writeExec(t, filepath.Join(fakes, "docker"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1" in
ps) [ -e %q ] && echo %s; exit 0;;
exec) while [ "$1" != "--" ]; do shift; done; shift; shift; exec "$@";;
esac
exit 99
`, r.docker, running, fakeCID))
	res := subproc.FixedResolver{"devcontainer": filepath.Join(fakes, "devcontainer"), "docker": filepath.Join(fakes, "docker")}
	run := subproc.Exec{Resolver: res}
	p := Policy{Capacity: 4, Backoff: 20 * time.Millisecond, BackoffMax: 80 * time.Millisecond, Budget: 6,
		BudgetWindow: time.Minute, RegistrationRetry: 100 * time.Millisecond, GateTimeout: 5 * time.Second,
		StopTimeout: 2 * time.Second, KillWait: time.Second, StopPoll: 50 * time.Millisecond, Cols: 200, Rows: 50,
		HeartbeatEvery: time.Second}
	for _, o := range opts {
		o(r, &p)
	}
	r.m = &Manager{DB: db.DB, Events: r.log, Env: sys.Env{Clock: sys.RealClock{}, Random: sys.CryptoRandom{}},
		Runtime: ContainerRuntime{Containers: container.Manager{Run: run, LabelPrefix: "drytest"}, PTY: run},
		Spec: func(context.Context, string) (container.SessionSpec, error) {
			return container.SessionSpec{WorkspaceID: wsID, Folder: filepath.Join(dir, "repo"),
				RemoteEnv: map[string]string{"CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX": "repo"}}, nil
		},
		PidFile: filepath.Join(dir, "rc.pid"), Policy: p, Logf: t.Logf}
	t.Cleanup(func() {
		r.m.Stop(context.Background(), wsID)
		r.m.Detach(5 * time.Second)
	})
	return r
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// script puts an executable in the "container"'s PATH.
func (r *rig) script(name, body string) {
	writeExec(r.t, filepath.Join(r.bin, name), "#!/bin/sh\n"+body)
}

// claude installs fakeclaude with these remote-control steps as the
// container's `claude`.
func (r *rig) claude(steps ...claudetest.Step) *claudetest.Fake {
	r.t.Helper()
	f := claudetest.Install(r.t, claudetest.Script{Version: claudetest.Version, RemoteControl: steps})
	if err := os.Link(f.Path, filepath.Join(r.bin, "claude")); err != nil {
		r.t.Fatal(err)
	}
	// fakeclaude reads its script beside the path it was run as.
	b, err := os.ReadFile(f.Path + ".json")
	if err != nil {
		r.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.bin, "claude.json"), b, 0o600)
	r.fake = f
	return f
}

func dur(d time.Duration) claudetest.Duration { return claudetest.Duration(d) }

func (r *rig) start() {
	r.t.Helper()
	if err := r.m.Start(context.Background(), wsID); err != nil {
		r.t.Fatal(err)
	}
}

// row is the supervisor row's state and restart count.
func (r *rig) row() (State, int) {
	var st string
	var n int
	err := r.db.QueryRow(`SELECT state, restart_count FROM supervisor WHERE workspace_id = ?`, wsID).Scan(&st, &n)
	if err == sql.ErrNoRows {
		return "", 0
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return State(st), n
}

// sups are the supervisor.state events' data, in order.
func (r *rig) sups() []workspace.SupervisorData {
	evs, err := r.log.ForWorkspace(context.Background(), wsID, 1000)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []workspace.SupervisorData
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != workspace.KindSupervisor {
			continue
		}
		var d workspace.SupervisorData
		json.Unmarshal(evs[i].Data, &d)
		out = append(out, d)
	}
	return out
}

func (r *rig) last() workspace.SupervisorData {
	s := r.sups()
	if len(s) == 0 {
		return workspace.SupervisorData{}
	}
	return s[len(s)-1]
}

// waitFor polls until ok or d passes.
func (r *rig) waitFor(d time.Duration, what string, ok func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			r.t.Fatalf("not within %v: %s; last supervisor event %+v; log:\n%s", d, what, r.last(), r.logText())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *rig) waitState(st State, reason Reason) {
	r.t.Helper()
	r.waitFor(10*time.Second, fmt.Sprintf("state %s/%s", st, reason), func() bool {
		l := r.last()
		return l.State == string(st) && (reason == "" || l.Reason == string(reason))
	})
}

func (r *rig) logText() string {
	lines, _, _ := r.m.Logs(wsID, 0)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

func (r *rig) dockerLog() string {
	b, _ := os.ReadFile(r.docker)
	return string(b)
}

func (r *rig) invocations() int {
	if r.fake == nil {
		return 0
	}
	return len(claudetest.Kind(r.fake.Events(r.t), claudetest.EventStart))
}

func (r *rig) sessions() []string {
	rows, err := r.db.Query(`SELECT id FROM rc_session ORDER BY first_seen_at, id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (r *rig) environment() string {
	var env sql.NullString
	r.db.QueryRow(`SELECT environment_id FROM workspace WHERE id = ?`, wsID).Scan(&env)
	return env.String
}

// alive reports whether a process with this pid is running (and not a zombie).
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	return len(f) > 2 && f[2] != "Z"
}

func (r *rig) pid() int {
	b, err := os.ReadFile(filepath.Join(r.dir, "rc.pid"))
	if err != nil {
		return 0
	}
	var p int
	fmt.Sscan(string(b), &p)
	return p
}
