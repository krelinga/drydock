package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/reconcile"
	"github.com/krelinga/drydock/internal/workspace"
)

const testPrefix = "drydock.test.provision"

// fakeDocker is a docker stand-in holding its containers in a file, one
// "<id> <workspace> <status>" line each, so up, ps, inspect, stop and rm all
// see one world. It records argv beside the devcontainer fake's, first line
// "docker". A file docker-fail-<subcommand> in dir makes that subcommand
// fail; docker-sticky makes rm report success and remove nothing.
func fakeDocker(dir string) string {
	return `#!/bin/sh
dir='` + dir + `'
st="$dir/containers"
{ echo docker; printf '%s\n' "$@"; echo @@; } >> "$dir/argv"
touch "$st"
if [ -e "$dir/docker-fail-$1" ]; then echo "docker $1: the daemon said no" >&2; exit 1; fi
case "$1" in
ps)
  ws=
  for a in "$@"; do
    case "$a" in label=*.workspace=*) ws=${a#label=*.workspace=} ;; esac
  done
  if [ -n "$ws" ]; then awk -v ws="$ws" '$2==ws {print $1}' "$st"; else awk '{print $1}' "$st"; fi ;;
inspect)
  shift 3
  printf '['
  sep=
  for id; do
    printf '%s' "$sep"
    awk -v id="$id" -v p='` + testPrefix + `' '$1==id {printf "{\"Id\":\"%s\",\"State\":{\"Status\":\"%s\",\"Running\":%s},\"Config\":{\"Labels\":{\"%s.workspace\":\"%s\",\"%s.repository-id\":\"101\",\"%s.repo\":\"krelinga/alpha\",\"%s.branch\":\"main\"}}}", $1, $3, ($3=="running"?"true":"false"), p, $2, p, p, p}' "$st"
    sep=,
  done
  printf ']\n' ;;
stop)
  while [ "$1" != -- ]; do shift; done; shift
  for id; do awk -v id="$id" '{ if ($1==id) $3="exited"; print }' "$st" > "$st.t" && mv "$st.t" "$st"; done ;;
rm)
  [ -e "$dir/docker-sticky" ] && exit 0
  while [ "$1" != -- ]; do shift; done; shift
  for id; do awk -v id="$id" '$1!=id' "$st" > "$st.t" && mv "$st.t" "$st"; done ;;
*) exit 64 ;;
esac
`
}

// registeringUp is an up body that behaves as the real one does about
// containers: it finds the workspace's container by its id-label and
// reattaches to it, or makes one — and with --remove-existing-container it
// removes the old one first, so a rebuild gets a new id.
func registeringUp(dir string) string {
	return `ws=; prev=
for a in "$@"; do
  if [ "$prev" = --id-label ]; then case "$a" in *.workspace=*) ws=${a#*.workspace=} ;; esac; fi
  prev=$a
done
st='` + dir + `/containers'; touch "$st"
case " $* " in *" --remove-existing-container "*) awk -v ws="$ws" '$2!=ws' "$st" > "$st.t"; mv "$st.t" "$st" ;; esac
id=$(awk -v ws="$ws" '$2==ws {print $1; exit}' "$st")
if [ -n "$id" ]; then
  awk -v id="$id" '{ if ($1==id) $3="running"; print }' "$st" > "$st.t"; mv "$st.t" "$st"
else
  id=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n'); echo "$id $ws running" >> "$st"
fi
printf '{"outcome":"success","containerId":"%s","remoteUser":"vscode","remoteWorkspaceFolder":"/workspaces/repo"}\n' "$id"`
}

// lifecycleEnv is newEnv with a docker world: up registers containers.
func lifecycleEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.p.Containers.LabelPrefix = testPrefix
	e.cli.up = registeringUp(e.cli.dir)
	e.wire(t)
	return e
}

// containers is the fake docker's world for one workspace: id → status.
func (e *env) containers(t *testing.T, ws string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.cli.dir, "containers"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if f := strings.Fields(l); len(f) == 3 && f[1] == ws {
			out[f[0]] = f[2]
		}
	}
	return out
}

// actions is the workspace's workspace.action events as
// "action:step:status", in order.
func (e *env) actions(t *testing.T, id string) []string {
	t.Helper()
	evs, err := e.log.ForWorkspace(context.Background(), id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != KindAction {
			continue
		}
		var d struct{ Action, Step, Status string }
		json.Unmarshal(evs[i].Data, &d)
		out = append(out, d.Action+":"+d.Step+":"+d.Status)
	}
	return out
}

func (e *env) kinds(t *testing.T, id, kind string) int {
	t.Helper()
	evs, err := e.log.ForWorkspace(context.Background(), id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func (e *env) running(t *testing.T, repo int64) workspace.View {
	t.Helper()
	v := e.create(t, repo, "")
	if v.State != workspace.Running {
		t.Fatalf("setup: %s (%s) %+v", v.State, deref(v.StateDetail), v.Steps)
	}
	return v
}

func sequence(action string, steps ...string) []string {
	var out []string
	for _, s := range steps {
		out = append(out, action+":"+s+":started", action+":"+s+":done")
	}
	return out
}

// TestStopStopsTheContainerAndClosesTheSocket: a stop runs its sub-steps in
// order, each with its events; the container carrying the label is stopped,
// not removed; the broker socket is closed; the workspace is stopped with its
// clone intact and no longer counts against the cap. Start then reattaches —
// the same container, no --remove-existing-container. Around it, the
// refusals: a stop of a stopped workspace, of none, and of one mid-build,
// whose build carries on untouched.
func TestStopStopsTheContainerAndClosesTheSocket(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	e.p.Workspaces.Cap = 1
	v := e.running(t, alpha)
	before := e.containers(t, v.ID)
	if len(before) != 1 {
		t.Fatalf("setup: containers %v", before)
	}
	marker := filepath.Join(e.root, v.ID, "repo", "unpushed.txt")
	os.WriteFile(marker, []byte("work"), 0o600)

	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	after := e.view(t, v.ID)
	if after.State != workspace.Stopped {
		t.Fatalf("state %s (%s); actions %v", after.State, deref(after.StateDetail), e.actions(t, v.ID))
	}
	want := sequence(ActStop, SubSessionServer, SubContainer, SubBrokerSocket)
	if got := e.actions(t, v.ID); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("actions\n got %v\nwant %v", got, want)
	}
	for id, status := range e.containers(t, v.ID) {
		if status != "exited" {
			t.Errorf("container %s is %s after a stop", id, status)
		}
		if _, ok := before[id]; !ok {
			t.Errorf("a stop made container %s", id)
		}
	}
	if len(e.containers(t, v.ID)) != 1 {
		t.Error("a stop removed the container")
	}
	if c := e.broker.closes(); len(c) != 1 || c[0] != v.ID {
		t.Errorf("broker sockets closed: %v", c)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "work" {
		t.Errorf("the clone did not survive the stop: %v", err)
	}
	stops := e.cli.callsTo(t, "docker")
	var stopArgv []string
	for _, c := range stops {
		if len(c) > 1 && c[1] == "stop" {
			stopArgv = c
		}
	}
	for id := range before {
		if strings.Join(stopArgv, " ") != "docker stop -- "+id {
			t.Errorf("docker stop argv %q", stopArgv)
		}
	}

	// Refusals, each beside the control above.
	if err := e.p.Stop(ctx, v.ID); !errors.Is(err, workspace.ErrInProgress) {
		t.Errorf("stop of a stopped workspace = %v", err)
	}
	if err := e.p.Stop(ctx, "01JABCDEFGHJKMNPQRSTVWXYZ0"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("stop of no workspace = %v", err)
	}

	// A stopped workspace holds no slot: with a cap of 1, another can be
	// created now.
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
	e.wire(t)
	other := e.running(t, plain)
	// And starting the stopped one is at the cap until the other stops.
	if err := e.p.Start(ctx, v.ID); !errors.Is(err, workspace.ErrAtCap) {
		t.Errorf("start at the cap = %v", err)
	}
	if err := e.p.Stop(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()

	// Start reattaches: the same container, and up without the flag.
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/plain.git", "/krelinga/alpha.git")
	e.wire(t)
	if err := e.p.Start(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	started := e.view(t, v.ID)
	if started.State != workspace.Running {
		t.Fatalf("after start: %s (%s)", started.State, deref(started.StateDetail))
	}
	for id := range before {
		if started.ContainerID == nil || *started.ContainerID != id {
			t.Errorf("start from stopped got container %v, want the same %s", started.ContainerID, id)
		}
	}
	ups := e.cli.callsTo(t, "up")
	if last := ups[len(ups)-1]; strings.Contains(strings.Join(last, " "), "--remove-existing-container") {
		t.Errorf("start from stopped removed the container: %v", last)
	}

	// A stop mid-build is refused, and the build is not disturbed.
	e.cli.up = "sleep 2; " + registeringUp(e.cli.dir)
	e.wire(t)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Stop(ctx, v.ID); !errors.Is(err, workspace.ErrInProgress) {
		t.Errorf("stop during a build = %v, want ErrInProgress", err)
	}
	e.p.wg.Wait()
	if s := e.view(t, v.ID).State; s != workspace.Running {
		t.Errorf("the build a refused stop raced ended %s", s)
	}
}

// TestStopFailureLeavesItRunning: docker refusing the stop leaves the
// workspace running — which it is — with the failed sub-step named, and the
// socket still open, since nothing after the failure ran.
func TestStopFailureLeavesItRunning(t *testing.T) {
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-stop"), nil, 0o600)
	if err := e.p.Stop(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if s := e.view(t, v.ID).State; s != workspace.Running {
		t.Errorf("state %s after a failed stop", s)
	}
	got := e.actions(t, v.ID)
	if last := got[len(got)-1]; last != "stop:container:failed" {
		t.Errorf("actions %v", got)
	}
	if c := e.broker.closes(); len(c) != 0 {
		t.Errorf("the socket was closed after a failed stop: %v", c)
	}
	// Control: the same stop with docker willing.
	os.Remove(filepath.Join(e.cli.dir, "docker-fail-stop"))
	if err := e.p.Stop(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if s := e.view(t, v.ID).State; s != workspace.Stopped {
		t.Errorf("control: state %s", s)
	}
}

// TestRebuildReplacesTheContainer: a rebuild runs from resolve_config — no
// second clone — with --remove-existing-container, and comes back with a new
// container and the clone intact. Without the flag, up would reattach to the
// old one (the fake does, as the real CLI does) and the id would not change.
// Start from failed passes the flag too: a failed workspace's container is
// one whose up did not succeed.
func TestRebuildReplacesTheContainer(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	probe := e.cli.exec
	v := e.running(t, alpha)
	old := *v.ContainerID
	marker := filepath.Join(e.root, v.ID, "repo", "unpushed.txt")
	os.WriteFile(marker, []byte("work"), 0o600)

	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	r := e.view(t, v.ID)
	if r.State != workspace.Running || r.ContainerID == nil || *r.ContainerID == old {
		t.Fatalf("after rebuild: %s %v (old %s)", r.State, r.ContainerID, old)
	}
	if cs := e.containers(t, v.ID); len(cs) != 1 || cs[*r.ContainerID] != "running" {
		t.Errorf("containers after rebuild: %v", cs)
	}
	ups := e.cli.callsTo(t, "up")
	if len(ups) != 2 || !strings.Contains(strings.Join(ups[1], " "), "--remove-existing-container") {
		t.Errorf("the rebuild's up: %v", ups)
	}
	if n := countOf(e.stepEvents(t, v.ID), "clone:started"); n != 1 {
		t.Errorf("the clone ran %d times", n)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "work" {
		t.Errorf("the clone did not survive the rebuild: %v", err)
	}

	// Refusals beside the control: one run at a time; deleting; no row.
	e.cli.up = "sleep 2; " + registeringUp(e.cli.dir)
	e.wire(t)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Rebuild(ctx, v.ID); !errors.Is(err, workspace.ErrInProgress) {
		t.Errorf("a rebuild during a rebuild = %v", err)
	}
	if err := e.p.Start(ctx, v.ID); !errors.Is(err, workspace.ErrInProgress) {
		t.Errorf("a start during a rebuild = %v", err)
	}
	e.p.wg.Wait()
	if err := e.p.Rebuild(ctx, "01JABCDEFGHJKMNPQRSTVWXYZ0"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("rebuild of no workspace = %v", err)
	}

	// The cap: a running workspace holds its slot, so it rebuilds at the
	// cap; a stopped one does not, so it is refused there.
	e.cli.up = registeringUp(e.cli.dir)
	e.wire(t)
	e.p.Workspaces.Cap = 1
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Errorf("rebuild of a running workspace at the cap = %v", err)
	}
	e.p.wg.Wait()
	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
	e.wire(t)
	e.running(t, plain)
	if err := e.p.Rebuild(ctx, v.ID); !errors.Is(err, workspace.ErrAtCap) {
		t.Errorf("rebuild of a stopped workspace at the cap = %v", err)
	}
	e.p.Workspaces.Cap = 2
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/plain.git", "/krelinga/alpha.git")
	e.wire(t)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Errorf("control: under the cap, rebuild = %v", err)
	}
	e.p.wg.Wait()

	// Start from failed removes the leftover container.
	e.cli.exec = `case " $* " in *" drydock-probe "*) exit 1 ;; esac`
	e.wire(t)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	f := e.view(t, v.ID)
	if f.State != workspace.Failed || len(e.containers(t, v.ID)) != 1 {
		t.Fatalf("setup: a failed probe leaves %s with containers %v", f.State, e.containers(t, v.ID))
	}
	e.cli.exec = probe
	e.wire(t)
	if err := e.p.Start(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	ups = e.cli.callsTo(t, "up")
	if last := ups[len(ups)-1]; !strings.Contains(strings.Join(last, " "), "--remove-existing-container") {
		t.Errorf("start from failed reattached: %v", last)
	}
	if s := e.view(t, v.ID); s.State != workspace.Running || *s.ContainerID == *f.ContainerID {
		t.Errorf("start from failed: %s, container %v (failed one %v)", s.State, s.ContainerID, *f.ContainerID)
	}
}

// TestDeleteRemovesEverything: a delete takes every container carrying the
// label — a leftover second one included — the broker socket, the whole
// workspace directory, and the row, in that order, each sub-step with its
// events, then workspace.gone. A symlink in the clone pointing out of the
// tree loses the link and keeps its target. Before it, the confirm is
// refused when it is empty, a near miss, or differently cased, and nothing
// moves.
func TestDeleteRemovesEverything(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	leftover := strings.Repeat("ab", 32)
	f, _ := os.OpenFile(filepath.Join(e.cli.dir, "containers"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(leftover + " " + v.ID + " exited\n")
	f.Close()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "precious"), []byte("keep"), 0o600)
	os.Symlink(outside, filepath.Join(e.root, v.ID, "repo", "link-out"))
	os.Symlink(filepath.Join(outside, "precious"), filepath.Join(e.root, v.ID, "repo", "file-link"))

	for _, bad := range []string{"", "krelinga/alph", "Krelinga/Alpha", " krelinga/alpha", "krelinga/alpha ", "alpha"} {
		if err := e.p.Delete(ctx, v.ID, bad); !errors.Is(err, ErrConfirmMismatch) {
			t.Errorf("confirm %q = %v, want ErrConfirmMismatch", bad, err)
		}
	}
	if s := e.view(t, v.ID).State; s != workspace.Running || len(e.containers(t, v.ID)) != 2 {
		t.Fatalf("a refused delete changed something: %s, %v", s, e.containers(t, v.ID))
	}
	if err := e.p.Delete(ctx, "01JABCDEFGHJKMNPQRSTVWXYZ0", "krelinga/alpha"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("delete of no workspace = %v", err)
	}

	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("the row survived: %v; actions %v", err, e.actions(t, v.ID))
	}
	want := sequence(ActDelete, SubSessionServer, SubContainers, SubBrokerSocket, SubFiles)
	if got := e.actions(t, v.ID); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("actions\n got %v\nwant %v", got, want)
	}
	if n := e.kinds(t, v.ID, workspace.KindGone); n != 1 {
		t.Errorf("%d workspace.gone events", n)
	}
	if cs := e.containers(t, v.ID); len(cs) != 0 {
		t.Errorf("containers left: %v", cs)
	}
	var rm []string
	for _, c := range e.cli.callsTo(t, "docker") {
		if len(c) > 1 && c[1] == "rm" {
			rm = c
		}
	}
	if j := strings.Join(rm, " "); !strings.HasPrefix(j, "docker rm --force --volumes -- ") ||
		!strings.Contains(j, leftover) || !strings.Contains(j, *v.ContainerID) {
		t.Errorf("docker rm argv %q", rm)
	}
	if c := e.broker.closes(); len(c) != 1 || c[0] != v.ID {
		t.Errorf("broker sockets closed: %v", c)
	}
	if _, err := os.Lstat(filepath.Join(e.root, v.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace directory survived: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "precious")); err != nil || string(b) != "keep" {
		t.Errorf("a symlink's target outside the tree was removed: %v", err)
	}
	if _, err := os.Stat(e.root); err != nil {
		t.Errorf("the workspace root itself went: %v", err)
	}
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("a second delete = %v", err)
	}
}

// TestDeleteCancelsARunInFlight: a delete during provisioning cancels the run
// and waits for it — the run fails the step it was on, saying the workspace
// is being deleted, and never reaches running — and then removes what the run
// left. The delete is persisted before the run is told: the row is deleting
// the moment Delete returns.
func TestDeleteCancelsARunInFlight(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	// up makes its container, and when cancelled takes a moment to make a
	// second — as a container create the daemon finishes after the CLI is
	// gone would. A delete that did not wait for the run to end would look
	// before the straggler exists, and leave it.
	e.cli.up = `trap 'sleep 0.3; echo "$(od -An -N32 -tx1 /dev/urandom | tr -d " \n") $ws exited" >> '"'` +
		e.cli.dir + `/containers'"'; exit 143' TERM; ` + registeringUp(e.cli.dir) + ` >/dev/null; sleep 30 & wait`
	e.wire(t)
	w, err := e.p.Create(ctx, alpha, "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(e.containers(t, w.ID)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("up never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := e.p.Delete(ctx, w.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	if got, err := e.p.Workspaces.Get(ctx, w.ID); err == nil && got.State != workspace.Deleting {
		t.Errorf("state %s straight after Delete; the delete must be persisted first", got.State)
	}
	// A second delete while the first is in flight is a no-op.
	if err := e.p.Delete(ctx, w.ID, "krelinga/alpha"); err != nil && !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("a second delete in flight = %v", err)
	}
	e.p.wg.Wait()
	if time.Since(start) > 20*time.Second {
		t.Errorf("the delete waited out the run: %s", time.Since(start))
	}
	if _, err := e.p.Workspaces.Get(ctx, w.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("the row survived: %v; actions %v", err, e.actions(t, w.ID))
	}
	if cs := e.containers(t, w.ID); len(cs) != 0 {
		t.Errorf("the run's container survived: %v", cs)
	}
	// The directory went whole: the clone and .drydock/, the per-up TMPDIR
	// the cancelled up was using included.
	if _, err := os.Lstat(filepath.Join(e.root, w.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace directory survived: %v", err)
	}
	evs, _ := e.log.ForWorkspace(ctx, w.ID, 1000)
	sawCancel, sawRunning := false, false
	for _, ev := range evs {
		var d struct{ Step, Status, Detail, State string }
		json.Unmarshal(ev.Data, &d)
		if ev.Kind == workspace.KindStep && d.Step == "up" && d.Status == "failed" && strings.Contains(d.Detail, "being deleted") {
			sawCancel = true
		}
		if ev.Kind == workspace.KindState && d.State == "running" {
			sawRunning = true
		}
	}
	if !sawCancel || sawRunning {
		t.Errorf("the run: cancelled-with-reason %v, reached running %v", sawCancel, sawRunning)
	}
}

// TestRemoveWorkspaceDirRefusesAnythingElse is the delete's safety: it
// removes <root>/<id> and refuses everything it cannot prove is that. Each
// refusal leaves what it refused intact; the control removes a real
// workspace directory, read-only subdirectory and all.
func TestRemoveWorkspaceDirRefusesAnythingElse(t *testing.T) {
	const id = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	mk := func(t *testing.T) (root, outside string) {
		base := t.TempDir()
		root, outside = filepath.Join(base, "ws"), filepath.Join(base, "outside")
		os.MkdirAll(root, 0o700)
		os.MkdirAll(outside, 0o700)
		os.WriteFile(filepath.Join(outside, "precious"), []byte("keep"), 0o600)
		return root, outside
	}
	intact := func(t *testing.T, outside string) {
		t.Helper()
		if b, err := os.ReadFile(filepath.Join(outside, "precious")); err != nil || string(b) != "keep" {
			t.Errorf("something outside the workspace was removed: %v", err)
		}
	}
	ws := func(root, id string) workspace.Workspace {
		return workspace.Workspace{ID: id, HostPath: filepath.Join(root, id, "repo")}
	}

	t.Run("control", func(t *testing.T) {
		root, outside := mk(t)
		dir := filepath.Join(root, id)
		os.MkdirAll(filepath.Join(dir, "repo", "ro", "deep"), 0o700)
		os.WriteFile(filepath.Join(dir, "repo", "ro", "deep", "f"), nil, 0o400)
		os.Chmod(filepath.Join(dir, "repo", "ro"), 0o500)
		os.Symlink(outside, filepath.Join(dir, "repo", "out"))
		if err := removeWorkspaceDir(root, ws(root, id)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("not removed: %v", err)
		}
		intact(t, outside)
		if err := removeWorkspaceDir(root, ws(root, id)); err != nil {
			t.Errorf("an already-removed directory = %v; a resumed delete must not fail on it", err)
		}
	})
	t.Run("the directory is a symlink out", func(t *testing.T) {
		root, outside := mk(t)
		os.Symlink(outside, filepath.Join(root, id))
		if err := removeWorkspaceDir(root, ws(root, id)); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("= %v", err)
		}
		intact(t, outside)
	})
	t.Run("the directory is a file", func(t *testing.T) {
		root, _ := mk(t)
		os.WriteFile(filepath.Join(root, id), []byte("not a dir"), 0o600)
		if err := removeWorkspaceDir(root, ws(root, id)); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("= %v", err)
		}
	})
	for _, c := range []struct {
		name string
		root string
		w    func(root string) workspace.Workspace
	}{
		{"an id that is not a ULID", "", func(root string) workspace.Workspace { return ws(root, "..") }},
		{"an empty id", "", func(root string) workspace.Workspace { return ws(root, "") }},
		{"the row's clone is elsewhere", "", func(root string) workspace.Workspace {
			return workspace.Workspace{ID: id, HostPath: "/srv/drydock/ws/" + id + "/repo"}
		}},
		{"a relative root", "ws", func(root string) workspace.Workspace { return ws("ws", id) }},
		{"an unclean root", "/tmp/../tmp/ws", func(root string) workspace.Workspace { return ws(root, id) }},
		{"the filesystem root", "/", func(root string) workspace.Workspace { return ws(root, id) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, outside := mk(t)
			os.MkdirAll(filepath.Join(root, id, "repo"), 0o700)
			if c.root != "" {
				root = c.root
			}
			if err := removeWorkspaceDir(root, c.w(root)); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("= %v, want ErrUnsafePath", err)
			}
			intact(t, outside)
		})
	}
}

// TestDeleteRefusalStaysResumable: a delete whose directory step refuses —
// the directory is a symlink out of the root — leaves the workspace deleting,
// with a detail naming the step, and the symlink's target intact. Fixing the
// cause and asking again finishes it.
func TestDeleteRefusalStaysResumable(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	dir := filepath.Join(e.root, v.ID)
	moved := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	os.Symlink(moved, dir)

	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	got := e.view(t, v.ID)
	if got.State != workspace.Deleting || !strings.Contains(deref(got.StateDetail), "refused") {
		t.Errorf("after a refused files step: %s (%s)", got.State, deref(got.StateDetail))
	}
	if a := e.actions(t, v.ID); a[len(a)-1] != "delete:files:failed" {
		t.Errorf("actions %v", a)
	}
	if _, err := os.Stat(filepath.Join(moved, "repo", ".git")); err != nil {
		t.Errorf("the symlink's target was touched: %v", err)
	}

	os.Remove(dir)
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("asking again did not finish the delete: %v", err)
	}
}

// TestDeleteIsResumableAfterEverySubStep is the crash test. A delete is cut
// off after each sub-step in turn — and once before any, with only the move
// to deleting written — leaving exactly the durable state a crash there
// would. Then a new process: a fresh Provisioner on the same database and
// disk, and boot reconciliation with its Delete wired to ResumeDelete. Every
// case must finish — no container, no directory, no row, the socket closed,
// one workspace.gone — and a second reconciliation must find nothing to do.
func TestDeleteIsResumableAfterEverySubStep(t *testing.T) {
	ctx := context.Background()
	errCrash := errors.New("crash")
	for _, crashAfter := range []string{"", SubSessionServer, SubContainers, SubBrokerSocket, SubFiles} {
		name := crashAfter
		if name == "" {
			name = "before any"
		}
		t.Run(name, func(t *testing.T) {
			e := lifecycleEnv(t)
			v := e.running(t, alpha)
			if crashAfter == "" {
				if _, err := e.p.Workspaces.Move(ctx, v.ID, workspace.Deleting, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				e.p.afterStep = func(action, step string) error {
					if step == crashAfter {
						return errCrash
					}
					return nil
				}
				if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
					t.Fatal(err)
				}
				e.p.wg.Wait()
			}
			if got := e.view(t, v.ID); got.State != workspace.Deleting {
				t.Fatalf("after the crash: %s", got.State)
			}

			// The restart.
			b2 := &stubBroker{}
			p2 := &Provisioner{Workspaces: e.p.Workspaces, Events: e.log, Broker: b2,
				Cloner: e.p.Cloner, Containers: e.p.Containers, Logf: t.Logf}
			rec := &reconcile.Reconciler{Workspaces: e.p.Workspaces, Events: e.log, Containers: e.p.Containers,
				Busy: p2.Owns,
				Delete: func(ctx context.Context, w workspace.Workspace, _ string) error {
					return p2.ResumeDelete(ctx, w.ID)
				}}
			plan, err := rec.Run(ctx)
			if err != nil {
				t.Fatalf("reconcile: %v (plan %+v)", err, plan)
			}
			if len(plan) != 1 || plan[0].Kind != reconcile.ResumeDelete {
				t.Errorf("plan %+v", plan)
			}
			if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
				t.Errorf("the resumed delete left the row: %v; actions %v", err, e.actions(t, v.ID))
			}
			if cs := e.containers(t, v.ID); len(cs) != 0 {
				t.Errorf("containers left: %v", cs)
			}
			if _, err := os.Lstat(filepath.Join(e.root, v.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the directory survived: %v", err)
			}
			if c := b2.closes(); len(c) != 1 || c[0] != v.ID {
				t.Errorf("the resumed delete closed sockets %v", c)
			}
			if n := e.kinds(t, v.ID, workspace.KindGone); n != 1 {
				t.Errorf("%d workspace.gone events", n)
			}

			// Idempotent: a second boot finds nothing, and a late resume is
			// a not-found rather than a second delete.
			if plan, err := rec.Run(ctx); err != nil || len(plan) != 0 {
				t.Errorf("a second reconciliation: %+v %v", plan, err)
			}
			if err := p2.ResumeDelete(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
				t.Errorf("a second resume = %v", err)
			}
			if n := e.kinds(t, v.ID, workspace.KindGone); n != 1 {
				t.Errorf("%d workspace.gone events after a second boot", n)
			}
		})
	}
}

// TestDeleteNamesAStuckSubStep: docker failing the remove, and a container
// that survives a remove that said it worked, each leave the workspace
// deleting with the containers step failed and nothing after it run — the
// socket still there and the directory intact, since a container still
// holding the mount must not lose the clone under it.
func TestDeleteNamesAStuckSubStep(t *testing.T) {
	for _, c := range []struct{ name, file string }{
		{"docker fails", "docker-fail-rm"},
		{"the container survives", "docker-sticky"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := lifecycleEnv(t)
			v := e.running(t, alpha)
			os.WriteFile(filepath.Join(e.cli.dir, c.file), nil, 0o600)
			if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
				t.Fatal(err)
			}
			e.p.wg.Wait()
			got := e.view(t, v.ID)
			if got.State != workspace.Deleting || !strings.Contains(deref(got.StateDetail), "container") {
				t.Errorf("%s (%s)", got.State, deref(got.StateDetail))
			}
			if a := e.actions(t, v.ID); a[len(a)-1] != "delete:containers:failed" {
				t.Errorf("actions %v", a)
			}
			if len(e.broker.closes()) != 0 {
				t.Error("the socket was closed past a stuck containers step")
			}
			if _, err := os.Stat(filepath.Join(e.root, v.ID, "repo", ".git")); err != nil {
				t.Errorf("the clone went past a stuck containers step: %v", err)
			}
			// Control: docker recovers; asking again finishes it.
			os.Remove(filepath.Join(e.cli.dir, c.file))
			if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
				t.Fatal(err)
			}
			e.p.wg.Wait()
			if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
				t.Errorf("control: %v", err)
			}
		})
	}
}
