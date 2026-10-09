package container

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/dockerguard"
	"github.com/krelinga/drydock/internal/subproc"
)

const fixtureWS = "/srv/drydock/ws/FIXTURE"

// recordedUp is one recorded up (test/fixtures/devcontainer/docker-argv-*):
// every docker command CLI 0.89.0 ran, and the folder's merged
// read-configuration, both moved to root, with the paths a build names made
// on disk as the CLI had made them.
func recordedUp(t *testing.T, name, root string) ([][]string, Configuration) {
	t.Helper()
	dir := filepath.Join("..", "..", "test", "fixtures", "devcontainer")
	b, err := os.ReadFile(filepath.Join(dir, "docker-argv-"+name+".read-configuration.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfiguration([]byte(strings.ReplaceAll(string(b), fixtureWS, root)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	f, err := os.Open(filepath.Join(dir, "docker-argv-"+name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cmds [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var args []string
		if err := json.Unmarshal([]byte(strings.ReplaceAll(sc.Text(), fixtureWS, root)), &args); err != nil {
			t.Fatal(err)
		}
		for i, a := range args {
			p := a
			if j := strings.Index(a, root); j > 0 && strings.HasSuffix(a[:j], "=") {
				p = a[j:]
			}
			if !strings.HasPrefix(p, root+"/") || strings.Contains(p, ",") {
				continue
			}
			if i > 0 && args[i-1] == "-f" {
				os.MkdirAll(filepath.Dir(p), 0o700)
				os.WriteFile(p, nil, 0o600)
			} else {
				os.MkdirAll(p, 0o700)
			}
		}
		cmds = append(cmds, args)
	}
	return cmds, c
}

// recordedPolicy is the policy Up writes for a recorded up — the policy
// guardPolicy builds from the UpSpec provision would give it — approving
// approved.
func recordedPolicy(root string, approved []HostSetting) *dockerguard.Policy {
	m := Manager{LabelPrefix: "drydock"}
	s := UpSpec{WorkspaceID: "01JFIXTVRE0000000000000000", RepositoryID: 101, FullName: "krelinga/fixture", Branch: "main",
		Folder: filepath.Join(root, "repo"), TempDir: filepath.Join(root, ".drydock", "tmp"),
		ConfigDir: filepath.Join(root, "repo", ".devcontainer"), BrokerDir: filepath.Join(root, "sock"),
		ClaudeVolume: "drydock-record-claude", Approved: approved}
	os.MkdirAll(s.TempDir, 0o700)
	p := m.guardPolicy(s)
	p.Version = dockerguard.PolicyVersion
	return &p
}

func refusedBy(p *dockerguard.Policy, cmds [][]string) []string {
	seen := map[string]bool{}
	for _, args := range cmds {
		for _, s := range dockerguard.Check(p, args).Settings {
			seen[s] = true
		}
	}
	var out []string
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// The two halves as the CLI produced them: for each recorded up, step 3's
// subset is computed from the recorded read-configuration with HostAccessOf,
// and with that subset approved every docker command the up ran passes the
// guard. With nothing approved, exactly the commands that carry the subset
// are refused, naming it — so the approval is what let them through, and the
// guard reads each field where the CLI writes it.
func TestRecordedUpsPassWithTheirApproval(t *testing.T) {
	for name, withheld := range map[string][]string{
		"image":      nil,
		"dockerfile": nil,
		"drydock":    nil,
		"feature":    {dockerguard.SettingCapAdd},
		"dind":       {dockerguard.SettingPrivileged},
		"hostile": {dockerguard.SettingAppPort, dockerguard.SettingCapAdd, dockerguard.SettingMounts,
			dockerguard.SettingPrivileged, dockerguard.SettingRunArgs, dockerguard.SettingSecurityOpt},
		"compose": {dockerguard.SettingCompose},
		"build":   {dockerguard.SettingCacheFrom, dockerguard.SettingBuildOpts},
	} {
		root := t.TempDir()
		cmds, c := recordedUp(t, name, root)
		ha, err := HostAccessOf(c, filepath.Join(root, "repo"), false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := refusedBy(recordedPolicy(root, ha.Settings), cmds); got != nil {
			t.Errorf("%s, its subset approved: refused %v", name, got)
		}
		if got := refusedBy(recordedPolicy(root, nil), cmds); !reflect.DeepEqual(got, withheld) {
			t.Errorf("%s, nothing approved: refused %v, want %v", name, got, withheld)
		}
	}
}

// The same for a start: the container each recorded up made, as docker
// inspect read it back, starts with its configuration's subset approved,
// and with nothing approved is refused naming what its run was refused for
// — so CheckStarted reads each field where the daemon records it. (hostile's
// workspaceMount is the clone itself, and passes, as on run.)
func TestRecordedStartsPassWithTheirApproval(t *testing.T) {
	for name, withheld := range map[string][]string{
		"image": nil,
		"dind":  {dockerguard.SettingPrivileged},
		"hostile": {dockerguard.SettingAppPort, dockerguard.SettingCapAdd, dockerguard.SettingMounts,
			dockerguard.SettingPrivileged, dockerguard.SettingRunArgs, dockerguard.SettingSecurityOpt},
	} {
		root := t.TempDir()
		_, c := recordedUp(t, name, root)
		ha, err := HostAccessOf(c, filepath.Join(root, "repo"), false)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", "docker-inspect-"+name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		b = []byte(strings.ReplaceAll(string(b), fixtureWS, root))
		var ids []struct{ Id string }
		json.Unmarshal(b, &ids)
		start := []string{ids[0].Id}
		if d := dockerguard.CheckStarted(recordedPolicy(root, ha.Settings), start, b, nil); d.Refused {
			t.Errorf("%s, its subset approved: %+v", name, d)
		}
		d := dockerguard.CheckStarted(recordedPolicy(root, nil), start, b, nil)
		if !reflect.DeepEqual(d.Settings, withheld) {
			t.Errorf("%s, nothing approved: %v, want %v (%v)", name, d.Settings, withheld, d.Why)
		}
	}
}

// Each of the hostile configuration's settings is needed: approve all of
// them but one and the up is refused naming that one, where docker's argv
// carries it. (hostRequirements.gpu adds --gpus only on a host with a GPU,
// and the recording had none; initializeCommand never reaches docker.)
func TestEachApprovedSettingIsNeeded(t *testing.T) {
	root := t.TempDir()
	cmds, c := recordedUp(t, "hostile", root)
	ha, err := HostAccessOf(c, filepath.Join(root, "repo"), false)
	if err != nil {
		t.Fatal(err)
	}
	// workspaceMount is absent: the hostile one mounts the clone itself,
	// at another target, which is the mount Drydock gives every workspace
	// and no more (a workspaceMount of anything else is refused as mounts,
	// dockerguard's TestInjectedHostAccessIsRefused).
	asDocker := map[string]string{"runArgs": dockerguard.SettingRunArgs, "appPort": dockerguard.SettingAppPort,
		"privileged": dockerguard.SettingPrivileged, "capAdd": dockerguard.SettingCapAdd,
		"securityOpt": dockerguard.SettingSecurityOpt, "mounts": dockerguard.SettingMounts}
	checked := 0
	for i, s := range ha.Settings {
		want, ok := asDocker[s.Field]
		if !ok {
			continue
		}
		checked++
		rest := append(append([]HostSetting(nil), ha.Settings[:i]...), ha.Settings[i+1:]...)
		if got := refusedBy(recordedPolicy(root, rest), cmds); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("without %s: refused %v, want [%s]", s.Field, got, want)
		}
	}
	if checked != len(asDocker) {
		t.Errorf("checked %d settings; the subset is %+v", checked, ha.Settings)
	}
}

// upThroughGuard is a fake devcontainer whose up does what the real one
// does with --docker-path: it runs docker through it — here, one docker run
// with Drydock's mounts and labels as the CLI writes them, plus extra — and
// prints the CLI's result. The fake docker records the argv it is handed,
// so "docker ran" and "with what" are both observed.
func upThroughGuard(t *testing.T, extra string) (Manager, string) {
	t.Helper()
	body := `dp=; folder=; prev=; labels=; mounts=
for a; do
  case "$prev" in
  --docker-path) dp=$a ;;
  --workspace-folder) folder=$a ;;
  --id-label) labels="$labels -l $a" ;;
  --mount) mounts="$mounts --mount $a" ;;
  esac
  prev=$a
done
cp "$(dirname "$dp")/policy.json" "$(dirname "$0")/policy-seen.json"
"$dp" run --sig-proxy=false -a STDOUT -a STDERR --mount "type=bind,source=$folder,target=/workspaces/repo" $mounts $labels ` + extra + ` --entrypoint /bin/sh img -c x - || { echo '{"outcome":"error","message":"Command failed: docker run","description":"An error occurred setting up the container."}'; exit 1; }
cat <<'EOF'
` + fixture(t, "up-ok.json") + `
EOF
`
	run, dir := fakes(t, map[string]string{"devcontainer": body, "docker": ""})
	return guarded(t, run, "drydock.test"), dir
}

// An up whose docker run asks for privileged, approved nowhere, is refused
// by the guard: Up returns a GuardRefusal naming privileged, docker never
// ran, and the policy the guard read is gone after. The control approves
// privileged, and the same up runs docker with the argv unaltered.
func TestUpRefusedByTheGuard(t *testing.T) {
	m, dir := upThroughGuard(t, "--privileged")
	m.CleanupImage = testCleanupImage
	s := upSpec(t)
	s.BrokerDir = filepath.Join(filepath.Dir(s.Folder), "sock")
	s.ClaudeVolume = "drydock-claude-config"
	_, _, err := m.Up(context.Background(), s)
	var r *GuardRefusal
	if !errors.As(err, &r) || !reflect.DeepEqual(r.Settings, []string{dockerguard.SettingPrivileged}) {
		t.Fatalf("Up: %v, want a refusal naming privileged", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docker.argv")); err == nil {
		t.Error("docker ran")
	}
	guardDir := GuardDir(s.Folder)
	if _, err := os.Stat(filepath.Join(guardDir, dockerguard.PolicyName)); err == nil {
		t.Error("the policy outlived the up")
	}
	// What the guard was held to: this run's labels and mounts, no
	// approval, and the pinned image it probes the daemon's log default with.
	b, err := os.ReadFile(filepath.Join(dir, "policy-seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seen dockerguard.Policy
	json.Unmarshal(b, &seen)
	if seen.Clone != s.Folder || seen.TempDir != s.TempDir || seen.IDLabels["drydock.test.workspace"] != wsID ||
		len(seen.OwnMounts) != 2 || len(seen.Approved) != 0 || seen.ProbeImage != testCleanupImage {
		t.Errorf("policy %+v", seen)
	}

	s.Approved = []HostSetting{{Field: "privileged", Source: SourceFeature, Value: json.RawMessage(`true`)}}
	c, _, err := m.Up(context.Background(), s)
	if err != nil || c.Outcome != classify.ContainerRunning {
		t.Fatalf("approved: %+v %v", c, err)
	}
	got := argv(t, dir, "docker")
	if len(got) < 2 || got[0] != "run" || !has(got, "--privileged") ||
		!has(got, "type=bind,source="+s.BrokerDir+",target="+BrokerMountPoint) {
		t.Errorf("docker ran with %q", got)
	}
	if ref, _ := dockerguard.ReadRefusal(guardDir); ref != nil {
		t.Errorf("a refusal is left after an approved up: %+v", ref)
	}
}

// No guard, no up: the CLI is never run. The control is the same up with
// one.
func TestUpRefusesWithoutAGuard(t *testing.T) {
	run, dir := fakes(t, map[string]string{"devcontainer": "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n"})
	if _, _, err := (Manager{Run: run, LabelPrefix: "drydock"}).Up(context.Background(), upSpec(t)); !errors.Is(err, ErrNoGuard) {
		t.Errorf("Up without a guard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "devcontainer.argv")); err == nil {
		t.Error("the CLI ran without a guard")
	}
	if _, _, err := guarded(t, run, "drydock").Up(context.Background(), upSpec(t)); err != nil {
		t.Errorf("control: %v", err)
	}
}

// read-configuration and exec are given the guard too, and the session
// server's exec: each creates nothing, and the guard passes their docker
// commands through. The pair goes among the CLI's options, never into the
// command exec runs. Without a guard (a test's Manager) none is given.
func TestEveryInvocationIsGuarded(t *testing.T) {
	s := upSpec(t)
	want := filepath.Join(GuardDir(s.Folder), dockerguard.LinkName)
	self, _ := os.Executable()
	g := &dockerguard.Guard{Binary: self, Resolver: subproc.FixedResolver{"docker": "/bin/true"}}
	for _, guard := range []*dockerguard.Guard{g, nil} {
		rec := &recorder{}
		m := Manager{Run: rec, LabelPrefix: "drydock", Guard: guard}
		m.ReadConfiguration(context.Background(), s.Folder, "")
		if _, err := m.ExecIn(context.Background(), ExecSpec{WorkspaceID: wsID, Folder: s.Folder, Argv: []string{"echo", "--docker-path"}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.StartSession(context.Background(), rec, SessionSpec{WorkspaceID: wsID, Folder: s.Folder, Capacity: 4}, 80, 24); err != nil {
			t.Fatal(err)
		}
		if len(rec.calls) != 3 {
			t.Fatalf("%d calls", len(rec.calls))
		}
		for _, args := range rec.calls {
			dp, dd := -1, len(args)
			for i, a := range args {
				if a == "--docker-path" && dp < 0 {
					dp = i
				}
				if a == "--" && dd == len(args) {
					dd = i
				}
			}
			switch {
			case guard == nil && dp >= 0 && dp < dd:
				t.Errorf("no guard, but %q", args)
			case guard != nil && (dp < 0 || dp > dd || args[dp+1] != want):
				t.Errorf("guarded, but %q", args)
			}
		}
		if ex := rec.calls[1]; strings.Join(ex[len(ex)-3:], " ") != "-- echo --docker-path" {
			t.Errorf("exec's command was changed: %q", ex)
		}
	}
}

// recorder is a Runner and a PTYRunner that records argv and runs nothing.
type recorder struct{ calls [][]string }

func (r *recorder) Run(_ context.Context, c subproc.Cmd) subproc.Result {
	r.calls = append(r.calls, c.Args)
	return subproc.Result{ExitCode: 1}
}

func (r *recorder) Start(_ context.Context, c subproc.Cmd) (subproc.Process, error) {
	r.calls = append(r.calls, c.Args)
	return nil, errors.New("not started")
}

func (r *recorder) StartPTY(_ context.Context, c subproc.Cmd, _, _ int) (subproc.Process, *os.File, error) {
	r.calls = append(r.calls, c.Args)
	return nil, nil, nil
}

// wrapRun stands in for a bounding or tracing runner around another.
type wrapRun struct{ subproc.Runner }

func (w wrapRun) Unwrap() subproc.Runner { return w.Runner }

// The docker the guard passes commands to is the Exec's own, also when the
// Exec is wrapped: a wrapper must not make the guard run the real docker.
func TestDockerPathSeesThroughAWrappedRunner(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(t.TempDir(), wsID, "repo")
	if err := os.MkdirAll(GuardDir(folder), 0o700); err != nil {
		t.Fatal(err)
	}
	real := subproc.Exec{Resolver: subproc.FixedResolver{"docker": "/bin/true"}}
	for name, run := range map[string]subproc.Runner{"bare": real, "wrapped": wrapRun{wrapRun{real}}} {
		m := Manager{Run: run, Guard: &dockerguard.Guard{Binary: self}}
		if _, err := m.dockerPath(folder); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		link := filepath.Join(GuardDir(folder), dockerguard.RealName)
		got, err := os.Readlink(link)
		if err != nil || got != "/bin/true" {
			t.Errorf("%s: the guard's real docker is %q (%v), want /bin/true", name, got, err)
		}
		os.Remove(link)
	}
}
