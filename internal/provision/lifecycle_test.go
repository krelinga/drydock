package provision

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// fail, and docker-fail-cleanup-ps only the helper label's listing;
// docker-sticky makes rm report success and remove nothing. run is the
// cleanup helper: it empties its --mount's source, as `find -delete` would,
// and the helper label's listing by workspace is empty — helpers are never
// in the world. Leftover helpers, for the boot sweep, are "<id> <workspace>"
// lines in a file of their own, listed only by the bare cleanup label.
func fakeDocker(dir string) string {
	return `#!/bin/sh
dir='` + dir + `'
st="$dir/containers"
hs="$dir/helpers"
{ echo docker; printf '%s\n' "$@"; echo @@; } >> "$dir/argv"
touch "$st" "$hs"
if [ -e "$dir/docker-fail-$1" ]; then echo "docker $1: the daemon said no" >&2; exit 1; fi
case "$1" in
ps)
  ws=
  for a in "$@"; do
    case "$a" in
    label=*.workspace=*) ws=${a#label=*.workspace=} ;;
    label=*.cleanup=*) [ -e "$dir/docker-fail-cleanup-ps" ] && { echo "docker ps: no" >&2; exit 1; }; exit 0 ;;
    label=*.cleanup) awk '{print $1}' "$hs"; exit 0 ;;
    esac
  done
  if [ -n "$ws" ]; then awk -v ws="$ws" '$2==ws {print $1}' "$st"; else awk '{print $1}' "$st"; fi ;;
inspect)
  shift 3
  printf '['
  sep=
  for id; do
    printf '%s' "$sep"
    awk -v id="$id" -v p='` + testPrefix + `' '$1==id {printf "{\"Id\":\"%s\",\"State\":{\"Status\":\"%s\",\"Running\":%s},\"Config\":{\"Labels\":{\"%s.workspace\":\"%s\",\"%s.repository-id\":\"101\",\"%s.repo\":\"krelinga/alpha\",\"%s.branch\":\"main\"}}}", $1, $3, ($3=="running"?"true":"false"), p, $2, p, p, p}' "$st"
    awk -v id="$id" -v p='` + testPrefix + `' '$1==id {printf "{\"Id\":\"%s\",\"State\":{\"Status\":\"exited\",\"Running\":false},\"Config\":{\"Labels\":{\"%s.cleanup\":\"%s\"}}}", $1, p, $2}' "$hs"
    sep=,
  done
  printf ']\n' ;;
stop)
  while [ "$1" != -- ]; do shift; done; shift
  for id; do awk -v id="$id" '{ if ($1==id) $3="exited"; print }' "$st" > "$st.t" && mv "$st.t" "$st"; done ;;
rm)
  [ -e "$dir/docker-sticky" ] && exit 0
  while [ "$1" != -- ]; do shift; done; shift
  for id; do
    awk -v id="$id" '$1!=id' "$st" > "$st.t" && mv "$st.t" "$st"
    awk -v id="$id" '$1!=id' "$hs" > "$hs.t" && mv "$hs.t" "$hs"
  done ;;
run)
  for a in "$@"; do
    case "$a" in type=bind,source=*) src=${a#type=bind,source=}; src=${src%%,target=*} ;; esac
  done
  find "$src" -mindepth 1 -delete ;;
volume)
  # Volumes as files, vols/<name>.json being what inspect prints; create of
  # an existing name is a no-op, as the real one is.
  v="$dir/vols"; mkdir -p "$v"
  case "$2" in
  ls) ls "$v" | sed 's/\.json$//' ;;
  create)
    shift 2; label=
    while [ "$1" != -- ]; do [ "$1" = --label ] && { label=$2; shift; }; shift; done
    [ -e "$v/$2.json" ] || printf '{"Name":"%s","Driver":"local","Labels":{"%s":"%s"}}' "$2" "${label%%=*}" "${label#*=}" >"$v/$2.json"
    echo "$2" ;;
  inspect) printf '[%s]\n' "$(cat "$v/$4.json")" ;;
  *) exit 64 ;;
  esac ;;
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
	// The failure is on the row, where a list snapshot reads it (frontend
	// §4.5 #15), as the sub-step's own sentence; and the view carries the
	// failed sub-step as structure, so the card need not parse that.
	failedDetail := StopFailedDetail("docker could not stop the workspace's container.")
	failed := e.view(t, v.ID)
	if deref(failed.StateDetail) != failedDetail {
		t.Errorf("state_detail after a failed stop = %q, want %q", deref(failed.StateDetail), failedDetail)
	}
	if la := failed.LastAction; la == nil || la.Action != ActStop || la.Step != SubContainer || la.Status != "failed" {
		t.Errorf("last_action %+v", la)
	}
	if got := e.states(t, v.ID); got[len(got)-1] != "running|"+failedDetail {
		t.Errorf("the annotation is not a workspace.state event: %v", got)
	}

	// Control: the same stop with docker willing. The annotation goes as it
	// starts — a state event with no detail, before its first sub-step — and
	// the stop then lands with no detail either.
	os.Remove(filepath.Join(e.cli.dir, "docker-fail-stop"))
	mark := len(e.timeline(t, v.ID))
	if err := e.p.Stop(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	after := e.view(t, v.ID)
	if after.State != workspace.Stopped || after.StateDetail != nil {
		t.Errorf("control: state %s (%q)", after.State, deref(after.StateDetail))
	}
	if got := e.timeline(t, v.ID)[mark:]; len(got) < 2 || got[0] != "state:running|" || got[1] != "action:stop:session_server:started" {
		t.Errorf("a stop asked for again did not clear the failure first: %v", got)
	}

	// A rebuild after a failed stop clears it too: any move replaces it.
	e.wire(t)
	if err := e.p.Start(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-stop"), nil, 0o600)
	e.p.Stop(context.Background(), v.ID)
	e.p.wg.Wait()
	if d := deref(e.view(t, v.ID).StateDetail); d != failedDetail {
		t.Fatalf("setup: detail %q", d)
	}
	os.Remove(filepath.Join(e.cli.dir, "docker-fail-stop"))
	if err := e.p.Rebuild(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if r := e.view(t, v.ID); r.State != workspace.Running || r.StateDetail != nil {
		t.Errorf("after a rebuild: %s (%q)", r.State, deref(r.StateDetail))
	}
}

// states is the workspace's workspace.state events as "state|detail", in order.
func (e *env) states(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, l := range e.timeline(t, id) {
		if s, ok := strings.CutPrefix(l, "state:"); ok {
			out = append(out, s)
		}
	}
	return out
}

// timeline is the workspace's state and action events, in order:
// "state:<state>|<detail>" and "action:<action>:<step>:<status>".
func (e *env) timeline(t *testing.T, id string) []string {
	t.Helper()
	evs, err := e.log.ForWorkspace(context.Background(), id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		var d struct{ State, Detail, Action, Step, Status string }
		json.Unmarshal(evs[i].Data, &d)
		switch evs[i].Kind {
		case workspace.KindState:
			out = append(out, "state:"+d.State+"|"+d.Detail)
		case KindAction:
			out = append(out, "action:"+d.Action+":"+d.Step+":"+d.Status)
		}
	}
	return out
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
		if err := rwd(t, root, ws(root, id)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("not removed: %v", err)
		}
		intact(t, outside)
		if err := rwd(t, root, ws(root, id)); err != nil {
			t.Errorf("an already-removed directory = %v; a resumed delete must not fail on it", err)
		}
	})
	t.Run("the directory is a symlink out", func(t *testing.T) {
		root, outside := mk(t)
		os.Symlink(outside, filepath.Join(root, id))
		if err := rwd(t, root, ws(root, id)); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("= %v", err)
		}
		intact(t, outside)
	})
	t.Run("the directory is a file", func(t *testing.T) {
		root, _ := mk(t)
		os.WriteFile(filepath.Join(root, id), []byte("not a dir"), 0o600)
		if err := rwd(t, root, ws(root, id)); !errors.Is(err, ErrUnsafePath) {
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
			if err := rwd(t, root, c.w(root)); !errors.Is(err, ErrUnsafePath) {
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
			// Asking again while docker still refuses: the stuck note goes
			// as the resume starts (frontend §4.5 #16) — a state event with
			// no detail before the resume's first sub-step — and a new one
			// is written when it sticks again.
			stuck := deref(got.StateDetail)
			mark := len(e.timeline(t, v.ID))
			if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
				t.Fatal(err)
			}
			e.p.wg.Wait()
			tl := e.timeline(t, v.ID)[mark:]
			if len(tl) < 2 || tl[0] != "state:deleting|" || tl[1] != "action:delete:session_server:started" ||
				tl[len(tl)-1] != "state:deleting|"+stuck {
				t.Errorf("a resume that sticks again: %v", tl)
			}
			// Control: docker recovers; asking again finishes it.
			os.Remove(filepath.Join(e.cli.dir, c.file))
			mark = len(e.timeline(t, v.ID))
			if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
				t.Fatal(err)
			}
			e.p.wg.Wait()
			if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
				t.Errorf("control: %v", err)
			}
			if tl := e.timeline(t, v.ID)[mark:]; len(tl) == 0 || tl[0] != "state:deleting|" {
				t.Errorf("the resume did not clear the stuck note first: %v", tl)
			}
		})
	}
}

// rwd is removeWorkspaceDir with a helper that must never run: every caller
// here either refuses or succeeds on the host.
func rwd(t *testing.T, root string, w workspace.Workspace) error {
	t.Helper()
	_, err := removeWorkspaceDir(context.Background(), root, w, func(context.Context, string, string) error {
		t.Errorf("the helper container ran for %s", w.ID)
		return errors.New("no helper here")
	})
	return err
}

// unremovable makes the host removal fail while any file named "root-owned"
// is left under the directory — what a file root made in the clone does to
// the drydock user, which a unit test cannot make for real without root.
// The container tier makes it for real.
func unremovable(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { hostRemoveAll = os.RemoveAll })
	hostRemoveAll = func(dir string) error {
		found := false
		filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Name() == "root-owned" {
				found = true
			}
			return nil
		})
		if found {
			return &fs.PathError{Op: "unlinkat", Path: dir, Err: syscall.EACCES}
		}
		return os.RemoveAll(dir)
	}
}

// TestRemoveWorkspaceDirFallsBackToTheHelper: what the host cannot remove,
// the helper does — given the workspace's own directory, resolved, and
// nothing above it. The control is the same tree without the unremovable
// file: the host removes it all and the helper never runs. A failing helper
// is an error that says the helper ran. And a directory that stops being the
// workspace's between the host's attempt and the helper — swapped for a
// symlink out — is refused at the re-check, with the helper never run and
// the target intact.
func TestRemoveWorkspaceDirFallsBackToTheHelper(t *testing.T) {
	const id = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	ctx := context.Background()
	unremovable(t)
	mk := func(t *testing.T, stuck bool) (string, string, workspace.Workspace) {
		base := t.TempDir()
		root := filepath.Join(base, "ws")
		os.MkdirAll(filepath.Join(root, id, "repo", "build", "out"), 0o700)
		os.WriteFile(filepath.Join(base, "precious"), []byte("keep"), 0o600)
		if stuck {
			os.WriteFile(filepath.Join(root, id, "repo", "build", "out", "root-owned"), nil, 0o600)
		}
		return base, root, workspace.Workspace{ID: id, HostPath: filepath.Join(root, id, "repo")}
	}
	var given []string
	helper := func(_ context.Context, ws, dir string) error {
		given = append(given, ws+" "+dir)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
		return nil
	}

	// Control: nothing stuck, so no helper.
	_, root, w := mk(t, false)
	if helped, err := removeWorkspaceDir(ctx, root, w, helper); err != nil || helped || len(given) != 0 {
		t.Fatalf("control: helped %v, err %v, helper given %v", helped, err, given)
	}
	if _, err := os.Lstat(filepath.Join(root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control: not removed: %v", err)
	}

	// Stuck: the helper, given exactly <root>/<id>.
	base, root, w := mk(t, true)
	helped, err := removeWorkspaceDir(ctx, root, w, helper)
	if err != nil || !helped {
		t.Fatalf("helped %v, err %v", helped, err)
	}
	if want := []string{id + " " + filepath.Join(root, id)}; strings.Join(given, "|") != strings.Join(want, "|") {
		t.Errorf("the helper was given %v, want %v", given, want)
	}
	if _, err := os.Lstat(filepath.Join(root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not removed: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the root went: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "precious")); string(b) != "keep" {
		t.Error("something beside the root was removed")
	}

	// A helper that fails leaves the error, and says it ran.
	_, root, w = mk(t, true)
	helped, err = removeWorkspaceDir(ctx, root, w, func(context.Context, string, string) error { return errors.New("daemon said no") })
	if !helped || err == nil {
		t.Errorf("a failing helper: helped %v, err %v", helped, err)
	}

	// Swapped between the two checks: refused, helper not run.
	given = nil
	base, root, w = mk(t, true)
	outside := filepath.Join(base, "outside")
	os.MkdirAll(outside, 0o700)
	os.WriteFile(filepath.Join(outside, "precious"), []byte("keep"), 0o600)
	inner, calls := hostRemoveAll, 0
	hostRemoveAll = func(dir string) error {
		if calls++; calls < 2 {
			return inner(dir)
		}
		// The second, failing attempt — and then the swap.
		os.Rename(dir, filepath.Join(base, "moved"))
		os.Symlink(outside, dir)
		return &fs.PathError{Op: "unlinkat", Path: dir, Err: syscall.EACCES}
	}
	helped, err = removeWorkspaceDir(ctx, root, w, helper)
	hostRemoveAll = inner
	if !errors.Is(err, ErrUnsafePath) || helped || len(given) != 0 {
		t.Errorf("a swapped directory: helped %v, err %v, helper given %v", helped, err, given)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "precious")); string(b) != "keep" {
		t.Error("the symlink's target was emptied")
	}
}

// TestDeleteFallsBackToTheHelperContainer: through the delete job, files the
// host cannot remove go through `docker run` of the cleanup helper, the files
// sub-step says so, and the workspace is gone. The helper's argv is the whole
// of its reach, so it is asserted exactly: the pinned image, no network, the
// helper label (never the workspace label reconciliation lists by), and one
// bind mount whose source is <root>/<id>. The control is a workspace with
// nothing stuck, whose delete runs no helper; and a helper that fails leaves
// the workspace deleting, naming files, until asking again finishes it.
func TestDeleteFallsBackToTheHelperContainer(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	e.p.Containers.CleanupImage = "busybox:1.37.0@sha256:" + strings.Repeat("a", 64)
	unremovable(t)
	runs := func() [][]string {
		var out [][]string
		for _, c := range e.cli.callsTo(t, "docker") {
			if len(c) > 1 && c[1] == "run" {
				out = append(out, c[1:])
			}
		}
		return out
	}
	detail := func(id string) string {
		evs, _ := e.log.ForWorkspace(ctx, id, 1000)
		for _, ev := range evs {
			var d struct{ Step, Status, Detail string }
			json.Unmarshal(ev.Data, &d)
			if ev.Kind == KindAction && d.Step == SubFiles && d.Status != "started" {
				return d.Status + ": " + d.Detail
			}
		}
		return ""
	}

	// Control: nothing stuck.
	c := e.running(t, alpha)
	if err := e.p.Delete(ctx, c.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if _, err := e.p.Workspaces.Get(ctx, c.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("control: %v", err)
	}
	if r := runs(); len(r) != 0 {
		t.Errorf("control: the helper ran: %v", r)
	}
	if d := detail(c.ID); d != "done: " {
		t.Errorf("control: files %q", d)
	}

	v := e.running(t, alpha)
	os.MkdirAll(filepath.Join(e.root, v.ID, "repo", "build"), 0o700)
	os.WriteFile(filepath.Join(e.root, v.ID, "repo", "build", "root-owned"), nil, 0o600)
	if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if _, err := e.p.Workspaces.Get(ctx, v.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("the row survived: %v; actions %v", err, e.actions(t, v.ID))
	}
	if _, err := os.Lstat(filepath.Join(e.root, v.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace directory survived: %v", err)
	}
	if d := detail(v.ID); !strings.HasPrefix(d, "done: ") || !strings.Contains(d, "helper container") {
		t.Errorf("files %q: the event must say the helper was used", d)
	}
	r := runs()
	want := "run --rm --label " + testPrefix + ".cleanup=" + v.ID + " --network none --read-only" +
		" --cap-drop ALL --cap-add DAC_OVERRIDE --cap-add FOWNER --security-opt no-new-privileges --user 0:0" +
		" --mount type=bind,source=" + filepath.Join(e.root, v.ID) + ",target=/w --entrypoint find " +
		e.p.Containers.CleanupImage + " /w -mindepth 1 -delete"
	if len(r) != 1 || strings.Join(r[0], " ") != want {
		t.Errorf("helper argv\n got %q\nwant %q", r, want)
	}

	// The helper fails: stuck in deleting, naming files, with the directory
	// still there to retry on.
	f := e.running(t, alpha)
	os.WriteFile(filepath.Join(e.root, f.ID, "repo", "root-owned"), nil, 0o600)
	os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-run"), nil, 0o600)
	if err := e.p.Delete(ctx, f.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	got := e.view(t, f.ID)
	ran := "Drydock could not remove the workspace's directory, even with a helper container for the files it does not own."
	if got.State != workspace.Deleting || detail(f.ID) != "failed: "+ran {
		t.Errorf("a failing helper: %s (%s), files %q", got.State, deref(got.StateDetail), detail(f.ID))
	}
	if a := e.actions(t, f.ID); a[len(a)-1] != "delete:files:failed" {
		t.Errorf("actions %v", a)
	}
	os.Remove(filepath.Join(e.cli.dir, "docker-fail-run"))

	// The helper never ran, so the sentence must not say it was tried: an
	// image not pinned by digest names the flag, and a stray listing docker
	// refused says the helper could not start. Neither calls `docker run`;
	// the case above, which did, is their control.
	before := len(runs())
	for _, c := range []struct{ name, image, file, want string }{
		{"an unpinned image", "busybox:1.37.0", "",
			"Drydock could not remove the workspace's directory: some files belong to another user, and the helper container that removes those cannot run, because --cleanup-image is not pinned by digest."},
		{"docker cannot list strays", e.p.Containers.CleanupImage, "docker-fail-cleanup-ps",
			"Drydock could not remove the workspace's directory: some files belong to another user, and Drydock could not start the helper container that removes those."},
	} {
		pinned := e.p.Containers.CleanupImage
		e.p.Containers.CleanupImage = c.image
		if c.file != "" {
			os.WriteFile(filepath.Join(e.cli.dir, c.file), nil, 0o600)
		}
		if err := e.p.Delete(ctx, f.ID, "krelinga/alpha"); err != nil {
			t.Fatal(err)
		}
		e.p.wg.Wait()
		if d := detail(f.ID); d != "failed: "+c.want {
			t.Errorf("%s: files %q\nwant %q", c.name, d, "failed: "+c.want)
		}
		if v := e.view(t, f.ID); !strings.Contains(deref(v.StateDetail), c.want) {
			t.Errorf("%s: state_detail %q", c.name, deref(v.StateDetail))
		}
		if n := len(runs()); n != before {
			t.Errorf("%s: docker run was called", c.name)
		}
		e.p.Containers.CleanupImage = pinned
		if c.file != "" {
			os.Remove(filepath.Join(e.cli.dir, c.file))
		}
	}
	if err := e.p.Delete(ctx, f.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if _, err := e.p.Workspaces.Get(ctx, f.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("retry after the helper recovered: %v", err)
	}
}

// TestSweepHelpersSkipsAJobInFlight: the boot sweep removes leftover cleanup
// helpers by the bare cleanup label, writes one system event saying how many,
// and leaves alone a helper whose workspace has a job running here — a
// delete started since boot may be running that helper. The control is the
// same helper swept once the job has ended. That the sweep never touches a
// workspace's container is the container tier's to prove, against the real
// daemon's label filter (test/container).
func TestSweepHelpersSkipsAJobInFlight(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	busy := e.running(t, alpha)
	stray, held := strings.Repeat("1", 64), strings.Repeat("2", 64)
	os.WriteFile(filepath.Join(e.cli.dir, "helpers"),
		[]byte(stray+" 01JABCDEFGHJKMNPQRSTVWXYZ0\n"+held+" "+busy.ID+"\n"), 0o600)
	removed := func() []string {
		var out []string
		for _, c := range e.cli.callsTo(t, "docker") {
			if len(c) > 1 && c[1] == "rm" {
				out = append(out, strings.Join(c[1:], " "))
			}
		}
		return out
	}

	// A job in flight on the busy workspace: a rebuild whose up sleeps.
	e.cli.up = "sleep 2; " + registeringUp(e.cli.dir)
	e.wire(t)
	if err := e.p.Rebuild(ctx, busy.ID); err != nil {
		t.Fatal(err)
	}
	n, err := e.p.SweepHelpers(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	if got := removed(); len(got) != 1 || got[0] != "rm --force --volumes -- "+stray {
		t.Errorf("removed %q; want only the stray of the idle workspace", got)
	}
	evs, _ := e.log.Since(ctx, 0)
	var swept int
	for _, ev := range evs {
		if ev.Kind == KindHelpersSwept && ev.WorkspaceID == "" && string(ev.Data) == `{"count":1}` {
			swept++
		}
	}
	if swept != 1 {
		t.Errorf("%d %s events with count 1", swept, KindHelpersSwept)
	}

	// Control: the job ends, and the next sweep takes the other.
	e.p.wg.Wait()
	if n, err := e.p.SweepHelpers(ctx); err != nil || n != 1 {
		t.Errorf("sweep after the job = %d, %v", n, err)
	}
	if got := removed(); len(got) != 2 || got[1] != "rm --force --volumes -- "+held {
		t.Errorf("removed %q", got)
	}
	// Nothing left: nothing removed, nothing written.
	if n, err := e.p.SweepHelpers(ctx); err != nil || n != 0 {
		t.Errorf("an empty sweep = %d, %v", n, err)
	}
}
