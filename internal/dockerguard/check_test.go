package dockerguard

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureRoot is the workspace directory the recordings were rewritten to.
const fixtureRoot = "/srv/drydock/ws/FIXTURE"

// FixtureLabels are the id-labels record.sh gave every recorded up.
var FixtureLabels = map[string]string{
	"drydock.workspace":     "01JFIXTVRE0000000000000000",
	"drydock.repository-id": "101",
	"drydock.repo":          "krelinga/fixture",
	"drydock.branch":        "main",
}

// Recorded reads test/fixtures/devcontainer/docker-argv-<name>.jsonl — every
// docker command CLI 0.89.0 ran for one up, one JSON array a line — with the
// recording's workspace directory moved to root, and makes on disk every
// path under root a build names, as the CLI had made them: the guard
// resolves them.
func Recorded(t *testing.T, name, root string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "test", "fixtures", "devcontainer", "docker-argv-"+name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var args []string
		if err := json.Unmarshal([]byte(strings.ReplaceAll(sc.Text(), fixtureRoot, root)), &args); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, a := range args {
			p := a
			if j := strings.Index(a, root); j > 0 && strings.HasSuffix(a[:j], "=") {
				p = a[j:]
			}
			if !strings.HasPrefix(p, root+"/") || strings.Contains(p, ",") {
				continue
			}
			if i > 0 && (args[i-1] == "-f" || args[i-1] == "--file") {
				os.MkdirAll(filepath.Dir(p), 0o700)
				os.WriteFile(p, nil, 0o600)
			} else {
				os.MkdirAll(p, 0o700)
			}
		}
		out = append(out, args)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s: no recorded commands", name)
	}
	return out
}

// FixturePolicy is the policy Drydock would write for a recorded up moved
// to root, approving approved.
func FixturePolicy(root string, approved []Setting) *Policy {
	os.MkdirAll(filepath.Join(root, "repo", ".devcontainer"), 0o700)
	os.MkdirAll(filepath.Join(root, ".drydock", "tmp"), 0o700)
	return &Policy{
		Version: PolicyVersion, Clone: filepath.Join(root, "repo"), TempDir: filepath.Join(root, ".drydock", "tmp"),
		ConfigDir: filepath.Join(root, "repo", ".devcontainer"), LabelPrefix: "drydock", IDLabels: FixtureLabels,
		OwnMounts: []string{"type=bind,source=" + filepath.Join(root, "sock") + ",target=/run/drydock",
			"type=volume,source=drydock-record-claude,target=/home/vscode/.claude"},
		Approved: approved,
	}
}

func command(t *testing.T, cmds [][]string, prefix ...string) []string {
	t.Helper()
	for _, c := range cmds {
		if len(c) >= len(prefix) && reflect.DeepEqual(c[:len(prefix)], prefix) {
			return append([]string(nil), c...)
		}
	}
	t.Fatalf("no recorded %v", prefix)
	return nil
}

// insert puts extra after the first n elements of args.
func insert(args []string, n int, extra ...string) []string {
	out := append(append([]string(nil), args[:n]...), extra...)
	return append(out, args[n:]...)
}

// The ${devcontainerId} Drydock computes is the CLI's: the recording's
// docker-in-docker volume, named dind-var-lib-docker-${devcontainerId} in
// the Feature, carries it — and a different label set does not.
func TestDevcontainerIDIsTheCLIs(t *testing.T) {
	run := command(t, Recorded(t, "dind", t.TempDir()), "run")
	id := DevcontainerID(FixtureLabels)
	want := "type=volume,src=dind-var-lib-docker-" + id + ",dst=/var/lib/docker"
	found := false
	for _, a := range run {
		found = found || a == want
	}
	if !found || len(id) != 52 {
		t.Errorf("the recorded run has no %q (id %q):\n%q", want, id, run)
	}
	other := map[string]string{}
	for k, v := range FixtureLabels {
		other[k] = v
	}
	other["drydock.branch"] = "other"
	if DevcontainerID(other) == id {
		t.Error("another label set gave the same id")
	}
}

// Every recorded command of an up that asks for no host access passes with
// nothing approved — Drydock's own mounts, labels, the clone, the Features'
// build context in the CLI's TMPDIR, the volume named with the workspace's
// devcontainerId — and every command of an up that creates nothing passes
// with no policy at all.
func TestRecordedCommandsPass(t *testing.T) {
	for _, name := range []string{"image", "image-start", "image-rebuild", "dockerfile", "drydock"} {
		root := t.TempDir()
		p := FixturePolicy(root, nil)
		for _, args := range Recorded(t, name, root) {
			if d := Check(p, args); d.Refused {
				t.Errorf("%s: %q refused: %v %v", name, args, d.Settings, d.Why)
			}
		}
	}
	for _, args := range Recorded(t, "exec", t.TempDir()) {
		if d := Check(nil, args); d.Refused {
			t.Errorf("exec with no policy: %q refused: %v", args, d.Settings)
		}
	}
	// A start is passed on to CheckStarted, which reads the container.
	root := t.TempDir()
	st := command(t, Recorded(t, "image-start", root), "start")
	if d := Check(FixturePolicy(root, nil), st); d.Refused || !reflect.DeepEqual(d.Start, st[1:]) {
		t.Errorf("the recorded start: %+v", d)
	}
}

// Fail closed: with no policy, every recorded command that creates a
// container or builds an image is refused, as no_policy — and the control,
// the same command with its policy, passes.
func TestNoPolicyRefusesCreation(t *testing.T) {
	root := t.TempDir()
	cmds := Recorded(t, "dockerfile", root)
	for _, prefix := range [][]string{{"run"}, {"build"}, {"buildx", "build"}} {
		args := command(t, cmds, prefix...)
		if d := Check(nil, args); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingNoPolicy}) {
			t.Errorf("%v with no policy: %+v", prefix, d)
		}
		if d := Check(FixturePolicy(root, nil), args); d.Refused {
			t.Errorf("control: %v with its policy refused: %+v", prefix, d)
		}
	}
}

// Host access injected into a recorded command is refused, naming the
// setting the operator would approve it under — each beside the unaltered
// command passing (TestRecordedCommandsPass), and each approved form
// passing, so no refusal here is the guard refusing everything.
func TestInjectedHostAccessIsRefused(t *testing.T) {
	root := t.TempDir()
	cmds := Recorded(t, "image", root)
	run := command(t, cmds, "run")
	clone := filepath.Join(root, "repo")
	for _, c := range []struct {
		name     string
		extra    []string
		want     string
		approved []Setting // under which the same command passes; nil: none does
	}{
		{"privileged", []string{"--privileged"}, SettingPrivileged, []Setting{{"privileged", json.RawMessage(`true`)}}},
		{"privileged=true", []string{"--privileged=true"}, SettingPrivileged, []Setting{{"privileged", json.RawMessage(`true`)}}},
		{"a bind mount of /", []string{"--mount", "type=bind,source=/,target=/host"}, SettingMounts,
			[]Setting{{"mounts", json.RawMessage(`["type=bind,source=/,target=/host"]`)}}},
		{"an approved mount written as an object", []string{"--mount", "type=bind,src=/etc,dst=/host-etc"}, SettingMounts,
			[]Setting{{"mounts", json.RawMessage(`[{"source":"/etc","target":"/host-etc","type":"bind"}]`)}}},
		{"the docker socket", []string{"--mount", "source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"}, SettingMounts, nil},
		{"the credential volume elsewhere", []string{"--mount", "type=volume,source=drydock-record-claude,target=/x"}, SettingMounts, nil},
		{"another workspace's volume", []string{"--mount", "type=volume,source=dind-var-lib-docker-0abc,target=/var/lib/docker"}, SettingMounts, nil},
		{"a volume that binds", []string{"--mount", "type=volume,source=x-" + DevcontainerID(FixtureLabels) + ",target=/x,volume-opt=o=bind"}, SettingMounts, nil},
		{"a quoted mount", []string{"--mount", `"type=bind,source=/,target=/h"`}, SettingMounts, nil},
		{"the clone with an extra option", []string{"--mount", "type=bind,source=" + clone + ",target=/w,bind-propagation=rshared"}, SettingMounts, nil},
		{"SYS_ADMIN", []string{"--cap-add", "SYS_ADMIN"}, SettingCapAdd, []Setting{{"capAdd", json.RawMessage(`["SYS_ADMIN"]`)}}},
		{"CAP_SYS_ADMIN, approved as SYS_ADMIN", []string{"--cap-add", "CAP_SYS_ADMIN"}, SettingCapAdd, []Setting{{"capAdd", json.RawMessage(`["SYS_ADMIN"]`)}}},
		{"ALL", []string{"--cap-add", "ALL"}, SettingCapAdd, nil},
		{"apparmor", []string{"--security-opt", "apparmor=unconfined"}, SettingSecurityOpt,
			[]Setting{{"securityOpt", json.RawMessage(`["apparmor=unconfined"]`)}}},
		{"a seccomp profile from a host file", []string{"--security-opt", "seccomp=/etc/drydock/p.json"}, SettingSecurityOpt, nil},
		{"a published port", []string{"-p", "127.0.0.1:8080:8080"}, SettingAppPort, []Setting{{"appPort", json.RawMessage(`8080`)}}},
		{"a port on every address", []string{"-p", "8080:80"}, SettingAppPort, []Setting{{"appPort", json.RawMessage(`["8080:80"]`)}}},
		{"gpus", []string{"--gpus", "all"}, SettingGPU, []Setting{{"hostRequirements.gpu", json.RawMessage(`true`)}}},
		{"host network", []string{"--network", "host"}, SettingRunArgs, []Setting{{"runArgs", json.RawMessage(`["--network","host"]`)}}},
		{"host network, one word", []string{"--network=host"}, SettingRunArgs, []Setting{{"runArgs", json.RawMessage(`["--network=host"]`)}}},
		{"host pid", []string{"--pid", "host"}, SettingRunArgs, nil},
		{"-v", []string{"-v", "/:/host"}, SettingRunArgs, []Setting{{"runArgs", json.RawMessage(`["-v","/:/host"]`)}}},
		{"a device", []string{"--device", "/dev/kmsg"}, SettingRunArgs, nil},
		{"an option nobody knows", []string{"--frobnicate"}, SettingRunArgs, nil},
		{"a cluster", []string{"-it"}, SettingRunArgs, nil},
		{"an attached short value", []string{"-p8080:80"}, SettingRunArgs, nil},
		{"Drydock's label, another workspace", []string{"-l", "drydock.workspace=01OTHER0000000000000000000"}, SettingRunArgs, nil},
		{"Drydock's label, another key", []string{"--label", "drydock.cleanup=x"}, SettingRunArgs, nil},
		{"an env copied from Drydock's", []string{"-e", "HOME"}, SettingRunArgs, nil},
		{"--env-file", []string{"--env-file", "/etc/drydock/drydock.env"}, SettingRunArgs, nil},
		// Review of #78, round 2: each grants host access, and none is in
		// the run table, so each is refused unless runArgs carries it.
		{"a device cgroup rule", []string{"--device-cgroup-rule", "b *:* rwm"}, SettingRunArgs,
			[]Setting{{"runArgs", json.RawMessage(`["--device-cgroup-rule","b *:* rwm"]`)}}},
		{"systempaths=unconfined", []string{"--security-opt", "systempaths=unconfined"}, SettingSecurityOpt,
			[]Setting{{"securityOpt", json.RawMessage(`["systempaths=unconfined"]`)}}},
		{"a sysctl", []string{"--sysctl", "net.ipv4.ip_forward=1"}, SettingRunArgs, nil},
		{"a cgroup parent", []string{"--cgroup-parent", "foo"}, SettingRunArgs, nil},
		// Round 3: a log driver and its options, which run on the host.
		{"a log driver", []string{"--log-driver", "gelf", "--log-opt", "gelf-address=udp://127.0.0.1:12201"}, SettingRunArgs,
			[]Setting{{"runArgs", json.RawMessage(`["--log-driver","gelf","--log-opt","gelf-address=udp://127.0.0.1:12201"]`)}}},
		{"a log option alone", []string{"--log-opt", "max-size=1m"}, SettingRunArgs, nil},
		{"a bind shared back to the host", []string{"--mount", "type=bind,source=/etc,target=/e,bind-propagation=rshared"}, SettingMounts,
			[]Setting{{"mounts", json.RawMessage(`["type=bind,source=/etc,target=/e,bind-propagation=rshared"]`)}}},
	} {
		args := insert(run, 1, c.extra...)
		d := Check(FixturePolicy(root, nil), args)
		if !d.Refused || !reflect.DeepEqual(d.Settings, []string{c.want}) {
			t.Errorf("%s: %+v, want refused naming %s", c.name, d, c.want)
		}
		if c.approved != nil {
			if d := Check(FixturePolicy(root, c.approved), args); d.Refused {
				t.Errorf("%s, approved: refused %+v", c.name, d)
			}
		}
	}
	// What stays in the container needs nothing.
	for _, extra := range [][]string{{"--privileged=false"}, {"--cap-add", "SYS_PTRACE"}, {"--cap-add", "cap_sys_ptrace"},
		{"--security-opt", "seccomp=unconfined"}, {"-e", "A=B"}, {"--init"}, {"-l", "other.label=x"},
		{"--mount", "type=tmpfs,target=/scratch"}, {"--mount", "type=volume,target=/anon"},
		{"--mount", "source=cache-" + DevcontainerID(FixtureLabels) + ",target=/c,type=volume"}} {
		if d := Check(FixturePolicy(root, nil), insert(run, 1, extra...)); d.Refused {
			t.Errorf("%v refused: %+v", extra, d)
		}
	}
	// An approved runArgs is removed as the run it was approved as: the
	// same words in another order, or with another between them, are not it.
	approved := []Setting{{"runArgs", json.RawMessage(`["--network","host","--pid","host"]`)}}
	for _, extra := range [][]string{{"--pid", "host", "--network", "host"}, {"--network", "host", "--init", "--pid", "host"}} {
		if d := Check(FixturePolicy(root, approved), insert(run, 1, extra...)); !d.Refused {
			t.Errorf("runArgs recombined %v passed", extra)
		}
	}
}

// The same for a build: the recorded Dockerfile build passes, and what
// hands the daemon something of the host's is refused.
func TestInjectedBuildOptionsAreRefused(t *testing.T) {
	root := t.TempDir()
	build := command(t, Recorded(t, "dockerfile", root), "buildx", "build")
	clone := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(root, "elsewhere"), 0o700)
	os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(clone, "link"))
	for _, c := range []struct {
		name  string
		args  []string
		want  string
		under []Setting
	}{
		{"a named context of the host's", insert(build, 2, "--build-context", "x=/etc"), SettingBuildOpts,
			[]Setting{{"build.options", json.RawMessage(`["--build-context","x=/etc"]`)}}},
		{"the CLI's context name, elsewhere", insert(build, 2, "--build-context", "dev_containers_feature_content_source=/etc"), SettingBuildOpts, nil},
		{"a secret", insert(build, 2, "--secret", "id=k,src=/etc/drydock/secrets.key"), SettingBuildOpts, nil},
		{"host network", insert(build, 2, "--network", "host"), SettingBuildOpts, nil},
		{"an entitlement", insert(build, 2, "--allow", "security.insecure"), SettingBuildOpts, nil},
		{"an output on the host", insert(build, 2, "-o", "type=local,dest=/etc"), SettingBuildOpts, nil},
		{"a build-arg from Drydock's environment", insert(build, 2, "--build-arg", "HOME"), SettingBuildOpts, nil},
		{"a local cache", insert(build, 2, "--cache-from", "type=local,src=/etc"), SettingCacheFrom, nil},
		{"the context /", append(append([]string(nil), build[:len(build)-1]...), "/"), SettingBuildCtx, nil},
		{"a context outside the clone", append(append([]string(nil), build[:len(build)-1]...), filepath.Join(root, "elsewhere")), SettingBuildCtx,
			[]Setting{{"build.context", json.RawMessage(`"` + filepath.Join(root, "elsewhere") + `"`)}}},
		// Approved as a path in the clone, which the container then made a
		// link out of it: the approval is of the path, not of where it now
		// leads, so it is refused even approved.
		{"the context through a symlink out of the clone", append(append([]string(nil), build[:len(build)-1]...), filepath.Join(clone, "link")), SettingBuildCtx, nil},
		{"a Dockerfile outside", insert(build, 2, "-f", "/etc/hostname"), SettingDockerfile,
			[]Setting{{"build.dockerfile", json.RawMessage(`"/etc/hostname"`)}}},
	} {
		d := Check(FixturePolicy(root, nil), c.args)
		if !d.Refused || !reflect.DeepEqual(d.Settings, []string{c.want}) {
			t.Errorf("%s: %+v, want refused naming %s", c.name, d, c.want)
		}
		if c.under != nil {
			if d := Check(FixturePolicy(root, c.under), c.args); d.Refused {
				t.Errorf("%s, approved: refused %+v", c.name, d)
			}
		}
	}
	for _, ok := range [][]string{insert(build, 2, "--cache-from", "ghcr.io/x/y:cache"), insert(build, 2, "--no-cache")} {
		if d := Check(FixturePolicy(root, nil), ok); d.Refused {
			t.Errorf("%q refused: %+v", ok, d)
		}
	}
}

// A docker command the guard does not know is refused, and so is a global
// option ahead of a known one — with the commands the CLI runs passing, so
// the refusal is the command's.
func TestUnknownCommandsAreRefused(t *testing.T) {
	p := FixturePolicy(t.TempDir(), nil)
	for _, args := range [][]string{
		{"cp", "/etc/drydock/secrets.key", "abc:/tmp"},
		{"-H", "tcp://elsewhere:2375", "run", "busybox"},
		{"--context", "other", "ps"},
		{"buildx", "create", "--driver", "docker-container"},
		{"buildx", "bake"},
		{"volume", "create", "--opt", "o=bind", "--opt", "device=/", "x"},
		{"container", "cp", "a", "b"},
		{"plugin", "install", "x"},
		{"save", "-o", "/etc/x", "img"},
		// A docker-compose binary's argv, as the CLI would run the
		// --docker-compose-path it is given (the guard) without the plugin.
		{"-f", "/srv/x/compose.yml", "up", "-d"},
		{"--project-name", "p", "-f", "/srv/x/compose.yml", "up", "-d"},
	} {
		if d := Check(p, args); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingCommand}) {
			t.Errorf("%q: %+v", args, d)
		}
	}
	for _, args := range [][]string{{"-v"}, {"version", "--format", "x"}, {"buildx", "version"}, {"ps", "-q"},
		{"inspect", "--type", "image", "x"}, {"start", "abc"}, {"rm", "-f", "abc"}, {"events"}, {"info", "-f", "x"},
		{"container", "inspect", "x"}, {"image", "inspect", "x"}} {
		if d := Check(p, args); d.Refused {
			t.Errorf("control %q refused: %+v", args, d)
		}
	}
}

// exec creates nothing and passes — but --privileged hands an exec'd process
// every capability, so it needs privileged approved, as a run does.
func TestExecPassesButNotPrivileged(t *testing.T) {
	cmds := Recorded(t, "exec", t.TempDir())
	ex := command(t, cmds, "exec")
	if d := Check(nil, ex); d.Refused {
		t.Fatalf("recorded exec refused: %+v", d)
	}
	priv := insert(ex, 1, "--privileged")
	if d := Check(nil, priv); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingPrivileged}) {
		t.Errorf("exec --privileged: %+v", d)
	}
	root := t.TempDir()
	if d := Check(FixturePolicy(root, []Setting{{"privileged", json.RawMessage(`true`)}}), priv); d.Refused {
		t.Errorf("exec --privileged, approved: %+v", d)
	}
	for _, extra := range [][]string{{"--env-file", "/etc/drydock/drydock.env"}, {"-e", "HOME"}} {
		if d := Check(nil, insert(ex, 1, extra...)); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingExecOption}) {
			t.Errorf("exec %v: %+v", extra, d)
		}
	}
}

// docker compose runs only what creates nothing until the Compose setup is
// approved, as step 3 asks it to be.
func TestComposeNeedsItsApproval(t *testing.T) {
	root := t.TempDir()
	cmds := Recorded(t, "compose", root)
	var refused []string
	for _, args := range cmds {
		if args[0] != "compose" {
			continue
		}
		d := Check(FixturePolicy(root, nil), args)
		if d.Refused {
			refused = append(refused, args[len(args)-1])
			if !reflect.DeepEqual(d.Settings, []string{SettingCompose}) {
				t.Errorf("%q: %+v", args, d)
			}
		}
		if d := Check(FixturePolicy(root, []Setting{{"dockerComposeFile", json.RawMessage(`"compose.yml"`)}}), args); d.Refused {
			t.Errorf("approved: %q refused: %+v", args, d)
		}
	}
	// build and up -d refused; version and config passed.
	if strings.Join(refused, " ") != "build -d" {
		t.Errorf("refused %q, want build and up", refused)
	}
}

// --cache-from is read as buildx reads it (CSV, keys lowercased, the last
// type winning, a value with no "=" a registry reference). The three values
// round 1 of the review of #78 measured buildx importing as local caches
// from a host path, which a substring test passed, are refused — and the
// approved form of each passes.
func TestCacheFromIsReadAsBuildxReadsIt(t *testing.T) {
	root := t.TempDir()
	build := command(t, Recorded(t, "dockerfile", root), "buildx", "build")
	for _, v := range []string{
		"TYPE=local,src=/x",
		"type=registry,ref=x,type=local,src=/x",
		"type=local,src=/x/type=registry",
		"type=local,src=/x",
		"Type=Local,Src=/x",
		"src=/x",                           // no type: buildx refuses it, and so does the guard
		"ref=x",                            // no type either
		`type=registry,"ref=x,type=local"`, // quoting is where CSV readers differ
		"type=registry\ntype=local",
		"type=gha",
		"type=s3,region=x",
		"type=registry,ref", // a field without "="
	} {
		if CacheFromIsRegistry(v) {
			t.Errorf("%q read as a registry cache", v)
		}
		args := insert(build, 2, "--cache-from", v)
		if d := Check(FixturePolicy(root, nil), args); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingCacheFrom}) {
			t.Errorf("%q: %+v", v, d)
		}
		ok, _ := json.Marshal([]string{v})
		if d := Check(FixturePolicy(root, []Setting{{"build.cacheFrom", ok}}), args); d.Refused {
			t.Errorf("%q, approved: %+v", v, d)
		}
	}
	for _, v := range []string{"ghcr.io/x/y:cache", "type=registry,ref=ghcr.io/x/y:cache", "TYPE=registry,ref=x",
		"type=local,src=/x,type=registry,ref=y", ""} {
		if !CacheFromIsRegistry(v) {
			t.Errorf("control %q not read as a registry cache", v)
		}
	}
}

// A volume is this container's alone only when its name carries this
// workspace's devcontainerId and no other's.
func TestOwnVolumeIsExact(t *testing.T) {
	root := t.TempDir()
	run := command(t, Recorded(t, "image", root), "run")
	id := DevcontainerID(FixtureLabels)
	other := strings.Repeat("a", 52)
	for name, c := range map[string]struct {
		src  string
		pass bool
	}{
		"this workspace's":       {"cache-" + id, true},
		"twice this workspace's": {id + "-" + id, true},
		"this and another's":     {"cache-" + id + "-" + other, false},
		"another's":              {"cache-" + other, false},
		"a prefix of this one's": {"cache-" + id[:51], false},
		"no id":                  {"cache", false},
		"this one's, a bad name": {"/" + id, false},
	} {
		d := Check(FixturePolicy(root, nil), insert(run, 1, "--mount", "type=volume,source="+c.src+",target=/c"))
		if d.Refused == c.pass {
			t.Errorf("%s (%s): %+v", name, c.src, d)
		}
	}
}

// docker reads a mount's type case-insensitively, and so does the guard: a
// BIND of / is a bind, refused; and the clone's own bind passes as BIND too.
func TestMountTypeCase(t *testing.T) {
	root := t.TempDir()
	run := command(t, Recorded(t, "image", root), "run")
	if d := Check(FixturePolicy(root, nil), insert(run, 1, "--mount", "Type=BIND,Source=/,Target=/h")); !d.Refused {
		t.Errorf("a BIND of /: %+v", d)
	}
	if d := Check(FixturePolicy(root, nil), insert(run, 1, "--mount", "type=BIND,source="+filepath.Join(root, "repo")+",target=/w")); d.Refused {
		t.Errorf("the clone as BIND: %+v", d)
	}
}

// An approved bind mount of a path in the clone is refused once the path has
// become a link out of the clone — docker follows it — and passes while it
// is a directory, or a link that stays inside.
func TestApprovedBindInTheCloneStaysPut(t *testing.T) {
	root := t.TempDir()
	run := command(t, Recorded(t, "image", root), "run")
	clone := filepath.Join(root, "repo")
	data := filepath.Join(clone, "data")
	approved := []Setting{{"mounts", json.RawMessage(`["type=bind,source=${localWorkspaceFolder}/data,target=/data"]`)}}
	args := insert(run, 1, "--mount", "type=bind,source="+data+",target=/data")
	os.MkdirAll(data, 0o700)
	if d := Check(FixturePolicy(root, approved), args); d.Refused {
		t.Fatalf("control, a directory: %+v", d)
	}
	os.RemoveAll(data)
	os.MkdirAll(filepath.Join(clone, "inner"), 0o700)
	os.Symlink(filepath.Join(clone, "inner"), data)
	if d := Check(FixturePolicy(root, approved), args); d.Refused {
		t.Errorf("a link inside the clone: %+v", d)
	}
	os.Remove(data)
	os.Symlink("/", data)
	if d := Check(FixturePolicy(root, approved), args); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingMounts}) {
		t.Errorf("a link to /: %+v", d)
	}
	// A bind outside the clone, approved, is the host's path as written.
	out := []Setting{{"mounts", json.RawMessage(`["type=bind,source=/etc/hostname,target=/h"]`)}}
	if d := Check(FixturePolicy(root, out), insert(run, 1, "--mount", "type=bind,source=/etc/hostname,target=/h")); d.Refused {
		t.Errorf("an approved bind outside the clone: %+v", d)
	}
}
