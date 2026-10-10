package provision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/dockerguard"
	"github.com/krelinga/drydock/internal/workspace"
)

// lyingUp is registeringUp with what the real CLI does in between: it runs
// docker through the --docker-path it was given — the docker run CLI 0.89.0
// writes, with the workspace's id-labels and Drydock's mounts as the CLI
// renders them — and only if docker succeeded registers the container. A
// file "lie" beside the fake holds extra docker run options, which
// read-configuration never reported: the moved tag of design §6. A refused
// run prints the result the real CLI prints when a docker command fails.
func lyingUp(dir string) string {
	return `dp=; prev=; folder=; labels=; mounts=
for a in "$@"; do
  case "$prev" in
  --docker-path) dp=$a ;;
  --workspace-folder) folder=$a ;;
  --id-label) labels="$labels -l $a" ;;
  --mount) mounts="$mounts --mount $(printf '%s' "$a" | sed 's/,source=/,src=/; s/,target=/,dst=/')" ;;
  esac
  prev=$a
done
lie=$(cat '` + dir + `/lie' 2>/dev/null)
if ! "$dp" run --sig-proxy=false -a STDOUT -a STDERR --mount "type=bind,source=$folder,target=/workspaces/repo" $mounts $labels $lie --entrypoint /bin/sh vsc-repo-x-uid -c 'echo Container started' -; then
  echo '{"outcome":"error","message":"Command failed: docker run …","description":"An error occurred setting up the container."}'
  exit 1
fi
` + registeringUp(dir)
}

// TestTheGuardRefusesWhatTheCheckDidNotSee is the mutable-tag attack with
// the CLI faked (design §6, "The docker guard"): read-configuration reports
// harmless metadata, step 3 finds nothing to approve, and up then asks
// docker for --privileged — what a Feature or image tag that moved between
// the two would serve. The guard, which is this test binary run as docker
// (TestMain), refuses the docker run: up fails, naming privileged in a
// sentence of its own rather than as a failed up, and docker never creates
// the container.
//
// Two controls, so the refusal is the approval's and not the guard
// refusing everything: the honest up — the same docker run without the lie
// — runs and reaches running; and with privileged approved for the
// repository, the lying up runs too.
func TestTheGuardRefusesWhatTheCheckDidNotSee(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	e.cli.up = lyingUp(e.cli.dir)
	e.wire(t)
	created := func() []string {
		var out []string
		for _, c := range e.cli.calls(t) {
			if len(c) > 1 && c[0] == "docker" && c[1] == "run" && contains(c, "--sig-proxy=false") {
				out = append(out, strings.Join(c[1:], " "))
			}
		}
		return out
	}

	// The control: an honest configuration runs, through the guard.
	v := e.running(t, alpha)
	if got := created(); len(got) != 1 || strings.Contains(got[0], "--privileged") {
		t.Fatalf("the honest up's docker runs: %q", got)
	}
	// The guard labelled it as the dev container VS Code's Reopen in
	// Container of the clone looks for.
	repo := filepath.Join(e.root, v.ID, "repo")
	if got := created()[0]; !strings.HasPrefix(got, "run -l devcontainer.config_file="+filepath.Join(repo, ".devcontainer", "devcontainer.json")+
		" -l devcontainer.local_folder="+repo+" ") {
		t.Errorf("the honest up's docker run: %s", got)
	}

	// The lie: up asks docker for privileged.
	os.WriteFile(filepath.Join(e.cli.dir, "lie"), []byte("--privileged"), 0o600)
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	r := e.view(t, v.ID)
	st := r.Steps[workspace.StepUp]
	if r.State != workspace.Failed || st.Status != "failed" || !strings.Contains(st.Detail, "privileged") ||
		!strings.Contains(st.Detail, "no container was created") {
		t.Fatalf("the lying up: %s, up step %+v", r.State, st)
	}
	if rc := r.Steps[workspace.StepResolveConfig]; rc.Status != "done" || r.Approval != nil {
		t.Errorf("step 3 saw something to approve: %+v %+v", rc, r.Approval)
	}
	if got := created(); len(got) != 1 {
		t.Errorf("docker created a container for the lying up: %q", got)
	}
	if se := e.stepEvents(t, v.ID); se[len(se)-1] != "up:failed" {
		t.Errorf("a step ran after the refused up: %v", se[len(se)-3:])
	}
	// Nothing the fake CLI printed is in the event log: the sentence is
	// Drydock's, naming the setting from the guard's closed set.
	evs, _ := e.log.ForWorkspace(ctx, v.ID, 1000)
	for _, ev := range evs {
		if strings.Contains(ev.Message+string(ev.Data), "Command failed") {
			t.Errorf("the CLI's prose reached the event log: %s", ev.Data)
		}
	}
	w, err := e.p.Workspaces.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ref, _ := dockerguard.ReadRefusal(container.GuardDir(w.HostPath)); ref == nil {
		t.Error("no refusal recorded beside the guard")
	}
	if _, err := os.Stat(filepath.Join(container.GuardDir(w.HostPath), dockerguard.PolicyName)); err == nil {
		t.Error("the policy outlived the up")
	}

	// privileged approved for the repository: the same lying up runs.
	approve(t, e, alpha, container.HostSetting{Field: "privileged", Source: container.SourceFeature, Value: json.RawMessage(`true`)})
	if err := e.p.Start(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	if r := e.view(t, v.ID); r.State != workspace.Running {
		t.Fatalf("with privileged approved: %s (%s) %+v", r.State, deref(r.StateDetail), r.Steps[workspace.StepUp])
	}
	if got := created(); len(got) != 2 || !strings.Contains(got[1], "--privileged") {
		t.Errorf("with privileged approved, docker ran %q", got)
	}
}

// The refusal's sentence names what was refused, and only from the guard's
// closed set — and the no-policy refusal says something else.
func TestGuardRefusalSentence(t *testing.T) {
	s := GuardRefusalSentence([]string{dockerguard.SettingCapAdd, dockerguard.SettingPrivileged})
	if !strings.Contains(s, "has not approved for this repository: capAdd, privileged.") {
		t.Errorf("%q", s)
	}
	if s := GuardRefusalSentence([]string{dockerguard.SettingCommand}); !strings.Contains(s, "a docker command Drydock does not recognise") {
		t.Errorf("%q", s)
	}
	if s := GuardRefusalSentence([]string{dockerguard.SettingNoPolicy}); !strings.Contains(s, "had no record") {
		t.Errorf("%q", s)
	}
}

// approve records an approval for a repository, as POST …/config-approval
// would have.
func approve(t *testing.T, e *env, repo int64, s ...container.HostSetting) {
	t.Helper()
	b, _ := json.Marshal(s)
	if _, err := e.p.Workspaces.DB.Exec(`UPDATE config_approval SET superseded_at = ? WHERE repository_id = ? AND superseded_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), repo); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Workspaces.DB.Exec(`INSERT INTO config_approval (repository_id, hash, settings, approved_by, approved_at)
		VALUES (?, ?, ?, 'test', ?)`, repo, container.HashSettings(s), string(b), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Boot sweeps a guard policy an up killed mid-run left — but not one whose
// workspace has a job in flight, whose up may be reading it now.
func TestBootSweepsLeftoverGuardPolicies(t *testing.T) {
	e := newEnv(t)
	put := func(id string) string {
		dir := container.GuardDir(filepath.Join(e.p.Workspaces.Root, id, "repo"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := dockerguard.WritePolicy(dir, dockerguard.Policy{Clone: "/x", TempDir: "/y"}); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, dockerguard.RefusalName), []byte(`{}`), 0o600)
		return dir
	}
	left, busy := put("01LEFT0000000000000000000A"), put("01BUSY0000000000000000000A")
	e.p.mu.Lock()
	if e.p.active == nil {
		e.p.active = map[string]*job{}
	}
	e.p.active["01BUSY0000000000000000000A"] = &job{}
	e.p.mu.Unlock()
	if err := e.p.SweepGuardPolicies(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{dockerguard.PolicyName, dockerguard.RefusalName} {
		if _, err := os.Stat(filepath.Join(left, f)); err == nil {
			t.Errorf("%s left behind", f)
		}
	}
	if _, err := os.Stat(filepath.Join(busy, dockerguard.PolicyName)); err != nil {
		t.Errorf("the busy workspace's policy was swept: %v", err)
	}
}
